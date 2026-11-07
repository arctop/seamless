package main

// seam touchid enroll's install half: fetch or copy Seamless Approve, check
// its code signature, and move it into place. Every external program --
// codesign, spctl, ditto -- runs through env.exec, so a test fakes each.
//
// The release asset is fetched the way the updater fetches release assets
// (cmd/seamlessd/update_fetch.go): anonymously (update.NewHTTPClient: default
// TLS, the environment's proxy, https-only redirects, never a token, never
// config.HTTPClient), from the tag's own download directory, size-capped, and
// checked against the release's checksums.txt (update.VerifyChecksum, the
// installers' rule). What this CLI does not do is the updater's Sigstore check
// of checksums.txt: that verifier would more than double seam's size for
// every install. The publisher check is Apple's instead, and it is the one the
// key itself depends on -- the keychain access group that holds the Secure
// Enclave key can be claimed only by an app signed by the same team.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/arctop/seamless/internal/update"
)

// touchIDUserAgent is the fixed User-Agent of the release fetch: no version,
// host or install identifier.
const touchIDUserAgent = "seam-touchid-install"

// Size caps: the app's zip is a few MB and checksums.txt a few KB; the caps
// only bound a misrouted reply.
const (
	maxApproverZipBytes = 64 << 20
	maxChecksumsBytes   = 1 << 20
)

// teamIdentifier is the shape of an Apple Developer team ID. It goes into a
// code requirement, so nothing else may.
var teamIdentifier = regexp.MustCompile(`^[A-Z0-9]{10}$`)

// errApproverRefused is an app that failed a check: it is neither installed
// nor run.
var errApproverRefused = errors.New("refusing Seamless Approve")

// touchIDWorld is what seam touchid reaches beyond env: the home directory,
// this binary, its release version, and the release host. Tests replace
// newTouchIDWorld.
type touchIDWorld struct {
	home    func() (string, error)
	self    func() (string, error)
	version func() (update.Version, bool)
	client  *http.Client
	// downloadBase is update.ReleaseDownloadBase in production.
	downloadBase string
}

var newTouchIDWorld = func() touchIDWorld {
	return touchIDWorld{home: os.UserHomeDir, self: selfPath, version: ownReleaseVersion,
		client: update.NewHTTPClient(), downloadBase: update.ReleaseDownloadBase}
}

// selfPath is this seam binary, symlinks resolved (a Homebrew or ~/.local/bin
// link names the real file codesign reads).
func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// ownReleaseVersion is this seam's release version: the toolchain's record
// of the module version it was built at, when that is exactly a release tag.
// A build off a tag, from a dirty tree, or with no VCS stamp (go test,
// -buildvcs=false) has none, and so no release asset to fetch.
func ownReleaseVersion() (update.Version, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return update.Version{}, false
	}
	return update.ParseTag(info.Main.Version)
}

// installApprover gets Seamless Approve to target: the release asset of this
// seam's version, or the local build appFlag names. installed is false when
// the owner declined, with nothing changed. An app already at target is
// replaced only after a y/N; kept, it is checked as a local build is and used.
func installApprover(ctx context.Context, e *env, w touchIDWorld, in *bufio.Reader, appFlag *presentString, target string) (bool, error) {
	if _, err := os.Lstat(target); err == nil {
		ans, _ := prompt(e, in, fmt.Sprintf("Seamless Approve is already installed at %s. Replace it? [y/N] ", printable(target)))
		if !isYes(ans) {
			fmt.Fprintln(e.stdout, "Keeping the installed app.")
			if err := verifyApprover(ctx, e, w, in, target, false); err != nil {
				return false, err
			}
			return true, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("look for an installed Seamless Approve: %w", err)
	}

	var v update.Version
	release := !appFlag.set
	if release {
		var ok bool
		if v, ok = w.version(); !ok {
			return false, errors.New("this seam is not a release build, so it has no release of Seamless Approve to fetch: " +
				"install a local build with --app <path to Seamless Approve.app>")
		}
	}

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.MkdirTemp(dir, ".seamless-approve-")
	if err != nil {
		return false, fmt.Errorf("make a staging directory in %s: %w", dir, err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()
	staged := filepath.Join(tmp, approverAppName)
	if release {
		err = stageRelease(ctx, e, w, v, tmp)
	} else {
		err = stageLocal(ctx, e, appFlag.val, tmp, staged)
	}
	if err != nil {
		return false, err
	}
	if fi, err := os.Lstat(staged); err != nil || !fi.IsDir() {
		return false, fmt.Errorf("%w: the archive holds no %s", errApproverRefused, approverAppName)
	}
	if err := verifyApprover(ctx, e, w, in, staged, release); err != nil {
		return false, err
	}
	if err := swapInstall(staged, target, tmp, &keep); err != nil {
		return false, err
	}
	fmt.Fprintf(e.stdout, "Installed Seamless Approve at %s.\n", printable(target))
	return true, nil
}

// stageRelease fetches release v's app and its checksums.txt, checks the one
// against the other, and unpacks the app into tmp.
func stageRelease(ctx context.Context, e *env, w touchIDWorld, v update.Version, tmp string) error {
	name := update.ApproverAsset(v)
	fmt.Fprintf(e.stdout, "Fetching %s from release v%s...\n", name, v)
	sums, err := fetchReleaseAsset(ctx, w, update.AssetURL(w.downloadBase, v, update.ChecksumsAsset), maxChecksumsBytes)
	if err != nil {
		return err
	}
	zip, err := fetchReleaseAsset(ctx, w, update.AssetURL(w.downloadBase, v, name), maxApproverZipBytes)
	if err != nil {
		return err
	}
	if err := update.VerifyChecksum(sums, name, zip); err != nil {
		return fmt.Errorf("%w: %w", errApproverRefused, err)
	}
	archive := filepath.Join(tmp, name)
	if err := os.WriteFile(archive, zip, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", archive, err)
	}
	return ditto(ctx, e, "-x", "-k", archive, tmp)
}

// stageLocal copies a local build into tmp: an app bundle as it is, or a zip
// of one unpacked.
func stageLocal(ctx context.Context, e *env, src, tmp, staged string) error {
	abs, err := filepath.Abs(strings.TrimSpace(src))
	if err != nil {
		return fmt.Errorf("--app %s: %w", src, err)
	}
	fi, err := os.Stat(abs)
	switch {
	case err != nil:
		return fmt.Errorf("--app %s: %w", src, err)
	case fi.IsDir() && strings.HasSuffix(abs, ".app"):
		return ditto(ctx, e, abs, staged)
	case fi.Mode().IsRegular() && strings.HasSuffix(abs, ".zip"):
		return ditto(ctx, e, "-x", "-k", abs, tmp)
	default:
		return fmt.Errorf("--app %s is neither an .app bundle nor a .zip of one", src)
	}
}

// ditto runs ditto, which copies and unpacks app bundles with their code
// signatures, extended attributes and links intact.
func ditto(ctx context.Context, e *env, args ...string) error {
	var errb strings.Builder
	code, err := e.run(ctx, append([]string{"ditto"}, args...), nil, io.Discard, &errb)
	if err != nil {
		return fmt.Errorf("run ditto: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("ditto %s failed (exit status %d): %s", strings.Join(args, " "), code, strings.TrimSpace(errb.String()))
	}
	return nil
}

// fetchReleaseAsset downloads one release asset: https only, anonymous, the
// fixed User-Agent, at most limit bytes.
func fetchReleaseAsset(ctx context.Context, w touchIDWorld, url string, limit int64) ([]byte, error) {
	if err := update.RequireHTTPS(url); err != nil {
		return nil, fmt.Errorf("refusing to fetch: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", url, err)
	}
	req.Header.Set("User-Agent", touchIDUserAgent)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		return nil, fmt.Errorf("%s: not published (a release older than the Touch ID approver carries no app; install a local build with --app)", url)
	default:
		return nil, fmt.Errorf("%s: unexpected status %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read: %w", url, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: response exceeds %d bytes", url, limit)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%s: empty response", url)
	}
	return body, nil
}

// codeSignature is what codesign -dvv reports of a signature.
type codeSignature struct {
	identifier string
	team       string // "" when not set (ad hoc, or no Developer ID)
	authority  string // the leaf certificate's name, when there is one
}

// describeSignature reads a signature with codesign -dvv. A binary or bundle
// codesign cannot describe at all (unsigned) is an error.
func describeSignature(ctx context.Context, e *env, path string) (codeSignature, error) {
	var out strings.Builder
	code, err := e.run(ctx, []string{"codesign", "-dvv", path}, nil, &out, &out)
	if err != nil {
		return codeSignature{}, fmt.Errorf("run codesign: %w", err)
	}
	if code != 0 {
		return codeSignature{}, fmt.Errorf("codesign cannot read the signature of %s (exit status %d): %s", path, code, strings.TrimSpace(out.String()))
	}
	return parseSignature(out.String()), nil
}

// parseSignature reads codesign -dvv's lines: Identifier=, TeamIdentifier=
// (or "not set") and the first Authority=.
func parseSignature(out string) codeSignature {
	var sig codeSignature
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Identifier="):
			sig.identifier = strings.TrimPrefix(line, "Identifier=")
		case strings.HasPrefix(line, "TeamIdentifier="):
			if team := strings.TrimPrefix(line, "TeamIdentifier="); teamIdentifier.MatchString(team) {
				sig.team = team
			}
		case strings.HasPrefix(line, "Authority=") && sig.authority == "":
			sig.authority = strings.TrimPrefix(line, "Authority=")
		}
	}
	return sig
}

// ownTeam is this seam binary's Developer ID team, "" when it has none (a
// source build, or a release built without the signing secret).
func ownTeam(ctx context.Context, e *env, w touchIDWorld) string {
	self, err := w.self()
	if err != nil {
		return ""
	}
	sig, err := describeSignature(ctx, e, self)
	if err != nil {
		return ""
	}
	return sig.team
}

// approverRequirement is the code requirement the app must satisfy when this
// seam names a team: signed through Apple's CA, as the helper, by that team.
func approverRequirement(team string) string {
	return `anchor apple generic and identifier "` + approverBundleID + `" and certificate leaf[subject.OU] = "` + team + `"`
}

// verifyApprover checks the app at path before it is installed or used: its
// signature intact, the helper's identifier, Gatekeeper's notarization check
// for a release asset, and the publisher -- the same team as this seam, or,
// when this seam has no Developer ID, the owner's y/N on the team shown.
func verifyApprover(ctx context.Context, e *env, w touchIDWorld, in *bufio.Reader, path string, notarized bool) error {
	var errb strings.Builder
	code, err := e.run(ctx, []string{"codesign", "--verify", "--strict", "--deep", path}, nil, io.Discard, &errb)
	if err != nil {
		return fmt.Errorf("run codesign: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("%w: its code signature does not verify (it was changed after signing, or is not signed): %s",
			errApproverRefused, strings.TrimSpace(errb.String()))
	}
	sig, err := describeSignature(ctx, e, path)
	if err != nil {
		return fmt.Errorf("%w: %w", errApproverRefused, err)
	}
	if sig.identifier != approverBundleID {
		return fmt.Errorf("%w: it is signed as %q, not as %s", errApproverRefused, printable(sig.identifier), approverBundleID)
	}
	if notarized {
		var serr strings.Builder
		code, err := e.run(ctx, []string{"spctl", "--assess", "--type", "execute", path}, nil, io.Discard, &serr)
		if err != nil {
			return fmt.Errorf("run spctl: %w", err)
		}
		if code != 0 {
			return fmt.Errorf("%w: Gatekeeper does not accept it as notarized: %s", errApproverRefused, strings.TrimSpace(serr.String()))
		}
	}
	own := ownTeam(ctx, e, w)
	if own == "" {
		fmt.Fprintf(e.stdout, "This seam is not signed with a Developer ID, so the app's publisher cannot be checked against it.\n"+
			"The app is signed by %s%s.\n", teamWords(sig.team), authorityWords(sig.authority))
		ans, _ := prompt(e, in, "Install and use it? [y/N] ")
		if !isYes(ans) {
			return fmt.Errorf("%w: not confirmed", errApproverRefused)
		}
		return nil
	}
	if sig.team != own {
		return fmt.Errorf("%w: it is signed by %s, and this seam by team %s", errApproverRefused, teamWords(sig.team), own)
	}
	errb.Reset()
	code, err = e.run(ctx, []string{"codesign", "--verify", "--strict", "--deep", "-R=" + approverRequirement(own), path}, nil, io.Discard, &errb)
	if err != nil {
		return fmt.Errorf("run codesign: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("%w: its signature does not chain to Apple for team %s: %s", errApproverRefused, own, strings.TrimSpace(errb.String()))
	}
	return nil
}

func authorityWords(authority string) string {
	if authority == "" {
		return ""
	}
	return " (" + printable(authority) + ")"
}

// swapInstall moves staged to target in one rename, an app already there
// first moved aside into tmp and put back if the rename fails. keep is set
// when the previous app could not be put back, so tmp is not removed with it.
func swapInstall(staged, target, tmp string, keep *bool) error {
	prev := ""
	if _, err := os.Lstat(target); err == nil {
		prev = filepath.Join(tmp, "previous-"+approverAppName)
		if err := os.Rename(target, prev); err != nil {
			return fmt.Errorf("move the installed app aside: %w", err)
		}
	}
	if err := os.Rename(staged, target); err != nil {
		if prev != "" {
			if rerr := os.Rename(prev, target); rerr != nil {
				*keep = true
				return fmt.Errorf("install at %s: %w (and the previous app could not be put back: it is at %s)", target, err, prev)
			}
		}
		return fmt.Errorf("install at %s: %w", target, err)
	}
	return nil
}

// isYes reads a y/N answer.
func isYes(ans string) bool {
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "y", "yes":
		return true
	}
	return false
}

// isDir reports whether path is a directory (following no link).
func isDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
}
