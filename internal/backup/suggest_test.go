package backup

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o700))
	}
}

func TestSuggestions_MacOS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	home, volumes, data := t.TempDir(), t.TempDir(), t.TempDir()
	mkdirs(t, home,
		"Library/Mobile Documents/com~apple~CloudDocs",
		"Library/CloudStorage/GoogleDrive-me@example.com",
		"Library/CloudStorage/Dropbox",
		"Library/CloudStorage/OneDrive-Personal",
		"Library/CloudStorage/.hidden")
	mkdirs(t, volumes, "T7", ".timemachine")
	require.NoError(t, os.Symlink("/", filepath.Join(volumes, "Macintosh HD")))
	require.NoError(t, os.WriteFile(filepath.Join(volumes, "not-a-disk"), nil, 0o600))

	got := suggestions(suggestEnv{goos: "darwin", home: home, dataDir: data, roots: map[string]string{"/Volumes": volumes}})
	var labels []string
	for _, s := range got {
		labels = append(labels, s.Label+"/"+s.Kind)
		if s.Kind != PlaceLocal {
			require.Equal(t, filepath.Join(s.Place, SuggestedFolderName), s.Dir)
		}
	}
	require.Equal(t, []string{
		"iCloud Drive/icloud", "Dropbox/cloud", "Google Drive/cloud", "OneDrive/cloud", "T7/volume", "Seamless's data folder/local",
	}, labels)
	require.Equal(t, LocalDir(data), got[len(got)-1].Dir)
}

func TestSuggestions_Linux(t *testing.T) {
	home, media, run, data := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	mkdirs(t, home, "Dropbox")
	mkdirs(t, media, "me/USB")
	mkdirs(t, run, "me/NAS")
	got := suggestions(suggestEnv{
		goos: "linux", home: home, user: "me", dataDir: data,
		roots: map[string]string{"/media": media, "/run/media": run},
	})
	var labels []string
	for _, s := range got {
		labels = append(labels, s.Label)
	}
	require.Equal(t, []string{"Dropbox", "USB", "NAS", "Seamless's data folder"}, labels)
}

func TestSuggestions_Windows(t *testing.T) {
	onedrive, drive, data := t.TempDir(), t.TempDir(), t.TempDir()
	env := map[string]string{"OneDrive": onedrive, "OneDriveConsumer": onedrive}
	got := suggestions(suggestEnv{
		goos: "windows", dataDir: data, getenv: func(k string) string { return env[k] },
		drives: []string{drive, filepath.Join(drive, "absent")},
	})
	require.Len(t, got, 3, "OneDrive once, the drive that exists, the data folder")
	require.Equal(t, PlaceCloud, got[0].Kind)
	require.Equal(t, PlaceVolume, got[1].Kind)
	require.Equal(t, PlaceLocal, got[2].Kind)
}

func TestCloudLabel(t *testing.T) {
	require.Equal(t, "Google Drive", cloudLabel("GoogleDrive-me@example.com"))
	require.Equal(t, "OneDrive", cloudLabel("OneDrive-Personal"))
	require.Equal(t, "Box", cloudLabel("Box-Box"))
	require.Equal(t, "pCloud Drive", cloudLabel("pCloud Drive"))
}
