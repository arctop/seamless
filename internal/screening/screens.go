// Package screening is the write-safety layer behind the screening optional
// feature: the screens every agent write to a memory or note runs through,
// the policy that decides which writes are held out of the corpus for the
// owner, and the queue that releases or discards them.
//
// Seamless's constraints and conventions are injected into every session's
// briefing, so a poisoned one is the most leveraged persistent prompt
// injection an agent fleet has, and the gardener -- which reads memories and
// proposes rewrites -- can spread it. The screens look for what an injection
// or a leak looks like (secrets, injection markers, hidden characters, encoded
// blobs, unlisted links), never for ordinary imperative text: memories are
// instructions to agents by design.
//
// Everything here is generic and off by default; the surfaces (internal/mcp,
// internal/console, internal/hooks) ask features.Screening before calling in.
package screening

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Code is a verdict code: the only thing a screen reports about the text it
// matched. Codes go into events and the owner's queue; the matched text never
// leaves the write it was found in.
type Code string

// Secrets: well-known credential formats and private keys.
const (
	SecretPrivateKey   Code = "secret.private_key"    // a PEM or OpenSSH private key block
	SecretAWSAccessKey Code = "secret.aws_access_key" // an AWS access key id
	SecretGitHubToken  Code = "secret.github_token"   // a GitHub personal, OAuth, app or fine-grained token
	SecretSlackToken   Code = "secret.slack_token"    // a Slack token or incoming-webhook URL
	SecretOpenAIKey    Code = "secret.openai_key"     // an OpenAI API key
	SecretAnthropicKey Code = "secret.anthropic_key"  // an Anthropic API key
	SecretGoogleAPIKey Code = "secret.google_api_key" // a Google API key
	SecretStripeKey    Code = "secret.stripe_key"     // a live Stripe secret or restricted key
	SecretJWT          Code = "secret.jwt"            // a signed JSON Web Token
)

// Injection markers: instruction-shaped text aimed at the agent reading it.
const (
	InstructionOverride     Code = "instruction.override"     // telling the reader to drop its prior instructions
	InstructionRoleMarker   Code = "instruction.role_marker"  // chat-template, role or envelope tags impersonating the system
	InstructionConcealment  Code = "instruction.concealment"  // telling the reader to hide what it does from the user
	InstructionExfiltration Code = "instruction.exfiltration" // telling the reader to send credentials somewhere
)

// Hidden-text tricks: content a human reviewing the console does not see.
const (
	HiddenComment     Code = "hidden.comment"     // an HTML or markdown comment (rendered as nothing)
	HiddenStyle       Code = "hidden.style"       // an element styled or flagged invisible
	HiddenInstruction Code = "hidden.instruction" // agent-directed imperatives inside hidden text
)

// Hidden Unicode: characters that render as nothing or reorder what renders.
const (
	UnicodeTag       Code = "unicode.tag"        // tag characters U+E0000-U+E007F (they can spell hidden ASCII)
	UnicodeZeroWidth Code = "unicode.zero_width" // U+200B-U+200D, U+2060, U+FEFF (not a leading byte-order mark)
	UnicodeBidi      Code = "unicode.bidi"       // bidi embeddings, overrides and isolates U+202A-U+202E, U+2066-U+2069
)

// Long encoded blobs.
const (
	BlobBase64 Code = "blob.base64" // a base64 run at least Rules.BlobMinLength long
	BlobHex    Code = "blob.hex"    // a hex run at least Rules.BlobMinLength long
)

// URLUnlisted is a URL whose host is outside Rules.URLAllowlist.
const URLUnlisted Code = "url.unlisted"

// Codes lists every verdict code, grouped by screen. It is the canonical set
// the docs and tests derive from.
var Codes = []Code{
	SecretPrivateKey, SecretAWSAccessKey, SecretGitHubToken, SecretSlackToken, SecretOpenAIKey,
	SecretAnthropicKey, SecretGoogleAPIKey, SecretStripeKey, SecretJWT,
	InstructionOverride, InstructionRoleMarker, InstructionConcealment, InstructionExfiltration,
	HiddenComment, HiddenStyle, HiddenInstruction,
	UnicodeTag, UnicodeZeroWidth, UnicodeBidi,
	BlobBase64, BlobHex,
	URLUnlisted,
}

// recordOnly are the verdicts that are recorded but never hold a write on
// their own. An HTML or markdown comment, or an element styled invisible, is
// common enough in real notes (templates, TODOs, pasted HTML) that holding on
// it alone would bury the queue -- and the part that makes one dangerous, an
// imperative aimed at the agent inside it, is its own verdict
// (HiddenInstruction), which holds.
var recordOnly = map[Code]bool{
	HiddenComment: true,
	HiddenStyle:   true,
}

// Holds reports whether a verdict holds the write it was found in. Every code
// holds except the record-only ones; an unknown code holds, so a verdict added
// later is never silently treated as harmless.
func (c Code) Holds() bool { return !recordOnly[c] }

// Rules are the tunable parts of the screens (config.Screening).
type Rules struct {
	// URLAllowlist names the hosts URLs may point at; each covers its
	// subdomains. Empty turns the URL screen off.
	URLAllowlist []string
	// BlobMinLength is the shortest base64 or hex run that counts as a blob.
	BlobMinLength int
}

// defaultBlobMinLength mirrors config.DefaultScreeningBlobMinLength for a
// Rules built without a length (tests, a zero value).
const defaultBlobMinLength = 200

// Findings counts each verdict code's matches in one text.
type Findings map[Code]int

// Codes returns the codes found, in Codes order.
func (f Findings) Codes() []Code {
	var out []Code
	for _, c := range Codes {
		if f[c] > 0 {
			out = append(out, c)
		}
	}
	return out
}

// Introduced returns the codes a write adds over the text it replaces: those
// matching more often in f than in base, in Codes order. Comparing counts
// rather than presence is what lets an owner-released memory that legitimately
// quotes an injection marker (a note about prompt injection, say) take later
// edits without being held again, while a second marker added to it -- or one
// assembled across two appends -- still shows.
func (f Findings) Introduced(base Findings) []Code {
	var out []Code
	for _, c := range Codes {
		if f[c] > base[c] {
			out = append(out, c)
		}
	}
	return out
}

// HoldCodes filters codes to the ones that hold a write.
func HoldCodes(codes []Code) []Code {
	var out []Code
	for _, c := range codes {
		if c.Holds() {
			out = append(out, c)
		}
	}
	return out
}

// Scan runs every screen over text and counts what each found.
func Scan(text string, rules Rules) Findings {
	f := Findings{}
	scanUnicode(text, f)
	scanSecrets(text, f)
	scanBlobs(text, rules.blobMin(), f)
	scanURLs(text, rules.URLAllowlist, f)
	norm := normalize(text)
	scanInstructions(text, norm, rules.URLAllowlist, f)
	scanHidden(text, f)
	return f
}

func (r Rules) blobMin() int {
	if r.BlobMinLength > 0 {
		return r.BlobMinLength
	}
	return defaultBlobMinLength
}

// ---------------------------------------------------------------------------
// Hidden Unicode
// ---------------------------------------------------------------------------

// isTag, isZeroWidth and isBidi classify the hidden characters by code point.
// The ranges are written as numbers on purpose: the characters themselves
// must never appear in this source.
func isTag(r rune) bool { return r >= 0xE0000 && r <= 0xE007F }

func isZeroWidth(r rune) bool {
	return (r >= 0x200B && r <= 0x200D) || r == 0x2060 || r == 0xFEFF
}

func isBidi(r rune) bool { return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) }

// isHidden reports whether r is any of the hidden characters the Unicode
// screen counts.
func isHidden(r rune) bool { return isTag(r) || isZeroWidth(r) || isBidi(r) }

func scanUnicode(text string, f Findings) {
	for i, r := range text {
		switch {
		case isTag(r):
			f[UnicodeTag]++
		case r == 0xFEFF && i == 0:
			// A byte-order mark at the very start is an editor's artifact, not
			// a hidden payload.
		case isZeroWidth(r):
			f[UnicodeZeroWidth]++
		case isBidi(r):
			f[UnicodeBidi]++
		}
	}
}

// RevealHidden rewrites every hidden character as a visible [U+XXXX] marker,
// so the owner reviewing a held write sees exactly what an agent would read.
func RevealHidden(text string) string {
	if !strings.ContainsFunc(text, isHidden) {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		if isHidden(r) {
			fmt.Fprintf(&b, "[U+%04X]", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// normalize prepares text for the phrase screens: hidden characters removed
// (so a zero-width space cannot split "ignore" from detection), lower-cased,
// and whitespace runs collapsed to one space within each line.
func normalize(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	space := false
	for _, r := range text {
		if isHidden(r) || r == utf8.RuneError {
			continue
		}
		if r == '\n' {
			b.WriteRune('\n')
			space = false
			continue
		}
		if unicode.IsSpace(r) {
			if !space {
				b.WriteRune(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Secrets
// ---------------------------------------------------------------------------

var (
	privateKeyRe = regexp.MustCompile(`-----BEGIN (?:(?:RSA|EC|DSA|OPENSSH|ENCRYPTED|PGP) )?PRIVATE KEY(?: BLOCK)?-----`)
	awsKeyRe     = regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`)
	githubRe     = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,255})\b`)
	slackRe      = regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}|https://hooks\.slack\.com/services/T[A-Za-z0-9]+/B[A-Za-z0-9]+/[A-Za-z0-9]+`)
	anthropicRe  = regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)
	openaiRe     = regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`)
	googleKeyRe  = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)
	stripeRe     = regexp.MustCompile(`\b[rs]k_live_[0-9A-Za-z]{20,}\b`)
	jwtRe        = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
)

func scanSecrets(text string, f Findings) {
	f[SecretPrivateKey] += len(privateKeyRe.FindAllStringIndex(text, -1))
	for _, m := range awsKeyRe.FindAllString(text, -1) {
		// AWS's own documentation key (AKIAIOSFODNN7EXAMPLE and its kin) is the
		// one access key id every runbook quotes; it opens nothing.
		if !strings.HasSuffix(m, "EXAMPLE") {
			f[SecretAWSAccessKey]++
		}
	}
	f[SecretGitHubToken] += len(githubRe.FindAllStringIndex(text, -1))
	f[SecretSlackToken] += len(slackRe.FindAllStringIndex(text, -1))
	anthropic := anthropicRe.FindAllStringIndex(text, -1)
	f[SecretAnthropicKey] += len(anthropic)
	for _, loc := range openaiRe.FindAllStringIndex(text, -1) {
		m := text[loc[0]:loc[1]]
		if strings.HasPrefix(m, "sk-ant-") {
			continue // counted as an Anthropic key above
		}
		// A key is random: it mixes digits and letters. A long kebab-case word
		// that happens to start with "sk-" does not.
		if mixedClasses(m[3:], true, false) {
			f[SecretOpenAIKey]++
		}
	}
	f[SecretGoogleAPIKey] += len(googleKeyRe.FindAllStringIndex(text, -1))
	f[SecretStripeKey] += len(stripeRe.FindAllStringIndex(text, -1))
	f[SecretJWT] += len(jwtRe.FindAllStringIndex(text, -1))
}

// mixedClasses reports whether s carries a digit and a letter, and with
// needCase both an upper- and a lower-case letter too.
func mixedClasses(s string, needDigit, needCase bool) bool {
	var digit, upper, lower bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		}
	}
	if needDigit && !digit {
		return false
	}
	if needCase {
		return upper && lower
	}
	return upper || lower
}

// ---------------------------------------------------------------------------
// Long encoded blobs
// ---------------------------------------------------------------------------

func isBase64Rune(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
		r == '+' || r == '/' || r == '_' || r == '-' || r == '='
}

func isHexRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// scanBlobs counts unbroken runs of the base64 alphabet at least minLen long.
// A run made only of hex digits (with a digit and a letter among them) is a
// hex blob; any other run is base64 only when it mixes digits with upper- and
// lower-case letters, which is what keeps a long path or identifier of words
// from reading as encoded data.
func scanBlobs(text string, minLen int, f Findings) {
	start := -1
	n := 0
	flush := func(end int) {
		if start >= 0 && n >= minLen {
			run := strings.TrimRight(text[start:end], "=")
			switch {
			case strings.IndexFunc(run, func(r rune) bool { return !isHexRune(r) }) < 0 && mixedClasses(run, true, false):
				f[BlobHex]++
			case mixedClasses(run, true, true):
				f[BlobBase64]++
			}
		}
		start, n = -1, 0
	}
	for i, r := range text {
		if isBase64Rune(r) {
			if start < 0 {
				start = i
			}
			n++
			continue
		}
		flush(i)
	}
	flush(len(text))
}

// ---------------------------------------------------------------------------
// URLs
// ---------------------------------------------------------------------------

var urlRe = regexp.MustCompile(`(?i)\b(?:https?|ftp|wss?)://[^\s<>"'` + "`" + `)\]]+`)

func scanURLs(text string, allow []string, f Findings) {
	if len(allow) == 0 {
		return
	}
	for _, raw := range urlRe.FindAllString(text, -1) {
		if !hostAllowed(urlHost(raw), allow) {
			f[URLUnlisted]++
		}
	}
}

// urlHost extracts a URL's host, lower-cased, without port or trailing dot.
// An unparseable URL yields "", which no allowlist entry matches.
func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimRight(raw, ".,;:!?"))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// hostAllowed reports whether host is loopback or covered by an allowlist
// entry (the host itself or a subdomain of it). Loopback is always allowed: a
// URL that names this machine cannot carry anything off it, and memories about
// local services name them constantly.
func hostAllowed(host string, allow []string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	for _, entry := range allow {
		e := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(entry), "*."), "."), "."))
		if e == "" {
			continue
		}
		if host == e || strings.HasSuffix(host, "."+e) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Injection markers
// ---------------------------------------------------------------------------

// The phrase screens match the normalized text (lower case, hidden characters
// removed, spaces collapsed). Each targets a marker of an injection, not an
// imperative: "always run make check" is a constraint, "ignore all previous
// instructions" is an attack.
var (
	overrideRes = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:ignore|disregard|forget|override|bypass|skip|do not follow|don't follow|stop following)\s+(?:(?:all|any|every|the|your|of|these|those|my)\s+)*(?:previous|prior|preceding|above|earlier|original|initial|system|developer|safety)\s+(?:instructions?|prompts?|directions?|directives?|guidelines|guidance|messages?|context|rules)\b`),
		regexp.MustCompile(`\byou are now (?:an? )?(?:unrestricted|jailbroken|uncensored|in (?:developer|god|admin|debug|dan) mode|dan)\b`),
		regexp.MustCompile(`(?m)^\s*(?:new|updated|revised|real|actual|true)\s+(?:system\s+)?(?:instructions?|prompt)\s*:`),
		regexp.MustCompile(`\b(?:your|the) (?:new|real|actual|true) (?:instructions|task|objective|goal) (?:is|are)\b`),
	}

	// Role markers match the original text: chat-template tokens and role tags
	// are case-sensitive conventions, and some (Human:/Assistant: turns) only
	// mean something in their exact form.
	roleMarkerRes = []*regexp.Regexp{
		regexp.MustCompile(`<\|(?:im_start|im_end|system|user|assistant|endoftext|eot_id|start_header_id|end_header_id)\|>`),
		regexp.MustCompile(`\[/?INST\]|<</?SYS>>`),
		regexp.MustCompile(`(?i)</?(?:system|developer|system-reminder|system_prompt|tool_result|tool_use|function_results|function_calls)(?:\s[^>]*)?>`),
		regexp.MustCompile(`(?i)</?seam-(?:briefing|recall)(?:\s[^>]*)?>`),
		regexp.MustCompile(`(?m)^\s*(?:Human|Assistant):`),
		regexp.MustCompile(`(?i)\[(?:system|developer)(?: message| prompt| note)?\]`),
		regexp.MustCompile(`(?i)\b(?:begin|end) (?:of )?(?:the )?(?:system|developer) (?:prompt|message|instructions)\b`),
	}

	concealmentRes = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:keep|hide|conceal)\s+(?:this|these|it|that|them)(?:\s+(?:instructions?|messages?|steps|actions?))?\s+(?:(?:secret|hidden)\s+)?from\s+(?:the\s+)?(?:user|human|owner|operator)s?\b`),
		regexp.MustCompile(`\b(?:do not|don't|never|must not)\s+(?:reveal|disclose|mention|repeat|show)\s+(?:these|this|the|any of these)\s+(?:instructions?|message|prompt|directives?)\b`),
		regexp.MustCompile(`\bthe (?:user|human|owner|operator) (?:must|should|will) (?:not|never) (?:know|find out|learn|notice|see)\b`),
		regexp.MustCompile(`\b(?:secretly|covertly|silently|surreptitiously)\s+(?:send|upload|post|exfiltrate|forward|copy|run|execute|delete|modify|change|add|install|disable|remove)\b`),
		regexp.MustCompile(`\bwithout (?:the )?(?:user|human|owner|operator)(?:'s)? (?:knowing|noticing|knowledge)\b`),
	}

	exfilVerbRe   = regexp.MustCompile(`\b(?:send|post|upload|exfiltrate|leak|transmit|forward|email|e-mail|paste|submit)\b`)
	exfilSecretRe = regexp.MustCompile(`\b(?:credentials?|secrets?|api[ _-]?keys?|(?:access|auth|bearer|api|refresh|session) tokens?|passwords?|private keys?|ssh keys?|cookies|session ids?|environment variables|env vars|keychain)\b|\.env\b|\.aws/credentials|\.ssh/id_`)
	exfilDestRe   = regexp.MustCompile(`\b(?:webhook|pastebin|requestbin|ngrok|attacker)\b|\b(?:an?|the|some) (?:external|remote|outside|third[- ]party) (?:server|host|url|endpoint|address|site)\b`)
	// A URL only counts as where the secret goes when the sentence sends it
	// there ("to", "into", "at", "via"), never when it names where the secret
	// comes from ("copy the key from https://...").
	exfilToURLRe  = regexp.MustCompile(`\b(?:to|into|onto|via|at)\s+(?:(?:the|an?|this|that)\s+)?(?:[a-z-]+\s+){0,2}((?:https?|ftp|wss?)://[^\s<>"'` + "`" + `)\]]+)`)
	exfilCmdRe    = regexp.MustCompile(`\b(?:curl|wget|nc|ncat|netcat|scp|rsync|invoke-webrequest|iwr|invoke-restmethod)\b`)
	exfilPathRe   = regexp.MustCompile(`~/\.ssh/[a-z0-9_.-]+|~/\.aws/|\.aws/credentials|\bid_(?:rsa|ed25519|ecdsa|dsa)(?:\.pub)?\b|/etc/shadow|\.netrc\b|\.git-credentials\b|\.docker/config\.json|\$\(env\)|\bprintenv\b`)
	sentenceSplit = regexp.MustCompile(`[.!?;]\s|\n`)
)

var (
	fencedCodeRe = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~")
	inlineCodeRe = regexp.MustCompile("`[^`\n]+`")
)

// stripCode blanks markdown code -- fenced blocks and inline spans -- keeping
// line breaks, so line-anchored patterns still see the lines around it.
func stripCode(text string) string {
	blank := func(m string) string {
		return strings.Map(func(r rune) rune {
			if r == '\n' {
				return r
			}
			return ' '
		}, m)
	}
	text = fencedCodeRe.ReplaceAllStringFunc(text, blank)
	return inlineCodeRe.ReplaceAllStringFunc(text, blank)
}

func scanInstructions(text, norm string, allow []string, f Findings) {
	for _, re := range overrideRes {
		f[InstructionOverride] += len(re.FindAllStringIndex(norm, -1))
	}
	// A role tag quoted as code is documentation of the tag, not an attempt
	// to wear it: notes about agents mention `<seam-briefing>` and
	// `<system-reminder>` constantly, always inside backticks or a fence.
	unquoted := stripCode(text)
	for _, re := range roleMarkerRes {
		f[InstructionRoleMarker] += len(re.FindAllStringIndex(unquoted, -1))
	}
	for _, re := range concealmentRes {
		f[InstructionConcealment] += len(re.FindAllStringIndex(norm, -1))
	}
	f[InstructionExfiltration] += exfiltrations(norm, allow)
}

// exfiltrations counts the sentences that direct secret material somewhere
// outside: a sending verb, a secret noun and an outside destination together
// (a non-loopback URL outside the allowlist counts as one), or a network
// command reading a credential file. Copying a PUBLIC key (id_rsa.pub) is
// ordinary runbook text and does not count.
func exfiltrations(norm string, allow []string) int {
	n := 0
	for _, sentence := range sentenceSplit.Split(norm, -1) {
		if exfilVerbRe.MatchString(sentence) && exfilSecretRe.MatchString(sentence) && outsideDestination(sentence, allow) {
			n++
			continue
		}
		if exfilCmdRe.MatchString(sentence) {
			for _, p := range exfilPathRe.FindAllString(sentence, -1) {
				if !strings.HasSuffix(p, ".pub") {
					n++
					break
				}
			}
		}
	}
	return n
}

func outsideDestination(sentence string, allow []string) bool {
	if exfilDestRe.MatchString(sentence) {
		return true
	}
	for _, m := range exfilToURLRe.FindAllStringSubmatch(sentence, -1) {
		host := urlHost(m[1])
		if host == "" || hostAllowed(host, allow) {
			continue // loopback, or a host the owner allowlisted
		}
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Hidden-text tricks
// ---------------------------------------------------------------------------

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--(.*?)-->`)
	mdCommentRe   = regexp.MustCompile(`(?m)^\[(?://|comment|_)\]:\s*(?:#|<>)\s*\((.*)\)\s*$`)
	// An element styled or flagged invisible, with the text up to its next tag.
	hiddenStyleRe = regexp.MustCompile(`(?is)<[a-z][a-z0-9]*\b[^>]*\bstyle\s*=\s*["'][^"']*(?:display\s*:\s*none|visibility\s*:\s*hidden|font-size\s*:\s*0(?:\.0+)?(?:px|pt|em|rem|%)?\s*(?:;|["'])|opacity\s*:\s*0(?:\.0+)?\s*(?:;|["']))[^>]*>([^<]*)`)
	hiddenAttrRe  = regexp.MustCompile(`(?is)<[a-z][a-z0-9]*\b[^>]*?\s(?:hidden\b|aria-hidden\s*=\s*["']true["'])[^>]*>([^<]*)`)
	// Agent-directed imperatives: harmless in visible text (memories are
	// instructions), a marker when they are hidden from the human reviewer.
	hiddenImperativeRe = regexp.MustCompile(`\b(?:you (?:must|should|will|need to|are to)|ignore|disregard|always|never|run|execute|eval|fetch|download|curl|wget|install|send|post|upload|exfiltrate|do not (?:tell|mention|reveal))\b|\b(?:assistant|agent|ai|claude|model|llm)s?\s*[,:]`)
)

func scanHidden(text string, f Findings) {
	var hidden []string
	for _, m := range htmlCommentRe.FindAllStringSubmatch(text, -1) {
		if strings.TrimSpace(m[1]) != "" {
			f[HiddenComment]++
			hidden = append(hidden, m[1])
		}
	}
	for _, m := range mdCommentRe.FindAllStringSubmatch(text, -1) {
		if strings.TrimSpace(m[1]) != "" {
			f[HiddenComment]++
			hidden = append(hidden, m[1])
		}
	}
	for _, re := range []*regexp.Regexp{hiddenStyleRe, hiddenAttrRe} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			f[HiddenStyle]++
			hidden = append(hidden, m[1])
		}
	}
	for _, h := range hidden {
		f[HiddenInstruction] += len(hiddenImperativeRe.FindAllStringIndex(normalize(h), -1))
	}
}

// codeWords are the owner-facing descriptions of each code, for the console's
// held queue and the docs. One place, so the two cannot drift.
var codeWords = map[Code]string{
	SecretPrivateKey:          "contains a private key",
	SecretAWSAccessKey:        "contains an AWS access key id",
	SecretGitHubToken:         "contains a GitHub token",
	SecretSlackToken:          "contains a Slack token or webhook URL",
	SecretOpenAIKey:           "contains an OpenAI API key",
	SecretAnthropicKey:        "contains an Anthropic API key",
	SecretGoogleAPIKey:        "contains a Google API key",
	SecretStripeKey:           "contains a live Stripe key",
	SecretJWT:                 "contains a signed JSON Web Token",
	InstructionOverride:       "tells the reader to drop its previous instructions",
	InstructionRoleMarker:     "carries chat-template or role tags that impersonate the system",
	InstructionConcealment:    "tells the reader to hide what it does from the user",
	InstructionExfiltration:   "tells the reader to send credentials somewhere outside",
	HiddenComment:             "has a comment the console does not show",
	HiddenStyle:               "has text styled or flagged invisible",
	HiddenInstruction:         "hides instructions to the agent in text a reviewer does not see",
	UnicodeTag:                "carries invisible Unicode tag characters",
	UnicodeZeroWidth:          "carries zero-width characters",
	UnicodeBidi:               "carries bidirectional control characters that reorder what renders",
	BlobBase64:                "carries a long base64 blob",
	BlobHex:                   "carries a long hex blob",
	URLUnlisted:               "links to a host outside the URL allowlist",
	ReasonTaintedSession:      "a session that read outside text changed a binding rule",
	ReasonConstraintSupersede: "supersedes a constraint",
	ReasonConstraintDelete:    "deletes or archives a constraint",
	ReasonConstraintDemote:    "turns a constraint into another kind",
}

// Describe returns a code's owner-facing description ("" for an unknown one).
func Describe(c Code) string { return codeWords[c] }

// secretRes are the secret screens, for redaction. A private key redacts as
// its whole block -- header, body and footer -- since the body is the key.
var secretRes = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN (?:(?:RSA|EC|DSA|OPENSSH|ENCRYPTED|PGP) )?PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----|\z)`),
	awsKeyRe, githubRe, slackRe, anthropicRe, openaiRe, googleKeyRe, stripeRe, jwtRe,
}

// RedactSecrets replaces every match of a secret screen with a marker, so a
// held write can be reviewed -- and the review screen captured -- without the
// credential it carries.
func RedactSecrets(text string) string {
	for _, re := range secretRes {
		text = re.ReplaceAllString(text, "[redacted secret]")
	}
	return text
}
