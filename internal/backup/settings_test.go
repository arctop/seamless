package backup

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

func TestEffective(t *testing.T) {
	data := filepath.Join(string(filepath.Separator), "data")
	local := filepath.Join(data, LocalDirName)
	ext := filepath.Join(string(filepath.Separator), "Volumes", "T7", "Seamless Backups")
	cloud := filepath.Join(string(filepath.Separator), "cloud", "Backups")
	defaults := config.Backup{Every: config.Duration(config.DefaultBackupEvery), Keep: config.DefaultBackupKeep}

	cases := []struct {
		name     string
		base     config.Backup
		override config.BackupOverride
		release  bool
		want     Settings
	}{
		{
			name: "a release build with nothing set backs up daily into the data folder",
			base: defaults, release: true,
			want: Settings{
				Auto: true, AutoSource: SourceDefault, Every: 24 * time.Hour, EverySource: SourceDefault,
				Keep: 7, KeepSource: SourceDefault, Dir: local, DirSource: SourceDefault, DirIsLocal: true,
			},
		},
		{
			name: "a source build with nothing set does not",
			base: defaults, release: false,
			want: Settings{
				Auto: false, AutoSource: SourceDefault, Every: 24 * time.Hour, EverySource: SourceDefault,
				Keep: 7, KeepSource: SourceDefault, Dir: local, DirSource: SourceDefault, DirIsLocal: true,
			},
		},
		{
			name: "the file's values",
			base: config.Backup{Auto: ptr(true), Every: config.Duration(6 * time.Hour), Keep: 30, Dir: ext},
			want: Settings{
				Auto: true, AutoSource: SourceConfig, Every: 6 * time.Hour, EverySource: SourceConfig,
				Keep: 30, KeepSource: SourceConfig, Dir: ext, DirSource: SourceConfig,
			},
		},
		{
			name: "the console's override simply wins, field by field",
			base: config.Backup{Auto: ptr(false), Every: config.Duration(6 * time.Hour), Keep: 30, Dir: ext},
			override: config.BackupOverride{
				Auto: ptr(true), Every: ptr(config.Duration(time.Hour)), Keep: ptr(3), Dir: ptr(cloud),
			},
			release: false,
			want: Settings{
				Auto: true, AutoSource: SourceConsole, Every: time.Hour, EverySource: SourceConsole,
				Keep: 3, KeepSource: SourceConsole, Dir: cloud, DirSource: SourceConsole,
			},
		},
		{
			name:     "a set but empty override dir chooses the data folder over the file's",
			base:     config.Backup{Every: config.Duration(24 * time.Hour), Keep: 7, Dir: ext},
			override: config.BackupOverride{Dir: ptr("")},
			release:  true,
			want: Settings{
				Auto: true, AutoSource: SourceDefault, Every: 24 * time.Hour, EverySource: SourceDefault,
				Keep: 7, KeepSource: SourceDefault, Dir: local, DirSource: SourceConsole, DirIsLocal: true,
			},
		},
		{
			name: "a hand-built base: zero is the default, out of range is clamped, a relative dir is unset",
			base: config.Backup{Every: config.Duration(time.Minute), Keep: 1000, Dir: "relative/dir"},
			want: Settings{
				Auto: false, AutoSource: SourceDefault, Every: config.MinBackupEvery, EverySource: SourceConfig,
				Keep: config.MaxBackupKeep, KeepSource: SourceConfig, Dir: local, DirSource: SourceDefault, DirIsLocal: true,
			},
		},
		{
			name: "a hand-built override out of range is ignored",
			base: defaults, release: true,
			override: config.BackupOverride{Every: ptr(config.Duration(time.Second)), Keep: ptr(0), Dir: ptr("rel")},
			want: Settings{
				Auto: true, AutoSource: SourceDefault, Every: 24 * time.Hour, EverySource: SourceDefault,
				Keep: 7, KeepSource: SourceDefault, Dir: local, DirSource: SourceDefault, DirIsLocal: true,
			},
		},
		{
			name: "a zero base is the defaults",
			want: Settings{
				Auto: false, AutoSource: SourceDefault, Every: 24 * time.Hour, EverySource: SourceDefault,
				Keep: 7, KeepSource: SourceDefault, Dir: local, DirSource: SourceDefault, DirIsLocal: true,
			},
		},
		{
			name: "the file naming the data folder's own backups folder is still local",
			base: config.Backup{Every: config.Duration(24 * time.Hour), Keep: 7, Dir: local + string(filepath.Separator)},
			want: Settings{
				Auto: false, AutoSource: SourceDefault, Every: 24 * time.Hour, EverySource: SourceDefault,
				Keep: 7, KeepSource: SourceDefault, Dir: local, DirSource: SourceConfig, DirIsLocal: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Effective(tc.base, tc.override, tc.release, data))
		})
	}
}
