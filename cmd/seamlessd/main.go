// Command seamlessd is the Seamless server daemon and operator CLI.
//
// Subcommands:
//
//	seamlessd serve         start the HTTP server
//	seamlessd doctor        run configuration + database self-checks
//	seamlessd export        write the whole instance to one archive (corpus + db snapshot + manifest)
//	seamlessd import        import a Seam v1 data directory, or a seamlessd export archive
//	seamlessd install-hooks install agent hooks, MCP, and maintained skills
//	seamlessd client-config print the pairing commands for a client machine
//	seamlessd uninstall     remove Seamless (service, hooks, MCP, skills, binaries)
//	seamlessd update        upgrade in place to the newest release (runs that release's verified installer)
//	seamlessd map-repo      override a repo's auto-derived project slug (rarely needed)
//	seamlessd unmap-repo    remove this machine's mapping for a repo path, or every stale one
//	seamlessd retire-project mark a mistakenly minted, empty project retired (or --undo)
//	seamlessd family        manage project families
//	seamlessd console-open  open the console in a browser, pre-authenticated
//	seamlessd start         start|stop|restart|status the installed service
//	seamlessd version       print the version
package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arctop/seamless/internal/a2a"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// version is the seamlessd build version, injected from the git tag at build
// time: goreleaser sets it via -X main.version (see .goreleaser.yaml) and so
// does `make build`/`make install` (see the Makefile's VERSION). The git tag is
// the single source of truth -- this default is a non-release dev sentinel
// (never a real version number, which would silently go stale here). A build
// with no ldflags (`go install ...@vX.Y.Z`, a plain `go build`) has it replaced
// at startup from the binary's build info (applyBuildInfo, buildinfo.go);
// `go test` and -buildvcs=false builds keep it.
var version = devVersion

// commit and buildDate are link-time build metadata, set via
//
//	go build -ldflags "-X main.commit=$(git rev-parse --short HEAD) -X main.buildDate=<utc>"
//
// (see the Makefile and .goreleaser.yaml). With no ldflags they come from the
// build info's vcs.revision and vcs.time when the toolchain recorded them (a
// `go build` in a git checkout), and otherwise stay "unknown" (`go test`, a
// module-proxy `go install`).
var (
	commit    = unknownBuildInfo
	buildDate = unknownBuildInfo
)

// buildVersion is the version plus the short commit when known, e.g.
// "0.0.0-dev+1a2b3c4". It is surfaced in /healthz, the MCP handshake, and the
// startup log so a stale running daemon (older code than the working tree) is
// visible at a glance.
func buildVersion() string {
	if commit == "unknown" {
		return version
	}
	return version + "+" + commit
}

func main() {
	applyBuildInfo()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "doctor":
		err = runDoctor(args)
	case "export":
		err = runExport(args)
	case "import":
		err = runImport(args)
	case "install-hooks":
		err = runInstallHooks(args)
	case "install-summary":
		err = runInstallSummary(args)
	case "client-config":
		err = runClientConfig(args)
	case "uninstall":
		err = runUninstall(args)
	case "update":
		err = runUpdate(args)
	case "map-repo":
		err = runMapRepo(args)
	case "unmap-repo":
		err = runUnmapRepo(args)
	case "retire-project":
		err = runRetireProject(args)
	case "family":
		err = runFamily(args)
	case "console-open":
		err = runConsoleOpen(args)
	case "start":
		err = runServiceAction(actionStart, args)
	case "stop":
		err = runServiceAction(actionStop, args)
	case "restart":
		err = runServiceAction(actionRestart, args)
	case "status":
		err = runServiceAction(actionStatus, args)
	case "version", "-v", "--version":
		fmt.Printf("seamlessd %s (commit %s, built %s, %s build)\n", version, commit, buildDate, distribution)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		slog.Error("command failed", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `seamlessd %s -- Seamless server daemon

usage:
  seamlessd serve          start the HTTP server (127.0.0.1:8081)
  seamlessd doctor         run configuration + database self-checks
  seamlessd export         write the whole instance to one archive: corpus + db snapshot + manifest
                           (-o FILE, or -o - for stdout; --no-db for the markdown trees only)
  seamlessd import         import a Seam v1 data directory (--from ~/.seam), or a seamlessd export
                           archive (--from FILE|-; --dry-run previews, --force allows a live restore)
  seamlessd install-hooks  install Claude Code/Codex hooks, MCP, and maintained skills
  seamlessd client-config  print the commands that pair another machine as a client (run on the SERVER;
                           --redact masks the key so the output is safe to paste into a ticket)
  seamlessd uninstall      remove Seamless: service, hooks, MCP, skills, binaries
                           (--purge also deletes config + ~/.seamless; --dry-run to preview)
  seamlessd update         upgrade in place to the newest release by running its verified installer;
                           backed up, confirmed and rolled back on an installer-made install
                           (--check reports installed vs newest; --dry-run previews; honors SEAMLESS_VERSION)
  seamlessd map-repo       override a repo's auto-derived project slug (rarely needed;
                           repos self-map on first session -- repo_project_map)
  seamlessd unmap-repo     remove this machine's mapping for a repo (--path DIR), or every mapping
                           whose path no longer exists (--stale); --dry-run previews. Projects stay.
                           --host NAME --path DIR removes another machine's mapping on a shared daemon
  seamlessd retire-project mark a mistakenly minted project retired (--project SLUG); refused while
                           repos map to it or it holds memories, notes or open tasks; --undo; --dry-run
  seamlessd family         manage project families (list|add|remove)
  seamlessd console-open   open the console in a browser, pre-authenticated
                           (--browser "Google Chrome" targets a specific browser; macOS only)
  seamlessd start          start the installed service (launchd/systemd/Scheduled Task)
  seamlessd stop           stop the installed service
  seamlessd restart        restart the installed service in place
  seamlessd status         print the installed service's state
  seamlessd version        print the version
`, version)
}

// runServe starts the HTTP server and blocks until SIGINT/SIGTERM, then shuts
// down gracefully. It wires /healthz, the MCP tool endpoint at /api/mcp, the
// A2A endpoint at /api/a2a with its agent card at /.well-known/agent-card.json,
// the SessionStart/UserPromptSubmit/SessionEnd hooks under /api/hooks, and the
// server-rendered observability console under /console.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "", "HTTP bind address (overrides config)")
	cfgPath := fs.String("config", "", "path to seamless.yaml (overrides $SEAMLESS_CONFIG and the search path)")
	logFile := fs.String("log-file", "", "append logs to this file in addition to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}

	closeLog, err := applyServeEnv(*cfgPath, *logFile)
	if err != nil {
		return err
	}
	defer closeLog()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.serve: %w", err)
	}
	// Refuse before anything opens a file or a port. A client install has no
	// daemon of its own by definition, and serving one here would quietly give
	// the machine a second, empty corpus that its own hooks never write to.
	if cfg.IsClient() {
		return fmt.Errorf("seamlessd.serve: role: client runs no daemon -- this install dials %s; remove `role: client` to serve here", cfg.ServerURL())
	}
	var keyPath string
	cfg, keyPath, err = config.EnsureAPIKey(cfg)
	if err != nil {
		return fmt.Errorf("seamlessd.serve: %w", err)
	}
	if keyPath != "" {
		slog.Info("first run: generated mcp.api_key", "config", keyPath)
	}
	bind := cfg.Addr
	if *addr != "" {
		// The flag is the bind address, so it becomes the config's: everything
		// downstream that derives "where do clients reach this daemon" --
		// ServerURL, ServerHost, the Host allowlist, the agent card -- must
		// answer from the address actually listened on, not from a superseded
		// `addr:`. A configured server_url still wins over both.
		bind = *addr
		cfg.Addr = bind
	}
	serverURL := cfg.ServerURL()
	logger := slog.Default()

	// peer_auth: build the policy and refuse a deployment in which the boundary
	// would not hold, before anything opens a file in the data dir (preflight
	// creates a missing data dir 0700 once its parents have passed).
	var peer *peerAuthRuntime
	if cfg.PeerAuth.Enabled {
		peer, err = preparePeerAuth(cfg, defaultPeerAuthEnv())
		if err != nil {
			return fmt.Errorf("seamlessd.serve: %w", err)
		}
	}

	// One daemon per data dir, decided before anything touches it: a second
	// serve must never migrate, reconcile, or adopt anything under the one
	// already serving, and without this it did all three before failing to bind.
	// Deferred ahead of db.Close, so the lock outlives the database handle.
	dirLock, err := lockDataDir(context.Background(), cfg.DataDir, func(lock, holder string) {
		slog.Info("data dir in use; waiting for the daemon that holds it to exit",
			"lock", lock, "holder", holder, "wait", dataDirLockWait)
	})
	switch {
	case errors.Is(err, errLockUnsupported):
		// Refusing to serve would leave a data dir on such a mount unusable;
		// serving without the guard is what every earlier release did.
		slog.Warn("data dir lock unavailable on this file system; a second daemon on this data dir will not be refused",
			"data_dir", cfg.DataDir, "err", err)
	case err != nil:
		return fmt.Errorf("seamlessd.serve: %w", err)
	default:
		defer func() { _ = dirLock.Close() }()
	}

	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("seamlessd.serve: %w", err)
	}
	defer func() { _ = db.Close() }()
	if v, verr := store.SchemaVersion(db); verr == nil {
		slog.Info("database ready", "path", cfg.DBPath(), "schema_version", v)
		// store.Open applies nothing to a database AHEAD of this binary and
		// serves it anyway; say so in the same words doctor uses.
		if compiled := store.LatestSchemaVersion(); v > compiled {
			slog.Warn("database schema is newer than this seamlessd",
				"schema_version", v, "compiled_schema_version", compiled,
				"cause", schemaAheadCause, "fix", schemaAheadFix)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Claim this machine's identity before anything reads the repo map or a
	// session row: rows written before host scoping (and by seeders or a
	// map-repo that ran before the daemon ever did) carry no host, and every
	// resolver below asks for one. It is idempotent, so it runs on every start.
	if err := store.AdoptLocalHost(ctx, db, config.Hostname()); err != nil {
		return fmt.Errorf("seamlessd.serve: adopt local host: %w", err)
	}

	// peer_auth, second half of the preflight: the policy against the projects
	// table. A seal naming a project that does not exist protects nothing.
	if peer != nil {
		problems, perr := projectProblems(ctx, db, peer.policy)
		if perr != nil {
			return fmt.Errorf("seamlessd.serve: peer_auth projects: %w", perr)
		}
		if len(problems) > 0 {
			return fmt.Errorf("seamlessd.serve: %w: %s", errPeerAuthPreflight, strings.Join(problems, "; "))
		}
		peer.logPolicy(logger)
	}

	// Files subsystem: markdown source of truth, watcher, reconcile, and
	// embed-on-index when an embedder is configured.
	mgr, err := files.NewManager(cfg.DataDir, db, logger)
	if err != nil {
		return fmt.Errorf("seamlessd.serve: %w", err)
	}
	defer func() { _ = mgr.Close() }()

	// The console's off switch is read once, here, before anything holds the
	// embedder -- so a toggle applies on the next restart, never mid-process.
	embedMode := store.EmbedderModeAuto
	if mode, merr := store.EmbedderMode(ctx, db); merr != nil {
		slog.Warn("embedder mode override unreadable; defaulting to auto", "err", merr)
	} else {
		embedMode = mode
	}

	var embedder llm.Embedder
	embedRT := console.EmbeddingRuntime{Provider: cfg.LLM.Provider, Model: cfg.LLM.EmbeddingModel()}
	if embedMode == store.EmbedderModeOff {
		embedRT.OverriddenOff = true
		embedRT.Reason = "switched off in the console Settings"
		slog.Info("embeddings disabled by console override; recall degrades to FTS")
	} else if e, eerr := llm.NewEmbedder(cfg.LLM); eerr != nil {
		// Separate a deliberate opt-out (no credential) from a mistake (a
		// malformed base_url). Both leave recall lexical-only for the life of
		// the process, and that is the only other symptom the owner ever sees,
		// so a typo says so at Error rather than blending into the warnings.
		embedRT.Reason = eerr.Error()
		if errors.Is(eerr, llm.ErrConfig) {
			embedRT.Misconfigured = true
			slog.Error("embeddings disabled by a misconfiguration; recall stays FTS-only until it is fixed", "err", eerr)
		} else {
			slog.Warn("embeddings disabled; recall degrades to FTS", "err", eerr)
		}
	} else {
		embedder = e
		mgr.SetEmbedder(embedder)
		embedRT.Enabled = true
		embedRT.Model = embedder.Model()
		slog.Info("embeddings enabled", "provider", cfg.LLM.Provider, "model", embedder.Model())
	}
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("seamlessd.serve: files start: %w", err)
	}

	ret := retrieve.New(db, embedder, cfg.Budgets, logger)
	ret.SetBodyReader(mgr.Store()) // enables the pinned-stage briefing section
	ret.SetBriefingConfig(cfg.Briefing)
	ret.SetSearchConfig(cfg.Search)
	ret.SetFeaturesConfig(cfg.Features)
	rec := events.NewRecorder(db)
	rec.SetFeatures(cfg.Features) // arms the momentum milestone layer

	// One id per process, served by /healthz and kept in the update state: it
	// tells this daemon apart from another answering on the same port, and
	// from the one it replaced.
	instance, err := core.NewID()
	if err != nil {
		return fmt.Errorf("seamlessd.serve: instance id: %w", err)
	}
	// The request-activity tracker (activity.go) wraps the routes below
	// (daemonHandler), and the automatic update reads its snapshot to tell
	// whether anything is using the daemon. It is built here because the
	// checker takes it at construction; its clock starts a few milliseconds
	// before the listener does, which is nothing against the 90 seconds of
	// quiet the deadline waits for.
	activity := newActivityTracker(time.Now)
	// The background update check (internal/update): built here, started only
	// once the listener is bound, so a daemon that cannot bind never records
	// itself as running. The briefing asks it for the update notice.
	upd := newUpdateChecker(cfg, db, rec, instance, activity, logger)
	ret.SetUpdateNotice(upd.Notice)

	// Gardener: propose-only maintenance, exposed to the gardener_apply MCP tool
	// and run on a ticker. The chat client (for digests) is best-effort; without
	// it the digest pass simply no-ops.
	var chat llm.Chat
	if c, cerr := llm.NewChatClient(cfg.LLM); cerr != nil {
		if errors.Is(cerr, llm.ErrConfig) {
			slog.Error("gardener digests disabled by a misconfiguration", "err", cerr)
		} else {
			slog.Warn("gardener digests disabled; chat client unavailable", "err", cerr)
		}
	} else {
		chat = c
	}
	garden := gardener.New(db, mgr, embedder, chat, rec, gardener.FromConfig(cfg.Gardener), logger)

	if cfg.MCP.APIKey == "" {
		slog.Warn("mcp.api_key is empty; MCP and hook requests will be rejected -- set it in seamless.yaml")
	}
	mcpSrv := mcp.New(mcp.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, Gardener: garden, Embedder: embedder,
		APIKey: cfg.MCP.APIKey, Version: buildVersion(), LocalHost: config.Hostname(),
		ToolEventMaxChars:   cfg.Budgets.ToolEventMaxChars,
		CaptureAllowedPorts: cfg.Capture.AllowedPorts, Logger: logger,
		// The file/env optional-features base. The server layers the console's
		// stored override on top of it per request, so a toggle needs no restart
		// -- but without this line the file and SEAMLESS_FEATURES_* env would be
		// invisible to the tool gate, leaving the DB row as the only way in.
		Features: cfg.Features,
	})
	// A2A: the agent-to-agent surface, one recall skill over the same retrieve
	// service and bearer key as MCP. The endpoint URL in the card is the URL
	// clients actually dial (server_url, else the effective bind address), so a
	// non-default addr:, a wildcard bind and a TLS listener all advertise
	// themselves correctly.
	a2aSrv, err := a2a.New(a2a.Config{
		Retrieve: ret, Events: rec, APIKey: cfg.MCP.APIKey,
		Version: buildVersion(), Endpoint: a2aEndpoint(cfg),
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("seamlessd.serve: a2a: %w", err)
	}
	hooksH := hooks.NewHandler(hooks.Config{
		DB: db, Retrieve: ret, Events: rec, Files: mgr,
		APIKey: cfg.MCP.APIKey, MaxEventChars: cfg.Budgets.ToolEventMaxChars,
		PlanCapture: cfg.PlanCapture, LocalHost: config.Hostname(), Logger: logger,
	})
	// The console reads through a handle on which projects sealed to other OS
	// accounts do not exist, when there are any (store.OpenView); its writes go
	// to the main handle after reading their target through the filtered one.
	consoleDB, consoleEvents := db, rec
	var consoleWriteDB *sql.DB
	if peer != nil && len(peer.policy.SealedProjects()) > 0 {
		viewDB, verr := store.OpenView(cfg.DBPath(), peer.policy.SealedProjects())
		if verr != nil {
			return fmt.Errorf("seamlessd.serve: %w", verr)
		}
		defer func() { _ = viewDB.Close() }()
		consoleDB, consoleWriteDB, consoleEvents = viewDB, db, rec.ReadingFrom(viewDB)
	}
	consoleSrv, err := console.New(console.Config{
		DB: consoleDB, WriteDB: consoleWriteDB, Files: mgr, Gardener: garden, Events: consoleEvents, Retrieve: ret,
		APIKey: cfg.MCP.APIKey, DataDir: cfg.DataDir, ConfigPath: absConfigPath(cfg.SourcePath()),
		DBPath:  cfg.DBPath(),
		Budgets: cfg.Budgets, GardenerCfg: cfg.Gardener, BriefingCfg: cfg.Briefing,
		// The file/env optional-features base; the console layers its own stored
		// override on top per request (same precedence as BriefingCfg).
		Features: cfg.Features,
		// The file/env console level base, layered the same way. Presentation
		// only: nothing an agent receives reads it.
		Level:     cfg.Console.Level,
		Version:   buildVersion(),
		Embedding: embedRT,
		// The background update check backs Settings -> Updates, the Home
		// version fact and the "updated" banner.
		Updates:        upd,
		SessionIdleTTL: time.Duration(cfg.Gardener.SessionIdleMinutes) * time.Minute,
		// Secure only under TLS: a browser drops a Secure cookie arriving over
		// http, so setting it unconditionally would lock the owner out of the
		// console on the default loopback install.
		SecureCookies: cfg.TLSEnabled(),
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("seamlessd.serve: console: %w", err)
	}

	if cfg.Gardener.Enabled {
		garden.Start(ctx)
		slog.Info("gardener enabled", "interval", time.Duration(cfg.Gardener.IntervalMinutes)*time.Minute,
			"dedup_threshold", cfg.Gardener.DedupThreshold, "staleness_days", cfg.Gardener.StalenessDays)
	} else {
		slog.Info("gardener disabled")
	}

	mux := daemonRoutes{
		healthz:   healthzHandler(db, instance, upd.Status),
		mcp:       mcpSrv.Handler(),
		a2a:       a2aSrv.Handler(),
		agentCard: a2aSrv.CardHandler(),
		hooks:     hooksH,
		console:   consoleSrv,
	}.mux()

	// Host allowlist outermost, then the activity tracker, then the routes
	// (daemonHandler). The effective list is allowed_hosts plus the host of
	// server_url, so naming the daemon is all it takes to arm the guard --
	// including on a wildcard bind.
	tlsOn := cfg.TLSEnabled()
	// Under peer_auth every TCP request is the owner's: the bearer key the
	// routes check is the owner account's key.
	var tcpRoutes http.Handler = mux
	if peer != nil {
		tcpRoutes = ownerHandler(peer.policy, mux)
	}
	srv := newHTTPServer(ctx, bind, daemonHandler(bind, cfg.AllowedHostsEffective(), activity, tcpRoutes))
	if tlsOn {
		// TLS 1.2 floor: everything below it is either broken or obsolete, and
		// nothing that speaks to this daemon (Go, curl, a browser) needs it.
		// The key pair loads here, before the bind, so an unusable certificate
		// fails the start outright instead of after the update check has
		// recorded this process as the running daemon.
		pair, perr := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if perr != nil {
			stop()
			garden.Wait()
			return fmt.Errorf("seamlessd.serve: load TLS key pair: %w", perr)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	}
	warnNonLoopbackBind(bind, tlsOn)
	warnAdvertisedLoopback(bind, serverURL)

	// Bind first, then start the update check, then serve: the check writes
	// "this process is the running daemon" into the update state, which must
	// never describe a daemon that lost the port (another instance holds it).
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		stop() // stop background work (gardener, watcher ctx) on the error path too
		garden.Wait()
		return fmt.Errorf("seamlessd.serve: %w", err)
	}
	// The peer_auth socket: the same routes, no Host guard (a Unix socket has no
	// DNS name to rebind and no browser can reach it), and each connection's
	// principal read from the kernel at accept. The refusal of an unknown peer
	// sits outside the activity tracker for the guard's reason: a refused
	// knock is not someone using the daemon.
	var sockSrv *http.Server
	var sockLn net.Listener
	if peer != nil {
		sockLn, err = peer.listenSocket()
		if err != nil {
			_ = ln.Close()
			stop()
			garden.Wait()
			return fmt.Errorf("seamlessd.serve: %w", err)
		}
		sockSrv = newHTTPServer(ctx, peer.cfg.Socket.Path, peerHandler(peer.policy, logger, activity.wrap(mux)))
		sockSrv.ConnContext = peerConnContext
	}
	upd.Start(ctx)

	errCh := make(chan error, 2)
	go func() {
		slog.Info("seamlessd listening", "addr", bind, "url", serverURL, "tls", tlsOn,
			"data_dir", cfg.DataDir,
			"version", buildVersion(), "commit", commit, "built", buildDate, "mcp_tools", mcp.ToolCount,
			"distribution", distribution, "instance", instance)
		serve := func() error { return srv.Serve(ln) }
		if tlsOn {
			// The certificate is already in srv.TLSConfig, so no file names.
			serve = func() error { return srv.ServeTLS(ln, "", "") }
		}
		if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	if sockSrv != nil {
		go func() {
			slog.Info("seamlessd listening on the peer_auth socket", "socket", peer.cfg.Socket.Path)
			if err := sockSrv.Serve(sockLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-errCh:
		stop() // stop background work (gardener, watcher ctx) on the error path too
		if sockSrv != nil {
			_ = sockSrv.Close()
		}
		_ = srv.Close()
		garden.Wait()
		upd.Wait()
		return fmt.Errorf("seamlessd.serve: %w", err)
	}

	// Drain order: cancel ctx (stops the gardener and update loops and
	// unblocks long-lived request streams via the server's base context), shut
	// the listener down, then wait for both loops so nothing still touches the
	// DB when the deferred mgr.Close/db.Close run.
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if sockSrv != nil {
		// Closing the listener unlinks the socket file (net.UnixListener's
		// default), so a stopped daemon leaves nothing at the path to dial.
		if err := sockSrv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("seamlessd: peer_auth socket shutdown", "err", err)
		}
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		garden.Wait()
		upd.Wait()
		return fmt.Errorf("seamlessd.serve: shutdown: %w", err)
	}
	garden.Wait()
	upd.Wait()
	slog.Info("seamlessd stopped")
	return nil
}

// applyServeEnv wires the two supervisor-facing serve flags before config load
// and returns a cleanup that closes the log file (always safe to call).
//
// --config becomes config.Load's documented $SEAMLESS_CONFIG override, moved onto
// the command line so a supervisor that cannot carry an env prefix -- notably a
// Windows Scheduled Task -- can still pin the config explicitly. Exporting it keeps
// config.Load's search order the single code path, the same shape as `seam hook
// --config`.
//
// --log-file gives the daemon a log the supervisor does not capture for it:
// launchd/systemd redirect stderr to ~/.seamless/seamlessd.log, a Scheduled Task
// does not, so the task passes --log-file to reach the same file every doc and the
// installer's health-check message point at. stderr stays wired, so an interactive
// `serve` still prints and a foreground run tees to the file.
func applyServeEnv(cfgPath, logFile string) (func(), error) {
	noop := func() {}
	if cfgPath != "" {
		if err := os.Setenv("SEAMLESS_CONFIG", cfgPath); err != nil {
			return noop, fmt.Errorf("seamlessd.serve: set config path: %w", err)
		}
	}
	if logFile == "" {
		return noop, nil
	}
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return noop, fmt.Errorf("seamlessd.serve: open log file: %w", err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, f),
		&slog.HandlerOptions{Level: slog.LevelInfo})))
	return func() { _ = f.Close() }, nil
}

// routeRegistrar mounts its own routes on a mux (the hook handler, the
// console).
type routeRegistrar interface {
	Register(mux *http.ServeMux)
}

// daemonRoutes is every route runServe serves. It is split from runServe so a
// test can build the real route table: activity_test.go checks the activity
// tracker's exemptions (activityExempt) against these registrations, not
// against a copy of them that could drift.
type daemonRoutes struct {
	healthz, mcp, a2a, agentCard http.Handler
	hooks, console               routeRegistrar
}

// mux registers the routes on a new mux.
func (d daemonRoutes) mux() *http.ServeMux {
	mux := http.NewServeMux()
	// Redirect the bare root to the console; {$} matches only "/" so other
	// unmatched paths still return 404.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/", http.StatusFound)
	})
	mux.Handle("/healthz", d.healthz)
	mux.Handle("/api/mcp", d.mcp)
	mux.Handle("/api/a2a", d.a2a)
	// The agent card is public discovery metadata (RFC 8615); the endpoint it
	// names is what demands the bearer key.
	mux.Handle("/.well-known/agent-card.json", d.agentCard)
	d.hooks.Register(mux)
	d.console.Register(mux)
	return mux
}

// daemonHandler is what the daemon serves: the Host allowlist outermost, so a
// rebound request is refused before it can reach even an unauthenticated
// route (see netguard.go); inside it the activity tracker; inside that the
// routes. The tracker sits inside the guard so a request the guard refuses
// never counts: a page in the owner's browser knocking on the port under a
// rebound name is not someone using the daemon, and counting it would let any
// web page hold off an automatic update. Everything the guard lets through
// counts unless activityExempt says otherwise, a request the bearer check then
// refuses included; update.Decide allows for a client looping on a bad key.
func daemonHandler(bind string, allowedHosts []string, activity *activityTracker, routes http.Handler) http.Handler {
	return hostGuard(bind, allowedHosts, activity.wrap(routes))
}

// newHTTPServer builds the daemon's HTTP server. Requests inherit ctx as their
// base context, so cancelling it (the shutdown signal) also cancels in-flight
// request contexts -- without this, the console's open SSE streams never end and
// every Shutdown stalls for its full deadline, then fails.
//
// The timeouts are chosen around the one long-lived response this server has,
// the console's SSE stream (audit L7):
//
//   - ReadHeaderTimeout / ReadTimeout bound the request side. ReadTimeout is
//     safe here only because it is *per request* and the SSE handler explicitly
//     clears its own read deadline (see console.sse); left in place it would
//     expire mid-stream, and Go's background read would then cancel the request
//     context and kill the feed.
//   - WriteTimeout stays omitted for the same reason -- it has no per-handler
//     escape hatch, so any value at all would cap how long a feed may stay open.
//   - IdleTimeout bounds keep-alive connections between requests, which is the
//     half ReadHeaderTimeout never covered: without it a client can hold open
//     connections (and their goroutines) indefinitely by simply saying nothing.
func newHTTPServer(ctx context.Context, bind string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              bind,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB; Go's default, made explicit
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
}

// healthzHandler reports liveness plus a database ping, the build, this
// process's instance id, and -- when updates is set and its check found one --
// the newest release (healthzBody).
func healthzHandler(db *sql.DB, instance string, updates func() update.Status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, code := "ok", http.StatusOK
		if err := db.PingContext(r.Context()); err != nil {
			status, code = "degraded", http.StatusServiceUnavailable
			slog.Warn("healthz: db ping failed", "err", err)
		}
		var st *update.Status
		if updates != nil {
			s := updates()
			st = &s
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(healthzBody(status, instance, st)); err != nil {
			slog.Warn("healthz: encode response", "err", err)
		}
	}
}

// runDoctor runs self-checks. P0 fleshes this out step by step; later steps add
// config loading and database checks. See doctor.go.
func runDoctor(args []string) error {
	return doctor(args)
}
