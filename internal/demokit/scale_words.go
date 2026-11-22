// The scale layer's words: the projects it adds or grows and the phrases its
// memories, notes, sessions and tasks are built from. Every project is a
// fictional example- slug (the fleet's four keep their own names); nothing here
// names a deployment, a host or a person.

package demokit

import "github.com/arctop/seamless/internal/core"

// scaleProjectSpec is one project the scale layer adds to or creates: how many
// active memories and notes it gains, how many plans and off-plan tasks, and
// the things its items are about.
type scaleProjectSpec struct {
	slug, desc   string
	memories     int // active memories added
	notes        int // notes added, its plans' narrative notes included
	plans        int
	tasks        int // off-plan tasks
	age          int // a new project's age in days; 0 for one that exists
	starred      bool
	confidential bool
	things       []string
}

// ScaleLocked is the project the scale layer fences confidential so that
// peer_auth can lock it: a lock lives in the daemon's config (or a console
// lock made under peer_auth), never in seeded rows.
const ScaleLocked = "example-vault"

// scaleNewProjects are the projects the layer creates: with the fleet's four,
// the review's two working projects and its forty quiet ones, 55 in all.
var scaleNewProjects = []scaleProjectSpec{
	{slug: "example-platform", desc: "Internal platform: the job runner, service templates and the deploy pipeline",
		memories: 230, notes: 110, plans: 6, tasks: 40, age: 231, starred: true,
		things: []string{"job runner", "deploy pipeline", "service template", "build cache", "artifact store", "release train",
			"canary", "health check", "config loader", "worker pool", "retry policy", "base image", "migration runner",
			"feature flag", "sidecar", "rollout", "job queue", "scheduler", "log shipper", "secret mount"}},
	{slug: "example-payments", desc: "Payments: checkout, refunds, payouts and the ledger",
		memories: 160, notes: 70, plans: 4, tasks: 28, age: 226, starred: true,
		things: []string{"checkout", "refund", "payout", "ledger", "invoice", "webhook", "idempotency key", "dispute",
			"settlement", "card vault", "currency table", "tax rule", "reconciliation job", "fraud score", "receipt",
			"subscription", "proration", "chargeback"}},
	{slug: "example-infra", desc: "Cluster, network and the provisioning code",
		memories: 120, notes: 55, plans: 3, tasks: 22, age: 219,
		things: []string{"cluster", "node pool", "load balancer", "DNS zone", "TLS certificate", "firewall rule", "VPN",
			"bastion", "state file", "storage class", "ingress", "autoscaler", "image registry", "backup job",
			"NAT gateway", "subnet", "kernel patch"}},
	{slug: "example-gateway", desc: "API gateway: routing, auth and rate limits",
		memories: 75, notes: 34, plans: 2, tasks: 12, age: 212,
		things: []string{"route table", "auth filter", "rate limit", "API key", "token check", "CORS policy", "request log",
			"upstream pool", "circuit breaker", "timeout budget", "header rewrite", "quota", "cache rule"}},
	{slug: "example-warehouse", desc: "Data warehouse: models, nightly loads and the BI layer",
		memories: 65, notes: 30, plans: 2, tasks: 10, age: 208, starred: true,
		things: []string{"nightly load", "model", "fact table", "dimension", "partition", "BI dashboard", "freshness check",
			"backfill", "schema test", "export job", "query budget", "lineage graph", "staging table"}},
	{slug: "example-mobile", desc: "Shared mobile code: the sync engine, push and offline storage",
		memories: 55, notes: 24, plans: 1, tasks: 8, age: 204,
		things: []string{"sync engine", "push token", "offline cache", "deep link", "crash report", "release build",
			"feature gate", "background fetch", "local database", "image cache", "login flow"}},
	{slug: "example-ml", desc: "Model serving and the feature store",
		memories: 45, notes: 22, plans: 1, tasks: 6, age: 201,
		things: []string{"feature store", "model registry", "serving pod", "embedding job", "eval set", "training run",
			"GPU node", "batch scorer", "drift monitor", "model card", "tokenizer"}},
	{slug: "example-portal", desc: "Customer portal: accounts, billing pages and support",
		memories: 40, notes: 18, plans: 1, tasks: 6, age: 199,
		things: []string{"account page", "billing page", "invite flow", "SSO login", "support form", "audit log",
			"session cookie", "CSP header", "theme", "search box", "export button"}},
	{slug: ScaleLocked, desc: "Secrets handling and the key rotation jobs",
		memories: 25, notes: 10, plans: 1, tasks: 4, age: 196, confidential: true,
		things: []string{"secret store", "rotation job", "KMS key", "access policy", "audit trail", "break-glass account",
			"token lease", "sealed backup", "recovery key", "signing key"}},
}

// scaleGrownProjects are the fleet's and the review's active projects, and the
// global scope (""), each grown by the layer.
var scaleGrownProjects = []scaleProjectSpec{
	{slug: ReviewOps, memories: 180, notes: 70, plans: 3, tasks: 26,
		things: []string{"backup job", "alert router", "snapshot", "resolver pair", "metrics volume", "on-call rotation",
			"log retention", "firewall profile", "disk health", "container prune", "restore drill", "silence file"}},
	{slug: "orbital", memories: 90, notes: 40, plans: 2, tasks: 14,
		things: []string{"webhook ingest", "outbox", "dedupe store", "payments API", "rate limiter", "connection pool",
			"read replica", "invoice job", "provider event", "signature check", "API version"}},
	{slug: "orbital-web", memories: 40, notes: 18, plans: 1, tasks: 6,
		things: []string{"design token", "component library", "edge cache", "image loader", "route bundle",
			"form validation", "hydration", "analytics tag"}},
	{slug: "homelab", memories: 35, notes: 16, plans: 1, tasks: 6,
		things: []string{"hypervisor node", "storage pool", "nightly backup", "router", "DNS resolver", "UPS", "VLAN",
			"media server", "certificate"}},
	{slug: "dotfiles", memories: 15, notes: 6, tasks: 3,
		things: []string{"shell prompt", "editor config", "git alias", "package list", "ssh config", "tmux layout", "keymap"}},
	{slug: ReviewWeb, memories: 30, notes: 14, plans: 1, tasks: 5,
		things: []string{"CDN purge", "landing page", "sitemap", "contact form", "status banner", "image pipeline",
			"cookie banner", "search page"}},
	{slug: "", memories: 55, notes: 22,
		things: []string{"session handoff", "commit message", "test suite", "review checklist", "scratch daemon",
			"release note", "changelog", "lint rule", "shell quoting", "worktree"}},
}

// scaleGeneric are the things every project's items may also be about.
var scaleGeneric = []string{
	"cache", "config file", "deploy", "migration", "test suite", "cron job", "log level", "retry", "timeout",
	"lockfile", "CI job", "release tag", "env var", "metrics", "trace", "dashboard", "alert", "README",
}

// scaleKindWeights spread the memory kinds about as a working corpus does:
// gotcha and constraint most common, refuted rare.
var scaleKindWeights = []struct {
	kind   core.MemoryKind
	weight int
}{
	{core.KindGotcha, 24}, {core.KindConstraint, 20}, {core.KindDecision, 15}, {core.KindConvention, 10},
	{core.KindRunbook, 9}, {core.KindReference, 9}, {core.KindProtocol, 5}, {core.KindStage, 5},
	{core.KindRefuted, 3},
}

// scaleGlobalKindWeights are the global scope's: rules every project shares.
var scaleGlobalKindWeights = []struct {
	kind   core.MemoryKind
	weight int
}{
	{core.KindConstraint, 35}, {core.KindConvention, 25}, {core.KindProtocol, 15}, {core.KindGotcha, 15},
	{core.KindReference, 10},
}

// scaleTemplate is a memory's name and description, with slots: {a} {b} {c}
// (things), {A} ({a} capitalized), {v}/{vb} (a verb and its base form), {op}
// (an operation), {cond} {fix} {why} {reason} {when} {purpose}, and {n} {m}
// {k} (numbers).
type scaleTemplate struct{ name, desc string }

var scaleMemoryTemplates = map[core.MemoryKind][]scaleTemplate{
	core.KindGotcha: {
		{"{a}-{v}-{b}", "The {a} {v} the {b} when {cond}; {fix}."},
		{"{a}-{v}-{b}-under-load", "Under load the {a} {v} the {b}: {fix} before the next {c} change."},
		{"{a}-silently-{v}-{b}", "No error when the {a} {v} the {b}; only the {c} dashboard shows it, so {fix}."},
		{"{a}-{v}-{b}-after-{op}", "After a {op} the {a} {v} the {b} until {cond}."},
	},
	core.KindConstraint: {
		{"never-{op}-{a}-during-{b}", "Never {op} the {a} while the {b} runs: {why}."},
		{"{a}-changes-need-{b}-review", "Every change to the {a} needs a {b} review first; {why}."},
		{"keep-{a}-out-of-{b}", "Keep the {a} out of the {b}: {why}."},
		{"{a}-only-through-{b}", "Touch the {a} only through the {b}, never by hand; {why}."},
	},
	core.KindDecision: {
		{"{a}-moves-to-{b}", "The {a} moves to the {b}: {reason}. Revisit when {when}."},
		{"{a}-over-{b}", "We chose the {a} over the {b} for {purpose}; the {b} stays for the {c}."},
		{"drop-the-{a}", "Dropped the {a}: the {b} covers the same case with less upkeep."},
	},
	core.KindConvention: {
		{"{a}-naming", "{A} names follow system-action, lower case with hyphens; one {a} per {b}."},
		{"{a}-lives-in-{b}", "The {a} lives in the {b}; link to it rather than copying it."},
		{"{a}-{b}-layout", "Layout of the {a}: one folder per {b}, a README at the top, generated files kept apart."},
	},
	core.KindRunbook: {
		{"{op}-the-{a}", "Runbook: {op} the {a} without downtime. Drain it, {op} it, verify the {b}, then re-enable it."},
		{"restore-{a}-from-{b}", "Restore the {a} from the {b}: stop the writer, restore, verify the checksum, then swap."},
		{"{a}-recovery", "When the {a} stops answering: check the {b}, restart the {c}, page only if it fails twice."},
	},
	core.KindReference: {
		{"{a}-{b}-map", "Where the {a} keeps its {b}: paths, owners and the dashboards that show them."},
		{"{a}-limits", "The {a}'s limits: {n} requests a second, {m} MB a payload, {k} open connections."},
		{"{a}-and-{b}-endpoints", "Ports, names and health endpoints of the {a} and the {b}, with who owns each."},
	},
	core.KindProtocol: {
		{"{a}-handoff", "Before handing off {a} work: record the {b} state in the findings and release every claim."},
		{"{a}-change-protocol", "A change to the {a}: open a task, write the plan note, get the owner's yes, then claim the step."},
		{"{a}-review-protocol", "Review a {a} change against the {b} checklist; a second agent signs it off."},
	},
	core.KindStage: {
		{"{a}-cutover", "Stage: the {a} cutover to the {b}. The status is in the body; flip it when the switch is done."},
		{"{a}-migration-window", "Stage: the {a} migration window. While it holds, nobody touches the {b}."},
		{"{a}-freeze", "Stage: the {a} freeze before the release; only fixes land until it lifts."},
	},
	core.KindRefuted: {
		{"{a}-does-not-{vb}-{b}", "Refuted: the {a} does not {vb} the {b}; the slowdown came from the {c}."},
		{"{a}-is-not-the-bottleneck", "Refuted: the {a} is not the bottleneck. Profiling put {n}% of the time in the {b}."},
	},
}

// scaleVerbs are the gotchas' verbs, third person and base form.
var scaleVerbs = [][2]string{
	{"drops", "drop"}, {"hides", "hide"}, {"resets", "reset"}, {"doubles", "double"}, {"skips", "skip"},
	{"ignores", "ignore"}, {"truncates", "truncate"}, {"reorders", "reorder"}, {"caches", "cache"},
	{"leaks", "leak"}, {"outlives", "outlive"}, {"blocks", "block"}, {"rewrites", "rewrite"},
	{"swallows", "swallow"}, {"stalls", "stall"},
}

var scaleOps = []string{"restart", "migrate", "resize", "rotate", "redeploy", "purge", "rebuild", "reindex", "prune", "upgrade"}

var scaleConds = []string{
	"two workers start at once", "the config reloads", "a retry lands after the timeout", "the payload passes 1 MB",
	"the replica lags", "the token rotates mid-request", "the disk fills past 90%", "a deploy overlaps the nightly backup",
	"the clock skews by a second", "the connection pool runs dry", "the cache is cold",
	"a request carries no idempotency key", "the job resumes after a crash", "the region fails over",
	"the header arrives in lower case", "the queue backs up past 10k", "a migration runs in the same window",
	"a session resumes after sleep",
}

var scaleFixes = []string{
	"check the {c} first", "pin the {c} before the change", "restart the {c} after it", "wait for the {c} to settle",
	"read from the {c} instead", "set a timeout on the {c}", "keep the {c} out of the hot path",
	"log the {c} id on every retry", "drain the {c} before touching it", "compare against the {c} snapshot",
}

var scaleWhys = []string{
	"the {c} holds its locks until it finishes", "a half-applied change has no rollback",
	"the owner reviews every change there", "it pages the on-call rotation", "the audit trail must stay complete",
	"other agents read it at session start", "the last outage started that way", "the restore drill depends on it",
}

var scaleReasons = []string{
	"it halves the moving parts", "the owner wants one place to look", "it survives a restart",
	"the old path had no tests", "it costs a third as much", "the {c} already depends on it",
}

var scaleWhens = []string{
	"traffic doubles", "the next major release ships", "the vendor contract renews", "the {c} is rewritten",
	"a second region opens",
}

var scalePurposes = []string{"the hot path", "batch jobs", "anything the owner reviews", "new services"}

// scaleSentences build memory bodies and note sections.
var scaleSentences = []string{
	"A session found it while working on the {a}; nothing in the logs pointed at the {b}.",
	"It shows up about once in {n} runs, more often when {cond}.",
	"The {c} dashboard is the quickest way to see it: the error rate climbs while the {a} looks healthy.",
	"The fix that held: {fix}.",
	"Two earlier attempts failed: retrying the {a} only moved the problem to the {b}.",
	"Measured on {date}: {n} ms at the median, {m} ms at p99, against a budget of {k} ms.",
	"The owner asked to keep this in mind for every change to the {a}.",
	"Related: the {b} has the same shape, but its {c} hides it.",
	"If the {a} is replaced, check this again before carrying it over.",
	"The cost of ignoring it is a page at night and an hour of cleanup in the {b}.",
	"Nothing else depends on the {a} in this way, so the rule stays local to it.",
	"Verified again after the {c} upgrade; it still holds.",
	"Root cause: {cond}, which made the {a} {vb} the {b}.",
	"Open question: should the {a} own the {b}, or should the {c} keep it?",
	"Next step: {op} the {a}, then watch the {b} for a day.",
	"We measured the {a} under the {b}: {n}% of requests finished within {m} ms, and the tail came from the {c}.",
}

var scaleBullets = []string{
	"Check the {a} before the {b}.", "Never {op} the {c} while a deploy runs.", "Record the {a} id in the findings.",
	"Ask the owner before changing the {b}.", "Run the {c} smoke test after the change.",
	"Keep the {a} config in version control.", "Write the {b} state into the task body before releasing it.",
}

var scaleCommands = []string{
	"kubectl rollout status deploy/{as}", "systemctl restart {as}", "psql -c 'select count(*) from {au}'",
	"curl -s localhost:9100/metrics | grep {au}", "git log --oneline -- {as}/", "make check",
}

// scaleNoteKind is one kind of ordinary note: its tag, a title, a
// description and the sections its body carries.
type scaleNoteKind struct {
	tag, title, desc string
	sections         []string
}

// scaleNoteKinds are the ordinary notes' kinds; the first two are the ones a
// plan's supporting notes take.
var scaleNoteKinds = []scaleNoteKind{
	{"research", "Research: the {a} versus the {b}", "Measured the {a} against the {b} for {purpose}; the numbers and a recommendation.", []string{"Question", "Method", "Findings", "Recommendation"}},
	{"design", "Design: the {a} for the {b}", "Design note for the {a}: the shape, the failure modes and what the {b} needs from it.", []string{"Context", "Design", "Failure modes", "Open questions"}},
	{"postmortem", "Postmortem: {A} outage on {date}", "What happened to the {a} on {date}, why, and the action items that followed.", []string{"Timeline", "Impact", "Root cause", "Action items"}},
	{"meeting", "Meeting summary: {A} review, {date}", "Decisions and follow-ups from the {a} review: scope, owners and dates.", []string{"Attendees", "Decisions", "Follow-ups"}},
	{"ops", "Ops log: {A} {opnoun}", "Running log of the {a} {opnoun}: commands, checks and what to watch afterwards.", []string{"Before", "Steps", "Checks", "After"}},
	{"audit", "Audit: every {a} and its owner", "Inventory of every {a}, who owns it, and which ones have no owner yet.", []string{"Scope", "Inventory", "Gaps"}},
	{"decision-record", "Decision record: the {a} over the {b}", "Why the {a} won over the {b}: the options, the cost and the reversal plan.", []string{"Options", "Decision", "Consequences"}},
	{"incident", "Incident timeline: {date} {a} errors", "Raw timeline kept during the {a} incident on {date}; source for the postmortem.", []string{"Timeline", "Notes"}},
	{"retro", "Retro: the {a} {opnoun}", "What went well and what did not in the {a} {opnoun}.", []string{"Went well", "Did not", "Changes"}},
}

var scaleOpNouns = []string{"rotation", "upgrade", "cleanup", "migration", "resize", "rollout"}

// scaleStepTitles are a plan's steps, in the order a plan runs them.
var scaleStepTitles = []string{
	"Inventory the {a}", "Design the new {a}", "Build the {a} behind a flag", "Move the {b} to the new {a}",
	"Backfill the {a}", "Verify the {a} after the switch", "Remove the old {a}", "Write the {a} runbook",
}

var scalePlanGoals = []struct{ slug, phrase string }{
	{"v2", "a second version of the {a}"}, {"rework", "rework the {a}"}, {"cutover", "cut the {b} over to the new {a}"},
	{"cleanup", "clean up the {a}"}, {"hardening", "harden the {a}"}, {"rollout", "roll the {a} out everywhere"},
}

// scaleTaskTitles are off-plan tasks.
var scaleTaskTitles = []string{
	"Fix the {a} that {v} the {b}", "Add a {c} check to the {a}", "Rotate the {a}", "Upgrade the {a}",
	"Write the {a} runbook", "Investigate the {a} timeouts", "Remove the old {a}", "Document the {a} limits",
}

var scaleFindings = []string{
	"Worked the {a}: the change is merged and verified.", "Worked the {a}: two follow-ups filed.",
	"Worked the {a}: blocked on the owner's answer about the {b}.", "Measured the {a}: nothing to change yet.",
	"Updated the {a} runbook after the {b} change.", "Wrote down the {a} gotcha the {b} kept hitting.",
}

// scaleTricky are descriptions whose text YAML would misread unquoted, one in
// every few memories, so the parse test covers them.
var scaleTricky = []string{
	"Note: the {a} and the {b} share one config key: changing it moves both.",
	"\"Retry-After\" is ignored by the {a}; the {b} sets its own backoff.",
	"[{a}] the first line of the {b} log is the only one that names the request.",
	"# of {a} workers must match the {b} partitions, or one partition starves.",
	"- The {a} list is ordered: the {b} reads it top to bottom and stops at the first match.",
	"{a}: 3 retries, then the {b} gives up -- key: value pairs in its config are case sensitive.",
	"'{A}' in quotes is a literal name to the {b}, not a pattern.",
	"@{as} mentions in the {b} page nobody; the alias was retired.",
	"100% of {a} restarts during the window hit the {b}: schedule them after it.",
	"The {a} config accepts {braces} placeholders; an unknown one renders as empty, not as an error.",
}

// scaleAgentTypes are the subagent types a working copy names.
var scaleAgentTypes = []string{"Plan", "Explore", "Explore", "general-purpose"}

// scaleModels are the models the layer's sessions run.
var scaleModels = []string{modelOpus, modelOpus, modelFable, modelSonnet}
