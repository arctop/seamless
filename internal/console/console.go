// Package console serves the Seamless observability UI: server-rendered
// html/template pages plus an SSE feed, with no node/npm/React or build step.
// It is read-mostly -- the writes are the owner's overrides and curation
// actions: archiving a memory, approving a plan, force-releasing a task's claim
// lock, asking the gardener for proposals (request/split) and resolving them
// (apply/dismiss/retarget), saving or resetting the briefing settings, and
// steering updates: switching the background update check and automatic
// updates, asking it to check now, installing the newest release now, and
// resuming automatic updates after they paused themselves.
// Access is guarded by the same static bearer key as the MCP surface: a browser
// trades the key for a cookie at /console/login, and the seam CLI presents the
// key as a bearer token.
package console

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// cookieName holds the console session token (a hash of the static key, never
// the key itself). It is HttpOnly and scoped to /console.
const cookieName = "seamless_console"

const (
	formBodySmall    int64 = 8 << 10
	formBodyStandard int64 = 16 << 10
	formBodyFamily   int64 = 64 << 10
)

// Config wires the console's dependencies. Files/Gardener/Events are used by the
// write actions (archive, apply/dismiss) and the live feed; DB backs every read.
type Config struct {
	// DB serves every console read. Under peer_auth, with projects sealed to
	// other OS accounts, it is a filtered read-only handle on which those
	// projects do not exist (store.OpenView), and the writes go to WriteDB.
	DB *sql.DB
	// WriteDB is where console writes go when DB is a read-only filtered
	// handle; nil means DB itself (peer_auth off, or nothing sealed). A write
	// names its target by id or slug, and the handler reads that target through
	// DB first, so a project hidden from DB cannot be written either.
	WriteDB  *sql.DB
	Files    *files.Manager
	Gardener *gardener.Service
	Events   *events.Recorder
	// Retrieve backs the search page and command palette: the same fused
	// FTS+semantic engine recall uses, through its human-facing Search entry
	// point. A nil Retrieve degrades search to the structured entities only.
	Retrieve    *retrieve.Service
	APIKey      string
	DataDir     string // for resolving memory/note file paths to absolute editor links
	ConfigPath  string // absolute path of the config file this daemon loaded; empty when config is env-only
	DBPath      string // absolute path of the SQLite database, for the Settings storage panel
	Budgets     config.Budgets
	GardenerCfg config.Gardener // for the Settings page (read-only display)
	// Embedding describes the embedder this process resolved at serve start.
	// It is fixed for the daemon's life: the console's on/off override and any
	// config change apply from the next restart, and the Settings page uses the
	// gap between this and the stored override to say so.
	Embedding EmbeddingRuntime
	// BriefingCfg is the file/env briefing base the Settings page edits: the
	// form's effective values are this plus the store's override row, and a
	// save writes the override (never the file).
	BriefingCfg config.Briefing
	// Features is the file/env optional-features base. The effective state is
	// this plus the store's override row, resolved live per request (see
	// effectiveFeatures) so a Settings save applies without a restart.
	Features config.Features
	// Version is the daemon's build version (seamlessd's buildVersion), shown
	// on the Home health strip and in Settings -> Your setup. Empty omits it.
	Version string
	// Updates is the background update check (*update.Checker in the daemon):
	// Settings -> Updates, the health strip's version fact, the one-day
	// "updated" banner and the automatic-update trouble banner read it, and
	// the Updates POSTs steer it. nil means this process runs no update check
	// (tests, tools): the section says so, its POSTs answer with an error
	// flash, and the strip keeps the plain version.
	Updates UpdatesView
	// Level is the file/env console level base (config.Console.Level): how much
	// of the console the owner sees. The effective level is this plus the
	// store's level row, resolved live per request (see consoleLevel). Empty
	// means the config default; any other value must be one of
	// config.ConsoleLevels, or New refuses it.
	Level string
	// SessionIdleTTL is the configured live/idle threshold for session displays
	// (gardener.session_idle_minutes); <= 0 falls back to core.SessionIdleTTL.
	SessionIdleTTL time.Duration
	// SecureCookies marks the console session cookie Secure. It follows
	// config.TLSEnabled and nothing else: a browser silently DROPS a Secure
	// cookie that arrives over http, so setting it on a plaintext listener
	// would lock the owner out of their own console with no error anywhere.
	SecureCookies bool
	Logger        *slog.Logger
}

// Service renders the console and serves its routes.
type Service struct {
	cfg       Config
	logger    *slog.Logger
	pages     map[string]*template.Template
	fragments map[string]*template.Template // peek-body fragments, keyed by entity
	// host names the machine this daemon runs on, for the sidebar account row.
	// Resolved once at construction: it cannot change for the process.
	//
	// It reads "server <name>" rather than a bare hostname because the console
	// may now be open on a different machine than the daemon: the row used to
	// say "this machine", which stopped being true the moment a second device
	// could reach it. An unreadable hostname degrades to "server" alone -- never
	// to a guess, and never back to "this machine".
	host string
	// hostName is the bare, normalized hostname behind host ("boron"), so a
	// list can tag only the sessions that ran on a DIFFERENT machine instead
	// of stamping this one's name on every row.
	hostName string
	// baseLevel is cfg.Level parsed once: the level in force when no stored
	// row overrides it.
	baseLevel level
	// sky remembers the Overview's last few knowledge-sky charts, so a live
	// refresh sends a page only what changed in the chart it holds.
	sky skyLedger
	// stats gates the retrieval-stats rebuild the analytics screens ask for
	// on every render (freshRetrievalStats).
	stats statsGate
}

// New builds a console Service, parsing its templates once.
func New(cfg Config) (*Service, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = slog.New(clientGoneHandler{logger.Handler()})
	baseName := cfg.Level
	if baseName == "" {
		baseName = config.Defaults().Console.Level
	}
	baseLevel, err := parseLevel(baseName)
	if err != nil {
		return nil, fmt.Errorf("console.New: %w", err)
	}
	pages, fragments, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	host, hostName := "server", ""
	if name, herr := os.Hostname(); herr == nil && strings.TrimSpace(name) != "" {
		host = "server " + strings.TrimSpace(name)
		hostName = normHost(name)
	}
	return &Service{
		cfg: cfg, logger: logger, pages: pages, fragments: fragments,
		host: host, hostName: hostName, baseLevel: baseLevel,
	}, nil
}

// Register mounts the console routes on mux under /console. Public routes are the
// login page and the stylesheet; everything else requires the key.
//
// Every route -- public included -- is registered through `secured`, which
// attaches the console's security response headers. Going through one helper
// rather than each handler setting its own is what makes that coverage
// structural: a route added later cannot forget.
func (s *Service) Register(mux *http.ServeMux) {
	handle := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, secured(h)) }
	post := func(pattern string, maxBytes int64, h http.HandlerFunc) {
		handle(pattern, s.auth(s.parseForm(maxBytes, h)))
	}
	// gated composes into the same helpers, so an optional feature's routes
	// inherit security headers and auth exactly like every other route and can
	// only ever add the feature check on top.
	gated := func(key features.Key, h http.HandlerFunc) http.HandlerFunc { return s.gate(key, h) }

	handle("GET /console/static/console.css", s.serveCSS)
	handle("GET /console/static/interactions.js", s.serveJS)
	handle("GET /console/static/search.js", s.serveSearchJS)
	handle("GET /console/static/library.js", s.serveLibraryJS)
	handle("GET /console/static/charts.js", s.serveChartsJS)
	handle("GET /console/static/navigation.js", s.serveNavigationJS)
	handle("GET /console/static/shell.js", s.serveShellJS)
	handle("GET /console/static/sky.js", s.serveSkyJS)
	handle("GET /console/static/favicon.svg", s.serveFavicon)
	handle("GET /console/login", s.loginForm)
	handle("POST /console/login", s.parseForm(formBodySmall, s.loginSubmit))
	post("POST /console/logout", formBodySmall, s.logout)

	handle("GET /console/{$}", s.auth(s.overview))
	handle("GET /console/now", s.auth(s.nowPage))
	handle("GET /console/since", s.auth(s.sinceDigest))
	handle("GET /console/search", s.auth(s.searchPage))
	handle("GET /console/interactions", s.auth(s.interactions))
	handle("GET /console/sessions", s.auth(s.sessionsList))
	handle("GET /console/sessions/{id}", s.auth(s.sessionDetail))
	handle("GET /console/memories", s.auth(s.memoriesList))
	handle("GET /console/memories/{id}", s.auth(s.memoryDetail))
	post("POST /console/memories/{id}/archive", formBodySmall, s.memoryArchive)
	handle("GET /console/notes", s.auth(s.notesList))
	handle("GET /console/notes/{id}", s.auth(s.noteDetail))
	handle("GET /console/retrieval", s.auth(s.retrieval))
	handle("GET /console/tasks", s.auth(s.tasks))
	handle("GET /console/tasks/{id}", s.auth(s.taskDetail))
	post("POST /console/tasks/{id}/release", formBodySmall, s.taskRelease)
	handle("GET /console/plans", s.auth(s.plansList))
	handle("GET /console/plans/{slug}", s.auth(s.planDetail))
	post("POST /console/plans/{slug}/approve", formBodySmall, s.planApprove)
	handle("GET /console/labs", s.auth(gated(features.Research, s.labsList)))
	handle("GET /console/labs/{name...}", s.auth(gated(features.Research, s.labDetail)))
	handle("GET /console/trials", s.auth(gated(features.Research, s.trialsList)))
	handle("GET /console/trials/{id}", s.auth(gated(features.Research, s.trialDetail)))
	handle("GET /console/projects", s.auth(s.projectsList))
	handle("GET /console/projects/{slug}", s.auth(s.projectDetail))
	post("POST /console/projects/{slug}/isolation", formBodySmall, s.projectIsolationSet)
	handle("GET /console/context", s.auth(s.contextView))
	handle("GET /console/relations", s.auth(s.relationsRedirect))
	handle("GET /console/gardener", s.auth(s.gardenerPage))
	handle("GET /console/gardener/{id}", s.auth(s.gardenerDetail))
	post("POST /console/gardener/request", formBodyStandard, s.gardenerRequest)
	post("POST /console/gardener/split", formBodyStandard, s.gardenerSplit)
	post("POST /console/gardener/group/dismiss", formBodySmall, s.gardenerGroupDismiss)
	post("POST /console/gardener/plan/{slug}/apply", formBodySmall, s.gardenerApplyPlan)
	post("POST /console/gardener/{id}/apply", formBodySmall, s.gardenerApply)
	post("POST /console/gardener/{id}/dismiss", formBodySmall, s.gardenerDismiss)
	post("POST /console/gardener/{id}/hide", formBodySmall, s.gardenerHide)
	post("POST /console/gardener/{id}/unhide", formBodySmall, s.gardenerUnhide)
	post("POST /console/gardener/{id}/retarget", formBodySmall, s.gardenerRetarget)
	post("POST /console/gardener/{id}/undo", formBodySmall, s.gardenerUndo)
	post("POST /console/favorites/{kind}/{id}", formBodySmall, s.favoriteToggle)
	handle("GET /console/settings", s.auth(s.settings))
	post("POST /console/settings/briefing", formBodyStandard, s.settingsBriefingSave)
	post("POST /console/settings/briefing/reset", formBodySmall, s.settingsBriefingReset)
	// The preview is a read that POSTs only to carry the unsaved form; it
	// writes nothing (briefing_preview.go).
	handle("GET /console/settings/briefing/preview", s.auth(s.settingsBriefingPreview))
	post("POST /console/settings/briefing/preview", formBodyStandard, s.settingsBriefingPreview)
	post("POST /console/settings/utility", formBodySmall, s.settingsUtilityForce)
	post("POST /console/settings/embeddings/mode", formBodySmall, s.settingsEmbeddingsMode)
	post("POST /console/settings/embeddings/reembed", formBodySmall, s.settingsEmbeddingsReembed)
	post("POST /console/settings/families/save", formBodyFamily, s.settingsFamilySave)
	post("POST /console/settings/families/delete", formBodySmall, s.settingsFamilyDelete)
	post("POST /console/settings/level", formBodySmall, s.settingsLevelSave)
	post("POST /console/settings/level/reset", formBodySmall, s.settingsLevelReset)
	post("POST /console/settings/level/welcome", formBodySmall, s.settingsLevelWelcome)
	post("POST /console/settings/features", formBodySmall, s.settingsFeaturesSave)
	post("POST /console/settings/features/reset", formBodySmall, s.settingsFeaturesReset)
	post("POST /console/settings/updates", formBodySmall, s.settingsUpdatesSave)
	post("POST /console/settings/updates/reset", formBodySmall, s.settingsUpdatesReset)
	post("POST /console/settings/updates/check", formBodySmall, s.settingsUpdatesCheck)
	post("POST /console/settings/updates/auto", formBodySmall, s.settingsUpdatesAuto)
	post("POST /console/settings/updates/apply", formBodySmall, s.settingsUpdatesApply)
	post("POST /console/settings/updates/resume", formBodySmall, s.settingsUpdatesResume)
	handle("GET /console/events", s.auth(s.sse))
	handle("GET /console/events/{id}", s.auth(s.eventDetail))
	// Anything under /console/ no route above claims: the styled 404 inside the
	// console chrome, never Go's bare plaintext page. Authenticated first, so an
	// unknown path cannot render the sidebar's counts to a stranger.
	handle("GET /console/", s.auth(s.unknownPage))
}

// writeDB is the handle console writes go to (Config.WriteDB).
func (s *Service) writeDB() *sql.DB {
	if s.cfg.WriteDB != nil {
		return s.cfg.WriteDB
	}
	return s.cfg.DB
}

// parseForm is the structural body boundary for every console POST. Auth wraps
// it on protected routes, so an invalid credential is rejected before any body
// is consumed. The console's own forms and CLI both use URL encoding; a present
// body with another or malformed media type is an error, never an empty form.
func (s *Service) parseForm(maxBytes int64, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		if r.ContentLength > maxBytes {
			s.formError(w, r, http.StatusRequestEntityTooLarge, "form body exceeds the allowed size")
			return
		}
		if r.ContentLength != 0 {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/x-www-form-urlencoded" {
				s.formError(w, r, http.StatusBadRequest, "form body must use application/x-www-form-urlencoded")
				return
			}
		}
		if err := r.ParseForm(); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				s.formError(w, r, http.StatusRequestEntityTooLarge, "form body exceeds the allowed size")
				return
			}
			s.formError(w, r, http.StatusBadRequest, "could not parse form body")
			return
		}
		next(w, r)
	}
}

func (s *Service) formError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if wantsJSON(r) {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	http.Error(w, msg, status)
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

// auth wraps a handler so it runs only for an authenticated request: a valid
// console cookie (browser) or the static bearer key (CLI). Unauthenticated
// browsers are redirected to the login page; JSON callers get a 401.
//
// Cookie-authenticated writes additionally have to prove they came from this
// origin (see sameOriginRequest). The bearer path skips that check: a header
// the caller had to set by hand is not something a browser attaches on its own,
// so it cannot be forged by a page the operator merely visited.
func (s *Service) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// peer_auth: the console is the owner's surface. A configured OS account
		// on the socket works through its agents' tools, which carry its reach;
		// the console's pages are built for the owner's.
		if !access.OwnerOnly(r.Context()) {
			if wantsJSON(r) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: the console is the owner's surface"})
				return
			}
			http.Error(w, "forbidden: the console is the owner's surface", http.StatusForbidden)
			return
		}
		switch s.classify(r) {
		case credBearer:
			next(w, r)
		case credCookie:
			if !safeMethod(r.Method) && !sameOriginRequest(r) {
				s.logger.Warn("console: rejected cross-origin state-changing request",
					"path", r.URL.Path, "origin", r.Header.Get("Origin"),
					"sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
				http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
				return
			}
			next(w, r)
		default:
			if wantsJSON(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: bearer key required"})
				return
			}
			http.Redirect(w, r, "/console/login?next="+safeNext(r.URL.RequestURI()), http.StatusSeeOther)
		}
	}
}

// authed reports whether the request carries a valid credential, ignoring which
// one. Callers that only need "is this the owner" -- the login page deciding
// whether to redirect -- use this; the write path uses classify instead.
func (s *Service) authed(r *http.Request) bool {
	return s.classify(r) != credNone
}

// cookieEquals constant-time-compares the request's console cookie to the token
// derived from key.
func cookieEquals(r *http.Request, key string) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	want := consoleToken(key)
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

// consoleToken derives the cookie value from the static key, so the raw key is
// never stored in the browser's cookie jar.
func consoleToken(key string) string {
	sum := sha256.Sum256([]byte("seamless-console\x00" + key))
	return hex.EncodeToString(sum[:])
}

// bearerEquals constant-time-compares the request's bearer token to key.
func bearerEquals(r *http.Request, key string) bool {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(key)) == 1
}

// safeNext returns a same-origin relative path to redirect to after login, or
// "/console/" if the candidate is absolute/off-site (open-redirect guard).
func safeNext(raw string) string {
	if strings.HasPrefix(raw, "/console/") && !strings.HasPrefix(raw, "/console//") {
		return raw
	}
	return "/console/"
}

func (s *Service) loginForm(w http.ResponseWriter, r *http.Request) {
	if s.authed(r) {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, "")
}

func (s *Service) loginSubmit(w http.ResponseWriter, r *http.Request) {
	key := s.cfg.APIKey
	given := strings.TrimSpace(r.PostFormValue("key"))
	if key == "" || given == "" || subtle.ConstantTimeCompare([]byte(given), []byte(key)) != 1 {
		s.renderLogin(w, r, "invalid key")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    consoleToken(key),
		Path:     "/console",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   30 * 24 * 3600,
	})
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	// The delete carries the same attributes as the set. A browser matches a
	// cookie for replacement on name/domain/path, and sending the expiry
	// without Secure over https is the shape most likely to leave the original
	// in place -- a "logout" that silently kept the session.
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/console",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/console/login", http.StatusSeeOther)
}

type loginData struct {
	Error         string
	Next          string
	ConfigPath    string
	ConfigEditURL template.URL
	HasConfigFile bool
}

// renderLogin renders the standalone login page (no nav chrome).
func (s *Service) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	configPath := strings.TrimSpace(s.cfg.ConfigPath)
	var configEditURL template.URL
	if configPath != "" {
		_, configEditURL = absAndEditURL("", configPath)
	}
	data := loginData{
		Error:         errMsg,
		Next:          safeNext(r.URL.Query().Get("next")),
		ConfigPath:    configPath,
		ConfigEditURL: configEditURL,
		HasConfigFile: configPath != "",
	}
	tmpl := s.pages["login"]
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "login", data); err != nil {
		s.serverError(w, r, err)
	}
}

func (s *Service) serveCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(consoleCSS)
}

// serveJS serves the shared Interactions client module (value rendering + the
// volume histogram), used by both the live feed and the project-detail tab.
func (s *Service) serveJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(interactionsJS)
}

// serveSearchJS serves the command-palette client, loaded on every page.
func (s *Service) serveSearchJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(searchJS)
}

// serveLibraryJS serves the library-style client (rail selection + in-place
// reader swap on the library screens and Gardener), loaded on every page and
// inert without a #lib-reader.
func (s *Service) serveLibraryJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(libraryJS)
}

// serveChartsJS serves the chart hover readout (see charts.go + static/charts.js),
// loaded on every page: the charts are server-rendered into several of them.
func (s *Service) serveChartsJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(chartsJS)
}

// serveNavigationJS serves the shared partial-navigation client. Query controls
// and owner mutations fetch their next server-rendered state and morph only
// changed view nodes; the SSE refresher uses the same morph path.
func (s *Service) serveNavigationJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(navigationJS)
}

// sinceMaxAge bounds the check-in digest: past it, "since you were last here"
// is no longer a useful frame and the client does not ask.
const sinceMaxAge = 90 * 24 * time.Hour

// sinceDigest answers the Overview's "since you were last here" line:
// GET /console/since?t=<unix ms> returns what the fleet recorded from that
// moment to now. JSON only -- the page renders it. The client remembers when
// the owner last looked (a per-browser convenience); the server only counts.
// A missing, malformed, future, or too-old t is a 400 naming the problem,
// never a silently substituted default.
func (s *Service) sinceDigest(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("t")
	ms, err := strconv.ParseInt(raw, 10, 64)
	now := time.Now()
	if raw == "" || err != nil || ms <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "t must be a unix time in milliseconds"})
		return
	}
	since := time.UnixMilli(ms)
	if since.After(now) || now.Sub(since) > sinceMaxAge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "t must be in the past 90 days"})
		return
	}
	act, err := store.GetActivitySince(r.Context(), s.cfg.DB, since, now)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Since time.Time `json:"since"`
		store.ActivitySince
	}{since.UTC(), act})
}

// unknownPage answers a /console/ path that no route claims with the styled
// in-console 404 (sidebar, a search box, and the way back), so a mistyped or
// stale URL never drops the owner on Go's bare "404 page not found".
func (s *Service) unknownPage(w http.ResponseWriter, r *http.Request) {
	s.notFound(w, r, "Nothing lives at "+r.URL.Path+". The link may be mistyped, or what it pointed at has moved.")
}

// serveShellJS serves the console chrome client: the collapsible sidebar, the
// phone drawer, g-chord shortcuts and the ? sheet, flash-to-toast, and the
// palette's recents. Loaded on every page; each piece is inert without its markup.
func (s *Service) serveShellJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(shellJS)
}

// serveSkyJS serves the knowledge sky's page script, loaded by the Overview
// only: search, filters, and the live layer over the server-drawn chart.
func (s *Service) serveSkyJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(skyJS)
}

func (s *Service) serveFavicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(faviconSVG)
}

// ---------------------------------------------------------------------------
// Overview
// ---------------------------------------------------------------------------

// kindCount pairs a memory kind with its active count, for ordered display.
type kindCount struct {
	Kind string
	N    int
}

// attnCard is one entry in the Overview's attention strip: something waiting on
// the owner, with a severity that orders the strip and a screen that answers it.
// A bucket with nothing in it produces no card at all -- an empty "0 mishaps"
// panel would spend the most valuable strip on the page saying nothing.
type attnCard struct {
	Sev   string // "danger" | "warn" | "info" | "ok" (the finish-line cards, the strip's only positive entries)
	Icon  string
	Title string
	Sub   string
	Href  string
	// Bar is the finish-line card's progress draw (finishBar); empty on every
	// other card, so the rest of the strip's markup is byte-identical.
	Bar template.HTML
}

// liveSessionRow is one chip in the "live now" strip: a session heartbeating
// inside the configured idle window.
type liveSessionRow struct {
	ID      string
	Name    string
	Project string
	Age     string
}

// vital is one of the four Overview cards: a headline number with its
// comparison, the denominator that makes it readable, the threshold it is judged
// against, and the shape behind it. Value is pre-rendered ("61%", "1,620") and
// empty means "not measurable yet" -- which renders an em dash and drops the
// chip, band, and sparkline rather than showing a confident zero. Href is the
// screen that explains the number, carrying the active window so the click is a
// drill-down into the same observation, not a navigation away from it; an
// empty-state card keeps its link because the destination states WHY there is
// nothing, which a dead card cannot.
type vital struct {
	Label     string
	Icon      string
	Value     string
	Delta     delta
	Sub       string
	Band      band
	Spark     spark
	Href      string
	HrefTitle string // tooltip naming what the click answers
}

// overviewData is the pre-computed payload for the overview page.
type overviewData struct {
	Memories         int
	MemByKind        []kindCount
	Notes            int
	SessActive       int // live sessions: active AND within core.SessionIdleTTL, per the board query
	SessTotal        int
	TasksOpen        int
	TasksInProg      int
	TasksDone        int
	Injections       int                 // item-level injection volume in the window
	InjectedTokens   int                 // estimated tokens of injected context in the window (reach's cost side)
	MemoriesSurfaced int                 // distinct active memories surfaced (reach numerator)
	ActiveMemories   int                 // total active memories (reach denominator)
	ReachRate        int                 // MemoriesSurfaced / ActiveMemories, %
	SessionsReached  int                 // distinct sessions that received an injection
	Window           string              // selected retrieval window key ("24h"|"7d"|"30d"|"all")
	WindowLabel      string              // human label for the selected window
	Windows          []windowOption      // the window selector entries
	Trend            []store.TrendBucket // injection trend over the window, for the area chart
	TopInjected      []store.NamedCount
	Pending          int
	Recent           []eventRow
	Coverage         int                    // % of in-window sessions that retained knowledge
	Covered          int                    // in-window sessions with >=1 durable artifact
	CovTotal         int                    // in-window sessions (coverage denominator)
	CoverageRows     []coverageRow          // per-channel breakdown (findings/memories/notes/trials), in-window
	CoverageTrend    []store.CoverageBucket // windowed coverage-rate trend (nil = no in-window sessions)
	Projects         []projectGlanceRow     // top projects by recent activity ("projects at a glance")
	Mishaps          []mishapRow            // latest agent-reported mishaps (recurrence review rail)

	// Status-page surfaces: what needs attention, what is happening right now,
	// and the four judged vitals. Each list is empty rather than zero-filled when
	// its bucket is empty, so the template can drop the whole strip.
	Attention   []attnCard
	Live        []liveSessionRow
	Vitals      []vital
	StaleDays   int // the "going stale" horizon, in days, stated in the card
	StaleUnseen int // active memories that have not surfaced within it

	// Spotlight is the momentum memory-of-the-month rail panel: nil while the
	// feature is off (no panel, no JSON key), non-nil with Found false as the
	// honest empty state.
	Spotlight *spotlightData `json:"spotlight,omitempty"`

	// Sky is the knowledge constellation panel (HTML only; nil for JSON callers
	// and for a fleet with no active memories).
	Sky *skyData `json:"-"`

	// Health is the "is it working?" strip (HTML only): at Basic it leads the
	// page; above Basic it closes the vitals as one quiet line.
	Health []healthFact `json:"-"`

	// HealthLead places the health strip at the top (Basic) rather than as the
	// quiet line under the vitals: placement, not a gate -- both levels show it.
	HealthLead bool `json:"-"`

	// Welcome is the one-time level picker (HTML only), shown until the owner
	// chooses a level or dismisses it; nil once they have.
	Welcome *welcomeCard `json:"-"`
}

// welcomeCard is the Home welcome card: the first-run level choice, made
// non-blocking on purpose -- a newcomer cannot judge a level before seeing
// anything, and a wrong pick costs one click.
type welcomeCard struct {
	// Seeded marks an upgraded installation (the migration chose advanced):
	// the card announces the picker rather than asking a newcomer.
	Seeded bool
	Cards  []levelCard
}

// spotlightData is the memory-of-the-month panel payload: the winner (when
// one qualifies) and a pre-composed counts line backing the claim up.
type spotlightData struct {
	Found  bool                  `json:"found"`
	Memory store.SpotlightMemory `json:"memory"`
	Line   string                `json:"line,omitempty"` // "7 reads · 3 sessions · last 30 days"
}

// spotlightWindowDays is the fixed memory-of-the-month window. It deliberately
// ignores the Overview's ?w= selector: "of the month" is a claim about one
// stated window, not whichever the owner happens to be viewing.
const spotlightWindowDays = 30

// mishapRow is one entry in the overview's recurrence-review rail: an
// agent.mishap event reduced to what triage needs -- what happened, where, and
// which agent reported it (harness + model + session), so a report traces back
// to the exact task through its session.
type mishapRow struct {
	ID          string
	When        time.Time
	Project     string
	SessionID   string
	Harness     string // client discriminator (claude-code|codex), for the agent pill
	Model       string // model powering the reporting session, verbatim
	Description string
}

// projectGlanceRow is one row of the overview's "projects at a glance" table: a
// project's strict per-slug health (reach %, sessions, open tasks, memories,
// last-active) from the same batched query the board uses, so a row reconciles
// with the board exactly.
type projectGlanceRow struct {
	Slug       string
	Live       int
	Sessions   int
	OpenTasks  int
	Memories   int
	ReachRate  int
	HasReach   bool
	LastActive time.Time
}

// windowOption is one entry in the retrieval-health window selector: a stable key
// (the ?w= value), its display label, and whether it is the active selection.
type windowOption struct {
	Key    string
	Label  string
	Active bool
	// Wider marks the options past the active one: the choices a "nothing
	// older is shown" strip offers when a windowed list looks thin.
	Wider bool
}

// windowLabels maps a retrieval window key to its selector label. Every window
// selector in the console speaks this one vocabulary (Search adds "1y"); prose
// that names a window uses the store's lowercase RetrievalWindow.Label instead.
var windowLabels = map[string]string{"24h": "24h", "7d": "7d", "30d": "30d", "all": "All time"}

// windowOptions builds the ordered selector entries, flagging the active key.
func windowOptions(active string) []windowOption {
	out := make([]windowOption, 0, len(store.RetrievalWindowKeys))
	past := false
	for _, k := range store.RetrievalWindowKeys {
		out = append(out, windowOption{Key: k, Label: windowLabels[k], Active: k == active, Wider: past})
		if k == active {
			past = true
		}
	}
	return out
}

// coverageRow is one retention channel in the overview's "retained via"
// breakdown: how many sessions left this kind of artifact, and its share of all
// sessions (channels overlap, so the shares need not sum to 100%). Color is a
// server-controlled CSS token, safe to inline into the bar.
type coverageRow struct {
	Label string
	Count int
	Pct   int
	Color string
}

// coverageRows projects a SessionCoverage roll-up into the ordered channel rows
// the overview renders, each bar sized as its share of all sessions. The Trials
// channel is a research surface: it is dropped while that feature is off rather
// than reported as a flat 0%, which would read as a retention failure instead of
// a channel the owner switched off.
func coverageRows(c store.SessionCoverage, research bool) []coverageRow {
	rows := []coverageRow{
		{"Findings", c.Findings, percent(c.Findings, c.Total), "var(--brand)"},
		{"Memories", c.Memories, percent(c.Memories, c.Total), "var(--ok)"},
		{"Notes", c.Notes, percent(c.Notes, c.Total), "var(--pop)"},
	}
	if research {
		rows = append(rows, coverageRow{"Trials", c.Trials, percent(c.Trials, c.Total), "var(--warn)"})
	}
	return rows
}

func (s *Service) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Freshen retrieval stats so the numbers match the live event log.
	s.freshRetrievalStats(ctx)
	// Only the counts: the summary's event-log aggregates (the all-time
	// funnel above all) appear nowhere on this page, which re-renders on
	// every live event.
	sum, err := store.GetUsageCounts(ctx, s.cfg.DB)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	recent, err := s.recentEvents(ctx, 6)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	mishaps, err := s.recentMishaps(ctx, 6)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	now := time.Now()
	win := store.ResolveRetrievalWindow(r.URL.Query().Get("w"), now)
	// The Basic Home carries nothing analytic: the vitals, the window, the
	// workspaces table, and the rail are Standard screens' furniture. Their
	// queries run only when something will show them -- but a JSON caller always
	// gets the complete answer, whatever the level (hidden, not different).
	lvlState := s.consoleLevel(ctx)
	lvl := lvlState.Level
	analytics := wantsJSON(r) || lvl >= levelStandard
	var (
		report   store.RetrievalReport
		cov      store.SessionCoverage
		covTrend []store.CoverageBucket
		board    []store.ProjectBoardRow
	)
	if analytics {
		if report, err = store.BuildRetrievalReport(ctx, s.cfg.DB, win, 5); err != nil {
			s.serverError(w, r, err)
			return
		}
		if cov, err = store.GetSessionCoverage(ctx, s.cfg.DB, win.Since); err != nil {
			s.serverError(w, r, err)
			return
		}
		if covTrend, err = store.SessionCoverageBuckets(ctx, s.cfg.DB, win, now); err != nil {
			s.serverError(w, r, err)
			return
		}
		// "Projects at a glance": the top projects by recent activity, from the
		// same batched board query -- strict per-slug counts, so these rows equal
		// the board's rows exactly. The global ("") scope is not a project row.
		if board, err = store.ProjectsWithCounts(ctx, s.cfg.DB, win, now, s.cfg.SessionIdleTTL); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	// Live sessions across every scope (global "" included), TTL-aware via the
	// board query, so the headline matches the Sessions screen instead of the raw
	// active status count an idle session inflates until the reaper runs.
	liveSessions := 0
	glance := make([]projectGlanceRow, 0, len(board))
	for _, b := range board {
		liveSessions += b.LiveSessions
		if b.Project == "" {
			continue
		}
		glance = append(glance, projectGlanceRow{
			Slug: b.Project, Live: b.LiveSessions, Sessions: b.Sessions,
			OpenTasks: b.OpenTasks, Memories: b.Memories,
			ReachRate: b.ReachRate, HasReach: b.Active > 0, LastActive: b.LastActive,
		})
	}
	sort.SliceStable(glance, func(i, j int) bool {
		if !glance[i].LastActive.Equal(glance[j].LastActive) {
			return glance[i].LastActive.After(glance[j].LastActive)
		}
		return glance[i].Slug < glance[j].Slug
	})
	if len(glance) > 8 {
		glance = glance[:8]
	}

	var (
		prior    store.WindowVitals
		hasPrior bool
	)
	if analytics {
		prior, hasPrior = s.priorVitals(ctx, win, now)
	}
	staleUnseen, err := store.CountMemoriesUnsurfacedSince(ctx, s.cfg.DB, now.UTC().AddDate(0, 0, -staleSurfacedDays))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pending := sumValues(sum.GardenerPending)
	live := s.liveSessions(ctx, now, 6)

	data := overviewData{
		Trend:            report.Trend,
		CoverageTrend:    covTrend,
		Memories:         sum.Memories.Active,
		MemByKind:        orderKinds(sum.Memories.ByKind),
		Notes:            sum.Notes,
		SessActive:       liveSessions,
		SessTotal:        sumValues(sum.Sessions),
		TasksOpen:        sum.Tasks[string(core.TaskOpen)],
		TasksInProg:      sum.Tasks[string(core.TaskInProgress)],
		TasksDone:        sum.Tasks[string(core.TaskDone)],
		Injections:       report.Injected,
		InjectedTokens:   report.InjectedTokens,
		MemoriesSurfaced: report.MemoriesSurfaced,
		ActiveMemories:   report.ActiveMemories,
		ReachRate:        report.ReachRate,
		SessionsReached:  report.SessionsReached,
		Window:           win.Key,
		WindowLabel:      win.Label,
		Windows:          windowOptions(win.Key),
		TopInjected:      sum.Retrieval.TopInjected,
		Pending:          pending,
		Recent:           recent,
		Coverage:         percent(cov.Covered, cov.Total),
		Covered:          cov.Covered,
		CovTotal:         cov.Total,
		CoverageRows:     coverageRows(cov, features.Enabled(s.effectiveFeatures(ctx), features.Research)),
		Projects:         glance,
		Mishaps:          mishaps,
		Live:             live,
		StaleDays:        staleSurfacedDays,
		StaleUnseen:      staleUnseen,
	}
	data.Attention = s.attentionCards(ctx, data)
	if analytics {
		data.Vitals = overviewVitals(data, report, covTrend, prior, hasPrior, win)
		data.Spotlight = s.memorySpotlight(ctx, now)
	}
	data.Sky = s.knowledgeSky(ctx, now, r)
	if !wantsJSON(r) {
		data.Health = s.healthFacts(ctx)
		data.HealthLead = lvl < levelStandard
		if !lvlState.Welcomed {
			data.Welcome = &welcomeCard{
				Seeded: lvlState.Source == store.ConsoleLevelSeeded,
				Cards:  levelCards(s.effectiveFeatures(ctx), lvl),
			}
		}
	}
	s.render(w, r, "overview", pageData{Title: "Overview", Active: "overview", Data: data})
}

// knowledgeSky builds the Overview's constellation. It is decoration over data
// the page already states elsewhere, so a failed read degrades to no sky (with
// a warning) rather than failing the whole status page. JSON callers never pay
// for it: the panel is HTML-only. A live or in-place fetch from a page that
// already holds a chart gets only what changed since (sky_live.go).
func (s *Service) knowledgeSky(ctx context.Context, now time.Time, r *http.Request) *skyData {
	if wantsJSON(r) {
		return nil
	}
	mems, err := store.AllMemoriesIncludingInvalid(ctx, s.cfg.DB)
	if err != nil {
		s.logger.Warn("console: knowledge sky: memories", "error", err)
		return nil
	}
	stats, err := store.AllRetrievalStats(ctx, s.cfg.DB)
	if err != nil {
		s.logger.Warn("console: knowledge sky: retrieval stats", "error", err)
		return nil
	}
	sky := buildSky(mems, stats, now)
	if sky != nil {
		s.sky.answer(sky, heldSky(r))
	}
	return sky
}

// memorySpotlight assembles the momentum memory-of-the-month panel: nil while
// the feature is off (the query never runs), the honest empty state when no
// active memory earned query-gated utility in the window, and failure-soft --
// a store error costs the panel, never the page.
func (s *Service) memorySpotlight(ctx context.Context, now time.Time) *spotlightData {
	if !features.Enabled(s.effectiveFeatures(ctx), features.Momentum) {
		return nil
	}
	mem, found, err := store.MemorySpotlight(ctx, s.cfg.DB, now.AddDate(0, 0, -spotlightWindowDays), now)
	if err != nil {
		s.logger.Warn("console: memory spotlight", "error", err)
		return nil
	}
	sp := &spotlightData{Found: found, Memory: mem}
	if found {
		sp.Line = fmt.Sprintf("%s · %s · last %d days",
			plural(mem.Reads, "read", "reads"),
			plural(mem.Readers, "session", "sessions"), spotlightWindowDays)
	}
	return sp
}

// attentionCards builds the Overview's attention strip in severity order:
// mishaps first (something went wrong), then the gardener queue (something is
// waiting on a decision), then knowledge going quiet (something is drifting).
// A bucket with nothing in it contributes no card.
func (s *Service) attentionCards(ctx context.Context, d overviewData) []attnCard {
	var out []attnCard
	if n := len(d.Mishaps); n > 0 {
		latest := d.Mishaps[0]
		where := "global scope"
		if latest.Project != "" {
			where = latest.Project
		}
		out = append(out, attnCard{
			Sev: "danger", Icon: "triangle-alert",
			Title: plural(n, "agent-reported mishap", "agent-reported mishaps"),
			Sub:   "latest " + ago(latest.When) + " in " + where + " · review recurrence",
			Href:  "/console/events/" + latest.ID,
		})
	}
	if d.Pending > 0 {
		sub := "waiting for review"
		// The oldest pending proposal is what makes the queue feel stale, and
		// PendingProposals is newest-first, so it is the last row. A failure here
		// costs the age line, not the card.
		if props, err := store.PendingProposals(ctx, s.cfg.DB, ""); err != nil {
			s.logger.Warn("console: pending proposals for attention strip", "error", err)
		} else if len(props) > 0 {
			sub = "oldest " + ago(props[len(props)-1].CreatedAt) + " · merges, retirements, and digests"
		}
		out = append(out, attnCard{
			Sev: "warn", Icon: "sprout",
			Title: plural(d.Pending, "gardener proposal waiting", "gardener proposals waiting"),
			Sub:   sub,
			Href:  "/console/gardener",
		})
	}
	if d.StaleUnseen > 0 {
		out = append(out, attnCard{
			Sev: "info", Icon: "timer",
			Title: plural(d.StaleUnseen, "memory going stale", "memories going stale"),
			Sub:   fmt.Sprintf("not surfaced in %dd · consider retiring", d.StaleDays),
			Href:  "/console/retrieval",
		})
	}
	// Momentum's finish-line cards close the strip: the only positive entry, so
	// the problems keep the lead. Gated in the handler rather than the template
	// because a disabled install must not even run the query; the briefing's
	// plan-line emphasis checks the same effective config, so the two surfaces
	// switch together.
	if features.Enabled(s.effectiveFeatures(ctx), features.Momentum) {
		plans, err := store.FinishLinePlans(ctx, s.cfg.DB)
		if err != nil {
			s.logger.Warn("console: finish-line plans for attention strip", "error", err)
		}
		for _, p := range plans {
			out = append(out, attnCard{
				Sev: "ok", Icon: "flag",
				Title: p.Slug + " -- " + p.FinishLinePhrase(),
				Sub:   "left: " + strings.Join(p.Remaining, " · "),
				Href:  "/console/plans/" + p.Slug,
				Bar:   finishBar(p.PlanRollup),
			})
		}
	}
	return out
}

// overviewVitals assembles the four judged cards. Each one answers a different
// question -- is knowledge reaching the work, is the work leaving knowledge
// behind, how much context is moving, and how far it spreads -- and each renders
// an em dash with no chip, band, or sparkline when its denominator is still
// empty, because a fresh install has no reach to report rather than 0% reach.
func overviewVitals(d overviewData, report store.RetrievalReport, covTrend []store.CoverageBucket,
	prior store.WindowVitals, hasPrior bool, win store.RetrievalWindow,
) []vital {
	note := noteBand(priorLabel(win, hasPrior))

	// Each card links to the screen that explains its number, carrying the
	// active window: three into Retrieval (whose hero and delivery funnel ARE
	// these numbers over the same report), and continuity into the Sessions
	// list filtered to the sessions that retained nothing -- the write-side
	// question Retrieval deliberately does not answer.
	reach := vital{Label: "Memory reach", Icon: "gauge", Sub: "no active memories to reach yet",
		Href: "/console/retrieval?w=" + win.Key, HrefTitle: "Reach detail in Retrieval"}
	if d.ActiveMemories > 0 {
		reach.Value = strconv.Itoa(d.ReachRate) + "%"
		reach.Delta = pointDelta(d.ReachRate, prior.ReachRate, hasPrior, riseGood)
		reach.Sub = fmt.Sprintf("%d of %d active memories surfaced", d.MemoriesSurfaced, d.ActiveMemories)
		reach.Band = floorBand(d.ReachRate, reachTargetPct, true)
		reach.Spark = spark{Points: report.SurfacedTrend, Label: "Distinct memories surfaced per period"}
	}

	continuity := vital{Label: "Knowledge continuity", Icon: "brain", Sub: "no sessions observed in this window",
		Href: "/console/sessions?w=" + win.Key + "&retained=no", HrefTitle: "Sessions that retained nothing"}
	if d.CovTotal > 0 {
		continuity.Value = strconv.Itoa(d.Coverage) + "%"
		continuity.Delta = pointDelta(d.Coverage, prior.Coverage, hasPrior && prior.CovTotal > 0, riseGood)
		continuity.Sub = fmt.Sprintf("%d of %d sessions retained knowledge", d.Covered, d.CovTotal)
		continuity.Band = floorBand(d.Coverage, continuityTargetPct, true)
		// A rate plots against 100, never against its own peak.
		continuity.Spark = spark{Points: coverageRateSeries(covTrend), Tone: "ok", Max: 100, Label: "Share of sessions retaining knowledge per period"}
	}

	injections := vital{Label: "Context injections", Icon: "arrow-down-to-line", Sub: "no context injected in this window",
		Href: "/console/retrieval?w=" + win.Key + "#retrieval-delivery-title", HrefTitle: "Delivery path in Retrieval"}
	if d.Injections > 0 {
		injections.Value = compactNum(d.Injections)
		injections.Delta = volumeDelta(d.Injections, prior.Injections, hasPrior, noJudgment)
		injections.Sub = "~" + compactNum(d.InjectedTokens) + " est. tokens injected"
		injections.Band = note
		injections.Spark = spark{Points: report.Trend, Label: "Injections per period"}
	}

	reached := vital{Label: "Sessions reached", Icon: "terminal", Sub: "no session received shared context yet",
		Href: "/console/retrieval?w=" + win.Key, HrefTitle: "Sessions reached in Retrieval"}
	if d.SessionsReached > 0 {
		reached.Value = compactNum(d.SessionsReached)
		reached.Delta = volumeDelta(d.SessionsReached, prior.SessionsReached, hasPrior, noJudgment)
		reached.Sub = fmt.Sprintf("received shared context, of %d recorded", d.SessTotal)
		reached.Band = note
		reached.Spark = spark{Points: report.SessionTrend, Label: "Distinct sessions reached per period"}
	}

	return []vital{reach, continuity, injections, reached}
}

// coverageRateSeries projects the coverage buckets onto a 0-100 rate series. A
// bucket with no sessions has no rate, and is carried at 0 for the same reason
// coverageTrend does: a quiet stretch is a dip in retention, not a ceiling.
func coverageRateSeries(buckets []store.CoverageBucket) []store.TrendBucket {
	out := make([]store.TrendBucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, store.TrendBucket{Label: b.Label, Count: percent(b.Covered, b.Total)})
	}
	return out
}

// liveSessions lists the sessions heartbeating right now, newest first, for the
// Overview's live strip. Best-effort: the strip is a pulse, not a record, so a
// query failure drops it rather than failing the page.
func (s *Service) liveSessions(ctx context.Context, now time.Time, limit int) []liveSessionRow {
	ttl := s.cfg.SessionIdleTTL
	if ttl <= 0 {
		ttl = core.SessionIdleTTL
	}
	sessions, err := store.ListSessions(ctx, s.cfg.DB, core.SessionActive, now.Add(-ttl), limit*2)
	if err != nil {
		s.logger.Warn("console: live sessions", "error", err)
		return nil
	}
	out := make([]liveSessionRow, 0, limit)
	for _, sess := range sessions {
		if !sess.LiveAsOf(now, ttl) {
			continue
		}
		name := sess.Name
		if name == "" {
			name = shortID(sess.ID)
		}
		project := sess.ProjectSlug
		if project == "" {
			project = "global"
		}
		out = append(out, liveSessionRow{ID: sess.ID, Name: name, Project: project, Age: ago(sess.UpdatedAt)})
		if len(out) == limit {
			break
		}
	}
	return out
}

// orderKinds lists memory kinds in canonical order, dropping absent ones.
func orderKinds(byKind map[string]int) []kindCount {
	var out []kindCount
	for _, k := range core.MemoryKinds {
		if n := byKind[string(k)]; n > 0 {
			out = append(out, kindCount{Kind: string(k), N: n})
		}
	}
	return out
}

func sumValues(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}

// navCounts fills the sidebar badges. Best-effort: a query error yields zeros
// rather than failing the page. Badges belonging to a disabled optional feature
// are zeroed here as well as hidden in the layout, so a stale template can never
// leak a count for a screen the owner switched off.
func (s *Service) navCounts(ctx context.Context, feats config.Features) navCounts {
	n, err := store.GetNavCounts(ctx, s.cfg.DB)
	if err != nil {
		s.logger.Warn("console: nav counts", "error", err)
		return navCounts{}
	}
	counts := navCounts{
		Sessions:  n.Sessions,
		Memories:  n.Memories,
		Notes:     n.Notes,
		Tasks:     n.OpenTasks,
		Proposals: n.PendingProposals,
		Projects:  n.Projects,
		Plans:     n.Plans,
		Labs:      n.Labs,
		Trials:    n.Trials,
	}
	if !features.Enabled(feats, features.Research) {
		counts.Labs, counts.Trials = 0, 0
	}
	// The Now badge is the live agent count -- the one sidebar number that is a
	// pulse rather than an inventory. Same best-effort rule as the rest.
	ttl := s.cfg.SessionIdleTTL
	if ttl <= 0 {
		ttl = core.SessionIdleTTL
	}
	if liveN, lerr := store.LiveSessionCount(ctx, s.cfg.DB, time.Now().UTC().Add(-ttl)); lerr != nil {
		s.logger.Warn("console: nav live count", "error", lerr)
	} else {
		counts.Now = liveN
	}
	return counts
}

// navCounts are the sidebar badge numbers.
type navCounts struct {
	Now       int // live agents right now
	Sessions  int
	Memories  int
	Notes     int
	Tasks     int
	Proposals int
	Projects  int
	Plans     int
	Labs      int
	Trials    int
}
