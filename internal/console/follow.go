package console

import "net/http"

// The in-place client (static/navigation.js) sends the owner's POSTs with
// fetch(), and fetch() follows a redirect out of sight: the redirect's own
// headers never reach the page, and a Response's url never carries a
// fragment. A POST answered "303 See Other" to
// /console/settings?s=backups&notice=...#backups would land on the section but
// never on the block its #anchor names. So the client asks, with followHeader,
// for the redirect to be named instead (204, its location in locationHeader),
// and fetches that location itself, fragment kept.
//
// Only a request that asks gets the named form. A browser without script, the
// seam CLI, and a page still running an older navigation.js (the console is
// one document that outlives a daemon update) get the redirect as before.
const (
	followHeader   = "X-Seam-Follow"
	locationHeader = "X-Seam-Location"
)

// followable answers h's redirects as named locations when the request asks to
// follow them itself. post() wraps every owner POST in it, so a redirect added
// later is covered without knowing about it.
func followable(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(followHeader) != "1" {
			h(w, r)
			return
		}
		h(&followWriter{ResponseWriter: w}, r)
	}
}

// followWriter turns a redirect the browser would follow with a GET (301, 302,
// 303) into a 204 that names its location. A 307 or 308 repeats the method and
// body, which only the browser can do, so it passes through, as does every
// answer that is not a redirect.
type followWriter struct {
	http.ResponseWriter
	named bool
}

func (f *followWriter) WriteHeader(code int) {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther:
		h := f.Header()
		if loc := h.Get("Location"); loc != "" {
			h.Del("Location")
			h.Set(locationHeader, loc)
			// The answer is a pointer, not a page: never store it.
			h.Set("Cache-Control", "no-store")
			f.named = true
			code = http.StatusNoContent
		}
	}
	f.ResponseWriter.WriteHeader(code)
}

// Write drops a named redirect's body (http.Redirect writes one only for a GET,
// but a 204 must carry none whatever wrote it).
func (f *followWriter) Write(b []byte) (int, error) {
	if f.named {
		return len(b), nil
	}
	return f.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (f *followWriter) Unwrap() http.ResponseWriter { return f.ResponseWriter }
