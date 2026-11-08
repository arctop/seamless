package update

import (
	"slices"
	"time"
)

// Repo is where Seamless releases live. The installer scripts hardcode the
// same "arctop/seamless".
const Repo = "arctop/seamless"

// releaseTagURLPrefix is the constant half of every release link this package
// renders; ReleaseURL appends a parsed version, never API-supplied text.
const releaseTagURLPrefix = "https://github.com/" + Repo + "/releases/tag/v"

// ReleaseURL is the release page for v. It is built from a template and a
// parsed version only (never the API's html_url), so a release's own text can
// never steer what a notice links to.
func ReleaseURL(v Version) string { return releaseTagURLPrefix + v.String() }

// APIRelease is the sliver of a GitHub release object the update check reads.
// Every other field -- name, body, html_url -- is deliberately not decoded:
// nothing downstream may render release text (see the package doc).
type APIRelease struct {
	TagName     string     `json:"tag_name"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	PublishedAt *time.Time `json:"published_at"`
	Assets      []APIAsset `json:"assets"`
}

// APIAsset is one uploaded file of a release.
type APIAsset struct {
	Name string `json:"name"`
	// State is "uploaded" once the asset is complete; GitHub lists an asset
	// while its upload is still "open".
	State string `json:"state"`
}

// Release is one installable release as the update check keeps it: a parsed
// version and the time GitHub published it. It is also the shape state.json
// caches, so a restart does not need the network to know what exists.
type Release struct {
	Version     Version   `json:"version"`
	PublishedAt time.Time `json:"published_at"`
	// ChecksumsBundle reports whether the release carries an uploaded
	// checksums.txt.sigstore.json. The unattended updater verifies
	// checksums.txt against it, by the exact tag, so an automatic update
	// (Target) and Update now only take a release that has one. Telling the
	// owner about a release does not need it: no release up to v0.6.0
	// carries the bundle, and those stay visible.
	ChecksumsBundle bool `json:"checksums_bundle,omitempty"`
}

// filterRevision names what Filter records about a release. The cached list
// in state.json is tagged with the revision that produced it; a list from
// another revision -- an older release's, which never recorded
// ChecksumsBundle, or a newer one's -- is re-read in full at the next check
// (the ETag is dropped), so a 304 can never keep a list this binary would
// misjudge.
const filterRevision = 1

// requiredAssets names the release assets an update for goos needs before the
// release counts as installable: the checksums every installer verifies the
// archive against, and the installer script with the Sigstore bundle
// `seamlessd update` verifies it with. goreleaser publishes a release only
// after its uploads finish (it drafts first), but a release made by hand, or
// one whose publish failed partway, can still be public with assets missing,
// and telling someone to update to it would fail on a 404.
func requiredAssets(goos string) []string {
	if goos == "windows" {
		return []string{"checksums.txt", "install.ps1", "install.ps1.sigstore.json"}
	}
	return []string{"checksums.txt", "install", "install.sigstore.json"}
}

// Filter keeps the releases an update on goos may target and returns them
// newest version first. A release survives only if all of these hold:
//
//   - it is neither a draft nor a prerelease (flipping a release to
//     prerelease is how a bad one is pulled);
//   - its tag is exactly vX.Y.Z (ParseTag), and it has a published_at;
//   - every asset requiredAssets names for goos is present and uploaded.
//
// Each kept release records whether it carries ChecksumsBundleAsset
// (ChecksumsBundle); a release without it is still kept, for the notice.
// Two releases with the same version keep the earlier publication.
func Filter(api []APIRelease, goos string) []Release {
	need := requiredAssets(goos)
	out := make([]Release, 0, len(api))
	for _, r := range api {
		if r.Draft || r.Prerelease || r.PublishedAt == nil || r.PublishedAt.IsZero() {
			continue
		}
		v, ok := ParseTag(r.TagName)
		if !ok || !hasAssets(r.Assets, need) {
			continue
		}
		rel := Release{
			Version:         v,
			PublishedAt:     r.PublishedAt.UTC(),
			ChecksumsBundle: hasAssets(r.Assets, []string{ChecksumsBundleAsset}),
		}
		if i := slices.IndexFunc(out, func(o Release) bool { return o.Version == v }); i >= 0 {
			if rel.PublishedAt.Before(out[i].PublishedAt) {
				out[i] = rel
			}
			continue
		}
		out = append(out, rel)
	}
	SortNewestFirst(out)
	return out
}

// SortNewestFirst orders rels by version, highest first.
func SortNewestFirst(rels []Release) {
	slices.SortStableFunc(rels, func(a, b Release) int { return b.Version.Compare(a.Version) })
}

// hasAssets reports whether every name in need is an uploaded asset.
func hasAssets(assets []APIAsset, need []string) bool {
	for _, n := range need {
		if !slices.ContainsFunc(assets, func(a APIAsset) bool {
			return a.Name == n && (a.State == "" || a.State == "uploaded")
		}) {
			return false
		}
	}
	return true
}

// Newest returns the highest version in rels, and false when rels is empty.
// It is the maximum by version, never the first entry: GitHub lists releases
// by creation, and a backport cut after a newer release is created later
// (memory github-releases-latest-is-not-max-version).
func Newest(rels []Release) (Release, bool) {
	if len(rels) == 0 {
		return Release{}, false
	}
	best := rels[0]
	for _, r := range rels[1:] {
		if r.Version.Compare(best.Version) > 0 {
			best = r
		}
	}
	return best, true
}
