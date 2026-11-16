package main

// Claude Code's user-scope MCP registration, read as data. The claude CLI has
// no machine-readable mcp get or list (get prints a health report), so the
// installer reads what `claude mcp add-json --scope user` stores instead: the
// reserved "seamless" key of the top-level mcpServers object in Claude Code's
// config file, as desktop_mcp.go reads the Claude app's. That file also holds
// the account's projects, history and sign-in, so it is decoded into raw
// messages, only the reserved entry is looked at, and no value of it reaches a
// diagnostic: the drift labels name fields, never what they hold. Nothing here
// writes the file; every change goes through the claude CLI.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// claudeCodeConfigFile is the name of Claude Code's config file, in the home
// directory or in $CLAUDE_CONFIG_DIR.
const claudeCodeConfigFile = ".claude.json"

// claudeCodeConfigPathFor is where the installer looks for Claude Code's
// user-scope MCP servers: .claude.json in configDir ($CLAUDE_CONFIG_DIR, the
// directory Claude Code is told to keep its config in) when that is set, else
// in home. A file that is not there judges nothing, and the registration is
// then left as it is. An unknown home is an error, never a guessed path.
func claudeCodeConfigPathFor(home, configDir string) (string, error) {
	if dir := strings.TrimSpace(configDir); dir != "" {
		return filepath.Join(dir, claudeCodeConfigFile), nil
	}
	if home == "" {
		return "", errors.New("claude code config: home directory is unknown")
	}
	return filepath.Join(home, claudeCodeConfigFile), nil
}

func defaultClaudeCodeConfigPath() (string, error) {
	return claudeCodeConfigPathFor(homeDir(), os.Getenv("CLAUDE_CONFIG_DIR"))
}

// claudeCodeMCPEntry is a registration reduced to what the installer compares.
// The env is recorded as presence only: a foreign entry's values may hold
// credentials.
type claudeCodeMCPEntry struct {
	Type    string // "" when the entry names none, which is stdio when it has a command
	Command string
	Args    []string
	HasEnv  bool // a non-empty env
}

// stdio reports whether the entry is a stdio server.
func (e claudeCodeMCPEntry) stdio() bool {
	return e.Type == "stdio" || (e.Type == "" && e.Command != "")
}

// parseClaudeCodeMCPEntry decodes one mcpServers entry. An entry that is not an
// object, or whose type, command, args or env has another JSON type than
// Claude Code's, is an error: the caller cannot tell whose it is.
func parseClaudeCodeMCPEntry(raw json.RawMessage) (claudeCodeMCPEntry, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return claudeCodeMCPEntry{}, fmt.Errorf("parse Claude Code MCP entry: %w", err)
	}
	if fields == nil {
		return claudeCodeMCPEntry{}, errors.New("parse Claude Code MCP entry: expected object")
	}
	var e claudeCodeMCPEntry
	var env map[string]string
	for _, f := range []struct {
		key string
		dst any
	}{{"type", &e.Type}, {"command", &e.Command}, {"args", &e.Args}, {"env", &env}} {
		if v, ok := fields[f.key]; ok {
			if err := json.Unmarshal(v, f.dst); err != nil {
				return claudeCodeMCPEntry{}, fmt.Errorf("parse Claude Code MCP entry field %q: %w", f.key, err)
			}
		}
	}
	e.HasEnv = len(env) > 0
	return e, nil
}

// desiredClaudeCodeMCPEntry is the entry claudeMCPAddArgs registers, read back
// from that argv's own JSON, so the registration and the comparison cannot
// describe two different entries.
func desiredClaudeCodeMCPEntry(baseURL, seamBin, configPath string) (claudeCodeMCPEntry, error) {
	args := claudeMCPAddArgs(baseURL, seamBin, configPath)
	return parseClaudeCodeMCPEntry(json.RawMessage(args[len(args)-1]))
}

// readClaudeCodeMCPEntry reads the reserved user-scope entry from the Claude
// Code config file at path. present is false when the file, its mcpServers or
// the entry is not there.
func readClaudeCodeMCPEntry(path string) (entry claudeCodeMCPEntry, present bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return claudeCodeMCPEntry{}, false, nil
	}
	if err != nil {
		return claudeCodeMCPEntry{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return claudeCodeMCPEntry{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	rawServers, ok := top["mcpServers"]
	if !ok {
		return claudeCodeMCPEntry{}, false, nil
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(rawServers, &servers); err != nil {
		return claudeCodeMCPEntry{}, false, fmt.Errorf("parse %s: %w: %w", path, errMCPServersNotObject, err)
	}
	raw, ok := servers[seamlessMCPName]
	if !ok {
		return claudeCodeMCPEntry{}, false, nil
	}
	entry, err = parseClaudeCodeMCPEntry(raw)
	if err != nil {
		return claudeCodeMCPEntry{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return entry, true, nil
}

// claudeCodeBridgeDrift compares an entry with the desired stdio bridge, field
// by field, as claudeDesktopMCPDrift does for the Claude app's.
func claudeCodeBridgeDrift(got, want claudeCodeMCPEntry) []string {
	if !got.stdio() {
		return []string{"transport type differs"}
	}
	var drift []string
	if got.Command != want.Command {
		drift = append(drift, "bridge command differs")
	}
	if !slices.Equal(got.Args, want.Args) {
		drift = append(drift, "bridge arguments differ")
	}
	if got.HasEnv {
		drift = append(drift, "bridge environment present")
	}
	return drift
}

// claudeCodeRepairDrift is why an existing user-scope registration is
// registered again, nil when it is left as it is. Only the one shape the
// installer knows as its own is repaired: a stdio bridge, where the desired
// registration is one too (an https or socket server_url), whose command is
// the desired seam or recognizably a seam CLI (codexMCPIsOwned's rule), and
// that differs from the desired bridge. An http registration, a bridge to
// another program, and a bridge where the desired registration is http all
// stay as they are.
func claudeCodeRepairDrift(got, want claudeCodeMCPEntry) []string {
	if !want.stdio() || !got.stdio() {
		return nil
	}
	if got.Command != want.Command && !isSeamCommand(got.Command) {
		return nil
	}
	return claudeCodeBridgeDrift(got, want)
}

// claudeCodeUserBridgeRepair reads the user-scope entry at path and says why
// it is to be registered again (claudeCodeRepairDrift). An unknown path, a
// file that does not read or parse, and an absent entry all say nothing: the
// installer then leaves the registration as it is, as it always has.
func claudeCodeUserBridgeRepair(path, baseURL, seamBin, configPath string) []string {
	if path == "" {
		return nil
	}
	want, err := desiredClaudeCodeMCPEntry(baseURL, seamBin, configPath)
	if err != nil {
		return nil
	}
	got, present, err := readClaudeCodeMCPEntry(path)
	if err != nil || !present {
		return nil
	}
	return claudeCodeRepairDrift(got, want)
}

// verifyClaudeCodeUserBridge re-reads the user-scope entry at path after a
// repair and requires the desired bridge there exactly: the repair is judged
// on that data, so its success is too, never on the add command's exit alone.
func verifyClaudeCodeUserBridge(path, baseURL, seamBin, configPath string) error {
	want, err := desiredClaudeCodeMCPEntry(baseURL, seamBin, configPath)
	if err != nil {
		return err
	}
	got, present, err := readClaudeCodeMCPEntry(path)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%s holds no user-scope %q entry", path, seamlessMCPName)
	}
	if drift := claudeCodeBridgeDrift(got, want); len(drift) > 0 {
		return fmt.Errorf("the user-scope %q entry in %s still differs (%s)", seamlessMCPName, path, strings.Join(drift, ", "))
	}
	return nil
}
