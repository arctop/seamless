package main

// The updater's reads from GitHub: one release re-read by its tag, and that
// release's assets. Every read is anonymous (update.NewHTTPClient: default
// TLS, the environment's proxy, https-only redirects, a fixed User-Agent,
// never a token), HTTPS-only on the first URL and on every redirect hop, and
// size-capped. A failure is classified for the attempt record, because the
// daemon blocks a release on StageVerify and only backs off from StageFetch:
//
//   - StageFetch, transient: the network (transport, timeout, 5xx, rate
//     limiting, a misrouted reply) and a release caught mid-upload
//     (errReleaseInFlight).
//   - StageVerify, the release's own: a tag re-read that finds the release
//     gone (404: deleted, or a draft, which is invisible), pulled (flipped to
//     prerelease), or published without an asset the update needs -- and,
//     after the downloads, a signature or checksums failure.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/update"
)

const (
	// githubAPIRepo is the release API of update.Repo; a release is read at
	// githubAPIRepo + "/releases/tags/vX.Y.Z".
	githubAPIRepo = "https://api.github.com/repos/" + update.Repo
	// releaseDownloadBase holds every release's assets, one directory per tag:
	// releaseDownloadBase + "/vX.Y.Z/<asset>". Never GitHub's "latest": the
	// updater always names the release it installs.
	releaseDownloadBase = "https://github.com/" + update.Repo + "/releases/download"

	// checksumsAsset is the manifest both installers verify the archive
	// against; bundleSuffix names the Sigstore bundle signing an asset.
	checksumsAsset = "checksums.txt"
	bundleSuffix   = ".sigstore.json"

	// Size caps. A script, a bundle and checksums.txt are a few KB each, and a
	// release object with its assets a few tens of KB; the caps only bound a
	// misrouted reply.
	maxAssetBytes   = 1 << 20
	maxReleaseBytes = 2 << 20
)

// Fetch failure classes. errFetchNetwork is the network's failure: it may
// clear by itself, so an automatic update backs off. errFetchMissing is the
// release's: GitHub answered that the release or asset does not exist.
var (
	errFetchNetwork = errors.New("could not reach GitHub")
	errFetchMissing = errors.New("not published")
)

// fetchError is one failed read, classified by class (errFetchNetwork or
// errFetchMissing, which errors.Is matches).
type fetchError struct {
	url    string
	class  error
	detail string // what went wrong, built from the status or the cause
	cause  error  // the transport error, when there was one
}

func (e *fetchError) Error() string {
	return fmt.Sprintf("%s: %s", e.url, e.detail)
}

func (e *fetchError) Unwrap() []error {
	if e.cause != nil {
		return []error{e.class, e.cause}
	}
	return []error{e.class}
}

// releaseSource is where the updater reads releases: GitHub in production
// (newReleaseSource), an httptest TLS server in tests.
type releaseSource struct {
	client       *http.Client
	apiRepo      string // the repository's API base: .../repos/<owner>/<repo>
	downloadBase string // the release-asset base: .../releases/download
}

// newReleaseSource reads from GitHub with the update client.
func newReleaseSource() releaseSource {
	return releaseSource{client: update.NewHTTPClient(), apiRepo: githubAPIRepo, downloadBase: releaseDownloadBase}
}

// assetURL is where release v's asset name downloads from.
func (s releaseSource) assetURL(v update.Version, name string) string {
	return s.downloadBase + "/v" + v.String() + "/" + name
}

// installerAsset is the installer script a release ships for goos: the
// PowerShell one on Windows, the POSIX one everywhere else.
func installerAsset(goos string) string {
	if goos == "windows" {
		return "install.ps1"
	}
	return "install"
}

// platformArchive is the release archive the installer downloads on goos and
// goarch, named the way goreleaser names it (.goreleaser.yaml:
// archives.name_template "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}",
// zip on Windows) and the way docs/install and docs/install.ps1 build the
// name: Os and Arch are Go's GOOS and GOARCH, which is the vocabulary both
// installers map uname -m and PROCESSOR_ARCHITECTURE onto.
func platformArchive(v update.Version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("seamless_%s_%s_%s.%s", v, goos, goarch, ext)
}

// releaseAssetNames lists what an update to or a rollback to a release needs
// from it on goos/goarch: the installer and its bundle, checksums.txt and its
// bundle, and the archive the installer will download.
func releaseAssetNames(v update.Version, goos, goarch string) []string {
	inst := installerAsset(goos)
	return []string{inst, inst + bundleSuffix, checksumsAsset, checksumsAsset + bundleSuffix,
		platformArchive(v, goos, goarch)}
}

// get sends one anonymous GET or HEAD for url and classifies a failure. The
// caller closes the body of a 200.
func (s releaseSource) get(ctx context.Context, method, url string, api bool) (*http.Response, error) {
	if err := update.RequireHTTPS(url); err != nil {
		// A URL this package built is always https; a test or a bad base is
		// the only way here, and it is no network failure.
		return nil, fmt.Errorf("refusing to fetch: %w", err)
	}
	// No per-request deadline here: the body is read after this returns, and
	// the client bounds the whole request (update.NewHTTPClient's Timeout).
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", url, err)
	}
	req.Header.Set("User-Agent", update.UserAgent)
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, &fetchError{url: url, class: errFetchNetwork, detail: err.Error(), cause: err}
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	_ = resp.Body.Close()
	class := errFetchNetwork
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		class = errFetchMissing
	}
	return nil, &fetchError{url: url, class: class, detail: "unexpected status " + resp.Status}
}

// readCapped reads a 200's body, refusing one over limit or an empty one --
// both a misrouted reply rather than anything a release publishes, so both
// are the network's failure.
func readCapped(resp *http.Response, url string, limit int64) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &fetchError{url: url, class: errFetchNetwork, detail: "read: " + err.Error(), cause: err}
	}
	if int64(len(body)) > limit {
		return nil, &fetchError{url: url, class: errFetchNetwork, detail: fmt.Sprintf("response exceeds %d bytes", limit)}
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, &fetchError{url: url, class: errFetchNetwork, detail: "empty response"}
	}
	return body, nil
}

// releaseByTag re-reads release v: GET /releases/tags/vX.Y.Z. The tag
// endpoint answers for a published release only -- a draft is invisible to an
// anonymous caller and a deleted release is gone -- so a 404 is the release
// having been pulled (errFetchMissing). A 200 still has to pass
// checkPublished.
func (s releaseSource) releaseByTag(ctx context.Context, v update.Version) (update.APIRelease, error) {
	url := s.apiRepo + "/releases/tags/v" + v.String()
	resp, err := s.get(ctx, http.MethodGet, url, true)
	if err != nil {
		return update.APIRelease{}, err
	}
	body, err := readCapped(resp, url, maxReleaseBytes)
	if err != nil {
		return update.APIRelease{}, err
	}
	var rel update.APIRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return update.APIRelease{}, &fetchError{url: url, class: errFetchNetwork, detail: "decode release: " + err.Error(), cause: err}
	}
	return rel, nil
}

// errReleasePulled is a release the tag re-read found pulled: a draft, a
// prerelease (flipping a release to prerelease is the fleet-wide yank), or
// one that is not published at all.
var errReleasePulled = errors.New("release pulled")

// errReleaseIncomplete is a published release missing an asset the updater
// needs, such as the checksums bundle every release from the floor on ships.
// A published release that does not list an asset is one whose asset was
// never attached or was removed -- the release's own state, not a moment in
// an upload -- so it is the release's failure, StageVerify.
var errReleaseIncomplete = errors.New("release incomplete")

// errReleaseInFlight is a release caught mid-upload: GitHub lists a needed
// asset whose upload has not finished (its state is "open", not "uploaded"),
// or the tag re-read listed an asset as uploaded that its download then could
// not find. Either may clear by itself within minutes, so it is the network's
// class of failure (StageFetch, which the daemon backs off from), never the
// release's: a block on it would hold back a good release for good.
// goreleaser drafts a release and publishes it only once every upload is
// done, so this is a hand-made release, or a publish that is still settling.
var errReleaseInFlight = errors.New("release still uploading")

// checkPublished holds a re-read release to what an update may install: not a
// draft or prerelease, published, tagged exactly vX.Y.Z for v, and carrying
// every asset in need, uploaded. A needed asset that is listed but not yet
// uploaded (update.APIAsset.State "open"; "" reads as uploaded, as
// update.Filter reads it) is errReleaseInFlight, and it wins over a missing
// one: a release still being assembled lists its later assets only once their
// upload starts. Its messages name v and the asset names this package asked
// for, never text from the release itself (constraint
// update-surfaces-render-parsed-versions-only).
func checkPublished(rel update.APIRelease, v update.Version, need []string) error {
	switch {
	case rel.Draft || rel.Prerelease:
		return fmt.Errorf("%w: v%s is marked a draft or prerelease on GitHub (a bad release is pulled that way)", errReleasePulled, v)
	case rel.PublishedAt == nil || rel.PublishedAt.IsZero():
		return fmt.Errorf("%w: v%s is not published", errReleasePulled, v)
	}
	if tag, ok := update.ParseTag(rel.TagName); !ok || tag != v {
		return fmt.Errorf("%w: GitHub's release for v%s does not carry that tag", errReleasePulled, v)
	}
	var missing, uploading []string
	for _, name := range need {
		switch {
		case slices.ContainsFunc(rel.Assets, func(a update.APIAsset) bool {
			return a.Name == name && (a.State == "" || a.State == "uploaded")
		}):
		case slices.ContainsFunc(rel.Assets, func(a update.APIAsset) bool { return a.Name == name }):
			uploading = append(uploading, name)
		default:
			missing = append(missing, name)
		}
	}
	if len(uploading) > 0 {
		return fmt.Errorf("%w: v%s is still uploading %s", errReleaseInFlight, v, strings.Join(uploading, ", "))
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: v%s has no %s", errReleaseIncomplete, v, strings.Join(missing, ", "))
	}
	return nil
}

// listedAssetGone reclassifies a download that failed after the tag re-read
// listed every needed asset as uploaded: a 404 then is the asset host not
// serving it yet (or a removal racing the update), not the release lacking it,
// so it is errReleaseInFlight. Any other failure passes through as it is.
func listedAssetGone(err error) error {
	if errors.Is(err, errFetchMissing) {
		return fmt.Errorf("%w: the release lists it as uploaded: %w", errReleaseInFlight, err)
	}
	return err
}

// fetchAsset downloads one small asset of release v.
func (s releaseSource) fetchAsset(ctx context.Context, v update.Version, name string) ([]byte, error) {
	url := s.assetURL(v, name)
	resp, err := s.get(ctx, http.MethodGet, url, false)
	if err != nil {
		return nil, err
	}
	return readCapped(resp, url, maxAssetBytes)
}

// headAsset checks that release v's asset name is downloadable, without
// downloading it: the archive a rollback would install.
func (s releaseSource) headAsset(ctx context.Context, v update.Version, name string) error {
	resp, err := s.get(ctx, http.MethodHead, s.assetURL(v, name), false)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// releaseDownload is what the updater fetched of one release before
// verifying any of it.
type releaseDownload struct {
	version         update.Version
	installer       []byte
	installerBundle []byte
	checksums       []byte // nil when not fetched (the floor fallback)
	checksumsBundle []byte
	// ownerSig is the owner's checksums.txt.sshsig, fetched only while
	// update.release_signers pins the owner's keys.
	ownerSig []byte
}

// downloadRelease fetches release v's installer for goos, checksums.txt and
// their two bundles. withChecksums false fetches the installer pair only.
func (s releaseSource) downloadRelease(ctx context.Context, v update.Version, goos string, withChecksums bool) (releaseDownload, error) {
	d := releaseDownload{version: v}
	inst := installerAsset(goos)
	names := []string{inst, inst + bundleSuffix}
	dst := []*[]byte{&d.installer, &d.installerBundle}
	if withChecksums {
		names = append(names, checksumsAsset, checksumsAsset+bundleSuffix)
		dst = append(dst, &d.checksums, &d.checksumsBundle)
	}
	for i, name := range names {
		body, err := s.fetchAsset(ctx, v, name)
		if err != nil {
			return releaseDownload{}, err
		}
		*dst[i] = body
	}
	return d, nil
}

// releaseAssets is one release as the updater acts on it, every byte verified.
type releaseAssets struct {
	// version is the release the installer installs (SEAMLESS_VERSION).
	version update.Version
	// installer is the verified install script, and signedFor the release
	// whose tag signed it: version itself, except in the floor fallback, where
	// the newest release's installer installs an older release.
	installer []byte
	signedFor update.Version
	// archive is this platform's archive, listed exactly once in pin's
	// checksums.txt; pin is the lowercase hex SHA-256 of that verified
	// checksums.txt (SEAMLESS_CHECKSUMS_SHA256). Both are empty in the floor
	// fallback, where the installer verifies the archive by itself.
	archive string
	pin     string
}

// verifyDownload checks a download against its bundles with the exact tag
// identity of d.version (releasesig.VerifyAsset, through verify) and returns what
// the installer is then run with. The installer must verify; so must
// checksums.txt when it was fetched, which must then list this platform's
// archive exactly once.
func verifyDownload(d releaseDownload, goos, goarch string, verify func(bundle, artifact []byte, v update.Version) error) (releaseAssets, error) {
	inst := installerAsset(goos)
	if err := verify(d.installerBundle, d.installer, d.version); err != nil {
		return releaseAssets{}, fmt.Errorf("%s of v%s: %w", inst, d.version, err)
	}
	out := releaseAssets{version: d.version, installer: d.installer, signedFor: d.version}
	if d.checksums == nil {
		return out, nil
	}
	if err := verify(d.checksumsBundle, d.checksums, d.version); err != nil {
		return releaseAssets{}, fmt.Errorf("%s of v%s: %w", checksumsAsset, d.version, err)
	}
	out.archive = platformArchive(d.version, goos, goarch)
	if _, err := archiveChecksum(d.checksums, out.archive); err != nil {
		return releaseAssets{}, fmt.Errorf("%s of v%s: %w", checksumsAsset, d.version, err)
	}
	sum := sha256.Sum256(d.checksums)
	out.pin = hex.EncodeToString(sum[:])
	return out, nil
}

// errChecksumsArchive is a checksums.txt that does not list this platform's
// archive exactly once with a well-formed SHA-256.
var errChecksumsArchive = errors.New("checksums.txt does not list the archive exactly once")

// archiveChecksum returns the SHA-256 checksums.txt lists for archive. The
// manifest is sha256sum's format, "<hex>  <name>" per line; the installers
// match the name field exactly (awk '$2 == f'), so this does too. Zero lines
// for the archive, several, or a malformed hash refuse the manifest: an
// installer reading it would install nothing, or pick one of two.
func archiveChecksum(checksums []byte, archive string) (string, error) {
	var found []string
	for line := range strings.SplitSeq(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == archive {
			found = append(found, fields[0])
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%w: %s appears %d times", errChecksumsArchive, archive, len(found))
	}
	sum := strings.ToLower(found[0])
	if raw, err := hex.DecodeString(sum); err != nil || len(raw) != sha256.Size {
		return "", fmt.Errorf("%w: %s has a malformed checksum", errChecksumsArchive, archive)
	}
	return sum, nil
}
