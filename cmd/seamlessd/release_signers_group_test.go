package main

import (
	"io"
	"os"
	"runtime"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/shellcmd"
)

// groupHoldsOnly reads the local account files; every way they could let
// another account into the group answers false.
func TestGroupHoldsOnly(t *testing.T) {
	const me, myGroup = 1000, 1000
	passwd := "root:x:0:0:root:/root:/bin/bash\n" +
		"# a comment\n\n" +
		"eitan:x:1000:1000:Eitan,,,:/home/eitan:/bin/bash\n" +
		"other:x:1001:1001::/home/other:/bin/sh\n"
	group := "root:x:0:\nsudo:x:27:eitan,other\neitan:x:1000:\nother:x:1001:\nsolo:x:1500:eitan\n"
	etc := func(passwd, group string) fstest.MapFS {
		return fstest.MapFS{"passwd": {Data: []byte(passwd)}, "group": {Data: []byte(group)}}
	}
	for _, tt := range []struct {
		name string
		etc  fstest.MapFS
		uid  int
		gid  uint32
		want bool
	}{
		{"the account's private group", etc(passwd, group), me, myGroup, true},
		{"a supplementary group of the account alone", etc(passwd, group), me, 1500, true},
		{"the private group listing its own account", etc(passwd, "eitan:x:1000:eitan\n"), me, myGroup, true},
		{"CRLF lines", etc("eitan:x:1000:1000::/home/eitan:/bin/sh\r\n", "eitan:x:1000:\r\n"), me, myGroup, true},
		{"a group with another member", etc(passwd, group), me, 27, false},
		{"another account's private group", etc(passwd, group), me, 1001, false},
		{"another account's primary group, no group entry naming it", etc(passwd+"guest:x:1002:1000::/:/bin/sh\n", group), me, myGroup, false},
		{"a second entry for the gid adds a member", etc(passwd, group+"dup:x:1000:other\n"), me, myGroup, false},
		{"no group entry for the gid", etc(passwd, group), me, 4242, false},
		{"an account the files do not hold", etc(passwd, group), 2000, myGroup, false},
		{"a NIS compat line in passwd", etc(passwd+"+::::::\n", group), me, myGroup, false},
		{"a NIS compat line in group", etc(passwd, group+"+:::\n"), me, myGroup, false},
		{"a malformed passwd line", etc(passwd+"broken:x:1003\n", group), me, myGroup, false},
		{"a non-numeric gid", etc(passwd, group+"bad:x:abc:\n"), me, myGroup, false},
		{"no group file", fstest.MapFS{"passwd": {Data: []byte(passwd)}}, me, myGroup, false},
		{"no passwd file", fstest.MapFS{"group": {Data: []byte(group)}}, me, myGroup, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, groupHoldsOnly(tt.etc, tt.uid, tt.gid))
		})
	}

	big := fstest.MapFS{"passwd": {Data: make([]byte, maxAccountFileBytes+1)}, "group": {Data: []byte(group)}}
	require.False(t, groupHoldsOnly(big, me, myGroup), "an account file over the bound is not read")
}

// On a per-user install a group write bit passes when the group is the
// account's own; under peer_auth, or world-writable, it never does.
func TestReleaseSignersFileProblem_OwnGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths: filepath.IsAbs and Dir read them as Windows paths")
	}
	const me, mine, shared = 1000, 1000, 100
	type node struct {
		uid, gid uint32
		mode     os.FileMode
	}
	path := "/home/eitan/.config/seamless/release_signers"
	// tree is the pin's path with ~/.config at dirMode and group dirGid; owner
	// owns the home and everything below it.
	treeOf := func(owner uint32, dirMode os.FileMode, dirGid uint32, file node) signersFS {
		dirs := map[string]node{
			"/": {0, 0, 0o755}, "/home": {0, 0, 0o755}, "/home/eitan": {owner, mine, 0o750},
			"/home/eitan/.config": {owner, dirGid, dirMode}, "/home/eitan/.config/seamless": {owner, mine, 0o755},
		}
		return signersFS{euid: me, open: func(string) (io.ReadCloser, error) { return nil, os.ErrNotExist },
			resolve:  func(p string) (string, error) { return p, nil },
			ownGroup: func(gid uint32) bool { return gid == mine },
			inspect: func(p string) (fileFacts, error) {
				if n, ok := dirs[p]; ok {
					return fileFacts{uid: n.uid, gid: n.gid, mode: n.mode | os.ModeDir, isDir: true}, nil
				}
				if p == path {
					return fileFacts{uid: file.uid, gid: file.gid, mode: file.mode}, nil
				}
				return fileFacts{}, os.ErrNotExist
			}}
	}
	tree := func(dirMode os.FileMode, dirGid uint32, file node) signersFS {
		return treeOf(me, dirMode, dirGid, file)
	}
	file := node{me, mine, 0o644}
	for _, tt := range []struct {
		name     string
		fsys     signersFS
		rootOnly bool
		want     string
	}{
		{"umask 002: a 0775 directory of the account's own group", tree(0o775, mine, file), false, ""},
		{"umask 002: a 0664 file of the account's own group", tree(0o755, mine, node{me, mine, 0o664}), false, ""},
		{"a 0775 directory of a shared group", tree(0o775, shared, file), false, "is mode 0775, writable by other accounts: chmod go-w /home/eitan/.config"},
		{"a 0664 file of a shared group", tree(0o755, mine, node{me, shared, 0o664}), false, "group- or world-writable"},
		{"a 0777 directory, own group or not", tree(0o777, mine, file), false, "is mode 0777"},
		{"a 0666 file, own group or not", tree(0o755, mine, node{me, mine, 0o666}), false, "group- or world-writable"},
		{"peer_auth: root's 0775 directory of a group the account holds alone", treeOf(0, 0o775, mine, node{0, 0, 0o644}), true, "is mode 0775"},
		{"peer_auth: root's 0664 file of a group the account holds alone", treeOf(0, 0o755, mine, node{0, mine, 0o664}), true, "group- or world-writable"},
		{"peer_auth: root's directories and file", treeOf(0, 0o755, 0, node{0, 0, 0o644}), true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := releaseSignersFileProblem(path, tt.rootOnly, tt.fsys)
			if tt.want == "" {
				require.Empty(t, p)
				return
			}
			require.Contains(t, p, tt.want)
		})
	}

	// Root's directory is changed through sudo, and the alternative names
	// whose directories pass.
	p := releaseSignersFileProblem(path, true, treeOf(0, 0o775, mine, node{0, 0, 0o644}))
	t4 := shellcmd.ToolsFor(runtime.GOOS)
	require.Contains(t, p, t4.AsRoot(t4.Chmod, "go-w", "/home/eitan/.config")+", or keep the file under directories only root can write")
	p = releaseSignersFileProblem(path, false, tree(0o775, shared, file))
	require.Contains(t, p, "chmod go-w /home/eitan/.config, or keep the file under directories only root or this account (uid 1000) can write")

	noGroups := tree(0o775, mine, file)
	noGroups.ownGroup = nil
	require.Contains(t, releaseSignersFileProblem(path, false, noGroups), "is mode 0775",
		"where no group can be read as the account's own (macOS), a group write bit is refused")
}
