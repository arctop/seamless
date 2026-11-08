/* Seamless console -- the Privacy screen's drawings (templates/privacy.html):
   the machine (who can reach what, in three setups, with a compromised agent
   switched on or off), the flow map of where each project's knowledge may
   go, the four fences' small diagrams, and the fence chooser.

   The server owns every name and number (the JSON in #pv-data); this script
   owns the pictures and their state. Motion is a few dots riding SVG paths,
   paced to 30 fps, for twelve seconds after a drawing comes into view or the
   owner changes it; it rests otherwise, and is absent under reduced motion. */
(function () {
  'use strict';
  var NS = 'http://www.w3.org/2000/svg';
  var dataEl = document.getElementById('pv-data');
  if (!dataEl) return;
  var D;
  try { D = JSON.parse(dataEl.textContent); } catch (e) { return; }
  var reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)');

  function el(name, attrs, parent) {
    var e = document.createElementNS(NS, name);
    for (var k in attrs) if (attrs[k] != null) e.setAttribute(k, String(attrs[k]));
    if (parent) parent.appendChild(e);
    return e;
  }
  function txt(parent, x, y, cls, s, anchor) { var t = el('text', { x: x, y: y, class: cls, 'text-anchor': anchor || 'start' }, parent); t.textContent = s; return t; }
  function plural(n, w) { return n + ' ' + w + (n === 1 ? '' : 's'); }

  /* ---- motion: dots riding paths ------------------------------------- */

  var riders = [], shown = {}, timer = 0, t0 = performance.now(), until = 0;
  function ride(path, cls, owner, o) {
    o = o || {};
    for (var i = 0; i < (o.n || 1); i++) {
      var dot = el('circle', { r: o.r || 3.6, class: 'pv-dot ' + cls }, path.parentNode);
      riders.push({ path: path, dot: dot, len: 0, speed: o.speed || 0.3, phase: i / (o.n || 1) + (o.offset || 0), mode: o.mode || 'loop', stopAt: o.stopAt || 0.86, owner: owner });
    }
  }
  function place(t) {
    riders.forEach(function (r) {
      if (!shown[r.owner] || !r.dot.isConnected) return;
      if (!r.len) r.len = r.path.getTotalLength();
      var u = (t * r.speed + r.phase) % 1, a = 1, k = u;
      if (r.mode === 'stop') { k = Math.min(u / 0.8, 1) * r.stopAt; a = u < 0.8 ? 1 : 1 - (u - 0.8) / 0.2; }
      else if (r.mode === 'bounce') { k = u < 0.5 ? u * 2 * r.stopAt : (1 - u) * 2 * r.stopAt; }
      else { a = Math.min(1, u * 6, (1 - u) * 6); }
      var p = r.path.getPointAtLength(k * r.len);
      r.dot.setAttribute('cx', p.x.toFixed(1));
      r.dot.setAttribute('cy', p.y.toFixed(1));
      r.dot.setAttribute('opacity', a.toFixed(2));
    });
  }
  function tick() {
    timer = 0;
    var now = performance.now();
    if (document.hidden || (reduce && reduce.matches)) return;
    place((now - t0) / 1000);
    if (now > until) return;
    timer = setTimeout(function () { requestAnimationFrame(tick); }, 33);
  }
  // Dots illustrate where knowledge may go; they run for twelve seconds after
  // a drawing comes into view or the owner changes it, then rest in place.
  function wake() { until = performance.now() + 12000; if (!timer) timer = setTimeout(function () { requestAnimationFrame(tick); }, 0); }
  function watch(node, id) {
    if (!('IntersectionObserver' in window)) { shown[id] = true; return; }
    new IntersectionObserver(function (es) {
      var was = !!shown[id];
      shown[id] = es[0].isIntersecting;
      if (!was && shown[id]) wake();
    }).observe(node);
  }
  document.addEventListener('visibilitychange', function () { if (!document.hidden) wake(); });

  /* ---- the machine ------------------------------------------------------ */

  var P = D.plan || {}, C = P.counts || {};
  var dedicated = P.mode === 'dedicated';
  var owner = P.owner || 'you';
  var svcUser = dedicated ? (P.daemonUser || '_seamless') : '_seamless';
  var svcData = dedicated ? (P.dataDir || '/opt/seamless/data') : '/opt/seamless/data';
  var agentUser = (P.restricted && P.restricted[0]) || '_agent';
  function pc(key, word) { var n = C[key] || 0; return plural(n, word || 'project'); }

  function lockedNode(x, y, w) {
    if (P.locking) return { id: 'p-locked', k: 'data', x: x, y: y, w: w, l1: 'Locked', l2: (C.locked ? pc('locked') + ', ' : '') + 'opens with your approval' };
    return { id: 'p-locked', k: 'data', x: x, y: y, w: w, l1: 'Locked', l2: 'opens with your approval' };
  }

  var MODES = {
    shared: {
      rooms: [{ x: 14, y: 14, w: 932, h: 412, name: 'Your account', sub: owner + ', the owner' }],
      boxes: [{ x: 630, y: 54, w: 300, h: 356, name: dedicated ? '~/.seamless' : (P.dataDir || '~/.seamless'), sub: 'Seamless data, readable by anything you run' }],
      nodes: [
        { id: 'cc', k: 'agent', x: 40, y: 118, w: 156, l1: 'Claude Code', l2: 'agent' },
        { id: 'cx', k: 'agent', x: 40, y: 262, w: 156, l1: 'Codex', l2: 'agent' },
        { id: 'ssh', x: 250, y: 50, w: 168, l1: '~/.ssh', l2: 'SSH keys' },
        { id: 'repos', x: 250, y: 118, w: 168, l1: '~/repos', l2: 'your code' },
        { id: 'browser', x: 250, y: 186, w: 168, l1: 'Browser data', l2: 'logins, cookies' },
        { id: 'cfg', k: 'key', x: 250, y: 316, w: 168, l1: 'seamless.yaml', l2: 'holds the bearer key' },
        { id: 'daemon', k: 'daemon', x: 446, y: 200, w: 158, l1: 'seamlessd', l2: 'runs as ' + owner },
        { id: 'p-open', k: 'data', x: 650, y: 94, w: 262, l1: 'Open projects', l2: pc('open') },
        { id: 'p-conf', k: 'data', x: 650, y: 166, w: 262, l1: 'Confidential', l2: pc('confidential') },
        { id: 'p-sealed', k: 'data', x: 650, y: 238, w: 262, l1: 'Sealed', l2: pc('sealed') },
        { id: 'p-locked', k: 'ghost', x: 650, y: 330, w: 262, l1: 'Locked', l2: 'needs its own account' }
      ],
      doors: [{ id: 'tcp', x: 477, y: 160, l: 'TCP + key' }],
      ok: [['cc', 'tcp'], ['cx', 'tcp'], ['tcp', 'daemon'], ['daemon', 'p-open'], ['daemon', 'p-conf'], ['daemon', 'p-sealed']],
      risk: [['cc', 'ssh'], ['cc', 'repos'], ['cc', 'browser'], ['cc', 'cfg'], ['cfg', 'daemon'], ['cc', 'p-open', 'over'], ['cc', 'p-conf', 'over'], ['cc', 'p-sealed', 'over']],
      hit: { ssh: 'READ', repos: 'READ', browser: 'READ', cfg: 'KEY TAKEN', daemon: 'AS YOU', 'p-open': 'FILES READ', 'p-conf': 'FILES READ', 'p-sealed': 'FILES READ' },
      calm: [['safe', 'Agents reach Seamless on <b>' + esc(P.addr || '127.0.0.1:8081') + '</b> with the bearer key.'],
        ['safe', 'Seamless applies each project&rsquo;s fence to every read and write it serves.'],
        ['gate', '<b>Locked projects aren&rsquo;t available</b> here: a lock needs Seamless in its own account.']],
      calmFoot: 'Turn on a compromised agent to see what the fences can&rsquo;t stop in this setup.',
      breach: [['risk', '<b>Reads every project&rsquo;s files directly</b>, sealed ones too. Fences are rules inside Seamless; a file read never meets them.'],
        ['risk', '<b>Takes the bearer key</b> from seamless.yaml and acts as you: changes a project&rsquo;s isolation, archives memories, approves its own access requests.'],
        ['risk', '<b>Edits memory files</b>, so other agents&rsquo; briefings carry its words. Write screening quarantines files changed outside Seamless.'],
        ['risk', '<b>Reaches the rest of your account</b>: SSH keys, code, browser data. That comes from your account, not from Seamless.']],
      breachFoot: 'To stop a compromised agent at the files, Seamless has to run as its own account.'
    },
    dedicated: {
      rooms: [{ x: 14, y: 14, w: 560, h: 412, name: 'Your account', sub: owner + ', the owner' }, { x: 616, y: 14, w: 330, h: 412, name: 'Seamless account', sub: svcUser + ', runs the daemon' }],
      boxes: [{ x: 634, y: 132, w: 294, h: 280, name: svcData, sub: 'mode 0700: only ' + svcUser + ' opens it' }],
      nodes: [
        { id: 'cc', k: 'agent', x: 40, y: 118, w: 156, l1: 'Claude Code', l2: 'agent' },
        { id: 'cx', k: 'agent', x: 40, y: 262, w: 156, l1: 'Codex', l2: 'agent' },
        { id: 'ssh', x: 236, y: 50, w: 168, l1: '~/.ssh', l2: 'SSH keys' },
        { id: 'repos', x: 236, y: 118, w: 168, l1: '~/repos', l2: 'your code' },
        { id: 'browser', x: 236, y: 186, w: 168, l1: 'Browser data', l2: 'logins, cookies' },
        { id: 'cfg', k: 'key', x: 236, y: 316, w: 168, l1: 'Client config', l2: 'the key, or none on the socket' },
        { id: 'daemon', k: 'daemon', x: 650, y: 56, w: 262, l1: 'seamlessd', l2: 'runs as ' + svcUser },
        { id: 'p-open', k: 'data', x: 652, y: 176, w: 258, l1: 'Open projects', l2: pc('open') },
        { id: 'p-conf', k: 'data', x: 652, y: 234, w: 258, l1: 'Confidential', l2: pc('confidential') },
        { id: 'p-sealed', k: 'data', x: 652, y: 292, w: 258, l1: 'Sealed', l2: pc('sealed') },
        lockedNode(652, 350, 258)
      ],
      doors: [{ id: 'tcp', x: 548, y: 112, l: 'TCP + key' }, { id: 'sock', x: 548, y: 258, l: 'Socket' }],
      ok: [['cc', 'tcp'], ['cx', 'sock'], ['tcp', 'daemon'], ['sock', 'daemon'], ['daemon', 'p-open'], ['daemon', 'p-conf'], ['daemon', 'p-sealed']],
      gateOk: [['daemon', 'p-locked']],
      risk: [['cc', 'ssh'], ['cc', 'repos'], ['cc', 'browser'], ['cc', 'cfg'], ['cfg', 'tcp'], ['tcp', 'daemon'], ['daemon', 'p-open'], ['daemon', 'p-conf'], ['daemon', 'p-sealed']],
      gate: [['daemon', 'p-locked']],
      blocked: [{ from: 'cc', to: [616, 392], tag: 'PERMISSION DENIED', tx: 470, ty: 404 }],
      hit: { ssh: 'READ', repos: 'READ', browser: 'READ', cfg: 'KEY TAKEN', daemon: 'AS YOU', 'p-open': 'VIA SEAMLESS', 'p-conf': 'VIA SEAMLESS', 'p-sealed': 'VIA SEAMLESS' },
      held: { 'p-locked': 'ASKS YOU' },
      calm: [['safe', 'The data folder belongs to <b>' + esc(svcUser) + '</b>; the operating system keeps every other account out.'],
        ['safe', 'Your agents reach Seamless through the doors: TCP with the key, or the socket, where the kernel names the caller.'],
        ['gate', '<b>Locked projects open only with your approval</b>, for one agent, for a time you choose.']],
      calmFoot: 'Turn on a compromised agent to see where the walls hold.',
      breach: [['safe', '<b>Can&rsquo;t open the data folder.</b> It belongs to ' + esc(svcUser) + ' with mode 0700, and the operating system refuses.'],
        ['risk', '<b>Still acts as you through Seamless</b>: open, confidential and sealed projects answer it, and every call lands in the event log.'],
        ['gate', '<b>Locked projects stay shut.</b> Opening one takes your Touch ID, a passkey or your password, which an agent can&rsquo;t produce.'],
        ['risk', '<b>Your home folder is still exposed.</b> That is your account&rsquo;s reach; moving Seamless doesn&rsquo;t change it.']],
      breachFoot: 'Give unattended agents their own account too, and a compromised one never reaches your home folder.'
    },
    restricted: {
      rooms: [{ x: 14, y: 14, w: 560, h: 196, name: 'Your account', sub: owner + ', the owner' }, { x: 14, y: 230, w: 560, h: 196, name: 'Agent account', sub: agentUser + ', restricted' },
        { x: 616, y: 14, w: 330, h: 412, name: 'Seamless account', sub: svcUser + ', runs the daemon' }],
      boxes: [{ x: 634, y: 132, w: 294, h: 280, name: svcData, sub: 'mode 0700: only ' + svcUser + ' opens it' }],
      nodes: [
        { id: 'cc', k: 'agent', x: 40, y: 82, w: 156, l1: 'Claude Code', l2: 'your agent' },
        { id: 'ssh', x: 236, y: 52, w: 160, l1: '~/.ssh', l2: 'SSH keys' },
        { id: 'repos', x: 236, y: 120, w: 160, l1: '~/repos', l2: 'your code' },
        { id: 'ra', k: 'agent', x: 40, y: 300, w: 156, l1: 'Unattended agent', l2: 'runs as ' + agentUser },
        { id: 'rahome', x: 236, y: 300, w: 160, l1: 'Its own clones', l2: 'in its home folder' },
        { id: 'daemon', k: 'daemon', x: 650, y: 56, w: 262, l1: 'seamlessd', l2: 'runs as ' + svcUser },
        { id: 'p-open', k: 'data', x: 652, y: 176, w: 258, l1: 'Open projects', l2: 'unlocked ones: read and write' },
        { id: 'p-conf', k: 'data', x: 652, y: 234, w: 258, l1: 'Confidential', l2: pc('confidential') },
        { id: 'p-sealed', k: 'data', x: 652, y: 292, w: 258, l1: 'Sealed', l2: pc('sealed') },
        lockedNode(652, 350, 258)
      ],
      doors: [{ id: 'tcp', x: 548, y: 86, l: 'TCP + key' }, { id: 'sock', x: 548, y: 304, l: 'Socket' }],
      ok: [['cc', 'tcp'], ['ra', 'sock'], ['tcp', 'daemon'], ['sock', 'daemon'], ['daemon', 'p-open'], ['daemon', 'p-conf'], ['daemon', 'p-sealed']],
      gateOk: [['daemon', 'p-locked']],
      risk: [['ra', 'rahome'], ['ra', 'sock'], ['sock', 'daemon'], ['daemon', 'p-open']],
      blocked: [{ from: 'ra', to: [120, 212], tag: 'OTHER ACCOUNT', tx: 128, ty: 226, up: true }],
      hit: { rahome: 'READ', daemon: 'AS ' + agentUser.toUpperCase(), 'p-open': 'LABELED' },
      safe: { 'p-conf': 'OUT OF REACH', 'p-sealed': 'OUT OF REACH', 'p-locked': 'OUT OF REACH', ssh: 'WALLED OFF', repos: 'WALLED OFF' },
      calm: [['safe', 'Unattended agents run as <b>' + esc(agentUser) + '</b>, a second account that can&rsquo;t read your home folder.'],
        ['safe', 'It reaches Seamless only through the socket, where the kernel names it.'],
        ['gate', 'Its constraints and conventions wait for you as <b>proposals</b>.']],
      calmFoot: 'Turn on a compromised agent: here it is the unattended one.',
      breach: [['safe', '<b>Can&rsquo;t read your home folder.</b> It runs as a different account.'],
        ['risk', '<b>Reads and writes open projects</b> that aren&rsquo;t locked. Everything it writes carries its account&rsquo;s name.'],
        ['gate', '<b>Its constraints and conventions wait for you</b> as proposals before any briefing carries them.'],
        ['safe', '<b>Confidential, sealed and locked projects don&rsquo;t exist for it</b>: they&rsquo;re absent from its lists and its errors.']],
      breachFoot: 'The worst a compromised unattended agent does is write labeled knowledge into open projects.'
    }
  };
  function esc(s) { return String(s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }

  var plan = document.getElementById('pv-plan'), reach = document.getElementById('pv-reach');
  var mode = (P.restricted && P.restricted.length) ? 'restricted' : dedicated ? 'dedicated' : 'shared', breach = false;

  function nodeBox(m, id) {
    var n = m.nodes.filter(function (x) { return x.id === id; })[0];
    if (n) return { x: n.x, y: n.y, w: n.w, h: 48 };
    var d = m.doors.filter(function (x) { return x.id === id; })[0];
    return { x: d.x, y: d.y, w: 96, h: 30 };
  }
  function link(m, a, b, how) {
    var A = nodeBox(m, a), B = nodeBox(m, b), ax, ay, bx, by;
    if (B.x >= A.x + A.w - 4) { ax = A.x + A.w; ay = A.y + A.h / 2; bx = B.x; by = B.y + B.h / 2; }
    else if (B.x + B.w <= A.x + 4) { ax = A.x; ay = A.y + A.h / 2; bx = B.x + B.w; by = B.y + B.h / 2; }
    else { ax = A.x + A.w / 2; ay = B.y > A.y ? A.y + A.h : A.y; bx = B.x + B.w / 2; by = B.y > A.y ? B.y : B.y + B.h; }
    if (how === 'over') {
      var lift = Math.min(ay, by) - 46 - (by - 100) * 0.08;
      return 'M' + ax + ' ' + ay + ' C' + (ax + 120) + ' ' + lift + ' ' + (bx - 160) + ' ' + lift + ' ' + bx + ' ' + by;
    }
    var dx = Math.max(40, Math.abs(bx - ax) * 0.5);
    if (Math.abs(bx - ax) < 30) return 'M' + ax + ' ' + ay + ' C' + ax + ' ' + (ay + (by - ay) * 0.5) + ' ' + bx + ' ' + (ay + (by - ay) * 0.5) + ' ' + bx + ' ' + by;
    var sx = bx > ax ? 1 : -1;
    return 'M' + ax + ' ' + ay + ' C' + (ax + sx * dx) + ' ' + ay + ' ' + (bx - sx * dx) + ' ' + by + ' ' + bx + ' ' + by;
  }

  function drawPlan() {
    var m = MODES[mode];
    while (plan.lastChild && plan.lastChild.nodeName !== 'title') plan.removeChild(plan.lastChild);
    riders = riders.filter(function (r) { return r.owner !== 'plan'; });
    var g = el('g', { class: 'pv-fade' }, plan);
    m.rooms.forEach(function (r) {
      var rg = el('g', { class: 'pv-room' }, g);
      el('rect', { class: 'pv-wall', x: r.x, y: r.y, width: r.w, height: r.h, rx: 18 }, rg);
      var nm = txt(rg, r.x + 18, r.y + 26, 'pv-rname', r.name);
      txt(rg, r.x + 18 + r.name.length * 8.4 + 14, r.y + 26, 'pv-rsub', r.sub);
      nm.setAttribute('data-room', r.name);
    });
    m.boxes.forEach(function (b) {
      el('rect', { x: b.x, y: b.y, width: b.w, height: b.h, rx: 12, class: 'pv-box' }, g);
      txt(g, b.x + 12, b.y + 20, 'pv-boxname', b.name);
      txt(g, b.x + 12, b.y + b.h - 10, 'pv-note', b.sub);
    });
    var edges = el('g', {}, g), risky = el('g', { class: 'pv-risk-layer' }, g);
    function edge(pair, cls, layer) {
      var p = el('path', { d: link(m, pair[0], pair[1], pair[2]), class: 'pv-edge ' + cls }, layer);
      if (cls === 'ok') ride(p, 'ok', 'plan', { speed: 0.28, offset: Math.random() });
      if (cls === 'risk') ride(p, 'risk', 'plan', { speed: 0.36, n: 2, offset: Math.random() });
      if (cls === 'gate') ride(p, 'gate', 'plan', { speed: 0.3, mode: 'stop', stopAt: 0.82 });
    }
    m.ok.forEach(function (e) { edge(e, 'ok', edges); });
    (m.gateOk || []).forEach(function (e) { edge(e, 'gate', edges); });
    m.risk.forEach(function (e) { edge(e, 'risk', risky); });
    (m.gate || []).forEach(function (e) { edge(e, 'gate', risky); });
    (m.blocked || []).forEach(function (b) {
      var A = nodeBox(m, b.from), ax = A.x + A.w / 2, ay = b.up ? A.y : A.y + A.h;
      if (!b.up) { ax = A.x + A.w; ay = A.y + A.h / 2 + 8; }
      var d = 'M' + ax + ' ' + ay + ' C' + (b.up ? ax : ax + 160) + ' ' + (b.up ? ay - 30 : ay + 60) + ' ' + (b.to[0] - (b.up ? 0 : 80)) + ' ' + (b.to[1] + (b.up ? 20 : 0)) + ' ' + b.to[0] + ' ' + b.to[1];
      var p = el('path', { d: d, class: 'pv-edge blocked' }, risky);
      ride(p, 'risk', 'plan', { speed: 0.4, mode: 'stop', stopAt: 0.97 });
      el('circle', { class: 'pv-stop', cx: b.to[0], cy: b.to[1], r: 9 }, risky);
      el('path', { class: 'pv-stop-x', d: 'M' + (b.to[0] - 4) + ' ' + (b.to[1] - 4) + ' L' + (b.to[0] + 4) + ' ' + (b.to[1] + 4) + ' M' + (b.to[0] + 4) + ' ' + (b.to[1] - 4) + ' L' + (b.to[0] - 4) + ' ' + (b.to[1] + 4) }, risky);
      if (b.tag) txt(risky, b.tx, b.ty, 'pv-tag risk', b.tag);
    });
    m.doors.forEach(function (d) {
      var dg = el('g', { class: 'pv-door' }, g);
      el('rect', { x: d.x, y: d.y, width: 96, height: 30, rx: 8 }, dg);
      txt(dg, d.x + 48, d.y + 19, '', d.l, 'middle');
    });
    m.nodes.forEach(function (n) {
      var ng = el('g', { class: 'pv-node' + (n.k ? ' ' + n.k : ''), 'data-node': n.id }, g);
      el('rect', { x: n.x, y: n.y, width: n.w, height: 48, rx: 10 }, ng);
      txt(ng, n.x + 14, n.y + 21, 'n1', n.l1);
      txt(ng, n.x + 14, n.y + 38, 'n2', n.l2);
    });
    function mark(map, cls, tagCls) {
      Object.keys(map || {}).forEach(function (id) {
        var n = m.nodes.filter(function (x) { return x.id === id; })[0];
        if (!n) return;
        txt(risky, n.x + n.w - 10, n.y - 6, 'pv-tag ' + tagCls, map[id], 'end');
        if (breach) plan.querySelector('[data-node="' + id + '"]').classList.add(cls);
      });
    }
    mark(m.hit, 'hit', 'risk');
    mark(m.held, 'held', 'gate');
    mark(m.safe, 'safe', 'safe');
    plan.classList.toggle('compromised', breach);
    var items = breach ? m.breach : m.calm, mk = { risk: '!', gate: '?', safe: '✓' };
    reach.innerHTML = '<h3>' + (breach ? '<span class="pv-mk risk" aria-hidden="true">!</span>If an agent here is compromised' : 'How it works in this setup') + '</h3><ul>' +
      items.map(function (it) { return '<li><span class="pv-mk ' + it[0] + '" aria-hidden="true">' + mk[it[0]] + '</span><span>' + it[1] + '</span></li>'; }).join('') +
      '</ul><p class="pv-foot">' + (breach ? m.breachFoot : m.calmFoot) + '</p>';
    wake();
  }
  document.querySelectorAll('[data-pv-mode]').forEach(function (b) {
    b.setAttribute('aria-pressed', b.getAttribute('data-pv-mode') === mode ? 'true' : 'false');
    b.addEventListener('click', function () {
      mode = b.getAttribute('data-pv-mode');
      document.querySelectorAll('[data-pv-mode]').forEach(function (x) { x.setAttribute('aria-pressed', x === b ? 'true' : 'false'); });
      drawPlan();
    });
  });
  var sw = document.getElementById('pv-breach');
  if (sw) sw.addEventListener('click', function () {
    breach = sw.getAttribute('aria-checked') !== 'true';
    sw.setAttribute('aria-checked', breach ? 'true' : 'false');
    drawPlan();
  });
  if (plan && reach) { watch(plan, 'plan'); drawPlan(); }

  /* ---- the flow map ------------------------------------------------------- */

  var fm = document.getElementById('pv-flowmap');
  if (fm) (function () {
    var PR = P.projects || {}, cx = 480, cy = 250, g = el('g', {}, fm);
    var links = el('g', {}, g), nodes = el('g', {}, g);
    var open = (PR.open || []).slice(0, 10), conf = (PR.confidential || []).slice(0, 4), sealed = (PR.sealed || []).slice(0, 3), locked = (PR.locked || []).slice(0, 5);
    var pos = {};
    open.forEach(function (p, i) { var a = (-90 + i * 360 / Math.max(1, open.length)) * Math.PI / 180; pos['o:' + p.slug] = [cx + Math.cos(a) * 158, cy + Math.sin(a) * 142]; });
    conf.forEach(function (p, i) { pos['c:' + p.slug] = [118, 250 + (i - (conf.length - 1) / 2) * 120]; });
    sealed.forEach(function (p, i) { pos['s:' + p.slug] = [842, 250 + (i - (sealed.length - 1) / 2) * 120]; });
    locked.forEach(function (p, i) { pos['l:' + p.slug] = [cx + (i - (locked.length - 1) / 2) * 120, 462]; });
    function zone(x, y, s, v) { txt(g, x, y, 'pv-zone', s, 'middle').setAttribute('fill', v); }
    zone(cx, 52, 'OPEN', 'var(--pv-open)'); zone(118, 52, 'CONFIDENTIAL', 'var(--pv-conf)'); zone(842, 52, 'SEALED', 'var(--pv-sealed)'); zone(cx, 508, 'LOCKED', 'var(--pv-locked)');
    function path(d, cls) { return el('path', { d: d, class: 'pv-link ' + cls }, links); }
    function curve(a, b, bend) {
      var mx = (a[0] + b[0]) / 2, my = (a[1] + b[1]) / 2, dx = b[0] - a[0], dy = b[1] - a[1];
      return 'M' + a[0] + ' ' + a[1] + ' Q' + (mx - dy * bend) + ' ' + (my + dx * bend) + ' ' + b[0] + ' ' + b[1];
    }
    open.forEach(function (p) { ride(path(curve([cx, cy], pos['o:' + p.slug], 0.08), 'in'), 'in', 'flow', { speed: 0.22, n: 2, r: 3, offset: Math.random() }); });
    conf.forEach(function (p) { ride(path(curve([cx, cy], pos['c:' + p.slug], 0.08), 'in'), 'in', 'flow', { speed: 0.22, n: 2, r: 3, offset: Math.random() }); });
    if (open.length > 1) {
      for (var i = 0; i < open.length; i++) {
        var a = pos['o:' + open[i].slug], b = pos['o:' + open[(i + 1) % open.length].slug];
        ride(path(curve(a, b, -0.18), 'cross'), 'cross', 'flow', { speed: 0.16, r: 3, offset: i * 0.2 });
      }
    }
    conf.forEach(function (p) {
      var s = pos['c:' + p.slug], t = [s[0] + 150, s[1] + (s[1] < cy ? 30 : -30)];
      ride(path(curve(s, t, 0.1), 'cut'), 'bounce', 'flow', { speed: 0.35, r: 3, mode: 'bounce', stopAt: 0.42 });
      var bx = s[0] + 66, by = s[1] + (s[1] < cy ? 13 : (s[1] > cy ? -13 : 0));
      el('line', { x1: bx, y1: by - 10, x2: bx, y2: by + 10, class: 'pv-wallbar conf' }, links);
    });
    sealed.forEach(function (p) {
      var s = pos['s:' + p.slug];
      ride(path(curve([cx + 40, cy], [s[0] - 46, s[1]], 0.16), 'cut'), 'in', 'flow', { speed: 0.25, r: 3, mode: 'stop', stopAt: 0.92 });
      el('line', { x1: s[0] - 50, y1: s[1] - 12, x2: s[0] - 50, y2: s[1] + 12, class: 'pv-wallbar sealed' }, links);
    });
    var gg = el('g', { class: 'pv-global' }, nodes);
    el('circle', { cx: cx, cy: cy, r: 64, class: 'halo' }, gg);
    el('circle', { cx: cx, cy: cy, r: 40, class: 'core' }, gg);
    txt(gg, cx, cy - 2, 'pv-label', 'Global', 'middle');
    txt(gg, cx, cy + 14, 'pv-sub', plural(P.global || 0, 'memory').replace('memorys', 'memories'), 'middle');
    function node(p, cls, key) {
      var s = pos[key], a = el('a', { href: p.href, class: 'pv-fnode ' + cls }, nodes);
      a.setAttribute('aria-label', p.slug + ', ' + cls + ', ' + plural(p.memories, 'memory').replace('memorys', 'memories'));
      if (cls === 'confidential') el('circle', { cx: s[0], cy: s[1], r: 34, class: 'fence' }, a);
      if (cls === 'sealed') { el('circle', { cx: s[0], cy: s[1], r: 34, class: 'fence' }, a); el('circle', { cx: s[0], cy: s[1], r: 40, class: 'fence2' }, a); }
      el('circle', { cx: s[0], cy: s[1], r: 25, class: 'body' }, a);
      if (cls === 'locked') el('path', { d: 'M' + (s[0] - 7) + ' ' + (s[1] - 2) + ' v-5 a7 7 0 0 1 14 0 v5 M' + (s[0] - 10) + ' ' + (s[1] - 2) + ' h20 v11 h-20 z', class: 'lockglyph' }, a);
      else txt(a, s[0], s[1] + 4, 'pv-sub', p.memories, 'middle');
      var name = p.slug.length > 18 ? p.slug.slice(0, 17) + '…' : p.slug;
      txt(a, s[0], s[1] + (cls === 'open' && s[1] < cy - 60 ? -36 : 46), 'pv-label', name, 'middle');
    }
    open.forEach(function (p) { node(p, 'open', 'o:' + p.slug); });
    conf.forEach(function (p) { node(p, 'confidential', 'c:' + p.slug); });
    sealed.forEach(function (p) { node(p, 'sealed', 's:' + p.slug); });
    locked.forEach(function (p) { node(p, 'locked', 'l:' + p.slug); });
    function ghost(x, y, l1, l2, right) {
      var lg = el('g', { class: 'pv-fnode ghost' }, nodes);
      el('circle', { cx: x, cy: y, r: 25, class: 'body' }, lg);
      txt(lg, right ? x + 40 : x, right ? y - 4 : y + 46, 'pv-label', l1, right ? 'start' : 'middle');
      txt(lg, right ? x + 40 : x, right ? y + 12 : y + 62, 'pv-sub', l2, right ? 'start' : 'middle');
    }
    if (!open.length) ghost(cx, cy - 142, 'No open projects', '', false);
    if (!conf.length) ghost(118, 250, 'None yet', 'confidential', false);
    if (!sealed.length) ghost(842, 250, 'None yet', 'sealed', false);
    if (!locked.length) ghost(cx, 462, 'No locked projects', P.locking ? 'list one under peer_auth.locked' : 'needs Seamless in its own account', true);
    watch(fm, 'flow');
  })();

  /* ---- the fences' small diagrams --------------------------------------- */

  document.querySelectorAll('svg[data-fence-art]').forEach(function (svg) {
    var f = svg.getAttribute('data-fence-art'), id = 'fence-' + f, cx = 130, cy = 46, R = 28, col = 'var(--fc)';
    if (f === 'sealed') el('circle', { cx: cx, cy: cy, r: R + 6, fill: 'none', stroke: col, 'stroke-width': 1, opacity: 0.55 }, svg);
    el('circle', { cx: cx, cy: cy, r: R, fill: 'color-mix(in oklab, var(--fc) 10%, transparent)', stroke: col, 'stroke-width': f === 'open' ? 1.4 : 2, 'stroke-dasharray': f === 'open' ? '3 4' : null }, svg);
    if (f === 'locked') el('path', { d: 'M' + (cx - 6) + ' ' + (cy - 1) + ' v-5 a6 6 0 0 1 12 0 v5 M' + (cx - 9) + ' ' + (cy - 1) + ' h18 v12 h-18 z', fill: 'none', stroke: col, 'stroke-width': 1.8 }, svg);
    function track(d, mode2, stopAt, n, speed, fill) {
      var p = el('path', { d: d, fill: 'none', stroke: 'none' }, svg);
      for (var i = 0; i < (n || 1); i++) {
        var dot = el('circle', { r: 3, fill: fill || col }, svg);
        riders.push({ path: p, dot: dot, len: 0, speed: speed || 0.3, phase: i / (n || 1) + Math.random() * 0.2, mode: mode2 || 'loop', stopAt: stopAt || 1, owner: id });
      }
    }
    if (f === 'open') { track('M20 30 L240 62', 'loop', 1, 2, 0.22); track('M240 24 L20 70', 'loop', 1, 2, 0.19); track('M60 88 L200 4', 'loop', 1, 1, 0.24); }
    else if (f === 'confidential') {
      track('M14 46 L' + cx + ' 46', 'loop', 1, 2, 0.24, 'var(--match)');
      track('M' + cx + ' 46 L246 46', 'bounce', (R - 3) / 116, 1, 0.5); track('M' + cx + ' 46 L' + (cx + 60) + ' 92', 'bounce', 0.38, 1, 0.45);
    } else if (f === 'sealed') {
      track('M14 46 L' + cx + ' 46', 'bounce', (cx - R - 18) / (cx - 14), 1, 0.4, 'var(--match)');
      track('M' + (cx - 14) + ' 46 L' + (cx + 14) + ' 46', 'bounce', 1, 1, 0.6); track('M' + cx + ' ' + (cy - 14) + ' L' + cx + ' ' + (cy + 14), 'bounce', 1, 1, 0.5);
    } else {
      track('M14 60 L' + (cx - 8) + ' 60', 'stop', (cx - R - 26) / (cx - 22), 1, 0.25, 'var(--pop)');
      track('M246 30 L' + (cx + 8) + ' 40', 'stop', 0.78, 1, 0.21, 'var(--pop)');
    }
    watch(svg, id);
  });

  /* ---- which fence fits ---------------------------------------------------- */

  var ans = { 1: null, 2: null, 3: null };
  var answer = document.getElementById('pv-answer');
  function verdict() {
    if (!answer) return;
    if (ans[1] === null) { answer.style.setProperty('--fc', 'var(--brand)'); answer.innerHTML = '<p class="pv-kicker">Recommendation</p><p class="pv-verdict">Answer the first question.</p><p>Three yes-or-no questions are enough to place any project.</p>'; return; }
    var F;
    if (ans[1] === 'no') F = 'open';
    else if (ans[2] === null) { answer.style.setProperty('--fc', 'var(--pv-conf)'); answer.innerHTML = '<p class="pv-kicker">Recommendation</p><p class="pv-verdict">It needs a fence.</p><p>One more question decides which one.</p>'; return; }
    else F = ans[2] === 'yes' ? 'confidential' : 'sealed';
    var why = {
      open: 'Shared knowledge helps here, and nothing in it would hurt another project. New projects start open.',
      confidential: 'Nothing will leave it, and its agents still get your global conventions and can read your open projects.',
      sealed: 'Nothing will enter or leave it: no global memories, no family, no cross-project reads.'
    };
    var fc = 'var(--pv-' + (F === 'confidential' ? 'conf' : F) + ')', extra = '';
    var how = F === 'open' ? 'seam project isolation <slug> open' : 'seam project isolation <slug> ' + F + ' --yes';
    if (F !== 'open' && ans[3] === 'yes') {
      extra = '<p><b>And lock it.</b> ' + (P.locking ? 'List the project under <code>peer_auth.locked</code> in the daemon&rsquo;s config and restart it;' : 'A lock needs Seamless in its own account first; then list the project under <code>peer_auth.locked</code>, and') + ' its agents wait for your Touch ID, passkey or password.</p>';
      fc = 'var(--pv-locked)';
    }
    answer.style.setProperty('--fc', fc);
    answer.innerHTML = '<p class="pv-kicker">Recommendation</p><p class="pv-verdict">Make it <em>' + F + (extra ? ' and locked' : '') + '</em>.</p><p>' + why[F] + '</p>' + extra + '<pre class="pv-code">' + esc(how) + '</pre>';
  }
  function sync() {
    document.querySelectorAll('#pv-chooser [data-q]').forEach(function (q) {
      var n = +q.getAttribute('data-q');
      var enabled = n === 1 || (n === 2 && ans[1] === 'yes') || (n === 3 && ans[1] === 'yes' && ans[2] !== null);
      q.setAttribute('aria-disabled', enabled ? 'false' : 'true');
      q.querySelectorAll('button').forEach(function (b) { b.disabled = !enabled; b.setAttribute('aria-pressed', ans[n] === b.getAttribute('data-a') ? 'true' : 'false'); });
    });
    verdict();
  }
  document.querySelectorAll('#pv-chooser [data-q]').forEach(function (q) {
    var n = +q.getAttribute('data-q');
    q.querySelectorAll('button').forEach(function (b) {
      b.addEventListener('click', function () {
        ans[n] = b.getAttribute('data-a');
        if (n === 1) { ans[2] = null; ans[3] = null; }
        if (n === 2) ans[3] = null;
        sync();
      });
    });
  });
  sync();
})();
