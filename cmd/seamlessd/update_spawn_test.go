package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/update"
)

// spawnReq is a request every spawner accepts.
func spawnReq(t *testing.T) update.SpawnRequest {
	t.Helper()
	req := update.SpawnRequest{
		AttemptID: "01M4HFPS1DBMJXQ4CE8ZZ0JRDH",
		From:      mustVersion(t, "0.7.2"), To: mustVersion(t, "0.7.3"),
		Why: update.WhyAuto,
	}
	require.NoError(t, req.Validate())
	return req
}

func TestSystemdRunArgv(t *testing.T) {
	target := spawnTarget{exe: "/home/me/.local/bin/seamlessd", configPath: "/home/me/.config/seamless/seamless.yaml"}
	updater := []string{
		"--", "/home/me/.local/bin/seamlessd",
		"update", "--auto", "--attempt", "01M4HFPS1DBMJXQ4CE8ZZ0JRDH",
		"--from", "0.7.2", "--to", "0.7.3", "--why", "auto",
		"--config", "/home/me/.config/seamless/seamless.yaml",
	}
	head := []string{
		"systemd-run", "--user", "--unit=seamless-update-01M4HFPS1DBMJXQ4CE8ZZ0JRDH", "--collect",
		"-p", "Type=exec", "-p", "RuntimeMaxSec=1800",
	}
	tests := []struct {
		name    string
		environ []string
		setenv  []string
	}{
		{"nothing to forward", []string{"PATH=/usr/bin", "HOME=/home/me", "SEAMLESS_CONFIG=/home/me/.config/seamless/seamless.yaml"}, nil},
		{
			"the proxy and CA variables the daemon has, both spellings, in a fixed order",
			[]string{
				"SSL_CERT_FILE=/etc/ssl/corp.pem", "https_proxy=http://proxy.lan:3128",
				"HTTPS_PROXY=http://proxy.lan:3128", "NO_PROXY=localhost,127.0.0.1", "PATH=/usr/bin",
			},
			[]string{
				"--setenv=HTTPS_PROXY=http://proxy.lan:3128", "--setenv=https_proxy=http://proxy.lan:3128",
				"--setenv=NO_PROXY=localhost,127.0.0.1", "--setenv=SSL_CERT_FILE=/etc/ssl/corp.pem",
			},
		},
		{
			// An empty value is unset, and the bare --setenv=NAME form would fail
			// the whole spawn on systemd 249.
			"an empty value is never forwarded",
			[]string{"HTTP_PROXY=", "ALL_PROXY=socks5://h:1080", "SSL_CERT_DIR"},
			[]string{"--setenv=ALL_PROXY=socks5://h:1080"},
		},
		{"the last value wins, as in the real environment", []string{"HTTP_PROXY=http://a:1", "HTTP_PROXY=http://b:2"}, []string{"--setenv=HTTP_PROXY=http://b:2"}},
		{"a value with spaces stays one argument", []string{"NO_PROXY=a b"}, []string{"--setenv=NO_PROXY=a b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := systemdRunArgv(target, spawnReq(t), tt.environ)
			want := append(append(append([]string{}, head...), tt.setenv...), updater...)
			require.Equal(t, want, got)
			for _, arg := range got {
				if name, ok := strings.CutPrefix(arg, "--setenv="); ok {
					require.Contains(t, name, "=", "never the bare --setenv=NAME form")
				}
			}
		})
	}
}

func TestSystemdRunArgv_RuntimeIsTwiceTheInstallerTimeout(t *testing.T) {
	argv := systemdRunArgv(spawnTarget{exe: "/x", configPath: "/c"}, spawnReq(t), nil)
	require.Contains(t, argv, fmt.Sprintf("RuntimeMaxSec=%d", int(2*installerTimeout.Seconds())))
}

func TestSystemdRunEnv(t *testing.T) {
	// A peer_auth daemon's system unit has no XDG_RUNTIME_DIR: systemd-run
	// --user is pointed at the daemon account's own manager.
	got := systemdRunEnv([]string{"PATH=/usr/bin:/bin", "HOME=/opt/seamless/home"}, 480)
	require.Equal(t, []string{"PATH=/usr/bin:/bin", "HOME=/opt/seamless/home", "XDG_RUNTIME_DIR=/run/user/480"}, got)
	// A per-user unit's daemon keeps the one its manager gave it.
	own := []string{"XDG_RUNTIME_DIR=/run/user/1000", "PATH=/bin"}
	require.Equal(t, own, systemdRunEnv(own, 480))
}

func TestSystemdRunFailure(t *testing.T) {
	exit1 := errors.New("exit status 1")
	tests := []struct {
		name string
		out  string
		err  error
		why  string
	}{
		// The texts systemd 249-261 print (lab detached-updater-os-spikes).
		{"no user bus or manager (252)", "Failed to connect to bus: No medium found\n", exit1, "the systemd user manager is unreachable"},
		{"XDG_RUNTIME_DIR unset (259)", "Failed to connect to user scope bus via local transport: $DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined (consider using --machine=<user>@.host --user to connect to bus of other user)\n", exit1, "the systemd user manager is unreachable"},
		{"this attempt's unit exists", "Failed to start transient service unit: Unit seamless-update-01M4HFRZSRATKM96C31VKQNDVB.service was already loaded or has a fragment file.\n", exit1, "this attempt's unit already exists"},
		{"the binary is gone", "Failed to find executable /nonexistent/seamlessd: No such file or directory\n", exit1, "the seamlessd binary is missing or not executable"},
		{"the binary is not executable (249)", "Failed to find executable /tmp/noexec-spike: Permission denied\n", exit1, "the seamlessd binary is missing or not executable"},
		{"exec failed (Type=exec, 252+)", "Job for seamless-update-01M4HFPS1DBMJXQ4CE8ZZ0JRDH.service failed because the control process exited with error code.\nSee \"systemctl --user status seamless-update-01M4HFPS1DBMJXQ4CE8ZZ0JRDH.service\" and \"journalctl --user -xeu seamless-update-01M4HFPS1DBMJXQ4CE8ZZ0JRDH.service\" for details.\n", exit1, "the updater could not be executed"},
		{"systemd-run itself is missing", "", &exec.Error{Name: "systemd-run", Err: exec.ErrNotFound}, "systemd-run is not installed"},
		{"systemd-run hung", "", fmt.Errorf("%w after 30s: signal: killed", errHandoffTimeout), "systemd-run did not return"},
		{"the daemon is shutting down", "", fmt.Errorf("systemd-run may still hand the updater over: %w", context.Canceled), "the daemon stopped waiting for systemd-run"},
		{"anything else", "Failed to start transient service unit: Invalid environment block.\n", exit1, "systemd-run failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := systemdRunFailure([]byte(tt.out), tt.err)
			require.ErrorIs(t, err, tt.err)
			require.True(t, strings.HasPrefix(err.Error(), tt.why+": "), err.Error())
			if line := firstLine(tt.out); tt.out != "" {
				require.True(t, strings.HasSuffix(err.Error(), ": "+line), "the error carries systemd-run's first line: %s", err)
				require.NotContains(t, err.Error(), "\n")
			}
		})
	}
}

// taskDoc is the slice of a Task Scheduler definition the plan test checks.
type taskDoc struct {
	XMLName     xml.Name  `xml:"http://schemas.microsoft.com/windows/2004/02/mit/task Task"`
	Description string    `xml:"RegistrationInfo>Description"`
	Triggers    *struct{} `xml:"Triggers"`
	Principal   struct {
		ID        string `xml:"id,attr"`
		UserID    string `xml:"UserId"`
		LogonType string `xml:"LogonType"`
		RunLevel  string `xml:"RunLevel"`
	} `xml:"Principals>Principal"`
	Settings struct {
		MultipleInstancesPolicy    string `xml:"MultipleInstancesPolicy"`
		DisallowStartIfOnBatteries string `xml:"DisallowStartIfOnBatteries"`
		StopIfGoingOnBatteries     string `xml:"StopIfGoingOnBatteries"`
		AllowStartOnDemand         string `xml:"AllowStartOnDemand"`
		Enabled                    string `xml:"Enabled"`
		Hidden                     string `xml:"Hidden"`
		ExecutionTimeLimit         string `xml:"ExecutionTimeLimit"`
	} `xml:"Settings"`
	Actions struct {
		Context   string `xml:"Context,attr"`
		Command   string `xml:"Exec>Command"`
		Arguments string `xml:"Exec>Arguments"`
	} `xml:"Actions"`
}

func TestUpdateTaskPlan(t *testing.T) {
	const sid = "S-1-5-21-1-2-3-1001"
	tests := []struct {
		name, exe, arguments string
	}{
		{
			"the usual layout",
			`C:\Users\me\AppData\Local\Programs\Seamless\seamlessd.exe`,
			`update --auto --attempt 01M4HFPS1DBMJXQ4CE8ZZ0JRDH --from 0.7.2 --to 0.7.3 --why auto --config C:\Users\me\.config\seamless\seamless.yaml`,
		},
		{
			// taskArguments quotes a path with a space (syscall.EscapeArg); the
			// definition must carry the quotes through XML unchanged.
			"spaces in both paths",
			`C:\Users\Jane Doe\AppData\Local\Programs\Seamless\seamlessd.exe`,
			`update --auto --attempt 01M4HFPS1DBMJXQ4CE8ZZ0JRDH --from 0.7.2 --to 0.7.3 --why auto --config "C:\Users\Jane Doe\.config\seamless\seamless.yaml"`,
		},
		{
			"characters XML escapes",
			`C:\Users\A&B <x>\seamlessd.exe`,
			`update --config "C:\Users\A&B <x>\it's \"q\"\seamless.yaml"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := newUpdateTaskPlan(sid, tt.exe, tt.arguments)
			require.NoError(t, err)
			require.Equal(t, "SeamlessUpdate-S-1-5-21-1-2-3-1001", p.name)
			require.Equal(t, []byte{0xFF, 0xFE}, p.definition[:2], "UTF-16 little endian behind a byte-order mark")
			require.Zero(t, len(p.definition)%2)

			text := decodeUTF16(p.definition)
			require.True(t, strings.HasPrefix(text, `<?xml version="1.0" encoding="UTF-16"?>`), text)
			dec := xml.NewDecoder(strings.NewReader(text))
			dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
			var doc taskDoc
			require.NoError(t, dec.Decode(&doc))

			require.Equal(t, sid, doc.Principal.UserID)
			require.Equal(t, "InteractiveToken", doc.Principal.LogonType, "a standard user is refused S4U")
			require.Equal(t, "LeastPrivilege", doc.Principal.RunLevel)
			require.Equal(t, doc.Principal.ID, doc.Actions.Context)
			require.Nil(t, doc.Triggers, "no trigger: the task runs only when started")
			require.Equal(t, "IgnoreNew", doc.Settings.MultipleInstancesPolicy)
			require.Equal(t, "true", doc.Settings.Hidden)
			require.Equal(t, "PT1H", doc.Settings.ExecutionTimeLimit)
			require.Equal(t, "true", doc.Settings.AllowStartOnDemand)
			require.Equal(t, "true", doc.Settings.Enabled)
			require.Equal(t, "false", doc.Settings.DisallowStartIfOnBatteries)
			require.Equal(t, "false", doc.Settings.StopIfGoingOnBatteries)
			require.Equal(t, tt.exe, doc.Actions.Command)
			require.Equal(t, tt.arguments, doc.Actions.Arguments, "the argument string survives XML byte for byte")
			require.NotContains(t, doc.Description, update.InstallerMarker, "the update task is no service definition")

			def, err := parseTaskXML(p.definition)
			require.NoError(t, err, "the probe's own task parser reads it")
			require.Equal(t, sid, def.UserID)
			require.Equal(t, tt.exe, def.Command)
		})
	}
}

func TestUpdateTaskPlan_Commands(t *testing.T) {
	p, err := newUpdateTaskPlan("S-1-5-21-9", `C:\s.exe`, "update")
	require.NoError(t, err)
	require.Equal(t, []string{"schtasks", "/Create", "/TN", "SeamlessUpdate-S-1-5-21-9", "/XML", `C:\d\update\update-task-1.xml`, "/F"},
		p.create(`C:\d\update\update-task-1.xml`))
	require.Equal(t, []string{"schtasks", "/Run", "/TN", "SeamlessUpdate-S-1-5-21-9"}, p.run())
}

func TestWithoutBreakaway(t *testing.T) {
	denied := &os.PathError{Op: "fork/exec", Path: `C:\s.exe`, Err: errAccessDenied}
	tests := []struct {
		name      string
		flags     uint32
		err       error
		retry     bool
		wantFlags uint32
	}{
		{"a job that refuses breakaway: retry, still windowless in a group of its own", detachedChildFlags, denied, true, createNoWindow | createNewProcessGroup},
		{"already without breakaway: no second retry", createNoWindow | createNewProcessGroup, denied, false, createNoWindow | createNewProcessGroup},
		{"another error is not retried", detachedChildFlags, &os.PathError{Op: "fork/exec", Path: `C:\s.exe`, Err: os.ErrNotExist}, false, detachedChildFlags},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, retry := withoutBreakaway(tt.flags, tt.err)
			require.Equal(t, tt.retry, retry)
			require.Equal(t, tt.wantFlags, flags)
		})
	}
	require.Equal(t, uint32(0x09000200), uint32(detachedChildFlags),
		"CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | CREATE_BREAKAWAY_FROM_JOB")
}

// TestChildElseTask holds the Windows spawn order on every OS: the detached
// child first, the update task only when the child fails, and no task once the
// daemon is shutting down.
func TestChildElseTask(t *testing.T) {
	childErr := errors.New(`fork/exec C:\Seamless\seamlessd.exe: Access is denied`)
	taskErr := errors.New("schtasks /Create: exit status 1: ERROR: Access is denied")
	const warning = "update: the updater could not start as a detached process, so the update task starts it"
	tests := []struct {
		name         string
		shuttingDown bool
		child, task  error    // what each attempt returns
		calls        []string // the attempts made, in order
		wantErr      string   // "" when the updater started
		wantWarn     bool
	}{
		{"the child starts: the task never runs", false, nil, taskErr, []string{"child"}, "", false},
		{"the child fails and the task starts it: nil and one warning", false, childErr, nil, []string{"child", "task"}, "", true},
		{
			"both fail: one error naming both", false, childErr, taskErr, []string{"child", "task"},
			"seamlessd.spawn: a detached process: " + childErr.Error() + "; the update task: " + taskErr.Error(), false,
		},
		{"shutting down: the child still starts", true, nil, taskErr, []string{"child"}, "", false},
		{
			"shutting down: a failed child is final and the task never runs", true, childErr, nil, []string{"child"},
			"seamlessd.spawn: a detached process: " + childErr.Error(), false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.shuttingDown {
				cancel()
			}
			var logs bytes.Buffer
			var calls []string
			err := childElseTask(ctx, slog.New(slog.NewJSONHandler(&logs, nil)),
				func() error { calls = append(calls, "child"); return tt.child },
				func() error { calls = append(calls, "task"); return tt.task })
			require.Equal(t, tt.calls, calls)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tt.wantErr)
				require.ErrorIs(t, err, childErr)
				if len(calls) == 2 {
					require.ErrorIs(t, err, taskErr)
				}
			}

			var records []map[string]any
			for dec := json.NewDecoder(&logs); dec.More(); {
				var rec map[string]any
				require.NoError(t, dec.Decode(&rec))
				records = append(records, rec)
			}
			if !tt.wantWarn {
				require.Empty(t, records, "only a start through the task logs; a failure is returned, never also logged")
				return
			}
			require.Len(t, records, 1)
			require.Equal(t, "WARN", records[0]["level"])
			require.Equal(t, warning, records[0]["msg"])
			require.Equal(t, childErr.Error(), records[0]["err"], "the warning says why the child did not start")
		})
	}
}

func TestNewUpdateSpawner(t *testing.T) {
	require.Nil(t, newUpdateSpawner(config.Defaults(), nil), "no config file loaded: the updater could not be told which")

	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	sp := newUpdateSpawner(cfg, nil)
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		require.NotNil(t, sp)
		// Every spawner refuses a request Validate refuses before it builds
		// or starts anything.
		bad := spawnReq(t)
		bad.AttemptID = "not-a-ulid"
		require.ErrorIs(t, sp.Spawn(context.Background(), bad), update.ErrInvalidAttempt)
		backwards := spawnReq(t)
		backwards.From, backwards.To = backwards.To, backwards.From
		require.ErrorIs(t, sp.Spawn(context.Background(), backwards), update.ErrInvalidAttempt)
	default:
		require.Nil(t, sp, "an OS with no service has no updater to start")
	}
}
