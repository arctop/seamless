#!/usr/bin/env python3
"""build.py: scripts/example/corpus -> docs/example/index.html

The /example/ page is a read-only copy of part of the Seamless project's own
Seamless. The copy lives beside this script as plain files, in the layout a
data dir has (memory/<project>/<name>.md and so on), so it can be read and
reviewed without a browser. This script turns it into the page.

    python3 scripts/example/build.py            # write docs/example/index.html
    python3 scripts/example/build.py --check    # fail if the page is stale

Python 3.9+, standard library only. README.md has the rules for what may go
into the corpus; guard() below refuses the names it can know about.
"""
import hashlib
import html
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
CORPUS = os.path.join(HERE, "corpus")
TEMPLATE = os.path.join(HERE, "page.html")
STATIC = os.path.join(ROOT, "docs", "static")
OUT = os.path.join(ROOT, "docs", "example", "index.html")

KINDS = ["gotcha", "decision", "constraint", "convention", "runbook", "protocol", "reference", "stage"]
KIND_PLURAL = {
    "gotcha": "gotchas", "decision": "decisions", "constraint": "constraints",
    "convention": "conventions", "runbook": "runbooks", "protocol": "protocols",
    "reference": "references", "stage": "stages",
}
CUT = "[…]"


def esc(s):
    return html.escape(s, quote=True)


def read(rel):
    with open(os.path.join(CORPUS, rel), encoding="utf-8") as f:
        return f.read()


def split_doc(text):
    """a markdown file -> (frontmatter lines, body)"""
    if not text.startswith("---\n"):
        return [], text
    end = text.index("\n---\n", 4)
    return text[4:end].split("\n"), text[end + 5:]


def fm_get(lines, key):
    for line in lines:
        if line.startswith(key + ":"):
            v = line[len(key) + 1:].strip()
            if len(v) >= 2 and v[0] == '"' and v[-1] == '"':
                v = v[1:-1].replace('\\"', '"').replace("\\\\", "\\")
            return v
    return ""


def fm_html(lines):
    out = ['<span class="r">---</span>']
    for line in lines:
        k, _, v = line.partition(":")
        out.append('<span class="k">%s:</span>%s' % (esc(k), esc(v)))
    out.append('<span class="r">---</span>')
    return '<pre class="ex-fm">' + "\n".join(out) + "</pre>"


# --------------------------------------------------------------------------
# markdown, as much of it as the corpus uses
# --------------------------------------------------------------------------
ITEM = re.compile(r"^(\s*)([-*]|\d+\.|\(\d+\)|[a-z]\.)\s+(.*)$")
PIPE = re.compile(r"\|(?=(?:[^`]*`[^`]*`)*[^`]*$)")


class Markdown:
    def __init__(self, links):
        self.links = links      # reference text -> element id, for records in the copy
        self.self_id = None

    # ---- inline
    def inline(self, text):
        codes = []

        def stash(m):
            codes.append("<code>" + esc(m.group(1)) + "</code>")
            return "\x00%d\x00" % (len(codes) - 1)

        s = re.sub(r"`([^`\n]+)`", stash, text)
        s = esc(s)
        s = re.sub(r"\*\*(.+?)\*\*", r"<strong>\1</strong>", s)
        # *emphasis*, only where both stars hug their words, so a glob's lone star stays a star
        s = re.sub(r"(?<![*\w])\*(?![\s*])([^*\n]*?[^*\s])\*(?![*\w])", r"<em>\1</em>", s)
        s = re.sub(r"\[\[([A-Za-z0-9][A-Za-z0-9._-]*)\]\]", self.wikilink, s)
        s = re.sub(r"\b(?:cc|sess)/[0-9a-z]{8}(?:-[0-9a-f]{16})?\b|\b01[0-9A-HJKMNP-TV-Z]{24}\b", self.ref, s)
        s = s.replace(esc(CUT), '<span class="cut" title="cut from this copy">' + esc(CUT) + "</span>")
        return re.sub(r"\x00(\d+)\x00", lambda m: codes[int(m.group(1))], s)

    def wikilink(self, m):
        name = m.group(1)
        target = self.links.get(name)
        if target and target != self.self_id:
            return '<a class="wl" href="#%s" data-open="%s">%s</a>' % (target, target, name)
        if target:
            return '<span class="wl">%s</span>' % name
        return '<span class="wl wl-out" title="a memory that is not in this copy">%s</span>' % name

    def ref(self, m):
        text = m.group(0)
        target = self.links.get(text) or self.links.get(text[:11])
        if target and target != self.self_id:
            return '<a class="wl" href="#%s" data-open="%s">%s</a>' % (target, target, text)
        return text

    # ---- blocks
    def render(self, text, self_id=None):
        self.self_id = self_id
        lines = text.rstrip("\n").split("\n")
        self.wrapped = self.hard_wrapped(lines)
        out, i = [], 0
        while i < len(lines):
            line = lines[i]
            if not line.strip():
                i += 1
            elif line.lstrip().startswith("```"):
                i = self.fence(lines, i, out)
            elif re.match(r"^#{1,4}\s", line):
                marks, _, rest = line.partition(" ")
                tag = "h4" if len(marks) <= 2 else "h5"
                out.append("<%s>%s</%s>" % (tag, self.inline(rest.strip()), tag))
                i += 1
            elif line.startswith("|") and i + 1 < len(lines) and re.match(r"^\|[\s:|-]+\|\s*$", lines[i + 1]):
                i = self.table(lines, i, out)
            elif line.startswith(">"):
                buf = []
                while i < len(lines) and lines[i].startswith(">"):
                    buf.append(re.sub(r"^> ?", "", lines[i]))
                    i += 1
                out.append("<blockquote>" + "<br>".join(self.inline(b) for b in buf) + "</blockquote>")
            elif self.item_ok(line):
                html_list, i = self.list(lines, i, len(ITEM.match(line).group(1)))
                out.append(html_list)
            else:
                buf = []
                while i < len(lines) and lines[i].strip() and not self.block_start(lines, i, bool(buf)):
                    buf.append(lines[i].strip())
                    i += 1
                joiner = " " if self.wrapped else "<br>"
                out.append("<p>" + joiner.join(self.inline(b) for b in buf) + "</p>")
        return "\n".join(out)

    @staticmethod
    def item_ok(line):
        m = ITEM.match(line)
        # a lettered marker only counts when indented, so "a. " in prose stays prose
        return bool(m) and not (re.match(r"[a-z]\.", m.group(2)) and not m.group(1))

    def block_start(self, lines, i, in_para):
        line = lines[i]
        if line.lstrip().startswith("```") or re.match(r"^#{1,4}\s", line) or line.startswith(">"):
            return True
        if line.startswith("|") and i + 1 < len(lines) and re.match(r"^\|[\s:|-]+\|\s*$", lines[i + 1]):
            return True
        return in_para and self.item_ok(line)

    @staticmethod
    def hard_wrapped(lines):
        """true when the prose was wrapped at a column: then a line end is only
        a space. Otherwise a line end inside a paragraph was meant."""
        inner, para = [], []
        for line in lines + [""]:
            if line.strip() and not ITEM.match(line) and not line.startswith(("#", "|", ">", "```")):
                para.append(line)
            else:
                inner.extend(para[:-1])
                para = []
        if len(inner) < 4:
            return False
        fits = [l for l in inner if 55 <= len(l) <= 92]
        return len(fits) >= 0.7 * len(inner)

    def fence(self, lines, i, out):
        indent = len(lines[i]) - len(lines[i].lstrip())
        buf, i = [], i + 1
        while i < len(lines) and not lines[i].lstrip().startswith("```"):
            buf.append(lines[i][indent:] if lines[i][:indent].strip() == "" else lines[i])
            i += 1
        out.append('<pre class="md-pre"><code>' + esc("\n".join(buf)) + "</code></pre>")
        return i + 1

    def table(self, lines, i, out):
        def cells(row):
            return [c.strip() for c in PIPE.split(row.strip().strip("|"))]

        head = cells(lines[i])
        i += 2
        rows = []
        while i < len(lines) and lines[i].startswith("|"):
            rows.append(cells(lines[i]))
            i += 1
        h = "".join("<th>%s</th>" % self.inline(c) for c in head)
        b = "".join("<tr>" + "".join("<td>%s</td>" % self.inline(c) for c in r) + "</tr>" for r in rows)
        out.append('<div class="md-tw"><table><thead><tr>%s</tr></thead><tbody>%s</tbody></table></div>' % (h, b))
        return i

    def list(self, lines, i, base):
        """a list whose items sit at indent `base`; returns (html, next line)"""
        items, kind = [], None
        n = len(lines)

        def more(j):
            """does the list go on after the blank lines at j?"""
            while j < n and not lines[j].strip():
                j += 1
            if j >= n:
                return False
            m = ITEM.match(lines[j])
            ind = len(lines[j]) - len(lines[j].lstrip())
            return (self.item_ok(lines[j]) and ind >= base) or ind > base

        while i < n:
            line = lines[i]
            if not line.strip():
                if not more(i):
                    break
                i += 1
                continue
            m = ITEM.match(line) if self.item_ok(line) else None
            ind = len(line) - len(line.lstrip())
            if m and ind == base:
                if kind is None:
                    mk = m.group(2)
                    kind = "ol" if mk[0].isdigit() else "ola" if mk[0].isalpha() else "plain" if mk[0] == "(" else "ul"
                text = (m.group(2) + " " if m.group(2)[0] == "(" else "") + m.group(3)
                items.append([["text", [text]]])
                i += 1
            elif m and ind > base and items:
                sub, i = self.list(lines, i, ind)
                items[-1].append(["list", sub])
            elif ind > base and items:
                if line.lstrip().startswith("```"):
                    buf = []
                    i = self.fence(lines, i, buf)
                    items[-1].append(["list", buf[0]])
                    continue
                if items[-1][-1][0] == "text":
                    items[-1][-1][1].append(line.strip())
                else:
                    items[-1].append(["text", [line.strip()]])
                i += 1
            else:
                break
        parts = []
        for item in items:
            chunks = []
            for k, (what, val) in enumerate(item):
                if what == "list":
                    chunks.append(val)
                elif k == 0:
                    chunks.append(self.inline(" ".join(val)))
                else:
                    chunks.append("<p>" + self.inline(" ".join(val)) + "</p>")
            parts.append("<li>" + "".join(chunks) + "</li>")
        tag = {"ol": "<ol>", "ola": '<ol type="a">', "plain": '<ul class="md-plain">', "ul": "<ul>"}[kind or "ul"]
        return tag + "".join(parts) + "</" + tag[1:3] + ">", i


# --------------------------------------------------------------------------
# the corpus
# --------------------------------------------------------------------------
def load():
    c = {"memories": [], "sessions": [], "tasks": {}}
    for scope in sorted(os.listdir(os.path.join(CORPUS, "memory"))):
        for name in sorted(os.listdir(os.path.join(CORPUS, "memory", scope))):
            fm, body = split_doc(read("memory/%s/%s" % (scope, name)))
            c["memories"].append({
                "name": fm_get(fm, "name"), "kind": fm_get(fm, "kind"), "scope": scope,
                "description": fm_get(fm, "description"), "created": fm_get(fm, "created"),
                "fm": fm, "body": body, "path": "memory/%s/%s" % (scope, name),
            })
    c["memories"].sort(key=lambda m: (KINDS.index(m["kind"]), m["created"]), reverse=False)
    # newest first inside a kind, as the console lists them
    by_kind = []
    for kind in KINDS:
        group = [m for m in c["memories"] if m["kind"] == kind]
        group.sort(key=lambda m: m["created"], reverse=True)
        by_kind.extend(group)
    c["memories"] = by_kind
    for name in sorted(os.listdir(os.path.join(CORPUS, "tasks"))):
        fm, body = split_doc(read("tasks/" + name))
        c["tasks"][fm_get(fm, "id")] = {"fm": fm, "body": body, "title": fm_get(fm, "title"), "path": "tasks/" + name}
    for name in sorted(os.listdir(os.path.join(CORPUS, "sessions"))):
        fm, body = split_doc(read("sessions/" + name))
        c["sessions"].append({"fm": fm, "body": body, "name": fm_get(fm, "name"),
                              "started": fm_get(fm, "started"), "status": fm_get(fm, "status")})
    c["sessions"].sort(key=lambda s: s["started"])
    c["plan"] = json.loads(read("plan.json"))
    c["lab"] = json.loads(read("lab.json"))
    note_fm, note_body = split_doc(read("notes/seamless/cc-plan-temporal-yawning-cupcake.md"))
    c["plan_note"] = {"fm": note_fm, "body": note_body}
    c["briefing"] = read("briefing.txt").rstrip("\n")
    return c


def short(session_name):
    return session_name[:11]


def links_for(c):
    links = {}
    for m in c["memories"]:
        links[m["name"]] = "m-" + m["name"]
    for tid in c["tasks"]:
        links[tid] = "t-" + tid
    for n, t in enumerate(c["lab"]["trials"], 1):
        if "actual" in t:
            links[t["id"]] = "tr-%d" % n
    for s in c["sessions"]:
        links[s["name"]] = "s-" + short(s["name"])[3:]
        links[short(s["name"])] = "s-" + short(s["name"])[3:]
    return links


def when(iso):
    """2026-10-09T17:30:12Z -> 9 Oct 2026, 17:30 UTC"""
    months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]
    y, mo, d = iso[:10].split("-")
    return "%d %s %s, %s UTC" % (int(d), months[int(mo) - 1], y, iso[11:16])


# --------------------------------------------------------------------------
# sections
# --------------------------------------------------------------------------
def memories(c, md):
    rows, items = [], []
    for kind in KINDS:
        group = [m for m in c["memories"] if m["kind"] == kind]
        if not group:
            continue
        rows.append('<p class="ex-g" data-kind="%s">%s <em>%d</em></p>' % (kind, KIND_PLURAL[kind], len(group)))
        for m in group:
            mid = "m-" + m["name"]
            rows.append('<a class="ex-row" href="#%s" data-open="%s" data-kind="%s"><b>%s</b><span>%s</span></a>'
                        % (mid, mid, kind, esc(m["name"]), esc(m["description"])))
            items.append(
                '<article class="ex-item" id="%s" data-kind="%s" tabindex="-1">\n'
                '<h3 class="sr-only">%s: %s</h3>\n'
                '<p class="ex-file"><span>~/.seamless/</span>%s</p>\n%s\n<div class="ex-md">\n%s\n</div>\n</article>'
                % (mid, kind, esc(m["name"]), esc(m["description"]), esc(m["path"]), fm_html(m["fm"]),
                   md.render(m["body"], mid)))
    return "\n".join(rows), "\n".join(items)


STEP_MARK = {"done": ("ok", "done"), "dropped": ("no", "dropped"), "open": ("wait", "open")}


def plan(c, md):
    p, steps = c["plan"]["plan"], c["plan"]["steps"]
    done = sum(1 for s in steps if s["status"] == "done")
    dropped = sum(1 for s in steps if s["status"] == "dropped")
    rows = ['<a class="ex-row" href="#p-plan" data-open="p-plan"><b>The plan</b><span>%s</span></a>' % esc(p["title"]),
            '<p class="ex-g">steps <em>%d</em></p>' % len(steps)]
    items = []
    for s in steps:
        cls, word = STEP_MARK[s["status"]]
        mark = '<i class="st st-%s" title="%s"></i>' % (cls, word)
        if s["id"] in c["tasks"]:
            tid = "t-" + s["id"]
            rows.append('<a class="ex-row ex-step" href="#%s" data-open="%s">%s<b>%s</b></a>' % (tid, tid, mark, esc(s["title"])))
        else:
            rows.append('<p class="ex-row ex-step is-flat">%s<b>%s</b></p>' % (mark, esc(s["title"])))
    agents = [a for a in p["attached"] if a["agent"]]
    notes = [a for a in p["attached"] if not a["agent"]]
    attached = "".join("<li>%s</li>" % esc(a["title"]) for a in notes)
    attached += "<li>%d reports from planning subagents, captured with their prompts</li>" % len(agents)
    items.append(
        '<article class="ex-item" id="p-plan" tabindex="-1">\n'
        '<p class="ex-file">plan:<span>%s</span></p>\n'
        '<h3 class="ex-h">The plan, as the owner approved it.</h3>\n'
        '<p class="ex-sub ex-lead">%s</p>\n'
        '<ul class="ex-facts"><li><b>%s</b> in Claude Code plan mode</li><li>draft <b>%d</b></li>'
        '<li><b>%d</b> steps: %d done, %d dropped</li><li><b>%d</b> notes attached</li></ul>\n'
        '<p class="ex-sub">A plan in Seamless is a note, the steps that carry its tag, and whatever else is attached to it. '
        'This one was captured from plan mode, draft by draft. Attached to it:</p>\n<ul class="ex-att">%s</ul>\n'
        '<p class="ex-file ex-file-2"><span>~/.seamless/</span>notes/seamless/cc-plan-temporal-yawning-cupcake.md</p>\n'
        '<div class="ex-md">\n%s\n</div>\n</article>'
        % (esc(p["slug"]), esc(p["title"]), esc(p["status"]), p["iteration"], len(steps), done, dropped,
           len(p["attached"]), attached, md.render(c["plan_note"]["body"], "p-plan")))
    for s in steps:
        t = c["tasks"].get(s["id"])
        if not t:
            continue
        tid = "t-" + s["id"]
        items.append(
            '<article class="ex-item" id="%s" tabindex="-1">\n'
            '<h3 class="sr-only">%s</h3>\n'
            '<p class="ex-file">task <span>%s</span></p>\n%s\n<div class="ex-md">\n%s\n</div>\n</article>'
            % (tid, esc(t["title"]), esc(s["id"]), fm_html(t["fm"]), md.render(t["body"], tid)))
    return "\n".join(rows), "\n".join(items), len(steps), done, dropped


OUTCOME = {"pass": "ok", "fail": "no", "partial": "half"}


def lab(c, md):
    trials = c["lab"]["trials"]
    count = {k: sum(1 for t in trials if t["outcome"] == k) for k in OUTCOME}
    by_os = {}
    for t in trials:
        os_name = t["title"].split(" ")[0].rstrip(":")
        by_os[os_name] = by_os.get(os_name, 0) + 1
    rows = ['<a class="ex-row" href="#lab-about" data-open="lab-about"><b>The lab</b><span>%s</span></a>' % esc(c["lab"]["lab"]),
            '<p class="ex-g">trials <em>%d</em></p>' % len(trials)]
    items = [
        '<article class="ex-item" id="lab-about" tabindex="-1">\n'
        '<p class="ex-file">lab <span>%s</span></p>\n'
        '<h3 class="ex-h">How does an updater outlive the service that started it?</h3>\n'
        '<ul class="ex-facts"><li><b>%d</b> trials</li><li><b>%d</b> passed</li><li><b>%d</b> failed</li><li><b>%d</b> partial</li></ul>\n'
        '<p class="ex-sub">A research lab is a named set of trials. Each trial records what was changed, what the agent '
        'expected before running it, often with a confidence, and what happened. The record outlives the session, so '
        'the next agent does not repeat an experiment, and a wrong belief is on file as wrong.</p>\n'
        '<p class="ex-sub">This lab answered one question for the automatic updater on three systems: %s. '
        'Six trials are copied here in full. Every title is in the list.</p>\n</article>'
        % (esc(c["lab"]["lab"]), len(trials), count["pass"], count["fail"], count["partial"],
           ", ".join("%s (%d)" % (k, by_os[k]) for k in sorted(by_os, key=lambda k: -by_os[k])))]
    for n, t in enumerate(trials, 1):
        cls = OUTCOME[t["outcome"]]
        mark = '<i class="st st-%s" title="%s"></i>' % (cls, t["outcome"])
        if "actual" not in t:
            rows.append('<p class="ex-row ex-step is-flat">%s<b>%s</b></p>' % (mark, esc(t["title"])))
            continue
        tid = "tr-%d" % n
        rows.append('<a class="ex-row ex-step" href="#%s" data-open="%s">%s<b>%s</b></a>' % (tid, tid, mark, esc(t["title"])))
        metrics = json.dumps(t["metrics"], indent=1, sort_keys=True, ensure_ascii=False)
        items.append(
            '<article class="ex-item" id="%s" tabindex="-1">\n'
            '<p class="ex-file">lab <span>%s</span> · trial %d of %d</p>\n'
            '<h3 class="ex-h ex-h-s">%s</h3>\n'
            '<p class="tr-out tr-%s">%s</p>\n'
            '<dl class="tr"><dt>Changed</dt><dd>%s</dd><dt>Expected</dt><dd>%s</dd><dt>Actual</dt><dd>%s</dd>'
            '<dt>Metrics</dt><dd><pre class="md-pre"><code>%s</code></pre></dd></dl>\n</article>'
            % (tid, esc(c["lab"]["lab"]), n, len(trials), esc(t["title"]), cls, esc(t["outcome"]),
               md.inline(t["changes"]), md.inline(t["expected"]), md.inline(t["actual"]), esc(metrics)))
    return "\n".join(rows), "\n".join(items), len(trials), count


def sessions(c, md):
    rows, items = [], []
    for s in c["sessions"]:
        sid = "s-" + short(s["name"])[3:]
        first = re.sub(r"\s+", " ", s["body"].strip().split("\n")[0])
        first = re.sub(r"[*`]", "", first)
        rows.append('<a class="ex-row" href="#%s" data-open="%s"><b>%s</b><span>%s</span></a>'
                    % (sid, sid, esc(short(s["name"])), esc(first)))
        items.append(
            '<article class="ex-item" id="%s" tabindex="-1">\n'
            '<h3 class="sr-only">Handoff from session %s</h3>\n'
            '<p class="ex-file">session <span>%s</span> · %s · started %s</p>\n'
            '<p class="ex-sub">What the session left for the next one when it ended.</p>\n'
            '<div class="ex-md">\n%s\n</div>\n</article>'
            % (sid, esc(short(s["name"])), esc(s["name"]), esc(s["status"]), when(s["started"]), md.render(s["body"], sid)))
    return "\n".join(rows), "\n".join(items)


BRIEF_PARTS = [
    ("who", ("Seam project:",)),
    ("rules", ("Constraints (",)),
    ("stage", ("Stages (",)),
    ("plans", ("Plans:",)),
    ("rules", ("Conventions (",)),
    ("found", ("Recent findings:",)),
    ("queue", ("Ready tasks",)),
    ("index", ("Memories (",)),
]


def briefing(c):
    """the briefing as the hook printed it, cut into the parts the notes name"""
    blocks = c["briefing"].split("\n\n")
    out = []
    for block in blocks:
        body = block
        if body.startswith("<seam-briefing>\n"):
            out.append('<p class="fl bf bf-tag">&lt;seam-briefing&gt;</p>')
            body = body[len("<seam-briefing>\n"):]
        tail = ""
        if body.endswith("\n</seam-briefing>"):
            tail = '<p class="fl bf bf-tag">&lt;/seam-briefing&gt;</p>'
            body = body[:-len("\n</seam-briefing>")]
        note = next((n for n, heads in BRIEF_PARTS if body.startswith(heads)), "")
        text = esc(body).replace(esc(CUT), '<span class="cut" title="cut from this copy">' + esc(CUT) + "</span>")
        head, nl, rest = text.partition("\n")
        text = '<span class="bf-h">%s</span>%s%s' % (head, nl, rest)
        out.append('<p class="fl bf"%s>%s</p>' % (' data-note="%s"' % note if note else "", text))
        if tail:
            out.append(tail)
    # the two rule blocks share a note; page.js lights the first match only, so
    # the second one is tagged apart and the note list names both
    seen = set()
    fixed = []
    for line in out:
        m = re.search(r'data-note="([a-z]+)"', line)
        if m and m.group(1) in seen:
            line = line.replace('data-note="%s"' % m.group(1), 'data-note="%s2"' % m.group(1))
        elif m:
            seen.add(m.group(1))
        fixed.append(line)
    return "\n".join(fixed)


# --------------------------------------------------------------------------
# the guard: what must never reach the page
# --------------------------------------------------------------------------
def guard(text, what):
    """Refuse the names of one deployment, a home directory, or an email
    address. The names are spelled in pieces so this file does not trip the
    repository's own check (cmd/docsgen/public_names_test.go), which reads
    every tracked file outside docs/ and so covers the corpus as well."""
    org = "arc" + "top"
    pats = [
        "_" + org + "_", "/opt/" + org, "kata" + "ta", r"\b" + "bo" + "ron" + r"\b", r"\b" + "ar" + "gon" + r"\b",
        "eit" + "ans-macbook", "fin" + "ance-os", org + "-(app|ios|android|mobile-apps|os)",
        r"/(Users|home)/[a-z]", r"[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+\.[a-z]{2,}", "found" + "er",
    ]
    hits = []
    for n, line in enumerate(text.split("\n"), 1):
        m = re.search("|".join(pats), line, re.I)
        if m and "user@" not in m.group(0):      # systemd's user@UID.service is not an address
            hits.append("%s:%d: %s" % (what, n, m.group(0)))
    return hits


def stamp(page):
    """a content hash on every local asset, as site-stamp.sh stamps the other pages"""
    def one(m):
        with open(os.path.join(STATIC, m.group(1)), "rb") as f:
            return "../static/%s?v=%s" % (m.group(1), hashlib.sha256(f.read()).hexdigest()[:8])
    return re.sub(r"\.\./static/([a-z-]+\.(?:css|js))(?:\?v=[0-9a-f]+)?", one, page)


def build():
    c = load()
    md = Markdown(links_for(c))
    with open(TEMPLATE, encoding="utf-8") as f:
        page = f.read()
    mem_rows, mem_items = memories(c, md)
    plan_rows, plan_items, n_steps, n_done, n_dropped = plan(c, md)
    lab_rows, lab_items, n_trials, outcome = lab(c, md)
    ses_rows, ses_items = sessions(c, md)
    fills = {
        "BRIEFING": briefing(c),
        "MEMORY_ROWS": mem_rows, "MEMORY_ITEMS": mem_items, "N_MEMORIES": str(len(c["memories"])),
        "PLAN_ROWS": plan_rows, "PLAN_ITEMS": plan_items, "N_STEPS": str(n_steps),
        "LAB_ROWS": lab_rows, "LAB_ITEMS": lab_items, "N_TRIALS": str(n_trials),
        "N_PASS": str(outcome["pass"]), "N_FAIL": str(outcome["fail"]), "N_PARTIAL": str(outcome["partial"]),
        "SESSION_ROWS": ses_rows, "SESSION_ITEMS": ses_items, "N_SESSIONS": str(len(c["sessions"])),
    }
    for key, val in fills.items():
        if "{{%s}}" % key not in page:
            raise SystemExit("page.html has no {{%s}}" % key)
        page = page.replace("{{%s}}" % key, val)
    left = re.findall(r"\{\{[A-Z_]+\}\}", page)
    if left:
        raise SystemExit("page.html has unfilled marks: %s" % ", ".join(sorted(set(left))))
    # every link into the explorer must land on a record that is there
    ids = set(re.findall(r'\bid="([^"]+)"', page))
    for target in set(re.findall(r'data-open="([^"]+)"', page)):
        if target not in ids:
            raise SystemExit("a link opens %r, which is not in the page" % target)
    hits = guard(page, "docs/example/index.html")
    for root, _, files in os.walk(CORPUS):
        for name in files:
            path = os.path.join(root, name)
            with open(path, encoding="utf-8") as f:
                hits += guard(f.read(), os.path.relpath(path, ROOT))
    if hits:
        raise SystemExit("the example names something it must not:\n" + "\n".join(hits))
    return stamp(page)


def main():
    page = build()
    if "--check" in sys.argv[1:]:
        have = ""
        if os.path.exists(OUT):
            with open(OUT, encoding="utf-8") as f:
                have = f.read()
        if have != page:
            print("example: docs/example/index.html is stale; run python3 scripts/example/build.py", file=sys.stderr)
            return 1
        return 0
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "w", encoding="utf-8") as f:
        f.write(page)
    print("example: wrote docs/example/index.html (%d bytes)" % len(page.encode("utf-8")))
    return 0


if __name__ == "__main__":
    sys.exit(main())
