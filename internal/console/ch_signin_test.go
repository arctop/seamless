package console

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// chDeepLink is a console address whose query has more than one parameter:
// the sign-in redirect once kept only the first (?w=24h), because the whole
// address rode in next= unescaped.
const chDeepLink = "/console/sessions?w=24h&retained=no"

// chLoginNext follows a redirect to the login page and returns the address
// its next= names, decoded once, as the login page reads it.
func chLoginNext(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "/console/login", loc.Path)
	require.Len(t, loc.Query(), 1, "next is the login address's only parameter: %s", loc)
	return loc.Query().Get("next")
}

// With the key: an expired cookie on a deep link signs in back to the whole
// link, every parameter kept, and the page it lands on is that view.
func TestChSignInKeepsTheWholeDeepLink(t *testing.T) {
	mux := newTestMux(t)

	rr := do(mux, httptest.NewRequest(http.MethodGet, chDeepLink, nil))
	require.Equal(t, "/console/login?next="+url.QueryEscape(chDeepLink), rr.Header().Get("Location"))
	require.Equal(t, chDeepLink, chLoginNext(t, rr))

	login := do(mux, httptest.NewRequest(http.MethodGet, rr.Header().Get("Location"), nil))
	require.Equal(t, http.StatusOK, login.Code)
	require.Contains(t, login.Body.String(), `<input type="hidden" name="next" value="/console/sessions?w=24h&amp;retained=no">`)

	form := url.Values{"key": {testKey}, "next": {chDeepLink}}
	req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signed := do(mux, req)
	require.Equal(t, http.StatusSeeOther, signed.Code)
	require.Equal(t, chDeepLink, signed.Header().Get("Location"))

	back := httptest.NewRequest(http.MethodGet, signed.Header().Get("Location"), nil)
	back.AddCookie(consoleCookie())
	page := do(mux, back)
	require.Equal(t, http.StatusOK, page.Code)

	// Still only console addresses: a next that leaves the console, escaped or
	// not, signs in to the Overview.
	for _, next := range []string{"https://example.com/console/", "//example.com/console/", "/console//example.com"} {
		form := url.Values{"key": {testKey}, "next": {next}}
		req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		require.Equal(t, "/console/", do(mux, req).Header().Get("Location"), next)
	}
}

// Under passkey sign-in the same redirect, the login page's data-next and the
// finish's answer carry the whole link too.
func TestChPasskeySignInKeepsTheWholeDeepLink(t *testing.T) {
	sc := signinNewConsole(t, nil)
	a := signinAuthenticator(t)
	sc.signinEnroll(t, a, "laptop")
	b := signinNewBrowser(t)

	rr := sc.do(nil, b.request(t, http.MethodGet, chDeepLink, nil))
	require.Equal(t, chDeepLink, chLoginNext(t, rr))

	login := sc.do(nil, b.request(t, http.MethodGet, rr.Header().Get("Location"), nil))
	require.Equal(t, http.StatusOK, login.Code)
	m := regexp.MustCompile(`data-next="([^"]*)"`).FindStringSubmatch(login.Body.String())
	require.Len(t, m, 2, "the sign-in form names where to go")
	next := html.UnescapeString(m[1])
	require.Equal(t, chDeepLink, next)

	begin := sc.do(b, b.request(t, http.MethodPost, "/console/signin/begin", url.Values{}))
	require.Equal(t, http.StatusOK, begin.Code, begin.Body.String())
	begun := signinJSON(t, begin)
	asserted, err := a.Get(begun["options"], approverOrigin)
	require.NoError(t, err)
	finish := sc.do(b, b.request(t, http.MethodPost, "/console/signin/finish", url.Values{
		"ticket": {signinString(t, begun, "ticket")}, "response": {string(asserted)}, "next": {next}}))
	require.Equal(t, http.StatusOK, finish.Code, finish.Body.String())
	require.Equal(t, chDeepLink, signinString(t, signinJSON(t, finish), "next"))
}
