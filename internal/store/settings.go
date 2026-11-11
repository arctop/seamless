package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/config"
)

// SettingRepoProjectMap is the settings key holding the cwd->project-slug map: a
// JSON object {absolute-path: slug}. The hooks and session_start resolve an
// agent's working directory to a project slug through it.
const SettingRepoProjectMap = "repo_project_map"

// SettingProjectFamilies is the settings key holding project families: a JSON
// object {family-name: [slug, ...]}. Sibling briefings surface recent findings
// from a project's family members.
const SettingProjectFamilies = "project_families"

// ErrFamilyNotFound is returned by RemoveFamilyMembers when the named family does
// not exist.
var ErrFamilyNotFound = errors.New("store: project family not found")

// ErrFamilyExists is returned when a family create or rename would overwrite a
// different existing family.
var ErrFamilyExists = errors.New("store: project family already exists")

// ErrFamilyNoMembers is returned when a family replacement contains no usable
// project slugs. Empty families are not persisted.
var ErrFamilyNoMembers = errors.New("store: project family has no members")

// settingsExecutor is the read+write subset shared by *sql.DB and *sql.Tx (the
// mirror of rowQuerier in tasks.go), so a settings read-decode-mutate-write runs
// identically on the pool or inside one transaction. The family mutators need it
// to keep their read and their write-back in the same transaction.
type settingsExecutor interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ProjectFamilies decodes the project_families setting into a map. An unset or
// blank value yields an empty map (not an error).
func ProjectFamilies(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	return projectFamiliesTx(ctx, db)
}

// projectFamiliesTx decodes the project_families setting via any executor, so a
// mutator can read the map inside its own transaction.
func projectFamiliesTx(ctx context.Context, q settingsExecutor) (map[string][]string, error) {
	raw, found, err := getSettingTx(ctx, q, SettingProjectFamilies)
	if err != nil {
		return nil, err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return map[string][]string{}, nil
	}
	var m map[string][]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("store.ProjectFamilies: decode: %w", err)
	}
	return m, nil
}

// SiblingProjects returns the other project slugs sharing a family with project,
// deduped and excluding project itself. A project may appear in more than one
// family; all such siblings are unioned. Returns nil for the global scope or a
// project with no family.
func SiblingProjects(ctx context.Context, db *sql.DB, project string) ([]string, error) {
	if project == "" {
		return nil, nil
	}
	families, err := ProjectFamilies(ctx, db)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{project: true}
	var out []string
	for _, members := range families {
		if !slices.Contains(members, project) {
			continue
		}
		for _, slug := range members {
			if slug == "" || seen[slug] {
				continue
			}
			seen[slug] = true
			out = append(out, slug)
		}
	}
	return out, nil
}

// SetProjectFamilies persists the full families map as the project_families
// setting. Empty families (no members) are dropped and members are trimmed and
// deduped so the stored value stays canonical; passing an empty map clears the
// setting to "{}". It is the single writer the family CLI and mutators funnel
// through, so the setting never accumulates blanks or duplicates.
//
// It reads nothing, so unlike the read-modify-write mutators below it needs no
// transaction: last-write-wins is inherent to its "set the whole map" contract.
func SetProjectFamilies(ctx context.Context, db *sql.DB, families map[string][]string) error {
	return setProjectFamiliesTx(ctx, db, families)
}

// SaveProjectFamily creates, replaces, or renames one family atomically.
// previousName is empty for a create; otherwise that family must exist. A
// create or rename never merges into an existing family because silently
// combining their context boundaries would be surprising. The submitted
// member set replaces the old set in full and must contain at least one slug.
func SaveProjectFamily(ctx context.Context, db *sql.DB, previousName, name string, members []string) ([]string, error) {
	previousName = strings.TrimSpace(previousName)
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("store.SaveProjectFamily: family name is empty")
	}
	members = dedupeSlugs(members)
	if len(members) == 0 {
		return nil, fmt.Errorf("store.SaveProjectFamily: %w", ErrFamilyNoMembers)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.SaveProjectFamily: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	families, err := projectFamiliesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if previousName == "" {
		if _, exists := families[name]; exists {
			return nil, fmt.Errorf("store.SaveProjectFamily: %q: %w", name, ErrFamilyExists)
		}
	} else {
		if _, exists := families[previousName]; !exists {
			return nil, fmt.Errorf("store.SaveProjectFamily: %q: %w", previousName, ErrFamilyNotFound)
		}
		if previousName != name {
			if _, exists := families[name]; exists {
				return nil, fmt.Errorf("store.SaveProjectFamily: %q: %w", name, ErrFamilyExists)
			}
			delete(families, previousName)
		}
	}
	families[name] = members
	if err := setProjectFamiliesTx(ctx, tx, families); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.SaveProjectFamily: commit: %w", err)
	}
	return members, nil
}

// setProjectFamiliesTx canonicalizes and persists families via any executor, so a
// mutator can write back inside the transaction it read under.
func setProjectFamiliesTx(ctx context.Context, q settingsExecutor, families map[string][]string) error {
	clean := make(map[string][]string, len(families))
	for name, members := range families {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if deduped := dedupeSlugs(members); len(deduped) > 0 {
			clean[name] = deduped
		}
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return fmt.Errorf("store.SetProjectFamilies: %w", err)
	}
	return setSettingTx(ctx, q, SettingProjectFamilies, string(b))
}

// AddFamilyMembers adds slugs to the named family, creating the family when it is
// new, and persists the result. Existing members keep their order and duplicates
// are ignored, so callers may re-add the same slugs idempotently. Returns the
// family's resulting members.
//
// The read-decode-mutate-write runs inside one transaction (the AddRepoMapping
// recipe): the pool is capped at a single connection (see Open), so the whole
// mutation is serialized against concurrent mutators and two callers growing
// different families at once can no longer clobber each other's family. If the
// pool ever grows past one connection this needs BEGIN IMMEDIATE, since two
// deferred transactions could still interleave read-then-write.
func AddFamilyMembers(ctx context.Context, db *sql.DB, family string, slugs []string) ([]string, error) {
	family = strings.TrimSpace(family)
	if family == "" {
		return nil, fmt.Errorf("store.AddFamilyMembers: family name is empty")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.AddFamilyMembers: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	families, err := projectFamiliesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	members := dedupeSlugs(append(families[family], slugs...))
	families[family] = members
	if err := setProjectFamiliesTx(ctx, tx, families); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.AddFamilyMembers: commit: %w", err)
	}
	return members, nil
}

// RemoveFamilyMembers removes slugs from the named family and persists the
// result. Passing no slugs removes the whole family; a family left with no
// members after the removal is dropped as well. Returns the family's resulting
// members, empty when the family was removed. It errors with ErrFamilyNotFound
// when the named family does not exist.
//
// Like AddFamilyMembers the read-decode-mutate-write runs inside one transaction,
// serialized by the single-connection pool (see Open), so a concurrent removal
// from another family survives instead of being clobbered by this write-back.
// If the pool ever grows past one connection this needs BEGIN IMMEDIATE.
func RemoveFamilyMembers(ctx context.Context, db *sql.DB, family string, slugs []string) ([]string, error) {
	family = strings.TrimSpace(family)
	if family == "" {
		return nil, fmt.Errorf("store.RemoveFamilyMembers: family name is empty")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.RemoveFamilyMembers: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	families, err := projectFamiliesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if _, ok := families[family]; !ok {
		// Rollback of the read-only transaction is harmless.
		return nil, fmt.Errorf("store.RemoveFamilyMembers: %q: %w", family, ErrFamilyNotFound)
	}
	var kept []string
	if len(slugs) == 0 {
		delete(families, family) // no slugs named: the whole family goes
	} else {
		drop := make(map[string]bool, len(slugs))
		for _, s := range slugs {
			drop[strings.TrimSpace(s)] = true
		}
		for _, m := range families[family] {
			if !drop[m] {
				kept = append(kept, m)
			}
		}
		families[family] = kept // setProjectFamiliesTx drops it when kept is empty
	}
	if err := setProjectFamiliesTx(ctx, tx, families); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.RemoveFamilyMembers: commit: %w", err)
	}
	return kept, nil
}

// dedupeSlugs returns slugs with surrounding whitespace trimmed, blanks dropped,
// and duplicates removed, preserving first-seen order.
func dedupeSlugs(slugs []string) []string {
	seen := make(map[string]bool, len(slugs))
	out := make([]string, 0, len(slugs))
	for _, s := range slugs {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// SettingBriefingConfig is the settings key holding the console-saved briefing
// override: a JSON-encoded config.Briefing. When present it layers over the
// file/env briefing config (see BriefingConfig), so the owner can tune the
// SessionStart injection from the console without editing seamless.yaml or
// restarting the daemon.
const SettingBriefingConfig = "briefing_config"

// BriefingConfig returns the effective briefing config: base (the file/env
// values) with the console-saved override row, when present, decoded over it.
// overridden reports whether such a row exists. Absent fields in a stored
// override keep their base value, so a row written by an older console version
// stays forward-compatible.
func BriefingConfig(ctx context.Context, db *sql.DB, base config.Briefing) (cfg config.Briefing, overridden bool, err error) {
	raw, found, err := GetSetting(ctx, db, SettingBriefingConfig)
	if err != nil {
		return base, false, err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return base, false, nil
	}
	cfg = base
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return base, false, fmt.Errorf("store.BriefingConfig: decode: %w", err)
	}
	return cfg, true, nil
}

// SetBriefingConfig persists b as the console briefing override. Callers
// validate first (config.Briefing.Validate); this only encodes and stores.
func SetBriefingConfig(ctx context.Context, db *sql.DB, b config.Briefing) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("store.SetBriefingConfig: %w", err)
	}
	return SetSetting(ctx, db, SettingBriefingConfig, string(raw))
}

// ClearBriefingConfig removes the console briefing override, reverting the
// effective briefing config to the file/env base.
func ClearBriefingConfig(ctx context.Context, db *sql.DB) error {
	return DeleteSetting(ctx, db, SettingBriefingConfig)
}

// SettingFeaturesConfig is the settings key holding the optional-features
// override: a JSON-encoded config.Features. When present it layers over the
// file/env features config (see FeaturesConfig), so the owner can turn optional
// features on and off from the console without editing seamless.yaml or
// restarting the daemon.
//
// Two writers reach this row: the console Settings form, and the one-time
// grandfather migration that keeps research enabled on installations that
// already hold trial data. That is why the console calls it a "stored override"
// rather than implying the owner set it.
const SettingFeaturesConfig = "features_config"

// FeaturesConfig returns the effective optional-features config: base (the
// file/env values) with the stored override row, when present, decoded over it.
// overridden reports whether such a row exists. Absent fields in a stored
// override keep their base value, so a row written before a feature existed
// stays forward-compatible -- a newly added feature keeps its default rather
// than being zeroed off by an old row.
//
// Callers resolve this LIVE (per request or per assembly, like the briefing
// override) and must be failure-soft: on error, log and fall back to base rather
// than failing an agent call or a console page over a corrupt row.
func FeaturesConfig(ctx context.Context, db *sql.DB, base config.Features) (cfg config.Features, overridden bool, err error) {
	raw, found, err := GetSetting(ctx, db, SettingFeaturesConfig)
	if err != nil {
		return base, false, err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return base, false, nil
	}
	cfg = base
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return base, false, fmt.Errorf("store.FeaturesConfig: decode: %w", err)
	}
	return cfg, true, nil
}

// SetFeaturesConfig persists f as the optional-features override. Callers
// validate first; this only encodes and stores.
func SetFeaturesConfig(ctx context.Context, db *sql.DB, f config.Features) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("store.SetFeaturesConfig: %w", err)
	}
	return SetSetting(ctx, db, SettingFeaturesConfig, string(raw))
}

// ClearFeaturesConfig removes the optional-features override, reverting the
// effective config to the file/env base -- which, for an installation that never
// set the keys, means every optional feature is off again.
func ClearFeaturesConfig(ctx context.Context, db *sql.DB) error {
	return DeleteSetting(ctx, db, SettingFeaturesConfig)
}

// SettingConsoleLevel is the settings key holding the console experience level
// the owner chose, plus whether they have seen the Home welcome card: a JSON
// object {"level":"basic|standard|advanced","source":"chosen|seeded",
// "welcomed":true}. When it carries a level, that level layers over the
// file/env base (config.Console.Level) exactly like the features and briefing
// overrides -- the row wins until reset.
//
// The level is PRESENTATION ONLY: it changes which console screens and knobs
// are shown, never what agents receive. Nothing outside the console reads it.
//
// Two writers reach this row: the console (Settings -> Experience, the Home
// welcome card), which stamps source "chosen", and the one-time grandfather
// migration, which seeds "advanced" with source "seeded" on an installation that
// already recorded sessions -- so the console can say "set by the upgrade"
// rather than crediting the owner with a choice they never made.
//
// The welcomed flag lives in the same row, server-side, because the level is a
// property of the installation's owner rather than of a browser: a phone must
// not re-ask what the laptop already answered. A row may therefore carry the
// flag with NO level (the owner dismissed the card without choosing), which is
// "welcomed, still following file/env".
const SettingConsoleLevel = "console_level"

// Console level sources: who set the stored level.
const (
	// ConsoleLevelChosen marks a level the owner picked in the console.
	ConsoleLevelChosen = "chosen"
	// ConsoleLevelSeeded marks a level the upgrade migration seeded.
	ConsoleLevelSeeded = "seeded"
)

// ConsoleLevelSources is the canonical source set, for validation and for the
// message that names the accepted values.
var ConsoleLevelSources = []string{ConsoleLevelChosen, ConsoleLevelSeeded}

// consoleLevelRow is the stored JSON shape. Every field is optional on read: a
// row that only records the welcome carries no level and overrides nothing.
type consoleLevelRow struct {
	Level    string `json:"level,omitempty"`
	Source   string `json:"source,omitempty"`
	Welcomed bool   `json:"welcomed"`
}

// ConsoleLevel returns the effective console level: the stored level when the
// row carries one (overridden true, source naming who set it), else base (the
// file/env value). welcomed reports whether the owner has seen the Home welcome
// card; an absent row is a fresh installation, which has not.
//
// A corrupt row -- undecodable JSON, or a level or source outside the canonical
// sets -- is an error that reports the base: the caller degrades to file/env and
// logs, so presentation state can never take the console down.
func ConsoleLevel(ctx context.Context, db *sql.DB, base string) (level string, overridden bool, source string, welcomed bool, err error) {
	row, found, err := consoleLevelRowTx(ctx, db)
	if err != nil {
		return base, false, "", false, err
	}
	if !found || row.Level == "" {
		return base, false, "", row.Welcomed, nil
	}
	return row.Level, true, row.Source, row.Welcomed, nil
}

// consoleLevelRowTx reads and validates the stored row via any executor. found
// is false when there is no row (or a blank one).
func consoleLevelRowTx(ctx context.Context, q settingsExecutor) (consoleLevelRow, bool, error) {
	raw, found, err := getSettingTx(ctx, q, SettingConsoleLevel)
	if err != nil {
		return consoleLevelRow{}, false, err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return consoleLevelRow{}, false, nil
	}
	var row consoleLevelRow
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		return consoleLevelRow{}, false, fmt.Errorf("store.ConsoleLevel: decode: %w", err)
	}
	if row.Level != "" && !slices.Contains(config.ConsoleLevels, row.Level) {
		return consoleLevelRow{}, false, fmt.Errorf("store.ConsoleLevel: stored level %q: valid values are %s",
			row.Level, strings.Join(config.ConsoleLevels, ", "))
	}
	if row.Source != "" && !slices.Contains(ConsoleLevelSources, row.Source) {
		return consoleLevelRow{}, false, fmt.Errorf("store.ConsoleLevel: stored source %q: valid values are %s",
			row.Source, strings.Join(ConsoleLevelSources, ", "))
	}
	return row, true, nil
}

// SetConsoleLevel stores level as the console override, stamped with source,
// and marks the row welcomed: choosing a level anywhere answers the welcome
// card. A level or source outside the canonical sets is rejected, never stored.
func SetConsoleLevel(ctx context.Context, db *sql.DB, level, source string) error {
	if !slices.Contains(config.ConsoleLevels, level) {
		return fmt.Errorf("store.SetConsoleLevel: invalid level %q: valid values are %s",
			level, strings.Join(config.ConsoleLevels, ", "))
	}
	if !slices.Contains(ConsoleLevelSources, source) {
		return fmt.Errorf("store.SetConsoleLevel: invalid source %q: valid values are %s",
			source, strings.Join(ConsoleLevelSources, ", "))
	}
	return writeConsoleLevelRow(ctx, db, consoleLevelRow{Level: level, Source: source, Welcomed: true})
}

// MarkConsoleWelcomed records that the owner has seen the Home welcome card,
// leaving any stored level untouched. With no row it writes a welcome-only row,
// which overrides nothing: the level keeps following file/env. The read and the
// write-back share one transaction (the family mutators' recipe), so a
// concurrent level change cannot be clobbered by a stale copy of the row.
func MarkConsoleWelcomed(ctx context.Context, db *sql.DB) error {
	return mutateConsoleLevelRow(ctx, db, "store.MarkConsoleWelcomed", func(row *consoleLevelRow) {
		row.Welcomed = true
	})
}

// ClearConsoleLevel removes the stored level, reverting the effective level to
// the file/env base. The welcome flag survives a reset -- resetting the level is
// not a request to be asked again -- so a welcomed row shrinks to a welcome-only
// row and an unwelcomed one is deleted outright. Clearing twice is a no-op.
//
// A corrupt row is deleted rather than reported: reset is the owner's way out of
// whatever state the row is in, so it must not depend on reading it.
func ClearConsoleLevel(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.ClearConsoleLevel: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	row, found, rerr := consoleLevelRowTx(ctx, tx)
	if rerr != nil || !found || !row.Welcomed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, SettingConsoleLevel); err != nil {
			return fmt.Errorf("store.ClearConsoleLevel: %w", err)
		}
	} else if err := writeConsoleLevelRowTx(ctx, tx, consoleLevelRow{Welcomed: true}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.ClearConsoleLevel: commit: %w", err)
	}
	return nil
}

// mutateConsoleLevelRow runs a read-modify-write of the row inside one
// transaction. A corrupt row is an error here (unlike Clear): a mutation that
// keeps the rest of the row must be able to read it.
func mutateConsoleLevelRow(ctx context.Context, db *sql.DB, op string, fn func(*consoleLevelRow)) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	row, _, err := consoleLevelRowTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	fn(&row)
	if err := writeConsoleLevelRowTx(ctx, tx, row); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}
	return nil
}

func writeConsoleLevelRow(ctx context.Context, db *sql.DB, row consoleLevelRow) error {
	return writeConsoleLevelRowTx(ctx, db, row)
}

func writeConsoleLevelRowTx(ctx context.Context, q settingsExecutor, row consoleLevelRow) error {
	raw, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("store.SetConsoleLevel: %w", err)
	}
	return setSettingTx(ctx, q, SettingConsoleLevel, string(raw))
}

// SettingEmbedderMode is the settings key holding the owner's embedder
// override. The only stored value is EmbedderModeOff; EmbedderModeAuto clears
// the row, so "auto" and "no override" are the same state. The daemon reads it
// once at serve start (main.go resolves the embedder before anything holds it),
// so a console change applies from the next restart.
const SettingEmbedderMode = "embedder_mode"

// Embedder override modes. Auto defers to the LLM config (embeddings run when a
// provider is configured); Off disables them regardless of config.
const (
	EmbedderModeAuto = "auto"
	EmbedderModeOff  = "off"
)

// EmbedderMode returns the stored embedder override: EmbedderModeOff when the
// owner switched embeddings off, EmbedderModeAuto otherwise. An unset row or an
// unrecognized stored value both read as auto -- the override can only ever
// narrow behavior, never invent a new state.
func EmbedderMode(ctx context.Context, db *sql.DB) (string, error) {
	raw, found, err := GetSetting(ctx, db, SettingEmbedderMode)
	if err != nil {
		return EmbedderModeAuto, err
	}
	if found && strings.TrimSpace(raw) == EmbedderModeOff {
		return EmbedderModeOff, nil
	}
	return EmbedderModeAuto, nil
}

// SetEmbedderMode persists the embedder override. Auto deletes the row (no
// override); Off stores it; anything else is rejected.
func SetEmbedderMode(ctx context.Context, db *sql.DB, mode string) error {
	switch mode {
	case EmbedderModeAuto:
		return DeleteSetting(ctx, db, SettingEmbedderMode)
	case EmbedderModeOff:
		return SetSetting(ctx, db, SettingEmbedderMode, EmbedderModeOff)
	default:
		return fmt.Errorf("store.SetEmbedderMode: invalid mode %q: valid values are %s, %s",
			mode, EmbedderModeAuto, EmbedderModeOff)
	}
}

// GetSetting returns the value for a settings key. found is false when unset.
func GetSetting(ctx context.Context, db *sql.DB, key string) (string, bool, error) {
	return getSettingTx(ctx, db, key)
}

// getSettingTx reads a settings key via any executor. found is false when unset.
func getSettingTx(ctx context.Context, q settingsExecutor, key string) (string, bool, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store.GetSetting: %w", err)
	}
	return v, true, nil
}

// SetSetting upserts a settings key/value.
func SetSetting(ctx context.Context, db *sql.DB, key, value string) error {
	return setSettingTx(ctx, db, key, value)
}

// setSettingTx upserts a settings key/value via any executor.
func setSettingTx(ctx context.Context, q settingsExecutor, key, value string) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store.SetSetting: %w", err)
	}
	return nil
}

// DeleteSetting removes a settings key. Deleting an absent key is a no-op.
func DeleteSetting(ctx context.Context, db *sql.DB, key string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store.DeleteSetting: %w", err)
	}
	return nil
}

// RepoProjectMap decodes the repo_project_map setting into a map. An unset or
// blank value yields an empty map (not an error), so an unconfigured install
// simply resolves every cwd to the global scope.
//
// Since host-scoped identity landed, this setting is a MIRROR of the local
// host's repo_map rows rather than the map itself (see repomap.go). It is kept
// in step with every local mutation, and the console, doctor and `map-repo`
// still read it; the resolvers read the table. Phase 2 moves those readers onto
// RepoMapRows and the mirror goes away.
func RepoProjectMap(ctx context.Context, db *sql.DB) (map[string]string, error) {
	return repoMapMirror(ctx, db)
}

// SettingUpdateConfig is the settings key holding the console's update
// override: a JSON-encoded config.UpdateOverride. Unlike the briefing and
// features rows it does not simply win over the file/env values: the merge
// (internal/update) lets the more restrictive setting win, so an explicit
// file/env false locks the console toggle off. This package only stores the
// row and hands it back.
const SettingUpdateConfig = "update_config"

// UpdateOverride returns the stored console update override and whether a row
// exists. A missing or blank row is (zero, false, nil). Unlike BriefingConfig it
// decodes over nothing: a field the row does not carry stays nil, "no override
// for that toggle", and the merge with the file/env values lives in
// internal/update.
//
// A corrupt row is an error. Callers log it and fall back to the file/env
// values -- failure-soft, like the briefing and features readers -- so a bad
// row can neither take a surface down nor switch update checks on.
func UpdateOverride(ctx context.Context, db *sql.DB) (config.UpdateOverride, bool, error) {
	raw, found, err := GetSetting(ctx, db, SettingUpdateConfig)
	if err != nil {
		return config.UpdateOverride{}, false, err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return config.UpdateOverride{}, false, nil
	}
	var o config.UpdateOverride
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return config.UpdateOverride{}, false, fmt.Errorf("store.UpdateOverride: decode: %w", err)
	}
	return o, true, nil
}

// SetUpdateOverride persists o as the console update override. A nil field is
// left out of the row and overrides nothing. Callers decide what may be
// stored; this only encodes and stores.
func SetUpdateOverride(ctx context.Context, db *sql.DB, o config.UpdateOverride) error {
	raw, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("store.SetUpdateOverride: %w", err)
	}
	return SetSetting(ctx, db, SettingUpdateConfig, string(raw))
}

// ClearUpdateOverride removes the console update override, handing the update
// settings back to file/env. Clearing twice is a no-op.
func ClearUpdateOverride(ctx context.Context, db *sql.DB) error {
	return DeleteSetting(ctx, db, SettingUpdateConfig)
}

// SettingBackupConfig is the settings key holding the console's backup
// settings override: a JSON-encoded config.BackupOverride. Like the briefing
// and features rows, and unlike update_config, a field it sets simply wins
// over the file/env value until the owner resets it (the merge lives in
// internal/backup). A restore keeps it as it is today: where and how often
// this install backs up is not part of the data a backup brings back.
const SettingBackupConfig = "backup_config"

// BackupOverride returns the stored console backup override and whether a row
// exists. A missing or blank row is (zero, false, nil). It decodes over
// nothing: a field the row does not carry stays nil, "no override for that
// setting".
//
// A row that does not decode, or holds a value config.BackupOverride.Validate
// refuses (a hand edit, or a newer release's wider bounds), is an error.
// Callers log it and fall back to the file/env values -- failure-soft, like
// the other override readers -- so a bad row can neither take a surface down
// nor hand the scheduler a keep of zero.
func BackupOverride(ctx context.Context, db *sql.DB) (config.BackupOverride, bool, error) {
	raw, found, err := GetSetting(ctx, db, SettingBackupConfig)
	if err != nil {
		return config.BackupOverride{}, false, err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return config.BackupOverride{}, false, nil
	}
	var o config.BackupOverride
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return config.BackupOverride{}, false, fmt.Errorf("store.BackupOverride: decode: %w", err)
	}
	if err := o.Validate(); err != nil {
		return config.BackupOverride{}, false, fmt.Errorf("store.BackupOverride: %w", err)
	}
	return o, true, nil
}

// SetBackupOverride persists o as the console backup override, refusing one
// config.BackupOverride.Validate refuses, so the row BackupOverride reads back
// is always usable. A nil field is left out of the row and overrides nothing.
func SetBackupOverride(ctx context.Context, db *sql.DB, o config.BackupOverride) error {
	if err := o.Validate(); err != nil {
		return fmt.Errorf("store.SetBackupOverride: %w", err)
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("store.SetBackupOverride: %w", err)
	}
	return SetSetting(ctx, db, SettingBackupConfig, string(raw))
}

// ClearBackupOverride removes the console backup override, handing the backup
// settings back to file/env. Clearing twice is a no-op.
func ClearBackupOverride(ctx context.Context, db *sql.DB) error {
	return DeleteSetting(ctx, db, SettingBackupConfig)
}
