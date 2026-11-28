package hooks

import (
	"fmt"
	"strings"
)

// UninstallOptions configures an uninstall.
type UninstallOptions struct {
	Client       Client // agent client profile; "" (zero value) => Claude Code
	SettingsPath string // target file: CC settings.json or Codex hooks.json
	BaseURL      string // e.g. http://127.0.0.1:8081 -- the http-url ownership arm needs it
}

// UninstallResult reports what an uninstall did.
type UninstallResult struct {
	Changed    bool
	BackupPath string   // "" when no backup was written
	Actions    []string // per-hook: "SessionStart: removed|absent"
}

// Uninstall removes the client's Seamless hook entries from the settings/hooks
// file at opts.SettingsPath, the exact inverse of Install. It uses the shared
// definition classifier and removes current, marked-stale, or confidently
// legacy Seamless entries. It preserves every unknown key and every foreign
// entry -- including a v1 "seam_managed" hook at a different URL.
// An event array that empties is dropped, and the top-level "hooks" key is
// dropped if it empties (the file itself is never deleted, even if it becomes
// "{}"). It backs the file up once before the first change (reusing backupOnce,
// which is once-ever, so it never clobbers Install's original backup). It is
// idempotent: a file with nothing of ours -- or a missing file -- is left
// untouched with no error.
func Uninstall(opts UninstallOptions) (UninstallResult, error) {
	client, profile, err := resolveHookProfile(opts.Client)
	if err != nil {
		return UninstallResult{}, fmt.Errorf("hooks.Uninstall: %w", err)
	}
	if strings.TrimSpace(opts.SettingsPath) == "" {
		return UninstallResult{}, fmt.Errorf("hooks.Uninstall: settings path is required")
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return UninstallResult{}, fmt.Errorf("hooks.Uninstall: base URL is required")
	}

	settings, mode, err := loadSettings(opts.SettingsPath)
	if err != nil {
		return UninstallResult{}, err
	}
	hooksObj := nestedObject(settings, "hooks")

	var res UninstallResult
	for _, hs := range profile {
		arr := entryArray(hooksObj, hs.Event)
		desiredURL := strings.TrimRight(opts.BaseURL, "/") + hs.Endpoint
		// Uninstall does not need the original install paths: exact entries that
		// differ from this minimal desired value are still recognized as marked
		// or legacy by the same classifier, an http entry an earlier install
		// wrote among them.
		desired := buildEntry(client, hs, "", "")
		matches, _ := classifiedHookIndices(client, arr, desired, hs, desiredURL)
		if len(matches) == 0 {
			res.Actions = append(res.Actions, hs.Event+": absent")
			continue
		}
		arr = removeIndices(arr, matches)
		res.Changed = true
		res.Actions = append(res.Actions, hs.Event+": removed")
		// Drop an event whose array empties, rather than leaving "SessionStart": []
		// litter behind; keep the trimmed array otherwise.
		if len(arr) == 0 {
			delete(hooksObj, hs.Event)
		} else {
			hooksObj[hs.Event] = arr
		}
	}
	if !res.Changed {
		return res, nil
	}
	// hooksObj is the live settings["hooks"] map (nestedObject returns it by
	// reference when present, which it is whenever Changed is true). Drop the key
	// entirely when we emptied it, so an all-Seamless file collapses cleanly.
	if len(hooksObj) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooksObj
	}

	backup, err := backupOnce(opts.SettingsPath, mode)
	if err != nil {
		return UninstallResult{}, err
	}
	res.BackupPath = backup

	if err := writeSettings(opts.SettingsPath, settings, mode); err != nil {
		return UninstallResult{}, err
	}
	return res, nil
}
