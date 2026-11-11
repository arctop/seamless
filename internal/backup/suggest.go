package backup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SuggestedFolderName is the folder a suggestion creates inside the place it
// names.
const SuggestedFolderName = "Seamless Backups"

// The kinds of place a suggestion names.
const (
	PlaceLocal  = "local"  // Seamless's data folder
	PlaceICloud = "icloud" // iCloud Drive
	PlaceCloud  = "cloud"  // a synced folder: Dropbox, Google Drive, OneDrive
	PlaceVolume = "volume" // an external or network disk
)

// Suggestion is a place on this machine a backup folder could go.
type Suggestion struct {
	// Label names the place: "iCloud Drive", "Dropbox", "T7".
	Label string `json:"label"`
	// Kind is PlaceLocal, PlaceICloud, PlaceCloud or PlaceVolume.
	Kind string `json:"kind"`
	// Place is the existing directory; Dir the folder a choice fills in
	// (Place/SuggestedFolderName, or LocalDir itself).
	Place string `json:"place"`
	Dir   string `json:"dir"`
}

// suggestEnv is where Suggestions looks, injected so a test lays out a fake
// machine.
type suggestEnv struct {
	goos    string
	home    string
	user    string
	dataDir string
	getenv  func(string) string
	// roots maps the fixed system roots (/Volumes, /media, /run/media) to
	// where a test put them; nil uses the real ones.
	roots map[string]string
	// drives lists the Windows drive roots to try; nil is D: to Z:.
	drives []string
}

func (e suggestEnv) root(p string) string {
	if r, ok := e.roots[p]; ok {
		return r
	}
	return p
}

// suggestions lists the places that exist on this machine, Seamless's own
// folder last:
//
//   - macOS: iCloud Drive, every ~/Library/CloudStorage/* folder (Dropbox,
//     Google Drive, OneDrive, Box), and every disk under /Volumes but the boot
//     disk;
//   - Linux: ~/Dropbox, and every disk under /media/$USER and
//     /run/media/$USER;
//   - Windows: OneDrive, and every other drive;
//   - everywhere: Seamless's data folder (LocalDir).
func suggestions(e suggestEnv) []Suggestion {
	var out []Suggestion
	add := func(label, kind, place string) {
		fi, err := os.Stat(place)
		if err != nil || !fi.IsDir() {
			return
		}
		out = append(out, Suggestion{Label: label, Kind: kind, Place: place, Dir: filepath.Join(place, SuggestedFolderName)})
	}
	volumes := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, en := range entries {
			name := en.Name()
			// A link (macOS's "Macintosh HD" -> /) is not another disk, and a
			// dot entry is the system's.
			if strings.HasPrefix(name, ".") || en.Type()&os.ModeSymlink != 0 || !en.IsDir() {
				continue
			}
			add(name, PlaceVolume, filepath.Join(dir, name))
		}
	}

	switch e.goos {
	case "darwin":
		if e.home != "" {
			add("iCloud Drive", PlaceICloud, filepath.Join(e.home, "Library", "Mobile Documents", "com~apple~CloudDocs"))
			cloud := filepath.Join(e.home, "Library", "CloudStorage")
			if entries, err := os.ReadDir(cloud); err == nil {
				for _, en := range entries {
					if strings.HasPrefix(en.Name(), ".") {
						continue
					}
					add(cloudLabel(en.Name()), PlaceCloud, filepath.Join(cloud, en.Name()))
				}
			}
		}
		volumes(e.root("/Volumes"))
	case "linux":
		if e.home != "" {
			add("Dropbox", PlaceCloud, filepath.Join(e.home, "Dropbox"))
		}
		if e.user != "" {
			volumes(filepath.Join(e.root("/media"), e.user))
			volumes(filepath.Join(e.root("/run/media"), e.user))
		}
	case "windows":
		seen := map[string]bool{}
		for _, key := range []string{"OneDrive", "OneDriveConsumer", "OneDriveCommercial"} {
			if p := e.getenv(key); p != "" && !seen[strings.ToLower(p)] {
				seen[strings.ToLower(p)] = true
				add(filepath.Base(p), PlaceCloud, p)
			}
		}
		drives := e.drives
		if drives == nil {
			for c := 'D'; c <= 'Z'; c++ {
				drives = append(drives, string(c)+`:\`)
			}
		}
		for _, d := range drives {
			add(strings.TrimSuffix(d, `\`), PlaceVolume, d)
		}
	}

	if e.dataDir != "" {
		local := LocalDir(e.dataDir)
		out = append(out, Suggestion{Label: "Seamless's data folder", Kind: PlaceLocal, Place: e.dataDir, Dir: local})
	}
	return slices.CompactFunc(out, func(a, b Suggestion) bool { return a.Dir == b.Dir })
}

// cloudLabel names a ~/Library/CloudStorage folder: its provider, without the
// account the File Provider appends ("GoogleDrive-me@example.com" is "Google
// Drive", "OneDrive-Personal" is "OneDrive").
func cloudLabel(name string) string {
	provider, _, _ := strings.Cut(name, "-")
	switch strings.ToLower(provider) {
	case "googledrive":
		return "Google Drive"
	case "onedrive":
		return "OneDrive"
	case "dropbox":
		return "Dropbox"
	case "box":
		return "Box"
	}
	return name
}
