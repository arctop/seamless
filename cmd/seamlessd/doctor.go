package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/mcp"
	agentskills "github.com/arctop/seamless/internal/skills"
	"github.com/arctop/seamless/internal/store"
)

// checkStatus is the outcome of a single doctor check.
type checkStatus int

const (
	statusOK checkStatus = iota
	statusInfo
	statusWarn
	statusFail
)

func (s checkStatus) label() string {
	switch s {
	case statusOK:
		return "ok"
	case statusInfo:
		return "info"
	case statusWarn:
		return "warn"
	case statusFail:
		return "fail"
	default:
		return "unknown"
	}
}

// check is one line of the doctor report.
type check struct {
	status checkStatus
	name   string
	detail string
}

// doctor runs environment self-checks and prints a report. It exits non-zero
// (via a returned error) only when a check FAILs; warnings do not fail the run.
//
// P0 grows this: config loading and database reachability are added in later
// steps so the phase-0 acceptance ("doctor reports config + DB ok") is met.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	checks := []check{
		{statusOK, "binary", fmt.Sprintf("seamlessd %s runs", version)},
	}

	cfg, err := config.Load()
	if err != nil {
		checks = append(checks, check{statusFail, "config", err.Error()})
		return reportChecks(checks)
	}
	src := cfg.SourcePath()
	if src == "" {
		src = "defaults + env (no seamless.yaml found)"
	}
	checks = append(checks,
		check{statusOK, "config", "loaded from " + src},
		configPermissionsCheck(cfg.SourcePath(), runtime.GOOS),
	)

	// The role branch, and it is placed here because everything below it reads
	// LOCAL server state. store.Open is the sharp edge: it creates and migrates,
	// so running it against a client would MINT the ~/.seamless/seam.db whose
	// absence is what "role: client" means, and every later run would then find
	// that database and report it as healthy.
	if cfg.IsClient() {
		return reportChecks(append(checks, clientChecks(cfg)...))
	}

	checks = append(checks,
		check{statusOK, "data_dir", cfg.DataDir},
		apiKeyCheck(cfg),
		llmCheck(cfg),
		embedderCheck(cfg),
	)
	checks = append(checks, transportChecks(cfg)...)

	// Database: open (creating + migrating if needed) and report schema state.
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		checks = append(checks, check{statusFail, "database", err.Error()})
		return reportChecks(checks)
	}
	defer func() { _ = db.Close() }()
	ver, verr := store.SchemaVersion(db)
	tbls, terr := store.TableCount(db)
	if verr != nil || terr != nil {
		checks = append(checks, check{statusFail, "database", "opened but could not read schema"})
	} else {
		checks = append(checks, check{statusOK, "database",
			fmt.Sprintf("%s (schema v%d, %d tables)", cfg.DBPath(), ver, tbls)})
	}

	checks = append(checks, schemaVersionCheck(db))
	checks = append(checks, peerAuthChecks(context.Background(), cfg, db, defaultPeerAuthEnv())...)
	checks = append(checks, repoMapCheck(db))
	checks = append(checks, remoteSessionsCheck(db))
	checks = append(checks, mcpToolsCheck())
	checks = append(checks, claudeRuntimeChecks()...)
	checks = append(checks, hooksCheck(cfg))
	checks = append(checks, claudeDesktopChecks(resolveSeamBin(""), absConfigPath(cfg.SourcePath()))...)
	checks = append(checks, codexChecks(cfg, db)...)
	checks = append(checks, featureSkillsCheck(db, cfg))
	checks = append(checks, gardenerCheck(cfg))
	checks = append(checks, updatesCheck(context.Background(), cfg, db, time.Now()))
	checks = append(checks, updateConfirmChecks(cfg)...)

	return reportChecks(checks)
}

// clientChecks is the rest of the report for an install whose role is client.
//
// It is a separate list rather than a set of guards inside the server path
// because the two roles share almost nothing below config: a client has no
// listener, no database, no corpus and no gardener, so most server checks would
// not merely be uninformative on one, they would be answering about a machine
// that is somewhere else. What is left is genuinely this install's: which server
// it dials, whether that server answers, the key it will present, and the
// client-side wiring (hooks, skills, MCP registrations) that points at both.
//
// Deliberately absent, and each for its own reason:
//   - database, schema version, repo map, remote sessions, feature skills --
//     all read the local seam.db, which a client does not have.
//   - gardener -- a maintenance loop that runs inside the daemon; naming its
//     ticker here would describe the server's configuration from the wrong file.
//   - llm, embedder -- the SERVER embeds and calls the model. Warning a client
//     that "recall degrades to FTS" would report a degradation that is neither
//     this machine's nor, on a properly configured server, true.
//   - bind, tls -- transportChecks already refuses to report a bind address an
//     install never binds, or a server certificate it does not hold.
//
// The hook and MCP comparisons are the SAME desired-state ones the server role
// runs, built from the current config through doctorInstallOptions rather than
// from the installed artifact -- a client's stale hook is drift by the same rule.
//
// mcp_tools is the one check that changes MEANING rather than disappearing. On a
// server it counts REGISTRATION inside this process (mcpToolsCheck); on a client
// this process registers nothing and serves nothing, so the same line would be a
// fact about a daemon this install does not run. clientMCPToolsCheck asks the
// server instead, which is also the only way a client can learn that its key
// works at all.
func clientChecks(cfg config.Config) []check {
	checks := transportChecks(cfg)
	checks = append(checks, apiKeyCheck(cfg), clientMCPToolsCheck(cfg))
	checks = append(checks, claudeRuntimeChecks()...)
	checks = append(checks, hooksCheck(cfg))
	checks = append(checks, claudeDesktopChecks(resolveSeamBin(""), absConfigPath(cfg.SourcePath()))...)
	// A nil db skips the Codex hook-activity line, which reads the event log:
	// hooks on a client record their observations on the server, not here.
	checks = append(checks, codexChecks(cfg, nil)...)
	return checks
}

// configPermissionsCheck warns when an existing secret-bearing configuration
// file, or the directory containing it, is accessible to group/other users.
// Doctor never changes owner-authored permissions; the detail gives explicit
// repair commands. Windows ACLs are not meaningfully represented by FileMode,
// so that platform gets a separate informational result rather than a false OK.
func configPermissionsCheck(path, goos string) check {
	const name = "config permissions"
	if path == "" {
		return check{statusInfo, name, "no config file selected (defaults + environment only)"}
	}
	if goos == "windows" {
		return check{statusInfo, name, fmt.Sprintf(
			"FileMode cannot verify Windows ACLs for %s; ensure only your account can read the file and its directory", path)}
	}

	var problems []string
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("cannot inspect %s: %v", path, err))
	case info.Mode()&os.ModeSymlink != 0:
		problems = append(problems, fmt.Sprintf("%s is a symlink; replace it with a regular owner-only file", path))
	case !info.Mode().IsRegular():
		problems = append(problems, fmt.Sprintf("%s is not a regular file", path))
	case info.Mode().Perm()&0o077 != 0:
		problems = append(problems, fmt.Sprintf(
			"%s mode %04o grants group/other access; run: %s", path, info.Mode().Perm(), chmodRepairCommand("600", path)))
	}

	dir := filepath.Dir(path)
	dirInfo, dirErr := os.Lstat(dir)
	switch {
	case dirErr != nil:
		problems = append(problems, fmt.Sprintf("cannot inspect containing directory %s: %v", dir, dirErr))
	case dirInfo.Mode()&os.ModeSymlink != 0:
		problems = append(problems, fmt.Sprintf("containing directory %s is a symlink; use a real owner-only directory", dir))
	case !dirInfo.IsDir():
		problems = append(problems, fmt.Sprintf("containing path %s is not a directory", dir))
	case dirInfo.Mode().Perm()&0o077 != 0:
		problems = append(problems, fmt.Sprintf(
			"directory %s mode %04o grants group/other access; run: %s", dir, dirInfo.Mode().Perm(), chmodRepairCommand("700", dir)))
	}

	if len(problems) > 0 {
		return check{statusWarn, name, strings.Join(problems, "; ")}
	}
	return check{statusOK, name, fmt.Sprintf("%s and %s are owner-only regular paths", path, dir)}
}

// chmodRepairCommand returns a copy-pasteable Unix command with an absolute,
// shell-quoted path. Making the path absolute also keeps a leading dash from
// being interpreted as an option by chmod.
func chmodRepairCommand(mode, path string) string {
	target, err := filepath.Abs(path)
	if err != nil {
		target = path
		if !filepath.IsAbs(target) {
			target = "." + string(filepath.Separator) + target
		}
	}
	return fmt.Sprintf("chmod %s '%s'", mode, strings.ReplaceAll(target, "'", "'\"'\"'"))
}

// schemaAheadCause and schemaAheadFix are the one diagnosis of a database whose
// applied schema is NEWER than the migrations this binary compiles. serve's
// startup WARN and doctor's schema check both print them, so the two can never
// name different causes or different fixes.
//
// With automatic updates the state has two origins: the owner deliberately went
// back to an older release (a SEAMLESS_VERSION pin), or an update rolled back to
// the old binary without restoring the database the new one had already
// migrated. The remedy is the same either way: run a binary that knows the
// schema again, or put back the database from before the migration. The fix
// names seamlessd import rather than a specific file, because the backup may be
// one the owner exported by hand as well as one an update took.
const (
	schemaAheadCause = "this database was migrated by a newer seamlessd (a downgrade, or an update rollback that could not restore the pre-update database)"
	schemaAheadFix   = "run that newer release again (seamlessd update), or import a backup taken before it (seamlessd import)"
)

// schemaVersionCheck pairs what the database has APPLIED with what this binary
// COMPILES. doctor's own store.Open has already migrated forward by the time it
// runs, so "applied < compiled" is unreachable here and the interesting case is
// the other one: a database migrated by a NEWER seamlessd. store.Open applies
// nothing to it and opens it anyway, which is why serve runs on it under the
// same warning, but this build cannot understand it by migrating -- the
// migrations that produced it are not in it -- which is also why an archive
// from it is refused here. Info rather than ok, because the pair is a fact to
// read, not a condition to pass.
func schemaVersionCheck(db *sql.DB) check {
	const name = "schema version"
	applied, err := store.SchemaVersion(db)
	if err != nil {
		return check{statusWarn, name, "cannot read schema_migrations: " + err.Error()}
	}
	compiled := store.LatestSchemaVersion()
	detail := fmt.Sprintf("v%d applied / v%d compiled", applied, compiled)
	if applied > compiled {
		return check{statusWarn, name, detail + " -- " + schemaAheadCause + "; " + schemaAheadFix}
	}
	return check{statusInfo, name, detail}
}

// repoMapCheck reports dangling repo_map entries -- mapped paths that no longer
// exist on disk. A repo moved without a rename heals itself at its next session
// start (RegisterProjectForCWD adopts the project once every owner of the
// derived slug is dead), but a repo moved AND renamed derives a different slug
// and cannot be recognized; the printed map-repo override is the fix for that
// case. Dangling entries are otherwise harmless, so this warns rather than
// fails, and names unmap-repo --stale as the way to clear them.
//
// Only LOCAL rows are stat'd. A path on another machine is missing from this
// disk by definition, so stat'ing it would report every remote device's repos as
// dangling -- and a same-named checkout here would report a stale mapping as
// healthy, which is worse. The unnamed ("") host bucket counts as local: it is
// the pre-host-scoping legacy mirror of THIS machine, not an unknown one.
func repoMapCheck(db *sql.DB) check {
	ctx, cancel := context.WithTimeout(context.Background(), codexActivityTimeout)
	defer cancel()
	rows, err := store.RepoMapRows(ctx, db)
	if err != nil {
		return check{statusWarn, "repo map", "cannot read repo_map: " + err.Error()}
	}
	if len(rows) == 0 {
		return check{statusOK, "repo map", "no repos mapped yet (a repo maps itself on its first session)"}
	}
	local := config.Hostname()
	var localCount, remoteCount int
	var missing []string
	remoteHosts := map[string]bool{}
	for _, r := range rows {
		if r.Host != "" && r.Host != local {
			remoteCount++
			remoteHosts[r.Host] = true
			continue
		}
		localCount++
		if _, serr := os.Lstat(r.Path); errors.Is(serr, fs.ErrNotExist) {
			missing = append(missing, fmt.Sprintf("%s -> %s", r.Path, r.Slug))
		}
	}
	remote := ""
	if remoteCount > 0 {
		names := slices.Sorted(maps.Keys(remoteHosts))
		remote = fmt.Sprintf("; %d on %s, not verifiable from here",
			remoteCount, strings.Join(names, ", "))
	}
	if len(missing) == 0 {
		return check{statusOK, "repo map", fmt.Sprintf(
			"%d local mapped paths, all present on disk%s", localCount, remote)}
	}
	slices.Sort(missing)
	return check{statusWarn, "repo map", fmt.Sprintf(
		"%d of %d local mapped paths missing on disk: %s -- a moved repo adopts its project at its next session start; a renamed one needs `seamlessd map-repo --path <new-root> --project <slug>`; `seamlessd unmap-repo --stale` removes the dead entries%s",
		len(missing), localCount, strings.Join(missing, "; "), remote)}
}

// remoteSessionsCheck reports which machines have used this daemon lately and
// how many daemon-side captures were skipped for them.
//
// Info, never a warning: a shared daemon is a supported deployment and the skips
// are the design working. It exists because that design is SILENT otherwise --
// plan capture, git stamps and transcript harvest read the daemon's disk, so a
// remote agent simply gets none of them, and nothing else on any surface says so.
func remoteSessionsCheck(db *sql.DB) check {
	const name = "remote sessions"
	ctx, cancel := context.WithTimeout(context.Background(), codexActivityTimeout)
	defer cancel()
	since := time.Now().Add(-24 * time.Hour)
	hosts, err := store.SessionHostsSince(ctx, db, since)
	if err != nil {
		return check{statusWarn, name, "cannot read session hosts: " + err.Error()}
	}
	local := config.Hostname()
	var total, remote int
	var others []string
	for _, h := range hosts {
		total += h.Sessions
		// The unnamed bucket is this machine's own history (pre-host-scoping
		// rows, or a client too old to send a host), not another device.
		if h.Host == "" || h.Host == local {
			continue
		}
		remote += h.Sessions
		others = append(others, fmt.Sprintf("%s (%d)", h.Host, h.Sessions))
	}
	if remote == 0 {
		return check{statusInfo, name, fmt.Sprintf("none in 24h (%d sessions, all local)", total)}
	}
	skips, serr := store.RemoteSkipsSince(ctx, db, since)
	detail := fmt.Sprintf("%d of %d sessions in 24h from %s", remote, total, strings.Join(others, ", "))
	if serr != nil {
		return check{statusInfo, name, detail + "; skipped-capture count unreadable: " + serr.Error()}
	}
	return check{statusInfo, name, fmt.Sprintf(
		"%s; %d local captures skipped for them (plan capture, git stamps and transcript harvest read THIS machine's disk)",
		detail, skips)}
}

// transportChecks reports the network posture: what the daemon binds, what URL
// it hands clients, and whether its TLS material is usable.
//
// They run before the database opens because they are answerable from config
// alone, and because a wrong answer here is what makes every remote client fail
// in a way that looks like a network problem from the other end.
func transportChecks(cfg config.Config) []check {
	if cfg.IsClient() {
		// A client install has no listener, no allowlist and no server
		// certificate, so saying so is better than reporting a bind address it
		// never binds. The posture question it DOES have an answer to is the
		// other end: does the URL it dials reach anything.
		return []check{
			{statusInfo, "role", "client: this install runs no daemon and dials " + cfg.ServerURL()},
			clientServerURLCheck(cfg),
		}
	}
	return []check{bindCheck(cfg), serverURLCheck(cfg), tlsCheck(cfg)}
}

// clientServerURLCheck probes the server a client install dials.
//
// The severity differs from serverURLCheck's on purpose. On a server install an
// unanswered URL is INFO, because doctor runs with the local daemon stopped as
// often as not and `seamlessd serve` is the whole fix. A client has no such
// benign state: the server IS the install, so until it answers there are no
// briefings, no memories and no tools on this machine, and reporting that as
// information would bury the only fault the report is able to find.
func clientServerURLCheck(cfg config.Config) check {
	const name = "server_url"
	base := cfg.ServerURL()
	probe := reachabilityProbe()
	if cfg.SocketPath() != "" {
		// A peer_auth socket: the one client that can dial it is the shared
		// constructor (it has no TLS to skip), and the message names the socket
		// rather than the placeholder URL every socket request carries.
		sock, err := cfg.HTTPClient(codexActivityTimeout)
		if err != nil {
			return check{statusFail, name, err.Error()}
		}
		probe, base = sock, cfg.DaemonAddress()
	}
	resp, err := probe.Get(cfg.ServerURL() + "/healthz")
	if err != nil {
		return check{statusFail, name, fmt.Sprintf(
			"%s is not answering (%v) -- start seamlessd there, or correct server_url", base, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusMisdirectedRequest:
		// Same 421 as serverURLCheck's, but the repair is on the other machine:
		// pointing a client's operator at their own config would send them
		// looking for a key their install does not have.
		return check{statusFail, name, fmt.Sprintf(
			"%s answers 421 to Host %q: that host belongs in the SERVER's allowed_hosts (or its server_url); add it there and restart it",
			base, cfg.ServerHost())}
	case resp.StatusCode == http.StatusForbidden && cfg.SocketPath() != "":
		return check{statusFail, name, fmt.Sprintf(
			"%s refused this OS account (403): it is not the owner and not configured under the server's peer_auth.users", base)}
	case resp.StatusCode != http.StatusOK:
		return check{statusWarn, name, fmt.Sprintf("%s answered %s", base, resp.Status)}
	}
	return check{statusOK, name, base + " reaches the server"}
}

// reachabilityProbe is the HTTP client both server_url checks use.
//
// It skips certificate verification because it is a REACHABILITY probe and
// nothing else: no credential is sent and nothing is read from the body. A probe
// that refused a private CA would report "not answering" for a server that is
// answering perfectly well, which is the opposite of what this check is for.
// Whether this machine TRUSTS that certificate is tls.ca_file's question, and it
// is asked where it matters -- on the credential-bearing surfaces (`seam
// doctor`, the hooks, the MCP bridge), all of which fail loudly when it is wrong.
func reachabilityProbe() *http.Client {
	return &http.Client{
		Timeout: codexActivityTimeout,
		Transport: &http.Transport{
			//nolint:gosec // reachability probe: nothing is read from the body and no credential is sent
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
		},
	}
}

// bindCheck reports the listener's exposure. Non-loopback without TLS is a warn
// and not a fail for the same reason warnNonLoopbackBind is a warning: binding
// wide is a legitimate choice, and refusing it would only teach people to patch
// it out.
func bindCheck(cfg config.Config) check {
	const name = "bind"
	if isLoopbackBind(cfg.Addr) {
		return check{statusOK, name, cfg.Addr + " (reachable only from this machine)"}
	}
	if cfg.TLSEnabled() {
		return check{statusOK, name, cfg.Addr + " over TLS; the bearer key is still the only authentication"}
	}
	return check{statusWarn, name, cfg.Addr +
		" is reachable beyond this machine with no TLS: the bearer key, every hook payload and the whole console travel in the clear" +
		" -- set tls.cert_file/tls.key_file, or keep it inside a trusted LAN"}
}

// serverURLCheck proves the advertised URL actually reaches this daemon, which
// is the one transport fact no amount of local inspection can establish: the
// operator can only see it fail from the client, where it looks like the network.
//
// A 421 is called out by name because it is the failure mode with the least
// obvious cause: the URL resolves, the daemon answers, and it refuses the Host
// header it was just told to advertise.
func serverURLCheck(cfg config.Config) check {
	const name = "server_url"
	base := cfg.ServerURL()
	detail := base
	if strings.TrimSpace(cfg.AdvertisedURL) == "" {
		detail = base + " (derived from addr; set server_url when clients reach this daemon by another name)"
	}
	resp, err := reachabilityProbe().Get(base + "/healthz")
	if err != nil {
		// Not a failure: doctor runs with the daemon stopped as often as not.
		return check{statusInfo, name, detail + " -- not answering right now (" + err.Error() + ")"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusMisdirectedRequest {
		return check{statusFail, name, fmt.Sprintf(
			"%s answers 421 to its own Host header %q: add the host to allowed_hosts (or correct server_url) and restart",
			base, cfg.ServerHost())}
	}
	if resp.StatusCode != http.StatusOK {
		return check{statusWarn, name, fmt.Sprintf("%s answered %s", detail, resp.Status)}
	}
	return check{statusOK, name, detail + " reaches this daemon"}
}

// tlsExpiryWarning is how close to expiry a certificate earns a warning. A
// month is enough notice to renew a mkcert/private-CA certificate by hand, which
// is the documented setup and has no auto-renewal behind it.
const tlsExpiryWarning = 30 * 24 * time.Hour

// tlsCheck parses the configured pair and reports what a client will actually
// experience: an expiring certificate, or one whose SANs do not cover the name
// the daemon advertises. Both fail at the client's TLS handshake with a message
// that says nothing about which file on the server is wrong.
func tlsCheck(cfg config.Config) check {
	const name = "tls"
	if !cfg.TLSEnabled() {
		return check{statusInfo, name, "off (http); set tls.cert_file and tls.key_file to serve https"}
	}
	pair, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		return check{statusFail, name, "cannot load the certificate/key pair: " + err.Error()}
	}
	leaf := pair.Leaf
	if leaf == nil {
		// Go only populates Leaf when it parses the chain itself, which
		// LoadX509KeyPair does not guarantee across versions.
		leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return check{statusFail, name, "cannot parse the certificate: " + err.Error()}
		}
	}
	host := cfg.ServerHost()
	var problems []string
	switch left := time.Until(leaf.NotAfter); {
	case left <= 0:
		problems = append(problems, fmt.Sprintf("EXPIRED on %s", leaf.NotAfter.UTC().Format(time.DateOnly)))
	case left < tlsExpiryWarning:
		problems = append(problems, fmt.Sprintf("expires in %dd (%s)",
			int(left.Hours()/24), leaf.NotAfter.UTC().Format(time.DateOnly)))
	}
	if err := leaf.VerifyHostname(host); err != nil {
		problems = append(problems, fmt.Sprintf(
			"does not cover %q (SANs: %s) -- clients dialing server_url will refuse it",
			host, strings.Join(certNames(leaf), ", ")))
	}
	if len(problems) > 0 {
		return check{statusWarn, name, fmt.Sprintf("%s: %s", cfg.TLS.CertFile, strings.Join(problems, "; "))}
	}
	return check{statusOK, name, fmt.Sprintf("%s covers %s, valid until %s",
		cfg.TLS.CertFile, host, leaf.NotAfter.UTC().Format(time.DateOnly))}
}

// certNames lists a certificate's subject alternative names for the report: DNS
// names and IP addresses, which are the two forms a Seamless server_url can take.
func certNames(c *x509.Certificate) []string {
	names := append([]string(nil), c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

// hooksCheck reports whether the Claude Code Seamless hooks are installed. It
// looks in the global settings (~/.claude/settings.json) and the project-scoped
// dogfood settings (./.claude/settings.json), reporting the first location that
// has all exact current definitions, or a warning when they are partial, stale,
// or absent. Claude Code may strip the ownership marker; exact functional
// definitions remain current without it.
//
// When nothing is installed AND Claude Code is not detected on this machine, it
// resolves to a quiet OK "not detected" line rather than a warning -- symmetric
// with codexChecks, so a Codex-only user is not perpetually nagged to install a
// client they do not run.
func hooksCheck(cfg config.Config) check {
	installed, err := hooks.InstalledEvents(hooks.ClientClaudeCode)
	if err != nil {
		return check{statusFail, "hooks", "cannot build desired Claude Code definitions: " + err.Error()}
	}
	var candidates []string
	if home, err := expandHome("~/.claude/settings.json"); err == nil {
		candidates = append(candidates, home)
	}
	candidates = append(candidates, filepath.Join(".claude", "settings.json"))

	var best check
	found := false
	for _, path := range candidates {
		status, err := hooks.InstalledStatus(doctorInstallOptions(hooks.ClientClaudeCode, path, cfg))
		if err != nil {
			if !found {
				best = check{statusWarn, "hooks", fmt.Sprintf("cannot inspect %s: %v", path, err)}
				found = true
			}
			continue
		}
		if len(status.Owned) == 0 {
			continue
		}
		if hookDefinitionsCurrent(installed, status) {
			return check{statusOK, "hooks", hookDefinitionDetail(path, installed, status)}
		}
		if !found {
			best = check{statusWarn, "hooks",
				hookDefinitionDetail(path, installed, status) + "; run: seamlessd install-hooks --client claude"}
			found = true
		}
	}
	if found {
		return best
	}
	if !claudeDetected() {
		return check{statusOK, "hooks", "Claude Code not detected (no claude CLI or ~/.claude)"}
	}
	return check{statusWarn, "hooks", "not installed (run: seamlessd install-hooks)"}
}

// codexChecks reports the shared local Codex host used by the desktop app, CLI,
// and IDE extension: discoverable runtime versions, hooks in
// $CODEX_HOME/hooks.json, and whether the seam mcp-proxy bridge is registered
// with `codex mcp`. Definition validity, supported trust knowledge, observed
// activity, and MCP runnability are deliberately separate results: none is a
// proxy for another. It never FAILs -- Codex is an optional client -- so a
// machine with no Codex install/config resolves to one quiet OK line.
func codexChecks(cfg config.Config, db *sql.DB) []check {
	hooksPath, herr := expandHome(defaultCodexHooksPath())

	var status hooks.InstallStatus
	var statusErr error
	if herr == nil {
		status, statusErr = hooks.InstalledStatus(
			doctorInstallOptions(hooks.ClientCodex, hooksPath, cfg))
	}

	// A CODEX_HOME/~/.codex directory counts as detection even without a CLI on
	// PATH: it may contain opted-in hook or MCP configuration that still needs a
	// visible diagnosis. Only a genuinely absent, unconfigured client is quiet.
	if !codexDetected() && herr == nil && statusErr == nil && len(status.Owned) == 0 {
		return []check{{statusOK, "codex", "not detected (no Codex CLI, initialized home, or Seamless configuration)"}}
	}

	installed, eventsErr := hooks.InstalledEvents(hooks.ClientCodex)
	var hooksChk check
	switch {
	case herr != nil:
		hooksChk = check{statusWarn, "codex hooks", fmt.Sprintf(
			"cannot resolve hooks path: %v; set HOME/CODEX_HOME, then run: seamlessd install-hooks --client codex", herr)}
	case eventsErr != nil:
		hooksChk = check{statusWarn, "codex hooks", "cannot build desired definitions: " + eventsErr.Error() +
			"; run: seamlessd install-hooks --client codex"}
	case statusErr != nil:
		hooksChk = check{statusWarn, "codex hooks", fmt.Sprintf(
			"cannot inspect %s: %v; fix or restore the JSON, then run: seamlessd install-hooks --client codex",
			hooksPath, statusErr)}
	default:
		detail := hookDefinitionDetail(hooksPath, installed, status)
		problems := recordedHookPathProblems(hooks.ClientCodex, hooksPath)
		if len(problems) > 0 {
			detail += "; not runnable: " + strings.Join(problems, ", ")
		}
		if hookDefinitionsCurrent(installed, status) && len(problems) == 0 {
			hooksChk = check{statusOK, "codex hooks", detail}
		} else {
			hooksChk = check{statusWarn, "codex hooks",
				detail + "; run: seamlessd install-hooks --client codex"}
		}
	}

	checks := codexRuntimeChecks()
	checks = append(checks,
		hooksChk,
		check{statusWarn, "codex hook trust", "trust unverified; inspect /hooks in Codex CLI; the desktop app does not expose that command"},
	)
	if db != nil {
		checks = append(checks, codexHookActivityCheck(db))
	}
	checks = append(checks, codexMCPCheck(resolveSeamBin(""), absConfigPath(cfg.SourcePath())))
	return checks
}

// doctorInstallOptions builds the same desired definition install-hooks would
// write now. It must not recover the desired binary/config from the existing
// hook: comparing an old definition with paths read from itself makes uniform
// drift tautologically current.
func doctorInstallOptions(client hooks.Client, settingsPath string, cfg config.Config) hooks.InstallOptions {
	return hooks.InstallOptions{
		Client: client, SettingsPath: settingsPath, BaseURL: cfg.HookBaseURL(),
		APIKey: cfg.MCP.APIKey, SeamBin: resolveSeamBin(""), ConfigPath: absConfigPath(cfg.SourcePath()),
	}
}

func hookDefinitionsCurrent(want []string, status hooks.InstallStatus) bool {
	if len(status.Current) != len(want) || len(status.Stale) != 0 {
		return false
	}
	current := make(map[string]struct{}, len(status.Current))
	for _, event := range status.Current {
		current[event] = struct{}{}
	}
	for _, event := range want {
		if _, ok := current[event]; !ok {
			return false
		}
	}
	return true
}

func hookDefinitionDetail(path string, want []string, status hooks.InstallStatus) string {
	current := make(map[string]struct{}, len(status.Current))
	stale := make(map[string]struct{}, len(status.Stale))
	for _, event := range status.Current {
		current[event] = struct{}{}
	}
	for _, event := range status.Stale {
		stale[event] = struct{}{}
	}

	var currentNames, staleNames, missingNames []string
	for _, event := range want {
		switch {
		case hasHookEvent(current, event):
			currentNames = append(currentNames, event)
		case hasHookEvent(stale, event):
			staleNames = append(staleNames, event)
		default:
			missingNames = append(missingNames, event)
		}
	}
	return fmt.Sprintf("definitions in %s (current: %s; stale: %s; missing: %s)",
		path, hookEventNames(currentNames), hookEventNames(staleNames), hookEventNames(missingNames))
}

func hasHookEvent(set map[string]struct{}, event string) bool {
	_, ok := set[event]
	return ok
}

func hookEventNames(events []string) string {
	if len(events) == 0 {
		return "none"
	}
	return strings.Join(events, ", ")
}

// recordedHookPathProblems checks what Codex will actually execute, separately
// from desired-definition comparison. A definition can have the right shape
// and still be non-operational because its target was deleted after install.
func recordedHookPathProblems(client hooks.Client, settingsPath string) []string {
	seamBin, configPath, ok := hooks.RecordedCommandPaths(client, settingsPath)
	if !ok {
		return nil
	}
	if expanded, err := expandHome(seamBin); err == nil {
		seamBin = expanded
	}
	var problems []string
	if !commandPathExists(seamBin) {
		problems = append(problems, fmt.Sprintf("hook executable %q is missing", seamBin))
	}
	if configPath != "" {
		if expanded, err := expandHome(configPath); err == nil {
			configPath = expanded
		}
		if info, err := os.Stat(configPath); err != nil || info.IsDir() {
			problems = append(problems, fmt.Sprintf("hook config %q is missing", configPath))
		}
	}
	return problems
}

const codexActivityTimeout = 2 * time.Second

func codexHookActivityCheck(db *sql.DB) check {
	ctx, cancel := context.WithTimeout(context.Background(), codexActivityTimeout)
	defer cancel()
	event, observedAt, ok, err := latestCodexHookObservation(ctx, db)
	if err != nil {
		return check{statusWarn, "codex hook activity",
			"cannot read recent observations: " + err.Error() + "; inspect /hooks in Codex"}
	}
	if !ok {
		return check{statusInfo, "codex hook activity",
			"no SessionStart/UserPromptSubmit observation recorded; trust remains unverified"}
	}
	return check{statusInfo, "codex hook activity", fmt.Sprintf(
		"last observed %s at %s; supporting evidence only, not proof that current definitions are trusted",
		event, observedAt.UTC().Format(time.RFC3339))}
}

func latestCodexHookObservation(ctx context.Context, db *sql.DB) (string, time.Time, bool, error) {
	row := db.QueryRowContext(ctx, `
		SELECT ts, json_extract(payload, '$.hook')
		FROM events
		WHERE kind IN (?, ?)
		  AND CASE WHEN json_valid(payload) THEN
			json_extract(payload, '$.external_client') = ?
			AND json_extract(payload, '$.hook') IN (?, ?)
		  ELSE 0 END
		ORDER BY ts DESC, id DESC
		LIMIT 1`,
		string(core.EventInjected), string(core.EventHookPrompt), string(hooks.ClientCodex),
		"session-start", "user-prompt-submit")
	var tsText, hook string
	if err := row.Scan(&tsText, &hook); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", time.Time{}, false, nil
		}
		return "", time.Time{}, false, fmt.Errorf("query Codex hook activity: %w", err)
	}
	observedAt, err := core.ParseTime(tsText)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("parse Codex hook activity timestamp: %w", err)
	}
	switch hook {
	case "session-start":
		hook = "SessionStart"
	case "user-prompt-submit":
		hook = "UserPromptSubmit"
	}
	return hook, observedAt, true, nil
}

// codexMCPCheck compares Codex's machine-readable registration with the same
// desired state used by install-hooks. It is bounded by a short timeout and
// warns rather than fails because Codex is an optional client.
func codexMCPCheck(seamBin, configPath string) check {
	codex, err := exec.LookPath("codex")
	if err != nil {
		return check{statusWarn, "codex mcp",
			"management CLI not found; MCP state is unverified and automated setup is incomplete; " +
				codexAppMCPSetupHint(seamBin, configPath)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()
	return codexMCPCheckWithRunner(ctx, execMCPCommandRunner{
		client: "codex", path: codex, timeout: mcpCommandTimeout,
	}, seamBin, configPath)
}

func codexMCPCheckWithRunner(ctx context.Context, runner mcpCommandRunner, seamBin, configPath string) check {
	want, err := desiredCodexMCPState(seamBin, configPath)
	if err != nil {
		return check{statusWarn, "codex mcp", "cannot build desired registration: " + err.Error() +
			"; install the seam CLI, then run: seamlessd install-hooks --client codex"}
	}
	got, present, err := inspectCodexMCP(ctx, runner)
	if err != nil {
		return check{statusWarn, "codex mcp", "cannot inspect registration: " + err.Error() +
			"; run: codex mcp get seamless --json"}
	}
	if !present {
		return check{statusWarn, "codex mcp", "seamless not registered (run: seamlessd install-hooks --client codex)"}
	}
	class, drift := classifyCodexMCPState(got, want)
	switch class {
	case mcpRegIncompatible:
		return check{statusWarn, "codex mcp", fmt.Sprintf(
			"reserved name has an incompatible registration (%s); run: codex mcp remove seamless; then seamlessd install-hooks --client codex",
			strings.Join(drift, ", "))}
	case mcpRegOwnedDrifted:
		return check{statusWarn, "codex mcp", fmt.Sprintf(
			"owned registration is stale (%s; run: seamlessd install-hooks --client codex)",
			strings.Join(drift, ", "))}
	case mcpRegExact:
		// Continue to the local target checks below.
	default:
		return check{statusWarn, "codex mcp",
			"registration has an unknown classification; run: codex mcp get seamless --json"}
	}
	if problems := mcpBridgePathProblems(want.Transport.Command, want.Transport.Args); len(problems) > 0 {
		return check{statusWarn, "codex mcp", fmt.Sprintf(
			"exact registration is not runnable (%s; repair the targets, then run: seamlessd install-hooks --client codex)",
			strings.Join(problems, ", "))}
	}
	return check{statusOK, "codex mcp", "exact enabled stdio bridge (seam mcp-proxy); target paths exist"}
}

// claudeDesktopChecks reports the Claude app chat surface: whether the desktop
// config's reserved mcpServers entry matches the same desired stdio bridge
// install-hooks --client claude-desktop would write now -- never paths
// recovered from the entry itself, which would make uniform drift
// tautologically current. It emits no lines at all (symmetric with
// claudeRuntimeChecks) when neither the app nor a desktop config file is
// present, and it never FAILs: the chat surface is an optional, explicit
// opt-in target.
func claudeDesktopChecks(seamBin, configPath string) []check {
	path, err := defaultClaudeDesktopConfigPath()
	if err != nil {
		// No known config location (no macOS/Windows app layout, or no home):
		// the app cannot be installed here, so there is nothing to diagnose.
		return nil
	}
	return claudeDesktopChecksFor(path, claudeDesktopAppDetected(), seamBin, configPath)
}

func claudeDesktopChecksFor(path string, appDetected bool, seamBin, configPath string) []check {
	info, statErr := os.Lstat(path)
	configExists := statErr == nil && !info.IsDir()
	if !appDetected && !configExists {
		return nil
	}
	return []check{claudeDesktopMCPCheck(path, seamBin, configPath)}
}

// claudeDesktopMCPCheck compares the desktop config's reserved entry with the
// desired stdio bridge. Everything here is file evidence: the app reads the
// config only at startup and exposes no way to ask what it actually loaded, so
// even an exact entry reports the running app's state as unverifiable rather
// than assumed. An absent entry is informational, not drift -- the chat
// surface is never auto-selected by install-hooks.
func claudeDesktopMCPCheck(path, seamBin, configPath string) check {
	want, err := desiredClaudeDesktopMCPServer(seamBin, configPath)
	if err != nil {
		return check{statusWarn, "claude desktop mcp", "cannot build desired registration: " + err.Error() +
			"; install the seam CLI, then run: seamlessd install-hooks --client claude-desktop"}
	}
	_, servers, _, err := loadClaudeDesktopConfig(path)
	if err != nil {
		return check{statusWarn, "claude desktop mcp", fmt.Sprintf(
			"cannot inspect %s: %v; fix or restore the JSON, then run: seamlessd install-hooks --client claude-desktop", path, err)}
	}
	raw, present := servers[seamlessMCPName]
	if !present {
		return check{statusInfo, "claude desktop mcp",
			"chat surface not registered; opt in with: seamlessd install-hooks --client claude-desktop; or " +
				claudeDesktopMCPSetupHint(seamBin, configPath)}
	}
	got, parseErr := parseClaudeDesktopMCPServer(raw)
	if parseErr != nil {
		return check{statusWarn, "claude desktop mcp", fmt.Sprintf(
			"reserved name has an unrecognized shape in %s (%v); remove it in the Claude app (Settings > Developer > Edit Config), then run: seamlessd install-hooks --client claude-desktop",
			path, parseErr)}
	}
	class, drift := classifyClaudeDesktopMCP(got, want)
	switch class {
	case mcpRegIncompatible:
		return check{statusWarn, "claude desktop mcp", fmt.Sprintf(
			"reserved name has an incompatible registration (%s); remove it in the Claude app (Settings > Developer > Edit Config), then run: seamlessd install-hooks --client claude-desktop",
			strings.Join(drift, ", "))}
	case mcpRegOwnedDrifted:
		return check{statusWarn, "claude desktop mcp", fmt.Sprintf(
			"owned registration is stale (%s; run: seamlessd install-hooks --client claude-desktop)",
			strings.Join(drift, ", "))}
	case mcpRegExact:
		// Continue to the local target checks below.
	default:
		return check{statusWarn, "claude desktop mcp",
			"registration has an unknown classification; inspect " + path}
	}
	if problems := mcpBridgePathProblems(want.Command, want.Args); len(problems) > 0 {
		return check{statusWarn, "claude desktop mcp", fmt.Sprintf(
			"exact registration is not runnable (%s; repair the targets, then run: seamlessd install-hooks --client claude-desktop)",
			strings.Join(problems, ", "))}
	}
	return check{statusOK, "claude desktop mcp", fmt.Sprintf(
		"exact stdio bridge (seam mcp-proxy) in %s; target paths exist; whether the running app has loaded it is unverifiable (the app reads the config at startup)",
		path)}
}

// featureSkillsCheck reports a client-side skill package that documents an
// OPTIONAL feature the owner has switched off. The skill lives in the client's
// own config directory (~/.claude/skills, $CODEX_HOME/skills), which the daemon
// cannot reach when a toggle flips in the console -- and must not delete behind
// the owner's back -- so the skill and the feature drift apart until the next
// install-hooks run brings them back in line.
//
// That drift is informational, not broken: nothing fails, no data is at risk,
// and every gated tool is already refused by the MCP tool filter. What it costs
// is an agent reading a skill that names tools its server no longer exposes, so
// the line names the package, the feature, and both ways out.
func featureSkillsCheck(db *sql.DB, cfg config.Config) check {
	const name = "feature skills"
	ctx, cancel := context.WithTimeout(context.Background(), codexActivityTimeout)
	defer cancel()
	effective, _, err := store.FeaturesConfig(ctx, db, cfg.Features)
	if err != nil {
		return check{statusWarn, name, "cannot read the stored feature toggles: " + err.Error()}
	}
	opts, err := agentskills.OptionsFromEnvironment()
	if err != nil {
		return check{statusWarn, name, "cannot resolve the client skill homes: " + err.Error()}
	}

	var stale []string
	for _, feature := range features.Registry() {
		if feature.Skill == "" || feature.Enabled(effective) {
			continue
		}
		for _, client := range []agentskills.Client{agentskills.ClientClaude, agentskills.ClientCodex} {
			path, installed, err := agentskills.Installed(client, opts, feature.Skill)
			if err != nil {
				return check{statusWarn, name, fmt.Sprintf("cannot inspect the %s skill root: %v", client, err)}
			}
			if installed {
				stale = append(stale, fmt.Sprintf("%s (%s, feature %s is off)", path, client, feature.Key))
			}
		}
	}
	if len(stale) == 0 {
		return check{statusOK, name, "no installed skill documents a disabled optional feature"}
	}
	return check{statusInfo, name, fmt.Sprintf(
		"%s -- agents may see a skill referencing disabled tools; rerun `seamlessd install-hooks` to remove it, or re-enable the feature in the console (Settings -> Features)",
		strings.Join(stale, "; "))}
}

// gardenerCheck reports the gardener ticker configuration.
func gardenerCheck(cfg config.Config) check {
	g := cfg.Gardener
	if !g.Enabled {
		return check{statusWarn, "gardener", "disabled (set gardener.enabled: true to run maintenance passes)"}
	}
	return check{statusOK, "gardener", fmt.Sprintf(
		"enabled (every %dm; dedup>=%.2f, staleness %dd, digest %dd)",
		g.IntervalMinutes, g.DedupThreshold, g.StalenessDays, g.DigestDays)}
}

// embedderCheck probes the configured embedder so a misconfiguration (bad key,
// unreachable Ollama) is caught before it silently degrades recall to FTS. A
// missing credential or a failed probe is a warning, not a failure -- recall
// still works lexically.
func embedderCheck(cfg config.Config) check {
	if missing, why := missingEmbedCredential(cfg); missing {
		return check{statusWarn, "embedder", why + " (recall degrades to FTS)"}
	}
	e, err := llm.NewEmbedder(cfg.LLM)
	if err != nil {
		return check{statusWarn, "embedder", "disabled: " + err.Error() + " (recall degrades to FTS)"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := e.Embed(ctx, "seamless doctor reachability probe"); err != nil {
		return check{statusWarn, "embedder", fmt.Sprintf(
			"provider=%s model=%s unreachable: %v (recall degrades to FTS)", cfg.LLM.Provider, e.Model(), err)}
	}
	return check{statusOK, "embedder", fmt.Sprintf("provider=%s model=%s reachable", cfg.LLM.Provider, e.Model())}
}

// missingEmbedCredential reports whether the selected provider lacks the
// credential it needs to embed, so doctor can skip a doomed network probe.
func missingEmbedCredential(cfg config.Config) (bool, string) {
	switch cfg.LLM.Provider {
	case config.ProviderOpenAI:
		if strings.TrimSpace(cfg.LLM.OpenAI.APIKey) == "" {
			return true, "provider=openai but openai.api_key is empty"
		}
	case config.ProviderAnthropic:
		return true, "provider=anthropic has no embeddings API"
	}
	return false, ""
}

// mcpToolsCheck asserts the MCP server registers exactly the expected number of
// tools (the P4 target is 26). It builds a throwaway server -- tool registration
// touches no external dependency -- and compares the registered count to
// mcp.ToolCount, catching a tool that was written but never wired in (or vice
// versa).
//
// This counts REGISTRATION, which the optional-feature tool filter does not
// touch: gating shrinks the live tools/list only, so a disabled feature must
// never move this number. Registered-vs-exposed is the line the CLIENTS run --
// `seam doctor`, and clientMCPToolsCheck below -- because it needs a running
// server to ask; this one deliberately does not, which is what makes it the
// useful check on a SERVER install whose daemon may well be stopped.
func mcpToolsCheck() check {
	srv := mcp.New(mcp.Config{})
	n := srv.NumTools()
	if n != mcp.ToolCount {
		return check{statusFail, "mcp_tools",
			fmt.Sprintf("registered %d tools but ToolCount is %d", n, mcp.ToolCount)}
	}
	return check{statusOK, "mcp_tools", fmt.Sprintf("%d tools registered", n)}
}

// clientMCPTimeout bounds the whole tools/list probe: the handshake plus one
// list, both answered from the server's memory. Anything slower is a wedged
// daemon rather than a busy one, and doctor must not hang on it.
const clientMCPTimeout = 5 * time.Second

// clientMCPToolsCheck is the client-role mcp_tools line: what the SERVER exposes
// right now, counted by asking it, rather than what this binary happens to have
// compiled in.
//
// The distinction is the whole reason it exists. A client install runs no MCP
// server, so mcpToolsCheck's registration count is a fact about a process on
// another machine -- one this report cannot see, and one that would keep reading
// "ok" while the server was down, misconfigured, or refusing this install's key.
// Dialing it is also the only way a client learns its key works: nothing else in
// the client report presents a credential (clientServerURLCheck deliberately
// sends none, because it is a reachability probe).
//
// Feature-awareness is not optional here. Optional features ship OFF, so a
// default server EXPOSES fewer tools than it REGISTERS; judging the live count
// against a bare mcp.ToolCount would report a red on every healthy default
// install. The verdict is features.ToolCountVerdict -- the same one `seam
// doctor` renders, so the two clients cannot disagree about what a number means.
//
// It FAILs rather than warns when the dial does not answer: on a client the
// server IS the install, so a tool surface that cannot be reached is not a
// degraded report but the whole of what this machine was going to do.
func clientMCPToolsCheck(cfg config.Config) check {
	const name = "mcp_tools"
	base := cfg.ServerURL()

	ctx, cancel := context.WithTimeout(context.Background(), clientMCPTimeout)
	defer cancel()
	live, err := remoteToolCount(ctx, cfg)
	if err != nil {
		return check{statusFail, name, fmt.Sprintf(
			"tools/list failed against %s: %v -- check that seamlessd is running there and that mcp.api_key matches the server's key",
			base, err)}
	}

	// Failure-soft, exactly as in `seam doctor`: without the server's effective
	// feature state there is no single expected number, only a range, and the
	// verdict says out loud that it could not read the state rather than
	// asserting a number it does not know. A nil feats is what carries that.
	feats, ferr := clientConsoleFeatures(cfg, base)
	why := ""
	if ferr != nil {
		why = ferr.Error()
	}
	ok, detail := features.ToolCountVerdict(mcp.ToolCount, live, feats, why)
	if !ok {
		return check{statusFail, name, detail +
			" -- a gap this arithmetic cannot explain is either the tool gate misbehaving or a client and server on different versions"}
	}
	return check{statusOK, name, detail}
}

// remoteToolCount performs the MCP handshake against the configured server and
// returns how many tools its tools/list advertises.
//
// The HTTP client is config.Config.HTTPClient -- the ONE TLS trust decision,
// shared with the seam CLI and with install-hooks. That sharing is a
// prerequisite rather than a tidiness win: this function presents the bearer
// key, and the only reason a caller here would reach for its own client is to
// avoid the import, which is how a credential ends up on an unverified
// connection. A tls.ca_file this machine cannot use is an error from that
// constructor and is reported as one, never a silent fall back to the system
// pool.
func remoteToolCount(ctx context.Context, cfg config.Config) (int, error) {
	hc, err := cfg.HTTPClient(clientMCPTimeout)
	if err != nil {
		return 0, err
	}
	headers := map[string]string{"Authorization": "Bearer " + cfg.MCP.APIKey}
	if host := config.Hostname(); host != "" {
		// The same connection-scoped machine name every other client sends, so
		// the probe is attributed to this box rather than to the server itself.
		headers[mcp.HostHeader] = host
	}
	cli, err := mcpclient.NewStreamableHttpClient(cfg.ServerURL()+"/api/mcp",
		mcptransport.WithHTTPHeaders(headers),
		mcptransport.WithHTTPBasicClient(hc))
	if err != nil {
		return 0, err
	}
	defer func() { _ = cli.Close() }()
	if err := cli.Start(ctx); err != nil {
		return 0, err
	}
	var initReq mcpgo.InitializeRequest
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "seamlessd-doctor", Version: version}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		return 0, err
	}
	tools, err := cli.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		return 0, err
	}
	return len(tools.Tools), nil
}

// apiKeyCheck warns when the static bearer key is unset. On a true first run
// (no config file at all) the message points at serve, which generates one.
func apiKeyCheck(cfg config.Config) check {
	if cfg.IsClient() && cfg.SocketPath() != "" {
		return check{statusInfo, "mcp.api_key", "not used: server_url is a peer_auth socket, which identifies this OS account by its uid"}
	}
	if strings.TrimSpace(cfg.MCP.APIKey) == "" {
		if cfg.SourcePath() == "" {
			return check{statusWarn, "mcp.api_key", "empty -- `seamlessd serve` generates one on first run (or set SEAMLESS_MCP_API_KEY)"}
		}
		return check{statusWarn, "mcp.api_key", "empty -- set SEAMLESS_MCP_API_KEY (or mcp.api_key) before exposing /api/mcp"}
	}
	return check{statusOK, "mcp.api_key", "set"}
}

// llmCheck warns when the selected provider is missing the credential it needs.
func llmCheck(cfg config.Config) check {
	p := cfg.LLM.Provider
	switch p {
	case config.ProviderOpenAI:
		if strings.TrimSpace(cfg.LLM.OpenAI.APIKey) == "" {
			return check{statusWarn, "llm", "provider=openai but openai.api_key empty (chat + embeddings will fail)"}
		}
	case config.ProviderAnthropic:
		if strings.TrimSpace(cfg.LLM.Anthropic.APIKey) == "" {
			return check{statusWarn, "llm", "provider=anthropic but anthropic.api_key empty"}
		}
	case config.ProviderOllama:
		// Local; no credential required.
	}
	return check{statusOK, "llm", "provider=" + p}
}

// reportChecks prints each check and returns an error if any FAILed.
func reportChecks(checks []check) error {
	var failed int
	for _, c := range checks {
		fmt.Printf("  [%-4s] %s: %s\n", c.status.label(), c.name, c.detail)
		if c.status == statusFail {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("doctor: %d check(s) failed", failed)
	}
	return nil
}
