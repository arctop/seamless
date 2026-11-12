package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/arctop/seamless/internal/hooks"
)

// Claude Code's file-based managed settings: a system-wide managed-settings.json
// plus optional managed-settings.d/*.json drop-ins, written by an administrator
// or a device-management agent and not writable by the user. Their hooks run
// ALONGSIDE the user and project files' hooks -- precedence picks one value per
// key, but hook lists merge (unless allowManagedHooksOnly stops the non-managed
// ones running). So when the managed settings carry a Seamless hook, a
// second copy in ~/.claude/settings.json makes it fire twice: the briefing is
// injected twice, every prompt is matched twice. Doctor and install-hooks read
// these files to see that.
//
// The other managed sources (server-managed settings from the claude.ai console,
// MDM plists, the Windows registry) are not files Seamless can read, so they are
// out of reach here; the file form is the one a local agent guard writes.

const (
	claudeManagedSettingsFile    = "managed-settings.json"
	claudeManagedSettingsDropins = "managed-settings.d"
)

// claudeManagedSettingsDirFor is the system directory Claude Code reads
// file-based managed settings from (code.claude.com/docs/en/managed-settings).
// Claude Code does not read the legacy C:\ProgramData\ClaudeCode on Windows, so
// neither does this. "" on an OS Claude Code documents none for.
func claudeManagedSettingsDirFor(goos string) string {
	switch goos {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "linux":
		return "/etc/claude-code"
	case "windows":
		return `C:\Program Files\ClaudeCode`
	default:
		return ""
	}
}

// claudeManagedSettingsDir is the directory on this machine. It is a variable so
// tests point it at a temp dir: the machine running them may well have real
// managed settings (TestMain blanks it for the whole package).
var claudeManagedSettingsDir = func() string { return claudeManagedSettingsDirFor(runtime.GOOS) }

// claudeManagedSettingsFiles lists the managed settings documents in dir in
// Claude Code's merge order: managed-settings.json first, then every *.json in
// managed-settings.d/ alphabetically, skipping hidden files. Absent files and an
// absent drop-in directory are simply not listed; a drop-in directory that
// exists but cannot be read is an error.
func claudeManagedSettingsFiles(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	var files []string
	primary := filepath.Join(dir, claudeManagedSettingsFile)
	if info, err := os.Stat(primary); err == nil && !info.IsDir() {
		files = append(files, primary)
	}
	dropins := filepath.Join(dir, claudeManagedSettingsDropins)
	entries, err := os.ReadDir(dropins)
	if errors.Is(err, fs.ErrNotExist) {
		return files, nil
	}
	if err != nil {
		return files, fmt.Errorf("read %s: %w", dropins, err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") || entry.IsDir() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		files = append(files, filepath.Join(dropins, name))
	}
	return files, nil
}

// managedHookSource is one managed settings file that carries at least one
// Seamless hook, classified against the desired definitions.
type managedHookSource struct {
	path   string
	status hooks.InstallStatus
}

// claudeManagedHooks is what Claude Code's managed settings hold of the Seamless
// profile: the files that carry any of it, and the files that could not be read.
type claudeManagedHooks struct {
	sources []managedHookSource
	errs    []string
}

// inspectClaudeManagedHooks classifies every managed settings file in dir
// against the definitions opts describes (its SettingsPath is ignored).
// Ownership is the same classifier install and uninstall use, so a managed hook
// running some other seam executable still counts as Seamless's, just not
// current.
func inspectClaudeManagedHooks(dir string, opts hooks.InstallOptions) claudeManagedHooks {
	var out claudeManagedHooks
	files, err := claudeManagedSettingsFiles(dir)
	if err != nil {
		out.errs = append(out.errs, "cannot inspect managed settings: "+err.Error())
	}
	for _, path := range files {
		opts.SettingsPath = path
		status, err := hooks.InstalledStatus(opts)
		if err != nil {
			out.errs = append(out.errs, fmt.Sprintf("cannot inspect managed settings %s: %v", path, err))
			continue
		}
		if len(status.Owned) > 0 {
			out.sources = append(out.sources, managedHookSource{path: path, status: status})
		}
	}
	return out
}

// covered is the profile events any managed file carries, in profile order.
func (m claudeManagedHooks) covered(profile []string) []string {
	var out []string
	for _, event := range profile {
		if len(m.owners(event)) > 0 {
			out = append(out, event)
		}
	}
	return out
}

// owners is the managed files that carry event.
func (m claudeManagedHooks) owners(event string) []managedHookSource {
	var out []managedHookSource
	for _, src := range m.sources {
		if slices.Contains(src.status.Owned, event) {
			out = append(out, src)
		}
	}
	return out
}

// printManagedCoverage closes install-hooks' Claude Code block with the events
// it left to the managed settings, and anything there this install would have
// written differently: those files are not the user's, so the note names who
// can change them rather than offering a command.
func printManagedCoverage(m claudeManagedHooks) {
	for _, src := range m.sources {
		fieldRow("managed", dim(fmt.Sprintf("%s run from %s; not written here",
			hookEventNames(src.status.Owned), tildePath(src.path))))
		if len(src.status.Stale) > 0 {
			fmt.Printf("%s%s\n", fieldCont, yellow("differ from this install: "+hookEventNames(src.status.Stale)+
				"; the file is not yours to edit, so an administrator must update it"))
		}
	}
	for _, msg := range m.errs {
		fieldRow("managed", yellow(msg))
	}
}
