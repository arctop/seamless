package store

// Holders (migration 039): who an access approval, a grant, a move or a flow
// belongs to. The holder is the conversation, in the one form the request's
// transport vouches for -- the agent's kernel-verified process tree under
// peer_auth, the client's own session without it, or, for a client without
// hooks, the session it is bound to. Every row names exactly one, and every
// lookup by holder goes through one encoding (Holder.Key) and one pair of SQL
// clauses, so no two readers can disagree about whose a row is.
//
// The store only stores and lists. Who the holder of a request is at call time
// -- which process chain, which conversation, which session -- is the caller's
// to resolve (internal/access judges it), and nothing here infers one.

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// Holder kinds.
const (
	// HolderProcess is an agent process tree under peer_auth: its key is
	// access.ProcessToken.Key of the process a request was anchored to, which
	// every process it started shares.
	HolderProcess = "process"
	// HolderConversation is a client's own session without peer_auth: the
	// client (sessions.external_client) and the session id it issued
	// (sessions.claude_session_id). It is found from the agent-process
	// identity, never from the connection's binding, so neither session_start,
	// session= nor a move changes it.
	HolderConversation = "conversation"
	// HolderSession is the session of a client that sends no agent-process
	// identity: its ULID, today's advisory holder.
	HolderSession = "session"
)

// HolderKinds is the canonical holder kind set.
var HolderKinds = []string{HolderProcess, HolderConversation, HolderSession}

const (
	// holderMaxClientBytes caps a conversation's client name.
	holderMaxClientBytes = 64
	// holderMaxLookup bounds how many holders one lookup names: a process
	// chain is the longest list a request carries.
	holderMaxLookup = access.MaxChainHops
)

// Holder is who an access row belongs to, built only by HolderOfProcess,
// HolderOfConversation, HolderOfSession or HolderFromKey (or derived from a
// row). The zero Holder is no holder at all: it holds nothing, and a lookup that
// names only zero holders matches nothing.
type Holder struct {
	kind string
	// one is the process key, the conversation's client, or the session id;
	// two is the conversation's external session id.
	one, two string
}

// HolderOfProcess is the holder of the agent process tree whose anchoring
// process has this access.ProcessToken.Key (peer_auth).
func HolderOfProcess(key string) (Holder, error) {
	if err := holderToken("process key", key, maxAccessFieldBytes); err != nil {
		return Holder{}, err
	}
	return Holder{kind: HolderProcess, one: key}, nil
}

// HolderOfConversation is the holder of a conversation without peer_auth: the
// agent client and the session id it issued, as the sessions table records
// them (a restricted account's id already qualified). The client never holds
// "/", which ends it in the key.
func HolderOfConversation(client, externalID string) (Holder, error) {
	if err := holderToken("conversation client", client, holderMaxClientBytes); err != nil {
		return Holder{}, err
	}
	if strings.Contains(client, "/") {
		return Holder{}, fmt.Errorf("%w: conversation client %q holds a slash", ErrInvalidAccessRequest, client)
	}
	if err := holderToken("conversation id", externalID, maxAccessFieldBytes); err != nil {
		return Holder{}, err
	}
	return Holder{kind: HolderConversation, one: client, two: externalID}, nil
}

// HolderOfSession is the holder of a session whose client sends no
// agent-process identity.
func HolderOfSession(id string) (Holder, error) {
	if err := holderToken("session id", id, maxAccessFieldBytes); err != nil {
		return Holder{}, err
	}
	return Holder{kind: HolderSession, one: id}, nil
}

// HolderFromKey parses a key Holder.Key produced: "process:<key>",
// "conversation:<client>/<external id>" or "session:<id>".
func HolderFromKey(key string) (Holder, error) {
	kind, rest, ok := strings.Cut(key, ":")
	if !ok {
		return Holder{}, fmt.Errorf("%w: holder %q names no kind", ErrInvalidAccessRequest, key)
	}
	switch kind {
	case HolderProcess:
		return HolderOfProcess(rest)
	case HolderConversation:
		client, id, ok := strings.Cut(rest, "/")
		if !ok {
			return Holder{}, fmt.Errorf("%w: conversation holder %q names no client", ErrInvalidAccessRequest, key)
		}
		return HolderOfConversation(client, id)
	case HolderSession:
		return HolderOfSession(rest)
	}
	return Holder{}, fmt.Errorf("%w: holder kind %q: valid values are %s", ErrInvalidAccessRequest, kind, strings.Join(HolderKinds, ", "))
}

// holderToken checks one part of a holder: present, valid UTF-8, at most max
// bytes, and free of spaces and control characters, so a key is one token.
func holderToken(what, v string, max int) error {
	switch {
	case v == "":
		return fmt.Errorf("%w: no %s", ErrInvalidAccessRequest, what)
	case len(v) > max:
		return fmt.Errorf("%w: %s is longer than %d bytes", ErrInvalidAccessRequest, what, max)
	case !utf8.ValidString(v):
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidAccessRequest, what)
	}
	if i := strings.IndexFunc(v, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }); i >= 0 {
		return fmt.Errorf("%w: %s holds a space or a control character", ErrInvalidAccessRequest, what)
	}
	return nil
}

// Kind is HolderProcess, HolderConversation, HolderSession, or "" for none.
func (h Holder) Kind() string { return h.kind }

// IsZero reports that h names no holder.
func (h Holder) IsZero() bool { return h.kind == "" }

// Key is the holder's one encoding, the value of every holder column; "" for
// none. Two holders are the same holder exactly when their keys are equal.
func (h Holder) Key() string {
	switch h.kind {
	case HolderConversation:
		return h.kind + ":" + h.one + "/" + h.two
	case "":
		return ""
	}
	return h.kind + ":" + h.one
}

// String is Key.
func (h Holder) String() string { return h.Key() }

// ProcessKey is a process holder's access.ProcessToken.Key, "" for any other.
func (h Holder) ProcessKey() string {
	if h.kind != HolderProcess {
		return ""
	}
	return h.one
}

// Conversation is a conversation holder's client and external session id, ""
// for any other.
func (h Holder) Conversation() (client, externalID string) {
	if h.kind != HolderConversation {
		return "", ""
	}
	return h.one, h.two
}

// SessionID is a session holder's session id, "" for any other. (A process or
// conversation holder's rows still name the session that asked; that session
// is not their holder.)
func (h Holder) SessionID() string {
	if h.kind != HolderSession {
		return ""
	}
	return h.one
}

// holderOfRow derives the holder of an access_approvals or access_grants row
// from its columns, in the one order: the process when one is named, else the
// conversation, else the session. A row the previous release wrote names no
// conversation, so it keeps the holder it meant. A row whose holder does not
// parse holds nothing.
func holderOfRow(processKey, client, externalID, sessionID string) Holder {
	var (
		h   Holder
		err error
	)
	switch {
	case processKey != "":
		h, err = HolderOfProcess(processKey)
	case client != "" || externalID != "":
		h, err = HolderOfConversation(client, externalID)
	case sessionID != "":
		h, err = HolderOfSession(sessionID)
	}
	if err != nil {
		return Holder{}
	}
	return h
}

// holderLookup returns the distinct holders a lookup names, the zero ones
// dropped, refusing more than holderMaxLookup: a longer list did not come from a
// request.
func holderLookup(holders []Holder) ([]Holder, error) {
	var out []Holder
	for _, h := range holders {
		if !h.IsZero() && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	if len(out) > holderMaxLookup {
		return nil, fmt.Errorf("%w: %d holders, at most %d", ErrInvalidAccessRequest, len(out), holderMaxLookup)
	}
	return out, nil
}

// holderKeyClause matches the rows of a table with a holder column (the tables
// of migration 039) held by any of holders. ok is false when there is no holder
// at all, which matches nothing.
func holderKeyClause(column string, holders []Holder) (string, []any, bool, error) {
	hs, err := holderLookup(holders)
	if err != nil || len(hs) == 0 {
		return "", nil, false, err
	}
	args := make([]any, len(hs))
	for i, h := range hs {
		args[i] = h.Key()
	}
	return column + ` IN (` + placeholders(len(hs)) + `)`, args, true, nil
}

// holderAccessClause matches the access_approvals or access_grants rows held by
// any of holders, by the order holderOfRow derives: a process by process_key; a
// conversation by its two columns, on a row that names no process; a session by
// session_id, on a row that names neither a process nor a conversation -- a
// conversation's row still names the session that asked, and that session is
// not its holder. ok is false when there is no holder at all.
func holderAccessClause(holders []Holder) (string, []any, bool, error) {
	hs, err := holderLookup(holders)
	if err != nil || len(hs) == 0 {
		return "", nil, false, err
	}
	var (
		parts []string
		args  []any
		procs []any
	)
	for _, h := range hs {
		switch h.kind {
		case HolderProcess:
			procs = append(procs, h.one)
		case HolderConversation:
			// conversation_id <> '' is always true here; it lets SQLite use the
			// partial conversation indexes, which a bound value cannot prove.
			parts = append(parts, `(process_key = '' AND conversation_client = ? AND conversation_id = ? AND conversation_id <> '')`)
			args = append(args, h.one, h.two)
		case HolderSession:
			parts = append(parts, `(process_key = '' AND conversation_id = '' AND session_id = ?)`)
			args = append(args, h.one)
		}
	}
	if len(procs) > 0 {
		parts = append([]string{`process_key IN (` + placeholders(len(procs)) + `)`}, parts...)
		args = append(procs, args...)
	}
	return `(` + strings.Join(parts, ` OR `) + `)`, args, true, nil
}

// HolderLabelEntry is one fenced project in a holder's label: a confidential
// or sealed project whose content the holder may hold. It says why: the project
// is or was the holder's home, a target of its grants, or a project one of its
// moves left or entered. A label only grows: an ended grant, an expired lease or
// a restore never takes an entry out.
type HolderLabelEntry struct {
	Project string
	// State is the isolation that fences it now (store.IsolatedSlugs).
	State core.Isolation
	// Home reports that it is or was the holder's home: the home the caller
	// named, a project one of its grants was asked from, a project one of its
	// moves left or entered, or the project of one of its sessions.
	Home bool
	// Grants are the ids of the holder's grants (live or ended) whose target it
	// is, oldest first; Moves the ids of its moves into or out of it.
	Grants, Moves []string
}

// HolderLabel returns the label of the conversation holders name (the process
// chain under peer_auth, else one conversation or one session): every project
// it is, or was, homed in, every target of a grant it was ever approved, and
// every project it moved through, keeping the fenced ones (IsolatedSlugs, as
// they stand now), sorted by project. home is the conversation's home now, as
// the caller resolved it ("" for none or the global scope, which is never
// fenced). A conversation holder's sessions -- its ambient row, the explicit
// sessions that linked to it and a move's children -- all count as homes.
//
// The label is the input to the one rule (a write's destination must be
// cleared for everything the conversation holds); this function judges
// nothing.
func HolderLabel(ctx context.Context, db *sql.DB, holders []Holder, home string) ([]HolderLabelEntry, error) {
	const op = "store.HolderLabel"
	hs, err := holderLookup(holders)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	entries := map[string]*HolderLabelEntry{}
	entry := func(slug string) *HolderLabelEntry {
		e, ok := entries[slug]
		if !ok {
			e = &HolderLabelEntry{Project: slug}
			entries[slug] = e
		}
		return e
	}
	if home != "" {
		entry(home).Home = true
	}
	grants, err := HolderAccessGrants(ctx, db, hs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	// Oldest first, so a project's grant ids read in the order they came.
	for _, g := range slices.Backward(grants) {
		t := entry(g.Project)
		t.Grants = append(t.Grants, g.ID)
		entry(g.FromProject).Home = true
	}
	moves, err := MovesForHolder(ctx, db, hs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	for _, m := range moves {
		for _, slug := range []string{m.FromProject, m.ToProject} {
			if slug == "" {
				continue
			}
			e := entry(slug)
			e.Home = true
			e.Moves = append(e.Moves, m.ID)
		}
	}
	homes, err := holderSessionProjects(ctx, db, hs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	for _, slug := range homes {
		entry(slug).Home = true
	}
	delete(entries, "")
	fenced, err := IsolatedSlugs(ctx, db, slices.Sorted(maps.Keys(entries)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	out := make([]HolderLabelEntry, 0, len(fenced))
	for _, f := range fenced {
		e := *entries[f.Slug]
		e.State = f.State
		out = append(out, e)
	}
	return out, nil
}

// holderSessionProjects returns the projects of the holders' own sessions,
// distinct: for a conversation every session with its identity (any status,
// ambient or not), for a session holder that session. A process tree's sessions
// are not recorded against its key, so a process holder contributes none: its
// caller names the home.
func holderSessionProjects(ctx context.Context, db *sql.DB, holders []Holder) ([]string, error) {
	var (
		parts []string
		args  []any
	)
	for _, h := range holders {
		switch h.kind {
		case HolderConversation:
			parts = append(parts, `(external_client = ? AND claude_session_id = ?)`)
			args = append(args, h.one, h.two)
		case HolderSession:
			parts = append(parts, `id = ?`)
			args = append(args, h.one)
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT project_slug FROM sessions WHERE `+strings.Join(parts, ` OR `)+`
		ORDER BY project_slug`, args...)
	if err != nil {
		return nil, fmt.Errorf("sessions of the holder: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("sessions of the holder: %w", err)
		}
		out = append(out, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessions of the holder: %w", err)
	}
	return out, nil
}

// HolderAccessApprovals returns every request any of holders filed -- every
// kind, every status -- newest first, at most maxGrantRows, by the holder each
// row names (AccessApproval.Holder). With no holder it returns none.
func HolderAccessApprovals(ctx context.Context, db *sql.DB, holders []Holder) ([]AccessApproval, error) {
	clause, args, ok, err := holderAccessClause(holders)
	if err != nil {
		return nil, fmt.Errorf("store.HolderAccessApprovals: %w", err)
	}
	if !ok {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+approvalCols+` FROM access_approvals WHERE `+clause+`
		ORDER BY requested_at DESC, id DESC LIMIT `+fmt.Sprint(maxGrantRows), args...)
	if err != nil {
		return nil, fmt.Errorf("store.HolderAccessApprovals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AccessApproval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("store.HolderAccessApprovals: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.HolderAccessApprovals: %w", err)
	}
	return out, nil
}
