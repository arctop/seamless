// The Diary fixture's story (diary.go replays it): three generic projects, the
// plans and loose tasks their agents work, what they keep, and the sessions of
// each day. All of it is fictional and generic (example-* projects, /home/dev
// paths). Times are local clock times on the day a session lands on; offsets
// inside a session are minutes after its start.

package demokit

import "github.com/arctop/seamless/internal/core"

// The fixture's projects.
var diaryProjects = []struct{ slug, name, desc string }{
	{"example-api", "example-api", "Public HTTP API: accounts, search and rate limits"},
	{"example-web", "example-web", "Web app for the public API"},
	{"example-ops", "example-ops", "Infrastructure: backups, CI runners and certificates"},
}

// The plans the story works, by slug.
const (
	// planRateLimits ships: its last step closes on workday 6.
	planRateLimits = "rate-limits"
	// planSearchV2 was planned in Claude Code's plan mode (a captured,
	// approved cc-plan note) and is still in flight today.
	planSearchV2 = "search-v2"
	// planDependencyRefresh is the same slug in two projects: its narrative
	// note is example-web's, and example-api holds steps of it too.
	planDependencyRefresh = "dependency-refresh"
	// planBackupRotation is a task-only plan: steps, and no note anywhere
	// carrying its tag.
	planBackupRotation = "backup-rotation"
)

// dTask is one task the story creates. Its first tasks_add act creates it;
// deps name other tasks by key.
type dTask struct {
	key, project, plan, title, body string
	deps                            []string
}

var diaryTasks = []dTask{
	{key: "rl1", project: "example-api", plan: planRateLimits, title: "Token-bucket limiter middleware for the /v1 routes",
		body: "One bucket per API key, in front of every /v1 route. Defaults are in the plan note."},
	{key: "rl2", project: "example-api", plan: planRateLimits, title: "Return Retry-After and the X-RateLimit headers on 429",
		body: "Retry-After in seconds; X-RateLimit-Limit, -Remaining and -Reset on every limited response.", deps: []string{"rl1"}},
	{key: "rl3", project: "example-api", plan: planRateLimits, title: "Per-key limits from the accounts table",
		body: "Plan defaults with per-key overrides, cached for 60 seconds.", deps: []string{"rl2"}},
	{key: "rl4", project: "example-api", plan: planRateLimits, title: "Load test the limiter at 5x traffic",
		body: "Replay 5x production traffic on staging; p99 must stay within 5ms of the baseline.", deps: []string{"rl3"}},
	{key: "rl5", project: "example-api", plan: planRateLimits, title: "Document the limits in the public API reference",
		body: "The limits table, the headers and an example 429 body.", deps: []string{"rl4"}},

	{key: "sv1", project: "example-api", plan: planSearchV2, title: "Add the search_documents table and the backfill job"},
	{key: "sv2", project: "example-api", plan: planSearchV2, title: "Query parser with field filters and date ranges", deps: []string{"sv1"}},
	{key: "sv3", project: "example-api", plan: planSearchV2, title: "BM25 ranking with field boosts", deps: []string{"sv2"}},
	{key: "sv4", project: "example-api", plan: planSearchV2, title: "Search endpoint with cursor pagination", deps: []string{"sv3"}},
	{key: "sv5", project: "example-api", plan: planSearchV2, title: "Highlight matched terms in results", deps: []string{"sv4"}},
	{key: "sv6", project: "example-api", plan: planSearchV2, title: "Load test search at 2x traffic", deps: []string{"sv5"}},

	{key: "dr-w1", project: "example-web", plan: planDependencyRefresh, title: "Upgrade the bundler to v7"},
	{key: "dr-w2", project: "example-web", plan: planDependencyRefresh, title: "Replace the date library with the Temporal polyfill", deps: []string{"dr-w1"}},
	{key: "dr-w3", project: "example-web", plan: planDependencyRefresh, title: "Upgrade the test runner to v3", deps: []string{"dr-w2"}},
	{key: "dr-a1", project: "example-api", plan: planDependencyRefresh, title: "Upgrade the Postgres driver to v6"},
	{key: "dr-a2", project: "example-api", plan: planDependencyRefresh, title: "Bump the Go toolchain to the current release", deps: []string{"dr-a1"}},

	{key: "br1", project: "example-ops", plan: planBackupRotation, title: "Label nightly snapshots with a retention class"},
	{key: "br2", project: "example-ops", plan: planBackupRotation, title: "Prune snapshots older than 14 days, staging first", deps: []string{"br1"}},
	{key: "br3", project: "example-ops", plan: planBackupRotation, title: "Alert when the newest snapshot is older than 26 hours", deps: []string{"br2"}},

	{key: "web-focus", project: "example-web", title: "Fix the focus ring on the settings form"},
	{key: "ops-drain", project: "example-ops", title: "Drain the old build runner",
		body: "Move its jobs to the two new runners, then take it out of the pool."},
	{key: "ops-tls", project: "example-ops", title: "Renew the staging TLS certificate"},
	{key: "api-429-log", project: "example-api", title: "Log the API key id on 429 responses",
		body: "Support asks which key hit the limit; the access log only has the client address."},
	{key: "web-contrast", project: "example-web", title: "Dark-mode contrast for the data tables"},
	{key: "web-flaky", project: "example-web", title: "Flaky visual test on the billing page"},
	{key: "ops-cred", project: "example-ops", title: "Rotate the object-store credential for backups"},
}

// diaryDeletedTask is the task whose row the fixture removes once its history
// is written: "a task deleted after it closed".
const diaryDeletedTask = "ops-drain"

// dMemory is one memory the story writes; key is its name.
type dMemory struct {
	key, project string
	kind         core.MemoryKind
	desc, body   string
}

var diaryMemories = []dMemory{
	{"api-errors-are-problem-json", "example-api", core.KindConstraint,
		"Every API error is an application/problem+json body with type, title and status; never a bare string.",
		"Clients parse errors by type. A handler that writes a plain-text error breaks the SDKs' retry logic. Use the problem helper in internal/httpx: it sets the content type and the status together."},
	{"ops-changes-through-ci", "example-ops", core.KindConstraint,
		"Infrastructure changes land through the CI pipeline; no hand edits on the runners or the backup host.",
		"A hand edit is lost at the next provision and nobody can review it. Change the provisioning scripts and let the pipeline apply them; for an emergency, open the change as a pull request right after."},
	{"focus-ring-outline-offset", "example-web", core.KindGotcha,
		"Cards clip focus rings (overflow: hidden); use outline-offset: -2px inside a card, never remove the outline.",
		"The settings card sets overflow: hidden for its rounded corners, which cut the focus ring off. An inset outline keeps it visible. Removing the outline fails keyboard users and the accessibility check."},
	{"rate-limit-window-60s", "example-api", core.KindDecision,
		"Rate limits use a fixed 60-second window per API key, reset on the minute.",
		"A fixed window is the simplest thing that works for the first cut. Revisit if bursts at the minute boundary show up in the load test."},
	{"prune-targets-staging-first", "example-ops", core.KindConstraint,
		"Run snapshot prunes against staging first; prune production only after the staging run is checked.",
		"A prune deletes snapshots for good. Run it on staging, compare what it removed with the retention labels, and only then run it on production, with --dry-run first."},
	{"limiter-lock-contention", "example-api", core.KindGotcha,
		"One mutex around the bucket map costs 40ms at p99 under 5x load; shard the buckets by key hash.",
		"Every request takes the map lock to find its bucket, so the lock serializes the whole API under load. 64 shards keyed by a hash of the API key bring the cost under 3ms."},
	{"rate-limit-sliding-window", "example-api", core.KindDecision,
		"Rate limits use a sliding 60-second window per API key over 64 sharded buckets; a fixed window let bursts through.",
		"The load test showed clients doubling their rate across the minute boundary of the fixed window. A sliding window over the last 60 seconds removes that, and the sharded buckets keep the cost under 3ms at p99."},
	{"renew-staging-certificate", "example-ops", core.KindRunbook,
		"Renew the staging certificate: issue with the ACME job, check the chain, reload the proxy, confirm the expiry date.",
		"1. Run the acme-issue job for staging.\n2. Check the chain with openssl s_client against the staging host.\n3. Reload the proxy (no restart needed).\n4. Confirm the new expiry date on the status page."},
	{"temporal-for-dates", "example-web", core.KindDecision,
		"Dates go through the Temporal polyfill; no new imports of the old date library.",
		"The old date library is unmaintained and mutable. The polyfill follows the standard, so the code moves to the native API with no rewrite once browsers ship it."},
	{"visual-tests-wait-for-fonts", "example-web", core.KindGotcha,
		"Visual tests must wait for document.fonts.ready before the screenshot, or text renders in the fallback font.",
		"The web fonts load from a CDN; on a cold CI runner they arrive after the first paint, and the screenshot catches the fallback font. Await document.fonts.ready in the test helper."},
	{"search-backfill-batch-commits", "example-api", core.KindGotcha,
		"The search backfill must commit in batches of 500; one transaction over the whole table holds locks for minutes.",
		"The first backfill ran in one transaction and blocked writes to accounts for four minutes on staging. Batches of 500 rows keep each lock short."},
	{"backup-credential-expiry", "example-ops", core.KindGotcha,
		"The object-store credential for backups expires every 90 days and nothing warned; the backup job now checks it.",
		"The nightly backup failed when the credential expired. The job now reads the credential's expiry at start and fails loudly two weeks ahead."},
}

// dNote is one note the story writes; key is its slug. A capture names the
// Claude Code plan file a captured plan note (cc-plan-<basename>) came from.
type dNote struct {
	key, project, title, desc, body string
	tags                            []string
	capture                         *dCapture
}

// dCapture is a captured plan's plan-mode history: its file's basename, the
// plan slug its H1 becomes, and the plan text of each iteration.
type dCapture struct {
	basename, plan string
	iterations     []string
}

var diaryNotes = []dNote{
	{key: "plan-rate-limits", project: "example-api", title: "Plan: rate limits for the public API",
		desc: "Plan narrative for plan:rate-limits: per-key token buckets, 429 headers, limits from the accounts table, a load test, docs.",
		tags: []string{"plan:" + planRateLimits},
		body: "# Plan: rate limits for the public API\n\n" +
			"Every /v1 route gets a limit per API key, so one noisy client cannot slow the API for the rest.\n\n" +
			"## Design\n\n" +
			"- A token bucket per API key, refilled every second.\n" +
			"- Defaults per plan (free 60/min, team 600/min), with per-key overrides from the accounts table.\n" +
			"- A limited request gets 429 with a problem+json body, Retry-After and the X-RateLimit headers.\n\n" +
			"## Steps\n\n" +
			"1. Limiter middleware.\n2. The 429 headers.\n3. Limits from the accounts table.\n4. A load test at 5x traffic.\n5. The public API reference.\n"},
	{key: "plan-dependency-refresh", project: "example-web", title: "Plan: dependency refresh",
		desc: "Plan narrative for plan:dependency-refresh: the bundler, the date library and the test runner in the web app; the Postgres driver and the Go toolchain in the API.",
		tags: []string{"plan:" + planDependencyRefresh},
		body: "# Plan: dependency refresh\n\n" +
			"Five dependencies are two majors behind. Each upgrade is its own step so a regression points at one change.\n\n" +
			"## example-web\n\n1. The bundler to v7.\n2. The date library to the Temporal polyfill.\n3. The test runner to v3.\n\n" +
			"## example-api\n\n1. The Postgres driver to v6.\n2. The Go toolchain to the current release.\n"},
	{key: "rate-limit-load-test-results", project: "example-api", title: "Rate limit load test results",
		desc: "Two load tests of the limiter at 5x traffic: one mutex (p99 +40ms) and 64 sharded buckets (p99 +3ms).",
		tags: []string{"plan:" + planRateLimits, "research"},
		body: "# Rate limit load test results\n\n" +
			"| Run | Buckets | p99 change | Throughput |\n|---|---|---|---|\n" +
			"| 1 | one map, one mutex | +40ms | 5,200 rps |\n| 2 | 64 shards | +3ms | 5,200 rps |\n\n" +
			"The profile of run 1 put the time in the map lock. Sharding by a hash of the API key removed it.\n"},
	{key: "shell-snippets-flaky-tests", project: "", title: "Shell snippets for chasing flaky tests",
		desc: "Shell one-liners for finding and bisecting flaky tests: repeat runs, race detector, shuffled order.",
		tags: []string{"snippets"},
		body: "# Shell snippets for chasing flaky tests\n\n" +
			"- Repeat one test until it fails: `go test -run TestName -count=200 -failfast ./pkg/`\n" +
			"- Shuffle the order: `go test -shuffle=on ./...`\n" +
			"- Race detector on the suspect package: `go test -race -count=20 ./pkg/`\n" +
			"- Bisect a regression: `git bisect run go test -run TestName ./pkg/`\n"},
	{key: "cc-plan-steady-humming-otter", project: "example-api", title: "Search v2",
		capture: &dCapture{basename: "steady-humming-otter", plan: planSearchV2, iterations: []string{
			"# Search v2\n\nFull-text search over accounts and documents, with field filters and ranking.\n\n" +
				"## Approach\n\n- A search_documents table with a tsvector column.\n- BM25-style ranking.\n- A new /v1/search endpoint.\n",
			"# Search v2\n\nFull-text search over accounts and documents, with field filters and ranking.\n\n" +
				"## Steps\n\n" +
				"1. The search_documents table and a backfill job (batched).\n" +
				"2. A query parser: field filters, quoted phrases, date ranges.\n" +
				"3. BM25 ranking with field boosts (title x3).\n" +
				"4. The /v1/search endpoint with cursor pagination.\n" +
				"5. Highlighted matches in results.\n" +
				"6. A load test at 2x traffic.\n\n" +
				"## Out of scope\n\nEmbeddings: revisit if relevance asks for it.\n",
		}}},
	{key: "search-ranking-bm25-vs-embeddings", project: "example-api", title: "Search ranking: BM25 or embeddings",
		desc: "BM25 with field boosts beats embeddings on latency for search v2; embeddings later if relevance asks for it.",
		tags: []string{"plan:" + planSearchV2, "research"},
		body: "# Search ranking: BM25 or embeddings\n\n" +
			"BM25 with field boosts answers in 95ms warm over 1M documents; the embedding prototype needs 300ms and a vector index we do not run.\n\n" +
			"Relevance on the 120 labeled queries: BM25 0.71, embeddings 0.74. Not worth the cost yet.\n"},
	{key: "incident-nightly-backup-failed", project: "example-ops", title: "Incident: the nightly backup failed",
		desc: "The nightly backup failed when the object-store credential expired; rotated it, re-ran the backup, added an expiry check.",
		tags: []string{"incident"},
		body: "# Incident: the nightly backup failed\n\n" +
			"- 22:40 the backup job failed with 403 from the object store.\n" +
			"- Root cause: the credential expired at 22:00 (90-day lifetime, no warning).\n" +
			"- Fix: rotated the credential, re-ran the backup (green), added an expiry check at job start.\n" +
			"- Follow-up: none open; the check warns two weeks ahead.\n"},
}

// dTrial is one research trial the story records.
type dTrial struct {
	key, project, lab, title, changes, expected, actual string
	outcome                                             core.TrialOutcome
	metrics                                             map[string]any
}

var diaryTrials = []dTrial{
	{key: "rl-load-1", project: "example-api", lab: "rate-limit-load", title: "Limiter at 5x traffic, one mutex",
		changes: "Replayed 5x production traffic against staging for 20 minutes.", expected: "p99 latency within 5ms of the baseline.",
		actual: "p99 rose 40ms; the profile puts the time in the bucket map's mutex.", outcome: core.OutcomeFail,
		metrics: map[string]any{"p99_ms_delta": 40, "rps": 5200}},
	{key: "rl-load-2", project: "example-api", lab: "rate-limit-load", title: "Limiter at 5x traffic, 64 sharded buckets",
		changes: "Sharded the bucket map by a hash of the API key (64 shards); same replay.", expected: "p99 latency within 5ms of the baseline.",
		actual: "p99 rose 3ms; no lock in the profile's top 20.", outcome: core.OutcomePass,
		metrics: map[string]any{"p99_ms_delta": 3, "rps": 5200}},
	{key: "sv-latency", project: "example-api", lab: "search-latency", title: "BM25 over 1M documents",
		changes: "Loaded 1M synthetic documents into search_documents on staging.", expected: "p95 under 120ms.",
		actual: "p95 140ms cold, 95ms warm.", outcome: core.OutcomePartial,
		metrics: map[string]any{"p95_ms_cold": 140, "p95_ms_warm": 95}},
}

// The agent clients a session can come from: the external_client values the
// hooks write, and none for a session a client without hooks started with
// session_start.
const (
	clientClaude  = "claude-code"
	clientCodex   = "codex"
	clientNoHooks = ""
)

// The fixture's three models, verbatim as their providers name them.
const (
	modelOpus  = "claude-opus-5-5"
	modelFable = "claude-fable-5"
	modelGPT   = "gpt-5.5"
)

// dEnd is how a session ended.
type dEnd int

const (
	// endExplicit: the agent's session_end (findings it wrote), then, for
	// Claude Code, the SessionEnd hook's usage harvest.
	endExplicit dEnd = iota + 1
	// endHook: Claude Code's SessionEnd alone, with auto-harvested findings.
	endHook
	// endReaped: no end signal; the idle reaper expired it (Codex's usual end).
	endReaped
	// endLive: still running at the anchor (today).
	endLive
)

// dSession is one session of the story. at is its local start time on its
// day ("15:04"); a session of today starts ago minutes before the anchor
// instead. mins is how long it ran (for a reaped or live one, until its last
// activity). work is its main-thread activity (one span over the whole
// session when empty) and subs its subagents'. A brief session is a short
// question: small replies, at most one message a minute.
type dSession struct {
	key      string
	client   string
	model    string
	project  string
	at       string
	ago      int
	mins     int
	end      dEnd
	brief    bool
	reason   string // SessionEnd reason, for endHook ("" is prompt_input_exit)
	work     []dSpan
	subs     []dSpan
	prompts  []dPrompt
	acts     []dAct
	findings string
	flag     *dFlag
	mishaps  []string
	commits  int // commits the session recorded at session_end
}

// dSpan is a stretch of activity: at and mins in minutes from the session's
// start, pct the share of its minutes a model message lands in (0 is the
// default), model a subagent's model ("" is claude-fable-5).
type dSpan struct {
	at, mins, pct int
	model         string
}

// dPrompt is one user prompt; recall names the memories it matched (none: a
// prompt that matched nothing).
type dPrompt struct {
	at     int
	text   string
	recall []string
}

// dFlag is a session's flag, set at its session_end.
type dFlag struct {
	verdict core.SessionVerdict
	reason  string
}

// dDo is what an act does.
type dDo int

const (
	doAdd     dDo = iota + 1 // tasks_add key
	doClaim                  // tasks_claim key (n: lease seconds, 0 the default 900)
	doDone                   // tasks_update key status=done (n: commits recorded)
	doDrop                   // tasks_update key status=dropped
	doMemory                 // memory_write key
	doReplace                // memory_write key supersedes key2
	doNote                   // notes_create key
	doTrial                  // trial_record key
	doCapture                // plan mode: the plan file of note key, iteration n
	doPresent                // plan mode: note key presented
	doApprove                // plan mode: note key approved; key2 is its tracking task
)

// dAct is one thing a session does, at minutes after its start.
type dAct struct {
	at   int
	do   dDo
	key  string
	key2 string
	n    int
}

// diaryWorkdays are the weekday scripts, oldest first. The calendar lays them
// on the weekdays of the ledger's window newest first: the last script is the
// newest weekday before today. A 21-day window holds 14 or 15 weekdays; the
// first script (index 0) is used only when there are 15, so nothing later
// depends on it. diaryShortDay is the script whose sessions are all short.
var diaryWorkdays = [15][]dSession{
	// 0: only with 15 weekdays.
	{
		{key: "w0-web", client: clientClaude, model: modelFable, project: "example-web", at: "10:15", mins: 37, end: endHook,
			prompts:  []dPrompt{{at: 0, text: "tidy up the Storybook config warnings"}},
			findings: "Silenced the two Storybook config warnings by moving the addon list into main.ts."},
	},
	// 1
	{
		{key: "w1-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:30", mins: 155, end: endExplicit,
			subs:    []dSpan{{at: 40, mins: 18}},
			prompts: []dPrompt{{at: 0, text: "pick up the rate-limits plan: add its steps and start on the limiter middleware"}},
			acts: []dAct{{at: 3, do: doAdd, key: "rl1"}, {at: 4, do: doAdd, key: "rl2"}, {at: 5, do: doAdd, key: "rl3"},
				{at: 6, do: doAdd, key: "rl4"}, {at: 7, do: doAdd, key: "rl5"}, {at: 9, do: doClaim, key: "rl1"},
				{at: 140, do: doDone, key: "rl1", n: 2}},
			findings: "Added the five rate-limits steps and finished step 1: the token-bucket middleware sits in front of every /v1 route, keyed by API key. Next: Retry-After and the X-RateLimit headers."},
		{key: "w1-ops", client: clientCodex, model: modelGPT, project: "example-ops", at: "13:40", mins: 80, end: endReaped,
			prompts: []dPrompt{{at: 0, text: "set up the backup rotation work as three steps; no doc needed"}},
			acts: []dAct{{at: 2, do: doAdd, key: "br1"}, {at: 3, do: doAdd, key: "br2"}, {at: 4, do: doAdd, key: "br3"},
				{at: 6, do: doClaim, key: "br1"}, {at: 71, do: doDone, key: "br1"}},
			findings: "Nightly snapshots now carry a retention label and the rotation job reads it. Steps 2 and 3 are queued."},
		{key: "w1-web", client: clientClaude, model: modelFable, project: "example-web", at: "14:25", mins: 55, end: endExplicit,
			acts: []dAct{{at: 2, do: doAdd, key: "web-focus"}, {at: 3, do: doClaim, key: "web-focus"},
				{at: 41, do: doDone, key: "web-focus", n: 1}, {at: 44, do: doMemory, key: "focus-ring-outline-offset"}},
			findings: "Fixed the focus ring on the settings form: the card's overflow: hidden clipped the outline. Wrote focus-ring-outline-offset."},
	},
	// 2
	{
		{key: "w2-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:50", mins: 155, end: endExplicit,
			work: []dSpan{{at: 0, mins: 70}, {at: 85, mins: 70}},
			acts: []dAct{{at: 4, do: doClaim, key: "rl2"}, {at: 120, do: doMemory, key: "rate-limit-window-60s"},
				{at: 148, do: doDone, key: "rl2", n: 1}},
			findings: "Rate-limits step 2 is done: 429s carry Retry-After and the X-RateLimit-Limit, -Remaining and -Reset headers. Chose a fixed 60-second window for now (rate-limit-window-60s)."},
		{key: "w2-web", client: clientClaude, model: modelOpus, project: "example-web", at: "13:20", mins: 140, end: endExplicit,
			subs:    []dSpan{{at: 20, mins: 25}},
			prompts: []dPrompt{{at: 0, text: "plan a dependency refresh across the web app and the API; the bundler and the date library are two majors behind"}},
			acts: []dAct{{at: 50, do: doNote, key: "plan-dependency-refresh"}, {at: 55, do: doAdd, key: "dr-w1"},
				{at: 56, do: doAdd, key: "dr-w2"}, {at: 57, do: doAdd, key: "dr-w3"}, {at: 58, do: doAdd, key: "dr-a1"},
				{at: 59, do: doAdd, key: "dr-a2"}, {at: 70, do: doClaim, key: "dr-w1"}},
			findings: "Wrote the dependency-refresh plan: three web steps (the bundler, the date library, the test runner) and two API steps (the Postgres driver, the Go toolchain). Started on the bundler."},
		{key: "w2-api-cx", client: clientCodex, model: modelGPT, project: "example-api", at: "14:05", mins: 85, end: endReaped,
			acts:     []dAct{{at: 20, do: doClaim, key: "dr-a1"}, {at: 80, do: doDone, key: "dr-a1", n: 2}},
			findings: "Bumped the Postgres driver to v6; the pool settings moved to the new ConnConfig type. All API tests pass."},
	},
	// 3
	{
		{key: "w3-ops", client: clientClaude, model: modelOpus, project: "example-ops", at: "09:15", mins: 90, end: endExplicit,
			acts: []dAct{{at: 3, do: doClaim, key: "br2"}, {at: 60, do: doMemory, key: "prune-targets-staging-first"},
				{at: 78, do: doDone, key: "br2", n: 1}, {at: 82, do: doAdd, key: "ops-drain"}, {at: 84, do: doClaim, key: "ops-drain"}},
			findings: "Backup rotation step 2: snapshots older than 14 days are pruned nightly, staging first (prune-targets-staging-first). Started draining the old build runner."},
		{key: "w3-api-q", client: clientClaude, model: modelFable, project: "example-api", at: "11:00", mins: 35, end: endHook,
			prompts:  []dPrompt{{at: 0, text: "why does the limiter test flake under -race?"}},
			findings: "The flake was a clock shared between tests; each test now builds its own fake clock."},
		{key: "w3-api", client: clientClaude, model: modelOpus, project: "example-api", at: "13:00", mins: 200, end: endExplicit,
			work:     []dSpan{{at: 0, mins: 110}, {at: 125, mins: 75}},
			subs:     []dSpan{{at: 70, mins: 30}},
			acts:     []dAct{{at: 3, do: doClaim, key: "rl3"}, {at: 185, do: doDone, key: "rl3", n: 2}},
			findings: "Rate-limits step 3: limits come from the accounts table (plan defaults, per-key overrides), cached for 60 seconds."},
		{key: "w3-web-cx", client: clientCodex, model: modelGPT, project: "example-web", at: "14:10", mins: 75, end: endReaped,
			acts:     []dAct{{at: 4, do: doClaim, key: "dr-w1"}, {at: 70, do: doDone, key: "dr-w1", n: 1}},
			findings: "Bundler 7 is in: the dev server starts 40% faster and the build output is unchanged."},
	},
	// 4
	{
		{key: "w4-ops-cx", client: clientCodex, model: modelGPT, project: "example-ops", at: "10:00", mins: 50, end: endExplicit,
			acts:     []dAct{{at: 3, do: doClaim, key: "ops-drain"}, {at: 45, do: doDone, key: "ops-drain"}},
			findings: "Drained the old build runner and took it out of the pool; jobs now land on the two new runners."},
		{key: "w4-api", client: clientClaude, model: modelOpus, project: "example-api", at: "13:30", mins: 215, end: endExplicit,
			work:    []dSpan{{at: 0, mins: 95}, {at: 120, mins: 95}},
			subs:    []dSpan{{at: 30, mins: 30}},
			prompts: []dPrompt{{at: 0, text: "load test the limiter at 5x traffic", recall: []string{"rate-limit-window-60s"}}},
			acts: []dAct{{at: 4, do: doClaim, key: "rl4"}, {at: 150, do: doTrial, key: "rl-load-1"},
				{at: 160, do: doMemory, key: "limiter-lock-contention"}},
			findings: "Load test at 5x: the single mutex around the bucket map costs 40ms at p99. Recorded the trial; next is sharding the buckets."},
		{key: "w4-web", client: clientClaude, model: modelFable, project: "example-web", at: "14:00", mins: 55, end: endHook,
			prompts:  []dPrompt{{at: 0, text: "make the empty states match the new illustrations"}},
			findings: "Empty states on the projects and billing pages use the new illustrations; the old SVGs are deleted."},
	},
	// 5
	{
		{key: "w5-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:20", mins: 140, end: endExplicit,
			prompts: []dPrompt{{at: 0, text: "shard the limiter buckets and rerun the load test", recall: []string{"limiter-lock-contention"}}},
			acts: []dAct{{at: 3, do: doClaim, key: "rl4"}, {at: 100, do: doTrial, key: "rl-load-2"},
				{at: 108, do: doReplace, key: "rate-limit-sliding-window", key2: "rate-limit-window-60s"},
				{at: 112, do: doNote, key: "rate-limit-load-test-results"}, {at: 130, do: doDone, key: "rl4", n: 1}},
			findings: "Sharded the bucket map (64 shards): p99 at 5x is now +3ms. Moved to a sliding window, which supersedes rate-limit-window-60s. Step 4 is done."},
		{key: "w5-ops", client: clientClaude, model: modelFable, project: "example-ops", at: "14:00", mins: 40, end: endExplicit,
			acts: []dAct{{at: 2, do: doAdd, key: "ops-tls"}, {at: 3, do: doClaim, key: "ops-tls"},
				{at: 30, do: doDone, key: "ops-tls"}, {at: 34, do: doMemory, key: "renew-staging-certificate"}},
			findings: "Renewed the staging TLS certificate and wrote the runbook (renew-staging-certificate)."},
		{key: "w5-global", client: clientClaude, model: modelFable, project: "", at: "16:10", mins: 45, end: endExplicit,
			prompts:  []dPrompt{{at: 0, text: "collect the shell snippets I keep using to chase flaky tests"}},
			acts:     []dAct{{at: 38, do: doNote, key: "shell-snippets-flaky-tests"}},
			findings: "Collected the flaky-test shell snippets into a global note."},
	},
	// 6: rate-limits ships; search v2 is planned in plan mode.
	{
		{key: "w6-api", client: clientClaude, model: modelOpus, project: "example-api", at: "10:05", mins: 100, end: endExplicit,
			acts: []dAct{{at: 3, do: doClaim, key: "rl5"}, {at: 92, do: doDone, key: "rl5", n: 1},
				{at: 96, do: doAdd, key: "api-429-log"}},
			commits:  1,
			findings: "Shipped rate-limits: the limits are in the public API reference and all five steps are done. Filed a follow-up to log the key id on 429s."},
		{key: "w6-plan", client: clientClaude, model: modelOpus, project: "example-api", at: "13:30", mins: 145, end: endExplicit,
			subs:    []dSpan{{at: 6, mins: 18}, {at: 30, mins: 14}},
			prompts: []dPrompt{{at: 0, text: "plan search v2: full-text search with field filters and ranking"}},
			acts: []dAct{{at: 22, do: doCapture, key: "cc-plan-steady-humming-otter", n: 1},
				{at: 61, do: doCapture, key: "cc-plan-steady-humming-otter", n: 2},
				{at: 70, do: doPresent, key: "cc-plan-steady-humming-otter"},
				{at: 78, do: doApprove, key: "cc-plan-steady-humming-otter", key2: "sv-track"},
				{at: 95, do: doAdd, key: "sv1"}, {at: 96, do: doAdd, key: "sv2"}, {at: 97, do: doAdd, key: "sv3"},
				{at: 98, do: doAdd, key: "sv4"}, {at: 99, do: doAdd, key: "sv5"}, {at: 100, do: doAdd, key: "sv6"}},
			findings: "Planned search v2 in plan mode (approved): six steps, from the search_documents table to a load test at 2x traffic."},
		{key: "w6-web-cx", client: clientCodex, model: modelGPT, project: "example-web", at: "14:00", mins: 65, end: endReaped,
			acts:     []dAct{{at: 4, do: doClaim, key: "dr-w2"}},
			findings: "Started replacing the date library: 31 of 58 call sites are on the Temporal polyfill."},
	},
	// 7
	{
		{key: "w7-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:40", mins: 140, end: endExplicit,
			acts:     []dAct{{at: 3, do: doClaim, key: "sv1"}, {at: 128, do: doDone, key: "sv1", n: 2}},
			findings: "Search v2 step 1: the search_documents table and the backfill job are in; the backfill ran on staging in 11 minutes."},
		{key: "w7-web-cx", client: clientCodex, model: modelGPT, project: "example-web", at: "10:30", mins: 55, end: endExplicit,
			acts: []dAct{{at: 2, do: doClaim, key: "dr-w2"}, {at: 50, do: doDone, key: "dr-w2", n: 1},
				{at: 53, do: doMemory, key: "temporal-for-dates"}},
			findings: "Finished the date library swap: every call site uses the Temporal polyfill. Wrote temporal-for-dates."},
		{key: "w7-web", client: clientClaude, model: modelOpus, project: "example-web", at: "14:15", mins: 150, end: endExplicit,
			acts:     []dAct{{at: 3, do: doAdd, key: "web-contrast"}, {at: 5, do: doClaim, key: "web-contrast"}},
			findings: "Audited dark-mode contrast on the data tables: 14 token pairs fail AA. The fix is half done."},
		{key: "w7-api-q", client: clientClaude, model: modelFable, project: "example-api", at: "14:30", mins: 50, end: endHook,
			prompts:  []dPrompt{{at: 0, text: "what would it take to add highlights to search results?"}},
			findings: "Highlights need the matched offsets from the ranking query; noted it for step 5."},
	},
	// 8
	{
		{key: "w8-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:20", mins: 150, end: endExplicit,
			acts:     []dAct{{at: 3, do: doClaim, key: "sv2"}},
			findings: "The search v2 query parser handles field filters and quoted phrases; date ranges remain."},
		{key: "w8-web-cx", client: clientCodex, model: modelGPT, project: "example-web", at: "13:10", mins: 90, end: endExplicit,
			acts:     []dAct{{at: 3, do: doClaim, key: "web-contrast"}, {at: 80, do: doDone, key: "web-contrast", n: 1}},
			findings: "Dark-mode contrast fixed: the 14 failing token pairs now pass AA."},
		{key: "w8-api-research", client: clientClaude, model: modelFable, project: "example-api", at: "13:40", mins: 90, end: endExplicit,
			subs:     []dSpan{{at: 10, mins: 40}},
			acts:     []dAct{{at: 60, do: doNote, key: "search-ranking-bm25-vs-embeddings"}, {at: 70, do: doTrial, key: "sv-latency"}},
			findings: "Compared BM25 and embeddings for search ranking: BM25 with field boosts wins on latency. Embeddings later, if relevance asks for it."},
	},
	// 9: only short sessions.
	{
		{key: "w9-web", client: clientClaude, model: modelFable, project: "example-web", at: "09:12", mins: 6, end: endHook, brief: true,
			prompts:  []dPrompt{{at: 0, text: "why does the dev server port clash on CI?"}},
			findings: "Two jobs share port 5173; each job now picks its own port."},
		{key: "w9-api", client: clientClaude, model: modelOpus, project: "example-api", at: "10:40", mins: 7, end: endHook, brief: true,
			prompts:  []dPrompt{{at: 0, text: "explain the Retry-After value we return for burst limits"}},
			findings: "Retry-After is the seconds until the sliding window frees one request."},
		{key: "w9-ops-cx", client: clientCodex, model: modelGPT, project: "example-ops", at: "13:05", mins: 5, end: endReaped, brief: true,
			prompts:  []dPrompt{{at: 0, text: "how full is the runner disk?"}},
			findings: "Runner disk is at 61%; the image cache is the largest directory."},
		{key: "w9-global", client: clientClaude, model: modelFable, project: "", at: "15:30", mins: 8, end: endHook, brief: true,
			prompts:  []dPrompt{{at: 0, text: "turn this curl call into an httpie one"}},
			findings: "Gave the httpie form of the curl call, with the bearer header."},
		{key: "w9-api-2", client: clientClaude, model: modelOpus, project: "example-api", at: "17:20", mins: 4, end: endHook, brief: true,
			prompts:  []dPrompt{{at: 0, text: "what is left on search v2?"}},
			findings: "Search v2 has steps 2 to 6 left; step 2 is the next claimable one."},
	},
	// 10
	{
		{key: "w10-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:00", mins: 95, end: endExplicit,
			acts:     []dAct{{at: 3, do: doClaim, key: "sv2"}, {at: 88, do: doDone, key: "sv2", n: 1}},
			findings: "Search v2 step 2 is done: date ranges parse too, and the parser has table tests for 41 cases."},
		{key: "w10-ops", client: clientClaude, model: modelOpus, project: "example-ops", at: "11:00", mins: 22, end: endExplicit,
			prompts:  []dPrompt{{at: 0, text: "prune the old snapshots on staging", recall: []string{"prune-targets-staging-first"}}},
			mishaps:  []string{"Ran the snapshot prune against production before staging; it was a dry run and deleted nothing, but it breaks prune-targets-staging-first."},
			findings: "Pruned the old staging snapshots (14 removed). Reported a mishap: the first dry run pointed at production."},
		{key: "w10-api-cx", client: clientCodex, model: modelGPT, project: "example-api", at: "14:00", mins: 95, end: endReaped,
			prompts:  []dPrompt{{at: 0, text: "profile the search backfill"}},
			findings: "The backfill spends 70% of its time decoding JSON; a streaming decoder halves it."},
		{key: "w10-web", client: clientClaude, model: modelOpus, project: "example-web", at: "14:20", mins: 105, end: endHook,
			prompts:  []dPrompt{{at: 0, text: "the billing page visual test flakes on CI"}},
			acts:     []dAct{{at: 10, do: doAdd, key: "web-flaky"}, {at: 12, do: doClaim, key: "web-flaky"}},
			findings: "The billing page visual test flakes on font loading; waiting for the fonts helps, but not every time."},
	},
	// 11
	{
		{key: "w11-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:30", mins: 190, end: endExplicit,
			work:     []dSpan{{at: 0, mins: 100}, {at: 130, mins: 60}},
			subs:     []dSpan{{at: 60, mins: 30}},
			acts:     []dAct{{at: 3, do: doClaim, key: "sv3"}},
			findings: "Ranking for search v2: BM25 with title x3 and body x1 boosts; the relevance tests are half written."},
		{key: "w11-web-desktop", client: clientNoHooks, model: modelFable, project: "example-web", at: "13:00", mins: 50, end: endExplicit,
			acts:     []dAct{{at: 20, do: doMemory, key: "visual-tests-wait-for-fonts"}},
			findings: "Reviewed the visual test setup: tests must wait for document.fonts.ready before the screenshot (visual-tests-wait-for-fonts)."},
		{key: "w11-ops-cx", client: clientCodex, model: modelGPT, project: "example-ops", at: "14:10", mins: 50, end: endReaped,
			prompts:  []dPrompt{{at: 0, text: "check how old the newest snapshot is on each host"}},
			findings: "Every host's newest snapshot is under 20 hours old."},
	},
	// 12
	{
		{key: "w12-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:10", mins: 140, end: endExplicit,
			acts: []dAct{{at: 3, do: doClaim, key: "sv3"}, {at: 125, do: doDone, key: "sv3", n: 2},
				{at: 130, do: doMemory, key: "search-backfill-batch-commits"}},
			findings: "Search v2 step 3 is done: BM25 ranking with field boosts, relevance tests green. The backfill must commit in batches of 500 (search-backfill-batch-commits)."},
		{key: "w12-web", client: clientClaude, model: modelOpus, project: "example-web", at: "13:30", mins: 130, end: endExplicit,
			prompts:  []dPrompt{{at: 0, text: "the billing visual test still flakes", recall: []string{"visual-tests-wait-for-fonts"}}},
			acts:     []dAct{{at: 4, do: doClaim, key: "web-flaky"}, {at: 100, do: doDrop, key: "web-flaky"}},
			findings: "Dropped the flaky billing test task: the test is quarantined with an owner instead, and the root cause is the font CDN."},
		{key: "w12-api-cx", client: clientCodex, model: modelGPT, project: "example-api", at: "13:45", mins: 85, end: endReaped,
			acts:     []dAct{{at: 4, do: doClaim, key: "sv4"}},
			findings: "Cursor pagination encodes the last score and id; half of the handler tests pass."},
	},
	// 13
	{
		{key: "w13-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:30", mins: 160, end: endExplicit,
			acts:     []dAct{{at: 3, do: doClaim, key: "sv4"}, {at: 150, do: doDone, key: "sv4", n: 1}},
			findings: "Search v2 step 4 is done: the endpoint pages with opaque cursors."},
		{key: "w13-web", client: clientClaude, model: modelFable, project: "example-web", at: "14:00", mins: 45, end: endHook, reason: "clear",
			prompts:  []dPrompt{{at: 0, text: "bump the icon set and fix the two renamed icons"}},
			findings: "Bumped the icon set; the two renamed icons (gear, bell) are updated in four components."},
	},
	// 14: the newest weekday before today.
	{
		{key: "w14-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:40", mins: 135, end: endExplicit,
			subs:     []dSpan{{at: 40, mins: 30}},
			acts:     []dAct{{at: 5, do: doClaim, key: "sv5"}},
			findings: "Started search v2 step 5: highlights come from the ranking query's offsets; the snippet builder is half done."},
		{key: "w14-ops-cx", client: clientCodex, model: modelGPT, project: "example-ops", at: "13:30", mins: 50, end: endExplicit,
			acts:     []dAct{{at: 4, do: doClaim, key: "br3"}},
			findings: "Drafted the stale-snapshot alert; it needs a metrics endpoint the backup job does not expose yet."},
		{key: "w14-web", client: clientClaude, model: modelOpus, project: "example-web", at: "14:00", mins: 150, end: endHook,
			prompts:  []dPrompt{{at: 0, text: "tidy the settings page layout on narrow screens"}},
			findings: "The settings page stacks its two columns under 720px and the save bar stays visible."},
	},
}

// diaryShortDay is the index in diaryWorkdays of the day of only short
// sessions.
const diaryShortDay = 9

// diaryMidnight is the session that starts late on a weekday and ends after
// midnight: an incident, flagged by its agent.
var diaryMidnight = dSession{key: "night-ops", client: clientClaude, model: modelOpus, project: "example-ops", at: "23:05", mins: 135,
	end:     endExplicit,
	work:    []dSpan{{at: 0, mins: 135, pct: 70}},
	prompts: []dPrompt{{at: 0, text: "the nightly backup failed at 22:40; find out why"}},
	acts: []dAct{{at: 35, do: doAdd, key: "ops-cred"}, {at: 36, do: doClaim, key: "ops-cred"},
		{at: 118, do: doDone, key: "ops-cred", n: 1}, {at: 122, do: doMemory, key: "backup-credential-expiry"},
		{at: 125, do: doNote, key: "incident-nightly-backup-failed"}},
	flag:     &dFlag{verdict: core.VerdictNotable, reason: "A late-night incident handled end to end: root cause, fix, a check and the incident note in one session."},
	commits:  1,
	findings: "The nightly backup failed because the object-store credential expired at 22:00. Rotated it, re-ran the backup (green) and added an expiry check. Wrote backup-credential-expiry and the incident note."}

// diaryLightWeekend is the one session on a weekend: a short question.
var diaryLightWeekend = dSession{key: "weekend-web", client: clientClaude, model: modelFable, project: "example-web", at: "11:20", mins: 7,
	end: endHook, brief: true,
	prompts:  []dPrompt{{at: 0, text: "what is the keyboard shortcut for the theme toggle?"}},
	findings: "The theme toggle is Shift+D; it is listed in the shortcuts dialog."}

// diaryPreLedger is the day older than the ledger: these sessions ended
// before the install's ledger began, so they carry token totals and no
// ledger rows.
var diaryPreLedger = []dSession{
	{key: "pre-api", client: clientClaude, model: modelOpus, project: "example-api", at: "09:40", mins: 105, end: endExplicit,
		acts:     []dAct{{at: 30, do: doMemory, key: "api-errors-are-problem-json"}, {at: 80, do: doNote, key: "plan-rate-limits"}},
		findings: "Wrote the rate-limits plan narrative (plan-rate-limits): per-key token buckets, 429 headers, limits from the accounts table, a load test and docs. The steps go in next."},
	{key: "pre-ops-cx", client: clientCodex, model: modelGPT, project: "example-ops", at: "14:00", mins: 65, end: endReaped,
		acts:     []dAct{{at: 40, do: doMemory, key: "ops-changes-through-ci"}},
		findings: "Moved the runner provisioning script into the CI pipeline; the manual steps in the README are gone."},
	{key: "pre-web", client: clientClaude, model: modelFable, project: "example-web", at: "16:20", mins: 28, end: endHook,
		prompts:  []dPrompt{{at: 0, text: "why is the settings page bundle so large?"}},
		findings: "The settings page pulled in the whole icon set; it now imports the six icons it uses."},
}

// diaryToday is today, relative to the anchor: a finished morning on search
// v2, a short question, and two agents still at work in example-api.
var diaryToday = []dSession{
	{key: "today-api-am", client: clientClaude, model: modelOpus, project: "example-api", ago: 190, mins: 95, end: endExplicit,
		subs:     []dSpan{{at: 30, mins: 20}},
		acts:     []dAct{{at: 3, do: doClaim, key: "sv5"}},
		findings: "The highlight snippets are built from the ranking query's offsets; wiring them into the endpoint is next."},
	{key: "today-ops-q", client: clientClaude, model: modelFable, project: "example-ops", ago: 160, mins: 8, end: endHook, brief: true,
		prompts:  []dPrompt{{at: 0, text: "is the stale-snapshot alert wired up yet?"}},
		findings: "Not yet: backup-rotation step 3 is open and waits on a metrics endpoint."},
	{key: "today-api-live", client: clientClaude, model: modelOpus, project: "example-api", ago: 52, end: endLive,
		subs: []dSpan{{at: 14, mins: 12}},
		prompts: []dPrompt{{at: 0, text: "carry on with search v2 step 5: wire the highlights into the endpoint",
			recall: []string{"search-backfill-batch-commits"}}},
		acts: []dAct{{at: 2, do: doClaim, key: "sv5"}, {at: 48, do: doClaim, key: "sv5", n: 3600}}},
	{key: "today-api-cx", client: clientCodex, model: modelGPT, project: "example-api", ago: 31, end: endLive,
		prompts:  []dPrompt{{at: 0, text: "bump the Go toolchain to the current release and fix what breaks"}},
		acts:     []dAct{{at: 3, do: doClaim, key: "dr-a2", n: 3600}},
		findings: "The toolchain is bumped; two vet warnings are left in the search package."},
}
