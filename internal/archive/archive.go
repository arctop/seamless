// Package archive reads and writes Seamless instance archives: one gzipped tar
// carrying the markdown corpus, a consistent snapshot of seam.db, and a
// manifest describing what is inside.
//
// The archive is the whole instance minus its identity. Config and the MCP API
// key are never included -- a restored instance gets a new key and its own
// config, so an archive can be copied around without carrying a credential.
//
// Layout, in tar order:
//
//	manifest.json                     always FIRST, so a reader can refuse an
//	                                  archive before extracting a single byte
//	seam.db                           absent when exported with NoDB
//	memory/<project|_global>/<name>.md
//	notes/<project|_global>/<slug>.md
//
// Every entry is a regular file with mode 0600 and uid/gid zeroed: an archive
// restored as another user must not carry the exporting machine's ownership,
// and the corpus is owner-only on disk for the same reason the data dir is.
//
// The package imports core, events, files, store, and validate. It must NOT import
// internal/config: an archive is described by its manifest and its data dir
// argument, never by the config of whatever process happens to be holding it,
// which is what lets an export run against a data dir the caller names and an
// import target a directory that has no config yet.
package archive

import (
	"errors"
	"time"

	"github.com/arctop/seamless/internal/store"
)

// FormatVersion is the archive container version. It describes the tar layout
// and manifest shape, NOT the database schema: schema_version travels
// separately in the manifest because a v1 archive is written by every seamlessd
// regardless of how far its migrations have run.
const FormatVersion = 1

// The fixed entry names inside an archive. They are constants rather than
// literals at each site because the writer and the reader have to agree
// exactly, and a transcribed name drifts in silence (AGENTS.md).
const (
	// ManifestName is the first entry of every archive.
	ManifestName = "manifest.json"
	// DBName is the snapshot of seam.db, absent from a NoDB export.
	DBName = "seam.db"
	// MemoryTree and NotesTree are the two markdown trees carried verbatim.
	MemoryTree = "memory"
	NotesTree  = "notes"
)

// Sentinels. Each one names a distinct refusal, so callers can tell "this file
// is not one of ours" from "this file is ours and I am too old to read it"
// without matching on message text.
var (
	// ErrSchemaTooNew reports an archive whose schema_version exceeds this
	// binary's store.LatestSchemaVersion. Migrating forward cannot help: the
	// migrations that produced the snapshot are not in this build, so the only
	// remedy is a newer seamlessd.
	ErrSchemaTooNew = errors.New("archive was made by a newer seamlessd")

	// ErrUnsafeEntry reports a tar entry that must never be written to disk --
	// a symlink or hardlink, a non-regular file, a path escaping the
	// destination, or a name outside the archive layout. It also covers the
	// export side of the same rule: a memory/ or notes/ tree root that is not a
	// real directory is refused rather than silently exported as empty.
	ErrUnsafeEntry = errors.New("unsafe archive entry")

	// ErrNotArchive reports a file that is not a Seamless archive: not a
	// gzipped tar, or a tar whose first entry is not manifest.json.
	ErrNotArchive = errors.New("not a seamless archive")

	// ErrDaemonRunning reports that a daemon is answering on the target
	// instance's address. A fresh restore replaces seam.db underneath it, so it
	// refuses rather than corrupting a live instance. A merge that brings locks
	// refuses too: a daemon reads the locked projects when it starts.
	ErrDaemonRunning = errors.New("seamlessd is running against this data dir")

	// ErrLockCannotApply reports a merge that cannot keep a project locked here:
	// the project is open on this instance and has child projects, and a locked
	// project must be fenced, which a project with children cannot be.
	ErrLockCannotApply = errors.New("a lock in this archive cannot be applied here")
)

// The kinds of archive, recorded as Manifest.Kind: why one was written. An
// empty kind is a plain `seamlessd export`, and every archive written before
// the field existed.
const (
	// KindScheduled is an automatic backup the backup scheduler made.
	KindScheduled = "scheduled"
	// KindManual is a backup the owner asked for (Back up now).
	KindManual = "manual"
	// KindBeforeRestore is the safety backup of the data a restore replaced or
	// merged into.
	KindBeforeRestore = "before-restore"
	// KindPreUpdate is the updater's backup before it installs a release.
	KindPreUpdate = "pre-update"
)

// maxManifestProjects caps Manifest.Projects. The manifest is the first entry
// and is read whole (maxManifestBytes), so an instance with a very large number
// of projects records its largest ones and counts the rest in ProjectsOmitted,
// rather than writing a manifest no release could read.
const maxManifestProjects = 1000

// ProjectCounts is what one project held when the archive was written, so a
// list of backups can say what each one would bring back.
type ProjectCounts struct {
	// Slug is the project, "" for the global scope.
	Slug string `json:"slug"`
	// Name is the project's display name, when it differs from the slug.
	Name string `json:"name,omitempty"`
	// Memories are the active ones; Retired the superseded or archived ones
	// still on disk as tombstones. An archive without a database counts every
	// memory file as active: it has no index to tell them apart.
	Memories int `json:"memories"`
	Retired  int `json:"retired,omitempty"`
	Notes    int `json:"notes"`
	Tasks    int `json:"tasks,omitempty"`
	Sessions int `json:"sessions,omitempty"`
	Trials   int `json:"trials,omitempty"`
}

// total is everything the project holds, for ordering.
func (p ProjectCounts) total() int {
	return p.Memories + p.Retired + p.Notes + p.Tasks + p.Sessions + p.Trials
}

// Counts is the manifest's inventory: what a reader should expect to find, so a
// truncated or hand-edited archive is detectable before it is trusted.
type Counts struct {
	// MemoryFiles and NoteFiles count the .md entries in each tree.
	MemoryFiles int `json:"memory_files"`
	NoteFiles   int `json:"note_files"`

	// Tables maps table name to row count in the seam.db snapshot, read from
	// the snapshot after it verified rather than from the live database, so the
	// numbers describe the bytes actually in the archive. Empty for a NoDB
	// export. Enumerated from sqlite_master rather than a hand-written list, so
	// a table added by a later migration appears without anyone remembering to
	// add it here.
	Tables map[string]int `json:"tables,omitempty"`
}

// Manifest is manifest.json, the first entry of every archive.
type Manifest struct {
	// FormatVersion is the container version (FormatVersion).
	FormatVersion int `json:"format_version"`

	// SchemaVersion is the migration version of the snapshot, 0 for a NoDB
	// export. A value above the reader's store.LatestSchemaVersion is
	// ErrSchemaTooNew; anything at or below it migrates forward on import.
	SchemaVersion int `json:"schema_version"`

	// SeamlessdVersion is the build that wrote the archive, supplied by the
	// caller: the version is linked into the binary, and this package sits
	// below cmd/.
	SeamlessdVersion string `json:"seamlessd_version"`

	CreatedAt  time.Time `json:"created_at"`
	SourceHost string    `json:"source_host"`

	// IncludesDB is false for a knowledge-only (NoDB) export, where the tar
	// carries the two markdown trees and nothing else.
	IncludesDB bool `json:"includes_db"`

	// EmbeddingModels is DISTINCT model FROM embeddings in the snapshot. The
	// importer compares it with the destination's configured embedder: vectors
	// from another model are not comparable, so a mismatch is the signal to
	// re-embed rather than to trust the imported rows.
	EmbeddingModels []string `json:"embedding_models,omitempty"`

	Counts Counts `json:"counts"`

	// Kind says why the archive was written (KindScheduled, KindManual,
	// KindBeforeRestore, KindPreUpdate); empty for a plain export.
	Kind string `json:"kind,omitempty"`

	// InstallID names the install that wrote the archive (store.InstallID), so
	// a folder several installs share -- even two machines with the same
	// hostname -- prunes only its own backups. Empty when the writer named none.
	InstallID string `json:"install_id,omitempty"`

	// Projects is what each project held, largest first, at most
	// maxManifestProjects of them; ProjectsOmitted counts the rest. Counted from
	// the snapshot's index when the archive carries a database, else from the
	// markdown trees. Empty in an archive written before the field existed.
	Projects        []ProjectCounts `json:"projects,omitempty"`
	ProjectsOmitted int             `json:"projects_omitted,omitempty"`

	// Fingerprint is store.ContentFingerprint of the snapshot: a later backup
	// whose database has the same fingerprint would capture nothing new. Empty
	// for an archive without a database.
	Fingerprint string `json:"fingerprint,omitempty"`

	// Locks is what the import needs to keep this instance's locked projects
	// locked. Nil when none was locked, so an archive without locks reads as it
	// always has. It is here, and not only in the snapshot, because a lock can
	// live where the snapshot cannot see it: in the exporting machine's config
	// (peer_auth.force_locked), or in a database a --no-db export leaves out.
	Locks *Locks `json:"locks,omitempty"`

	// LocksError says why a --no-db export could not read the console's locks.
	// Such an export is how knowledge gets out of a damaged database, so it goes
	// ahead, and both ends say what it could not know.
	LocksError string `json:"locks_error,omitempty"`
}

// LockedProject is one project that was locked when the archive was written.
type LockedProject struct {
	Slug string `json:"slug"`
	// Isolation is the project's fence. A lock needs one, and a --no-db archive
	// carries no projects table to read it from. Empty when the exporter had no
	// database to ask.
	Isolation string `json:"isolation,omitempty"`
}

// Locks is the manifest's record of the locked projects and of who could open
// them. It names passkeys and never holds key material.
type Locks struct {
	// Projects are the locked projects, sorted by slug: the console's locks and
	// the exporting config's force locks alike.
	Projects []LockedProject `json:"projects"`
	// PasskeyRPID is the relying party the passkeys below were enrolled for: a
	// passkey signs for that one and no other. Empty when the exporter did not
	// know it.
	PasskeyRPID string `json:"passkey_rp_id,omitempty"`
	// Passkeys are the passkeys enrolled when the archive was written.
	Passkeys []store.PasskeyFact `json:"passkeys,omitempty"`
}

// Slugs returns the locked projects' slugs.
func (l Locks) Slugs() []string {
	out := make([]string, len(l.Projects))
	for i, p := range l.Projects {
		out[i] = p.Slug
	}
	return out
}
