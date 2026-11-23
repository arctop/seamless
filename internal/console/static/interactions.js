/* Shared Interactions client module (window.IX).
 *
 * All Interactions surfaces use it: the live feed (/console/interactions) builds
 * rows from JSON via IX.buildRow, while the project and session detail pages
 * upgrade their server-rendered rows via IX.enhance. One value renderer (JSON
 * pretty-print + syntax highlight + escape decode + line clamp), one section
 * builder (with a copy button wired to the global .copy-btn handler), one
 * highlights strip, and one volume-histogram renderer keep the three surfaces
 * visually and behaviorally aligned.
 *
 * All dynamic text enters the DOM via textContent (never innerHTML), so
 * highlighting stays XSS-safe. The only innerHTML use is for static, trusted
 * icon path constants below. */
(function (global) {
  'use strict';

  var TEXT_MAX_LINES = 12; // plain-text bodies taller than this start clamped
  var JSON_MAX_LINES = 14; // pretty-JSON bodies taller than this start clamped

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  // ---- icons (a small lucide subset, mirrors internal/console evtIcon) -------
  var ICON = {
    terminal: '<polyline points="4 17 10 11 4 5"/><line x1="12" x2="20" y1="19" y2="19"/>',
    brain: '<path d="M12 5a3 3 0 1 0-5.997.125 4 4 0 0 0-2.526 5.77 4 4 0 0 0 .556 6.588A4 4 0 1 0 12 18Z"/><path d="M12 5a3 3 0 1 1 5.997.125 4 4 0 0 1 2.526 5.77 4 4 0 0 1-.556 6.588A4 4 0 1 1 12 18Z"/><path d="M15 13a4.5 4.5 0 0 1-3-4 4.5 4.5 0 0 1-3 4"/>',
    search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
    circle: '<circle cx="12" cy="12" r="10"/>',
    'git-fork': '<circle cx="12" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><circle cx="18" cy="6" r="3"/><path d="M18 9v1a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2V9"/><path d="M12 12v3"/>',
    map: '<path d="M14.106 5.553a2 2 0 0 0 1.788 0l3.659-1.83A1 1 0 0 1 21 4.619v12.764a1 1 0 0 1-.553.894l-4.553 2.277a2 2 0 0 1-1.788 0l-4.212-2.106a2 2 0 0 0-1.788 0l-3.659 1.83A1 1 0 0 1 3 19.381V6.618a1 1 0 0 1 .553-.894l4.553-2.277a2 2 0 0 1 1.788 0z"/><path d="M15 5.764v15"/><path d="M9 3.236v15"/>',
    activity: '<polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/>',
    zap: '<path d="M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14z"/>',
    'trending-up': '<polyline points="22 7 13.5 15.5 8.5 10.5 2 17"/><polyline points="16 7 22 7 22 13"/>',
    flag: '<path d="M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z"/><line x1="4" x2="4" y1="22" y2="15"/>',
    award: '<path d="m15.477 12.89 1.515 8.526a.5.5 0 0 1-.81.47l-3.58-2.687a1 1 0 0 0-1.197 0l-3.586 2.686a.5.5 0 0 1-.81-.469l1.514-8.526"/><circle cx="12" cy="8" r="6"/>',
    copy: '<rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/>',
    check: '<path d="M20 6 9 17l-5-5"/>',
    trophy: '<path d="M6 9H4.5a2.5 2.5 0 0 1 0-5H6"/><path d="M18 9h1.5a2.5 2.5 0 0 0 0-5H18"/><path d="M4 22h16"/><path d="M10 14.66V17c0 .55-.47.98-.97 1.21C7.85 18.75 7 20.24 7 22"/><path d="M14 14.66V17c0 .55.47.98.97 1.21C16.15 18.75 17 20.24 17 22"/><path d="M18 2H6v7a6 6 0 0 0 12 0V2Z"/>',
    'triangle-alert': '<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
    pencil: '<path d="M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z"/><path d="m15 5 4 4"/>',
    eye: '<path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0"/><circle cx="12" cy="12" r="3"/>',
    archive: '<rect width="20" height="5" x="2" y="3" rx="1"/><path d="M4 8v11a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8"/><path d="M10 12h4"/>',
    'refresh-cw': '<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>',
    'folder-open': '<path d="m6 14 1.5-2.9A2 2 0 0 1 9.24 10H20a2 2 0 0 1 1.94 2.5l-1.54 6a2 2 0 0 1-1.95 1.5H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3.9a2 2 0 0 1 1.69.9l.81 1.2a2 2 0 0 0 1.67.9H18a2 2 0 0 1 2 2v2"/>',
    star: '<path d="M11.525 2.295a.53.53 0 0 1 .95 0l2.31 4.679a2.123 2.123 0 0 0 1.595 1.16l5.166.756a.53.53 0 0 1 .294.904l-3.736 3.638a2.123 2.123 0 0 0-.611 1.878l.882 5.14a.53.53 0 0 1-.771.56l-4.618-2.428a2.122 2.122 0 0 0-1.973 0L6.396 21.01a.53.53 0 0 1-.77-.56l.881-5.139a2.122 2.122 0 0 0-.611-1.879L2.16 9.795a.53.53 0 0 1 .294-.906l5.165-.755a2.122 2.122 0 0 0 1.597-1.16z"/>',
    'test-tube': '<path d="M14.5 2v17.5c0 1.4-1.1 2.5-2.5 2.5c-1.4 0-2.5-1.1-2.5-2.5V2"/><path d="M8.5 2h7"/><path d="M14.5 16h-5"/>',
    'list-checks': '<path d="m3 17 2 2 4-4"/><path d="m3 7 2 2 4-4"/><path d="M13 6h8"/><path d="M13 12h8"/><path d="M13 18h8"/>',
    sprout: '<path d="M7 20h10"/><path d="M10 20c5.5-2.5.8-6.4 3-10"/><path d="M9.5 9.4c1.1.8 1.8 2.2 2.3 3.7-2 .4-3.5.4-4.8-.3-1.2-.6-2.3-1.9-3-4.2 2.8-.5 4.4 0 5.5.8z"/><path d="M14.1 6a7 7 0 0 0-1.1 4c1.9-.1 3.3-.6 4.3-1.4 1-1 1.6-2.3 1.7-4.6-2.7.1-4 1-4.9 2z"/>'
  };

  function svg(name, cls) {
    return '<svg class="ico ' + (cls || '') + '" viewBox="0 0 24 24" fill="none" ' +
      'stroke="currentColor" stroke-width="1.75" stroke-linecap="round" ' +
      'stroke-linejoin="round" aria-hidden="true">' + (ICON[name] || ICON.activity) + '</svg>';
  }

  function iconEl(name, cls) {
    var s = el('span', 'ix-ico' + (cls ? ' ' + cls : ''));
    s.innerHTML = svg(name); // static path constant -- safe
    return s;
  }

  function evtIcon(kind) {
    if (kind === 'tool.call') return 'terminal';
    if (kind === 'retrieval.injected') return 'brain';
    if (kind === 'hook.prompt') return 'search';
    if (kind === 'subagent.captured') return 'git-fork';
    if (kind === 'memory.first_reuse') return 'zap';
    if (kind === 'project.stage_reached') return 'trending-up';
    if (kind === 'milestone.reached') return 'award';
    if (kind === 'plan.shipped') return 'flag';
    if (kind.indexOf('session.') === 0) return 'circle';
    if (kind.indexOf('plan.') === 0) return 'map';
    return 'activity';
  }

  // ---- value rendering -------------------------------------------------------
  var JSON_TOKEN = /("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false)\b|\b(null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g;

  function tryJSON(s) {
    var c = s.charAt(0);
    if (c !== '{' && c !== '[' && c !== '"') return null;
    try { return { v: JSON.parse(s) }; } catch (e) { return null; }
  }

  // Decode common backslash escapes for a single-line string that still carries
  // literal escape sequences; leave already-decoded text (real newlines/tabs) and
  // genuine backslashes (e.g. paths) untouched.
  function decodeEscapes(s) {
    if (/[\n\r\t]/.test(s)) return s;
    if (!/\\(?:u[0-9a-fA-F]{4}|[nrtbf"\\/])/.test(s)) return s;
    return s.replace(/\\u([0-9a-fA-F]{4})|\\([nrtbf"\\/])/g, function (m, u, c) {
      if (u) return String.fromCharCode(parseInt(u, 16));
      return { n: '\n', r: '\r', t: '\t', b: '\b', f: '\f', '"': '"', '\\': '\\', '/': '/' }[c] || m;
    });
  }

  function unescapeJSONText(tok) {
    return tok.replace(/\\u([0-9a-fA-F]{4})|\\([nrtbf"\\/])/g, function (m, u, c) {
      if (u) return String.fromCharCode(parseInt(u, 16));
      return { n: '\n', r: '', t: '\t', b: '', f: '', '"': '"', '\\': '\\', '/': '/' }[c];
    });
  }

  function jspan(cls, text) { return el('span', cls, text); }

  function highlightInto(pre, text) {
    JSON_TOKEN.lastIndex = 0;
    var last = 0, m;
    while ((m = JSON_TOKEN.exec(text)) !== null) {
      if (m.index > last) pre.appendChild(document.createTextNode(text.slice(last, m.index)));
      if (m[1] !== undefined) {
        if (m[2] !== undefined) {
          pre.appendChild(jspan('jk', unescapeJSONText(m[1])));
          pre.appendChild(document.createTextNode(m[2]));
        } else {
          pre.appendChild(jspan('js', unescapeJSONText(m[1])));
        }
      } else if (m[3] !== undefined) {
        pre.appendChild(jspan('jbool', m[3]));
      } else if (m[4] !== undefined) {
        pre.appendChild(jspan('jnull', m[4]));
      } else {
        pre.appendChild(jspan('jnum', m[5]));
      }
      last = JSON_TOKEN.lastIndex;
    }
    if (last < text.length) pre.appendChild(document.createTextNode(text.slice(last)));
  }

  // addToggle clamps a pre and appends a Show all/Show less button when the body
  // is taller than the threshold.
  function addToggle(wrap, pre, lines) {
    pre.classList.add('clamp');
    var btn = el('button', 'btn small ix-expand', 'Show all ' + lines + ' lines');
    btn.type = 'button';
    btn.onclick = function () {
      var collapsed = pre.classList.toggle('clamp');
      btn.textContent = collapsed ? ('Show all ' + lines + ' lines') : 'Show less';
    };
    wrap.appendChild(btn);
  }

  function countLines(s) {
    var n = 1;
    for (var i = 0; i < s.length; i++) if (s.charCodeAt(i) === 10) n++;
    return n;
  }

  function jsonBlock(value) {
    var pretty;
    try { pretty = JSON.stringify(value, null, 2); } catch (e) { return textBlock(String(value)); }
    var wrap = el('div', 'ix-val');
    var pre = el('pre', 'pre pre-json');
    highlightInto(pre, pretty);
    wrap.appendChild(pre);
    var lines = countLines(unescapeJSONText(pretty));
    if (lines > JSON_MAX_LINES) addToggle(wrap, pre, lines);
    return wrap;
  }

  function textBlock(text) {
    var wrap = el('div', 'ix-val');
    var pre = el('pre', 'pre');
    pre.textContent = text;
    wrap.appendChild(pre);
    var lines = countLines(text);
    if (lines > TEXT_MAX_LINES) addToggle(wrap, pre, lines);
    return wrap;
  }

  // renderValue turns a body string into a highlighted JSON block, decoded text,
  // or a plain clamped text block.
  function renderValue(value) {
    var s = String(value);
    var parsed = tryJSON(s.replace(/^\s+/, ''));
    if (parsed) {
      if (parsed.v !== null && typeof parsed.v === 'object') return jsonBlock(parsed.v);
      if (typeof parsed.v === 'string') return textBlock(parsed.v);
    }
    return textBlock(decodeEscapes(s));
  }

  // ---- section (a titled, copyable request/response panel) -------------------
  function section(title, raw) {
    var sec = el('section', 'ix-section');
    var head = el('div', 'ix-section-head');
    head.appendChild(el('span', 'ix-section-title', title));
    var copy = el('button', 'copy-btn ix-copy');
    copy.type = 'button';
    copy.title = 'Copy';
    copy.setAttribute('aria-label', 'Copy');
    copy.setAttribute('data-copy', String(raw)); // global .copy-btn handler reads this
    copy.innerHTML = svg('copy', 'ico-copy') + svg('check', 'ico-check');
    head.appendChild(copy);
    sec.appendChild(head);
    var body = el('div', 'ix-section-body');
    body.appendChild(renderValue(raw));
    sec.appendChild(body);
    return sec;
  }

  // ---- highlights strip ------------------------------------------------------
  function chip(label, value, tone) {
    var c = el('span', 'ix-chip' + (tone ? ' ' + tone : ''));
    c.appendChild(el('span', 'ix-chip-k', label));
    c.appendChild(el('span', 'ix-chip-v', value));
    return c;
  }

  function highlights(d) {
    var wrap = el('div', 'ix-highlights');
    var n = 0;
    if (d.sessionName || d.sessionId) { wrap.appendChild(chip('session', d.sessionName || shortId(d.sessionId))); n++; }
    if (d.project) { wrap.appendChild(chip('project', d.project)); n++; }
    if (d.ambient) { wrap.appendChild(chip('mode', 'ambient')); n++; }
    if (d.durationMs) { wrap.appendChild(chip('took', d.durationMs + 'ms')); n++; }
    if (d.items) { wrap.appendChild(chip('surfaced', d.items + (d.items === 1 ? ' memory' : ' memories'))); n++; }
    if (d.isError) { wrap.appendChild(chip('status', 'error', 'danger')); n++; }
    return n ? wrap : null;
  }

  function linkChip(label, value, href) {
    var c = el('a', 'ix-chip ix-chip-link');
    c.href = href;
    c.setAttribute('data-peek', '');
    c.appendChild(el('span', 'ix-chip-k', label));
    c.appendChild(el('span', 'ix-chip-v', value));
    return c;
  }

  // ---- agent attribution pill ------------------------------------------------
  // Mirrors agentPill in internal/console/agent.go (tone classes, short labels,
  // model shortening), so live-feed rows match the server-rendered surfaces.
  var AGENTS = {
    'claude-code': { cls: 'cc', label: 'cc', full: 'Claude Code' },
    codex: { cls: 'cx', label: 'cx', full: 'Codex' }
  };

  function modelShort(model) {
    var m = String(model || '').replace(/-20\d{6}$/, '');
    if (m.indexOf('claude-') === 0 && m.length > 7) m = m.slice(7);
    return m;
  }

  function agentPill(harness, model) {
    // hasOwnProperty guard: a harness value like 'constructor' or '__proto__'
    // must fall through to the neutral pass-through, not hit Object.prototype
    // and render with the label silently missing (agent.go: a future harness
    // must render, not vanish).
    var known = Object.prototype.hasOwnProperty.call(AGENTS, harness) ? AGENTS[harness] : null;
    var a = known || (harness ? { cls: '', label: harness, full: harness } : null);
    var text = [], title = [];
    if (a) { text.push(a.label); title.push(a.full); }
    var m = modelShort(model);
    if (m) text.push(m);
    if (model) title.push(model);
    if (!text.length) return null;
    var pill = el('span', 'agent-pill' + (a && a.cls ? ' ' + a.cls : ''), text.join(' · '));
    pill.title = title.join(' · ');
    return pill;
  }

  // setAgentPill reconciles one already-rendered row when a session's mutable
  // attribution becomes known later. Claude ambient sessions commonly start
  // before the transcript contains an assistant model; a later prompt/end row
  // should update the earlier live cards in place rather than requiring reload.
  function setAgentPill(row, harness, model) {
    var head = row && row.querySelector ? row.querySelector('.ix-row-head') : null;
    if (!head) return;
    var old = head.querySelector('.agent-pill');
    var next = agentPill(harness, model);
    if (old) {
      if (next) old.parentNode.replaceChild(next, old);
      else old.parentNode.removeChild(old);
      return;
    }
    if (!next) return;
    // Attribution belongs before ambient/error/meta badges, matching buildRow.
    var anchor = head.querySelector('.badge, .ix-meta');
    head.insertBefore(next, anchor || null);
  }

  // ---- time / id helpers -----------------------------------------------------
  function rel(ts) {
    var t = Date.parse(ts);
    if (isNaN(t)) return '';
    var s = Math.max(0, (Date.now() - t) / 1000);
    if (s < 1) return 'now';
    if (s < 60) return Math.floor(s) + 's';
    if (s < 3600) return Math.floor(s / 60) + 'm';
    if (s < 86400) return Math.floor(s / 3600) + 'h';
    return Math.floor(s / 86400) + 'd';
  }
  function shortId(id) { return id && id.length > 8 ? id.slice(-8) : (id || ''); }

  function sessionHref(id) { return '/console/sessions/' + encodeURIComponent(id); }
  function eventHref(id) { return '/console/events/' + encodeURIComponent(id); }

  // Sparse lifecycle/marker events have no request or response to inspect. Keep
  // their useful provenance visible in a compact second line and provide explicit
  // navigation; presenting a disclosure control with an empty body is a false
  // affordance.
  function staticMeta(d) {
    var meta = el('div', 'ix-static-meta');
    var facts = el('div', 'ix-static-facts');
    if (d.sessionId) {
      facts.appendChild(linkChip('session', d.sessionName || shortId(d.sessionId), sessionHref(d.sessionId)));
    }
    if (d.project) facts.appendChild(chip('project', d.project));
    meta.appendChild(facts);

    var event = el('a', 'ix-static-event', 'Event details \u2192');
    event.href = eventHref(d.id);
    event.setAttribute('data-peek', '');
    meta.appendChild(event);
    return meta;
  }

  // Stream severity -- mirrors evtSev/rowSev in internal/console/atoms.go, so a
  // live row is coded exactly like the same event on the session trace and the
  // Overview ledger. Four classes only: the left edge is scanned, not read.
  function rowSev(kind, isError) {
    if (isError) return 'danger';
    if (kind === 'agent.mishap' || kind === 'hook.error' || kind === 'update.failed') return 'danger';
    if (kind === 'retrieval.injected') return 'inject';
    if (kind === 'memory.written' || kind === 'note.written' || kind === 'trial.recorded') return 'write';
    return 'system';
  }

  // ---- buildRow (live feed) --------------------------------------------------
  function buildRow(d) {
    var expandable = !!(d.request || d.response);
    var row = el(expandable ? 'details' : 'article',
      'ix-row' + (d.tone ? ' tone-' + d.tone : '') + (d.isError ? ' err' : '') +
      (expandable ? ' ix-expandable' : ' ix-static'));
    row.setAttribute('data-id', d.id);
    row.setAttribute('data-session', d.sessionId || '');
    row.setAttribute('data-ts', d.ts || '');
    row.setAttribute('data-kind', d.kind || '');
    row.setAttribute('data-sev', rowSev(d.kind || '', !!d.isError));
    if (d.isError) row.setAttribute('data-err', '1');

    var sum = el(expandable ? 'summary' : 'div', 'ix-row-head');
    sum.appendChild(iconEl(d.icon || evtIcon(d.kind), 'ix-type'));
    var when = el('span', 'ix-when', rel(d.ts));
    when.title = d.ts;
    sum.appendChild(when);
    var chip = el('span', 'kind ' + (d.tone || ''), d.kindLabel || d.kind);
    chip.title = d.kind || '';
    sum.appendChild(chip);
    if (d.label) sum.appendChild(el('span', 'ix-label mono', d.label));
    sum.appendChild(el('span', 'ix-sum', d.summary || ''));
    var pill = agentPill(d.harness, d.model);
    if (pill) sum.appendChild(pill);
    if (d.ambient) sum.appendChild(el('span', 'badge', 'ambient'));
    if (d.isError) sum.appendChild(el('span', 'badge danger', 'error'));
    var meta = [];
    if (d.durationMs) meta.push(d.durationMs + 'ms');
    if (d.items) meta.push(d.items + (d.items === 1 ? ' item' : ' items'));
    if (meta.length) sum.appendChild(el('span', 'ix-meta', meta.join(' · ')));
    row.appendChild(sum);

    if (!expandable) {
      row.appendChild(staticMeta(d));
      return row;
    }

    var body = el('div', 'ix-body');
    var hl = highlights(d);
    if (hl) body.appendChild(hl);
    if (d.request) body.appendChild(section(d.kind === 'subagent.captured' ? 'Prompt' : 'Request', d.request));
    if (d.response) {
      var rh = 'Response';
      if (d.kind === 'retrieval.injected') rh = 'Injected content';
      else if (d.kind.indexOf('plan.') === 0) rh = 'Plan content';
      else if (d.kind === 'subagent.captured') rh = 'Report';
      body.appendChild(section(rh, d.response));
    }
    // data-peek on these links is consumed by the pane loader in layout.html:
    // on the live feed they open in the #detail-pane below the stream.
    var links = el('div', 'ix-links');
    if (d.sessionId) {
      var sl = el('a', null, (d.sessionName || shortId(d.sessionId)) + ' →');
      sl.href = sessionHref(d.sessionId);
      sl.setAttribute('data-peek', '');
      links.appendChild(sl);
    }
    var evl = el('a', null, 'event →');
    evl.href = eventHref(d.id);
    evl.setAttribute('data-peek', '');
    links.appendChild(evl);
    body.appendChild(links);
    row.appendChild(body);
    return row;
  }

  // ---- enhance (project-detail server-rendered rows) -------------------------
  // Upgrade each <pre class="ix-raw" data-ix-title="..."> emitted by the server
  // into a formatted, clamped, copyable section.
  function enhance(root) {
    var pres = (root || document).querySelectorAll('pre.ix-raw[data-ix-title]');
    for (var i = 0; i < pres.length; i++) {
      var pre = pres[i];
      var sec = section(pre.getAttribute('data-ix-title'), pre.textContent);
      // Event cards already provide a descriptive header. Move the copy action
      // into it instead of rendering a second, mostly-empty toolbar above the
      // value; this keeps full pages and constrained detail panes dense alike.
      var eventCard = pre.closest ? pre.closest('.event-content-card') : null;
      var eventHead = eventCard ? eventCard.querySelector('.event-card-head') : null;
      var sectionHead = sec.querySelector('.ix-section-head');
      var copy = sectionHead ? sectionHead.querySelector('.ix-copy') : null;
      if (eventHead && sectionHead && copy) {
        copy.classList.add('event-card-copy');
        eventHead.appendChild(copy);
        sectionHead.remove();
      }
      // Raw payload already has the outer <details> disclosure. A second
      // Show-all clamp only clips the JSON inside the inspector, so remove that
      // nested disclosure and turn the value itself into a keyboard-scrollable
      // region.
      var rawBody = pre.closest ? pre.closest('.event-raw-body') : null;
      if (rawBody) {
        var expand = sec.querySelector('.ix-expand');
        var payload = sec.querySelector('.pre, .pre-json');
        if (expand) expand.remove();
        if (payload) {
          payload.classList.remove('clamp');
          payload.tabIndex = 0;
          payload.setAttribute('aria-label', 'Scrollable raw payload');
        }
      }
      pre.parentNode.replaceChild(sec, pre);
    }
  }

  // ---- volume histogram ------------------------------------------------------
  var VOL_CATS = ['tool', 'inject', 'prompt', 'session', 'plan'];
  var VOL_META = {
    tool: 'Tools',
    inject: 'Injections',
    prompt: 'Prompts',
    session: 'Sessions',
    plan: 'Plans'
  };
  var volTipSeq = 0;

  function fmtClock(ts, seconds) {
    var t = Date.parse(ts);
    if (isNaN(t)) return '';
    var opts = { hour: 'numeric', minute: '2-digit' };
    if (seconds) opts.second = '2-digit';
    try { return new Date(t).toLocaleTimeString([], opts); }
    catch (e) { return ''; }
  }

  function fmtDay(ms) {
    var d = new Date(ms);
    var opts = { month: 'short', day: 'numeric' };
    if (d.getFullYear() !== new Date().getFullYear()) opts.year = 'numeric';
    try { return d.toLocaleDateString([], opts); }
    catch (e) { return ''; }
  }

  function sameDay(a, b) {
    var x = new Date(a), y = new Date(b);
    return x.getFullYear() === y.getFullYear() &&
      x.getMonth() === y.getMonth() && x.getDate() === y.getDate();
  }

  function fmtRange(from, to, sliceMs) {
    if (isNaN(from) || isNaN(to)) return '';
    var seconds = sliceMs < 60000;
    if (sameDay(from, to)) {
      return fmtDay(from) + ', ' + fmtClock(from, seconds) + '\u2013' + fmtClock(to, seconds);
    }
    return fmtDay(from) + ', ' + fmtClock(from, false) + ' \u2013 ' +
      fmtDay(to) + ', ' + fmtClock(to, false);
  }

  function bucketAria(b, range, action) {
    var parts = [range, b.n + ' interaction' + (b.n === 1 ? '' : 's')];
    VOL_CATS.forEach(function (cat) {
      if (b[cat]) parts.push(VOL_META[cat] + ' ' + b[cat]);
    });
    if (action === 'filter') parts.push('Activate to isolate this time slice');
    if (action === 'locate') parts.push('Activate to locate the event in the list');
    return parts.filter(Boolean).join('. ');
  }

  function fillVolumeTip(tip, b, range, action) {
    tip.textContent = '';
    tip.appendChild(el('div', 'ix-vol-tip-time', range));
    tip.appendChild(el('div', 'ix-vol-tip-total', b.n + ' interaction' + (b.n === 1 ? '' : 's')));
    var list = el('div', 'ix-vol-tip-list');
    VOL_CATS.forEach(function (cat) {
      if (!b[cat]) return;
      var row = el('div', 'ix-vol-tip-row');
      row.appendChild(el('span', 'ix-vol-tip-dot ' + cat));
      row.appendChild(el('span', 'ix-vol-tip-label', VOL_META[cat]));
      row.appendChild(el('strong', 'ix-vol-tip-n', b[cat]));
      list.appendChild(row);
    });
    tip.appendChild(list);
    if (action === 'filter') tip.appendChild(el('div', 'ix-vol-tip-action', 'Click to isolate this time slice'));
    if (action === 'locate') tip.appendChild(el('div', 'ix-vol-tip-action', 'Click to locate the event in the list'));
  }

  function placeVolumeTip(mount, tip, bar) {
    var mr = mount.getBoundingClientRect(), br = bar.getBoundingClientRect();
    var left = br.left - mr.left + br.width / 2 - tip.offsetWidth / 2;
    left = Math.max(6, Math.min(left, mount.clientWidth - tip.offsetWidth - 6));
    var top = br.top - mr.top - tip.offsetHeight - 10;
    if (mr.top + top < 8) top = br.bottom - mr.top + 10;
    tip.style.left = Math.round(left) + 'px';
    tip.style.top = Math.round(top) + 'px';
  }

  // renderVolume draws bucketed interaction volume as a stacked bar chart into
  // mount. Every non-empty bucket gets a visible hover/focus breakdown. An
  // opts.onSelect(fromMs, toMs, index, barEl, bucket) callback makes buckets
  // actionable. action="filter" describes the live time-slice toggle;
  // action="locate" describes the embedded charts' in-page row reveal.
  // canSelect can leave a bucket hover-only when its row is not in the snapshot.
  // Returns the bar elements for selection state on the live feed.
  function renderVolume(buckets, mount, opts) {
    opts = opts || {};
    mount.textContent = '';
    if (!buckets || !buckets.length) { mount.classList.add('empty'); return []; }
    var total = 0, maxN = 1;
    for (var j = 0; j < buckets.length; j++) { total += buckets[j].n; if (buckets[j].n > maxN) maxN = buckets[j].n; }
    if (!total) { mount.classList.add('empty'); return []; }
    mount.classList.remove('empty');

    var H = 46;
    var sliceMs = buckets.length > 1 ? (Date.parse(buckets[1].t) - Date.parse(buckets[0].t)) : 60000;
    if (!isFinite(sliceMs) || sliceMs <= 0) sliceMs = 60000;
    var track = el('div', 'ix-vol-track');
    var tip = el('div', 'ix-vol-tip');
    tip.id = 'ix-vol-tip-' + (++volTipSeq);
    tip.setAttribute('role', 'tooltip');
    tip.hidden = true;
    var bars = [];
    buckets.forEach(function (b, i) {
      var from = Date.parse(b.t), to = from + sliceMs;
      var selectable = b.n > 0 && (!opts.canSelect || opts.canSelect(b, from, to));
      var action = opts.onSelect && selectable ? (opts.action || 'filter') : '';
      var bar = el(action ? 'button' : 'div', 'ix-vbar');
      var barPx = b.n > 0 ? Math.max(2, Math.round(b.n / maxN * H)) : 0;
      bar.style.height = barPx + 'px';
      VOL_CATS.forEach(function (cat) {
        var cnt = b[cat] || 0;
        if (!cnt) return;
        var seg = el('div', 'ix-vseg ' + cat);
        seg.style.height = Math.max(1, Math.round(cnt / b.n * barPx)) + 'px';
        bar.appendChild(seg);
      });
      bar.setAttribute('data-from', String(from));
      bar.setAttribute('data-to', String(to));
      if (b.n > 0) {
        var range = fmtRange(from, to, sliceMs);
        bar.classList.add('has-data');
        bar.setAttribute('aria-label', bucketAria(b, range, action));
        bar.setAttribute('aria-describedby', tip.id);
        bar.onpointerenter = function () {
          fillVolumeTip(tip, b, range, action);
          tip.hidden = false;
          placeVolumeTip(mount, tip, bar);
        };
        bar.onpointerleave = function () { tip.hidden = true; };
        bar.onfocus = bar.onpointerenter;
        bar.onblur = bar.onpointerleave;
      }
      if (action) {
        bar.type = 'button';
        bar.classList.add('clickable');
        bar.onclick = (function (f, idx, elBar, bucket) {
          return function () { opts.onSelect(f, f + sliceMs, idx, elBar, bucket); };
        })(from, i, bar, b);
        if (action === 'filter') bar.setAttribute('aria-pressed', 'false');
      }
      track.appendChild(bar);
      bars.push(bar);
    });
    mount.appendChild(track);

    var axis = el('div', 'ix-vol-axis');
    axis.appendChild(el('span', 'ix-vol-t', fmtClock(buckets[0].t, false)));
    axis.appendChild(el('span', 'ix-vol-total', total + ' interactions'));
    axis.appendChild(el('span', 'ix-vol-t', 'now'));
    mount.appendChild(axis);
    mount.appendChild(tip);
    return bars;
  }

  // ---- the live feed's history window and arrival mark -------------------------
  // The live feed opens with the newest few interactions and adds older
  // history only when asked. What it remembers is only the window the owner
  // last loaded, in this browser: a
  // convenience, so storage that is blocked, full or absent (a private window)
  // remembers nothing rather than failing the page. Only a window the picker
  // offers (allowed) comes back; anything else reads as nothing remembered.
  var WINDOW_KEY = 'ix-history-window';

  function rememberedWindow(allowed) {
    try {
      var v = parseInt(localStorage.getItem(WINDOW_KEY), 10);
      return allowed.indexOf(v) !== -1 ? v : 0;
    } catch (e) {
      return 0;
    }
  }

  function rememberWindow(secs) {
    try { localStorage.setItem(WINDOW_KEY, String(secs)); } catch (e) {}
  }

  // arrivalDivider builds the mark between what arrived while the page was
  // open (above it) and the history the owner added (under it). at is the
  // page's arrival instant, an ISO timestamp, shown in this browser's clock.
  // It starts hidden: the feed shows it once loaded history is in view.
  function arrivalDivider(at) {
    var d = el('div', 'ix-arrived');
    d.setAttribute('role', 'separator');
    var label = el('span', 'ix-arrived-label', 'Before you arrived');
    var clock = fmtClock(at, false);
    if (clock) {
      label.appendChild(document.createTextNode(' · '));
      var t = el('time', null, clock);
      t.setAttribute('datetime', at);
      label.appendChild(t);
    }
    d.appendChild(label);
    d.hidden = true;
    return d;
  }

  // ---- category filter + volume mounting (server-rendered IX surfaces) --------
  // catOf maps an event kind to its filter/stack category (mirrors the server
  // volCategory and the live feed's local cat), so the .ix-seg segments and the
  // histogram bars agree.
  function catOf(kind) {
    if (kind === 'tool.call') return 'tool';
    if (kind === 'retrieval.injected') return 'inject';
    if (kind === 'hook.prompt') return 'prompt';
    if (kind.indexOf('session.') === 0) return 'session';
    if (kind.indexOf('plan.') === 0 || kind === 'subagent.captured') return 'plan';
    return '';
  }

  // wireKindFilter connects a segmented .ix-seg[data-cat] control to a
  // server-rendered .ix-feed, hiding rows by category (or errors-only). Shared by
  // the project-detail and session-detail surfaces; the live feed runs its own
  // compound filter (session AND category AND time bucket) instead.
  function wireKindFilter(kindsEl, feedEl) {
    if (!kindsEl || !feedEl) return;
    kindsEl.addEventListener('click', function (e) {
      var seg = e.target.closest ? e.target.closest('.ix-seg[data-cat]') : null;
      if (!seg) return;
      var c = seg.getAttribute('data-cat');
      var segs = kindsEl.querySelectorAll('.ix-seg');
      for (var i = 0; i < segs.length; i++) segs[i].classList.toggle('active', segs[i] === seg);
      var rows = feedEl.querySelectorAll('.ix-row');
      for (var j = 0; j < rows.length; j++) {
        var kind = rows[j].getAttribute('data-kind') || '';
        var show = c === 'all' ||
          (c === '__err__' ? rows[j].getAttribute('data-err') === '1' : catOf(kind) === c);
        rows[j].hidden = !show;
      }
    });
  }

  var revealTimer = null;

  function rowTime(row) {
    var raw = row.getAttribute('data-ts') || '';
    var n = Number(raw);
    if (raw && isFinite(n)) return n;
    return Date.parse(raw);
  }

  // rowForBucket prefers the bucket's newest event id. A project snapshot may
  // not include that exact row, so fall back to the newest rendered row inside
  // the same time slice. Buckets with no rendered row remain hover-only.
  function rowForBucket(feedEl, bucket, from, to) {
    if (!feedEl) return null;
    var rows = feedEl.querySelectorAll('.ix-row[data-id]');
    var fallback = null, fallbackTS = -Infinity;
    for (var i = 0; i < rows.length; i++) {
      if (bucket.latestId && rows[i].getAttribute('data-id') === bucket.latestId) return rows[i];
      var t = rowTime(rows[i]);
      if (!isNaN(t) && t >= from && t < to && t > fallbackTS) {
        fallback = rows[i];
        fallbackTS = t;
      }
    }
    return fallback;
  }

  // revealRow keeps chart navigation on the current page. It scrolls only when
  // the summary is outside the viewport, moves keyboard focus to that summary,
  // and leaves a conspicuous but temporary highlight behind.
  function revealRow(row) {
    if (!row) return false;
    if (revealTimer) clearTimeout(revealTimer);
    document.querySelectorAll('.ix-row.ix-located').forEach(function (old) {
      old.classList.remove('ix-located');
    });
    row.classList.remove('ix-located');
    void row.offsetWidth; // restart the highlight when the same bucket is clicked again
    row.classList.add('ix-located');

    var rect = row.getBoundingClientRect();
    var outside = rect.top < 16 || rect.bottom > window.innerHeight - 16;
    if (outside && row.scrollIntoView) {
      var reduced = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
      row.scrollIntoView({ behavior: reduced ? 'auto' : 'smooth', block: 'center', inline: 'nearest' });
    }
    var summary = row.querySelector('summary');
    if (summary && summary.focus) {
      try { summary.focus({ preventScroll: true }); } catch (e) { summary.focus(); }
    }
    revealTimer = setTimeout(function () {
      row.classList.remove('ix-located');
      revealTimer = null;
    }, 3200);
    return true;
  }

  function resetKindFilter(kindsEl) {
    if (!kindsEl) return;
    var all = kindsEl.querySelector('.ix-seg[data-cat="all"]');
    if (all && !all.classList.contains('active')) all.click();
  }

  // mountVolume renders the histogram a server embedded as JSON in el's data-vol
  // attribute. Buckets represented in feedEl locate and highlight their newest
  // visible row; the live feed calls renderVolume directly with time filtering.
  function mountVolume(el, feedEl, kindsEl) {
    if (!el) return;
    var buckets;
    try { buckets = JSON.parse(el.getAttribute('data-vol') || '[]'); } catch (e) { return; }
    renderVolume(buckets, el, {
      action: 'locate',
      canSelect: function (bucket, from, to) { return !!rowForBucket(feedEl, bucket, from, to); },
      onSelect: function (from, to, idx, bar, bucket) {
        var row = rowForBucket(feedEl, bucket, from, to);
        if (!row) return;
        resetKindFilter(kindsEl);
        revealRow(row);
      }
    });
    el.setAttribute('aria-hidden', buckets.length ? 'false' : 'true');
  }

  global.IX = {
    el: el,
    iconEl: iconEl,
    evtIcon: evtIcon,
    agentPill: agentPill,
    setAgentPill: setAgentPill,
    catOf: catOf,
    renderValue: renderValue,
    section: section,
    highlights: highlights,
    rel: rel,
    shortId: shortId,
    buildRow: buildRow,
    enhance: enhance,
    revealRow: revealRow,
    wireKindFilter: wireKindFilter,
    mountVolume: mountVolume,
    renderVolume: renderVolume,
    rememberedWindow: rememberedWindow,
    rememberWindow: rememberWindow,
    arrivalDivider: arrivalDivider
  };

  // Auto-upgrade any server-rendered <pre class="ix-raw" data-ix-title> on the
  // page into formatted, copyable IX sections once the DOM is ready, so a plain
  // detail page (event, session timeline, project tab) needs no per-page script
  // to get the feed's value rendering. Fragments injected later (the detail
  // pane) are enhanced by the pane loader in layout.html.
  function autoEnhance() { try { enhance(document); } catch (e) {} }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', autoEnhance);
  } else {
    autoEnhance();
  }
})(window);
