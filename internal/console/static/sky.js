/* The knowledge sky (Overview): search, filters, focus, selection, and the
   live layer over the star chart the server draws (constellation.go).

   The server owns the picture and every number. This script owns presentation
   state only -- what is searched, filtered, focused, hovered, or selected --
   and keeps it across the live morph two ways. Styling of server-rendered nodes
   (dimmed stars, a lit wedge, a revealed rim label) is generated into two
   stylesheets in <head>, which the morph never touches. Nodes this script
   creates (reticles, beams, the tooltip, the list and the card) live in the
   panel's data-live-skip containers. After every morph, collect() re-reads the
   stars and everything is re-derived from the state, so a star that moved
   keeps its reticle and a newly written one joins an active search.

   Every lookup starts from document.getElementById('sky'): the morph can
   replace nodes, and the panel itself appears only once a memory exists. */
(function () {
  'use strict';

  var NS = 'http://www.w3.org/2000/svg';
  var KINDS = ['constraint', 'convention', 'runbook', 'protocol', 'gotcha', 'decision', 'refuted', 'reference', 'stage'];
  var LIST_MAX = 80;
  var reduce = window.matchMedia ? window.matchMedia('(prefers-reduced-motion: reduce)') : null;

  // Committed state (the readout's buttons, the search box) and transient
  // state (what the pointer or the keyboard cursor is on right now).
  var state = { q: '', kinds: [], band: -1, stale: false, scope: null, sel: null };
  var peek = null;      // a readout row under the pointer previews its filter: {band}|{scope}|{kind}|{stale}
  var hover = null;     // id of the star under the pointer (or the keyboard cursor)
  var hoverWedge = -1;  // wedge under the pointer when no star is
  var cursor = -1;      // keyboard cursor in the result list
  var offscreen = false;

  var stars = [], byId = {}, wedges = [], known = null, listed = [];
  var geo = { c: 300, core: 38, rim: 262 };

  function sheet(id) {
    var el = document.getElementById(id);
    if (!el) {
      el = document.createElement('style');
      el.id = id;
      document.head.appendChild(el);
    }
    return el;
  }
  var filterSheet = sheet('sky-filter-state');
  var lightSheet = sheet('sky-light-state');
  // A rewritten sheet is re-parsed and re-matched against the whole chart, so
  // unchanged rules are not written again: moving from star to star inside one
  // wedge used to rewrite the same light on every move.
  function setSheet(el, text) { if (el.textContent !== text) el.textContent = text; }

  function sky() { return document.getElementById('sky'); }
  function part(sel) { var r = sky(); return r ? r.querySelector(sel) : null; }
  function calm() { return !!(reduce && reduce.matches); }

  /* ---- Reading the chart ---------------------------------------------- */

  function num(el, name) { var v = el.getAttribute(name); return v ? +v : 0; }

  function collect() {
    stars = []; byId = {}; wedges = [];
    var root = sky();
    if (!root) return;
    var svg = root.querySelector('.sky-svg');
    if (svg) geo = { c: num(svg, 'data-c') || 300, core: num(svg, 'data-core') || 38, rim: num(svg, 'data-rim') || 262 };
    var nodes = root.querySelectorAll('.sky-stars > .st');
    for (var i = 0; i < nodes.length; i++) {
      var el = nodes[i];
      var dot = el.querySelector('.dot');
      var s = {
        el: el, id: el.id.slice(2),
        x: num(el, 'data-x'), y: num(el, 'data-y'),
        k: el.getAttribute('data-k') || '', b: num(el, 'data-b'),
        p: el.getAttribute('data-p') || '',
        n: el.getAttribute('data-n') || '', d: el.getAttribute('data-d') || '', g: el.getAttribute('data-g') || '',
        t: num(el, 'data-t'), c: num(el, 'data-c'), i: num(el, 'data-i'), r: num(el, 'data-r'),
        stale: el.getAttribute('data-s') === '1', fav: el.classList.contains('fav'),
        rad: dot ? num(dot, 'r') || 2 : 2
      };
      s.hay = (s.n + ' ' + s.d + ' ' + s.g + ' ' + (s.p || 'global') + ' ' + s.k).toLowerCase();
      stars.push(s);
      byId[s.id] = s;
    }
    root.querySelectorAll('.sky-wedge').forEach(function (w) {
      wedges.push({ i: num(w, 'data-w'), p: w.getAttribute('data-p') || '', a0: num(w, 'data-a0'), a1: num(w, 'data-a1') });
    });
  }

  function wedgeOf(scope) {
    for (var i = 0; i < wedges.length; i++) if (wedges[i].p === scope) return wedges[i].i;
    return -1;
  }
  function scopeName(p) { return p || 'global'; }
  function kindVar(k) { return KINDS.indexOf(k) >= 0 ? 'var(--k-' + k + ')' : 'var(--muted)'; }
  function bandLabel(b) {
    var l = part('[data-sky-band="' + b + '"] .sky-band-l');
    return l ? l.textContent : '';
  }

  /* ---- Filters ----------------------------------------------------------- */

  function terms(q) { return q.toLowerCase().split(/\s+/).filter(Boolean); }
  function filtering(f) { return !!(f.q.trim() || f.kinds.length || f.band >= 0 || f.stale || f.scope !== null); }
  function effective() {
    var f = { q: state.q, kinds: state.kinds, band: state.band, stale: state.stale, scope: state.scope };
    if (peek) {
      if ('band' in peek) f.band = peek.band;
      if ('scope' in peek) f.scope = peek.scope;
      if ('kind' in peek) f.kinds = [peek.kind];
      if ('stale' in peek) f.stale = true;
    }
    return f;
  }
  function matches(s, f, t) {
    if (f.kinds.length && f.kinds.indexOf(s.k) < 0) return false;
    if (f.band >= 0 && s.b !== f.band) return false;
    if (f.stale && !s.stale) return false;
    if (f.scope !== null && s.p !== f.scope) return false;
    for (var i = 0; i < t.length; i++) if (s.hay.indexOf(t[i]) < 0) return false;
    return true;
  }
  function select(f) {
    var t = terms(f.q), out = [];
    for (var i = 0; i < stars.length; i++) if (matches(stars[i], f, t)) out.push(stars[i]);
    return out;
  }
  function idSel(s) { return '#sky #' + (window.CSS && CSS.escape ? CSS.escape(s.el.id) : s.el.id); }

  // paintFilter dims every star outside the effective filter. One rule lists
  // the matches by id, so it survives a morph that re-renders every star.
  var pool = null;
  function paintFilter() {
    var f = effective();
    var css = [];
    pool = null;
    if (filtering(f)) {
      pool = select(f);
      css.push('#sky .sky-stars>.st{opacity:.07}');
      if (pool.length) css.push(pool.map(idSel).join(',') + '{opacity:1}');
      css.push('#sky .sky-stars>.st.warm .dot{animation:none}');
    }
    if (filtering(state)) css.push('#sky .sky-summary{display:none}');
    if (offscreen) css.push('#sky .st .dot{animation-play-state:paused}');
    setSheet(filterSheet, css.join('\n'));
  }

  // paintLight lights one wedge -- the focused scope, else the one under the
  // pointer -- and brings its rim label forward (a tight label only shows then).
  function paintLight() {
    var f = effective();
    var w = -1;
    if (f.scope !== null) w = wedgeOf(f.scope);
    else if (hover && byId[hover]) w = wedgeOf(byId[hover].p);
    else w = hoverWedge;
    var css = [];
    if (w >= 0) {
      css.push('#sky-w-' + w + '{fill:color-mix(in oklab,var(--brand) 9%,transparent)}');
      css.push('#sky .sky-label[data-w="' + w + '"]{opacity:1;fill:var(--ink)}');
      css.push('#sky .sky-label:not(.tight):not([data-w="' + w + '"]){opacity:.22}');
    }
    setSheet(lightSheet, css.join('\n'));
  }

  /* ---- Effects layer: reticles, beams, flares ------------------------- */

  function svgNode(tag, attrs) {
    var el = document.createElementNS(NS, tag);
    for (var k in attrs) if (Object.prototype.hasOwnProperty.call(attrs, k)) el.setAttribute(k, attrs[k]);
    return el;
  }
  function fx() { return part('.sky-fx'); }

  function reticle(kind, s) {
    var layer = fx();
    if (!layer) return;
    var el = layer.querySelector('.sky-reticle.' + kind);
    if (!s) { if (el) el.remove(); return; }
    if (!el) {
      el = svgNode('circle', { class: 'sky-reticle ' + kind });
      layer.appendChild(el);
    }
    el.setAttribute('cx', s.x);
    el.setAttribute('cy', s.y);
    el.setAttribute('r', (s.rad + (kind === 'sel' ? 6.5 : 4.5)).toFixed(1));
    el.style.setProperty('--c', kindVar(s.k));
  }

  function play(el, frames, opts) {
    if (!el.animate) { el.remove(); return; }
    var a = el.animate(frames, opts);
    a.onfinish = a.oncancel = function () { el.remove(); };
  }

  function flare(s, delay) {
    var layer = fx();
    if (!layer) return;
    var ring = svgNode('circle', { class: 'sky-flare', cx: s.x, cy: s.y, r: s.rad });
    ring.style.setProperty('--c', kindVar(s.k));
    layer.appendChild(ring);
    play(ring, [
      { r: s.rad + 'px', opacity: 0.95, strokeWidth: '1.6px' },
      { r: (s.rad + 13) + 'px', opacity: 0, strokeWidth: '0.4px' }
    ], { duration: 950, delay: delay, easing: 'cubic-bezier(.2,.7,.3,1)', fill: 'both' });
  }

  // A beam is the memory travelling into an agent's context: a short dash of
  // the star's colour runs down the radius into the core.
  function beam(s, delay) {
    var layer = fx();
    if (!layer) return;
    var dx = s.x - geo.c, dy = s.y - geo.c;
    var dist = Math.sqrt(dx * dx + dy * dy);
    flare(s, delay);
    if (dist <= geo.core + 3) return;
    var ex = geo.c + dx / dist * (geo.core + 1), ey = geo.c + dy / dist * (geo.core + 1);
    var len = dist - geo.core - 1, dash = Math.min(34, len);
    var line = svgNode('line', { class: 'sky-beam', x1: s.x, y1: s.y, x2: ex.toFixed(1), y2: ey.toFixed(1) });
    line.style.setProperty('--c', kindVar(s.k));
    line.style.strokeDasharray = dash + ' ' + Math.ceil(len + dash + 4);
    layer.appendChild(line);
    play(line, [
      { strokeDashoffset: dash + 'px', opacity: 0 },
      { opacity: 1, offset: 0.12 },
      { strokeDashoffset: -(len - dash) + 'px', opacity: 1, offset: 0.88 },
      { strokeDashoffset: -len + 'px', opacity: 0 }
    ], { duration: 650 + len * 2.4, delay: delay, easing: 'cubic-bezier(.55,0,.25,1)', fill: 'both' });
  }

  function pulse(delay) {
    var layer = fx();
    if (!layer) return;
    var ring = svgNode('circle', { class: 'sky-pulse', cx: geo.c, cy: geo.c, r: geo.core });
    layer.appendChild(ring);
    play(ring, [
      { r: geo.core + 'px', opacity: 0.6 },
      { r: (geo.core + 34) + 'px', opacity: 0 }
    ], { duration: 1300, delay: delay, easing: 'cubic-bezier(.2,.7,.3,1)', fill: 'both' });
  }

  function birth(s, delay) {
    flare(s, delay);
    var dot = s.el.querySelector('.dot');
    if (dot && dot.animate) {
      dot.animate([
        { transform: 'scale(0)', opacity: 0 },
        { transform: 'scale(2.4)', opacity: 1, offset: 0.55 },
        { transform: 'scale(1)', opacity: 1 }
      ], { duration: 1100, delay: delay, easing: 'cubic-bezier(.34,1.56,.64,1)', fill: 'backwards' });
    }
  }

  /* ---- Tooltip ------------------------------------------------------------ */

  function node(tag, cls, text) {
    var el = document.createElement(tag);
    if (cls) el.className = cls;
    if (text != null) el.textContent = text;
    return el;
  }
  function ago(sec) {
    if (!sec) return '';
    var d = Math.max(0, Date.now() / 1000 - sec);
    if (d < 60) return 'just now';
    if (d < 3600) return Math.floor(d / 60) + 'm ago';
    if (d < 86400) return Math.floor(d / 3600) + 'h ago';
    if (d < 86400 * 60) return Math.floor(d / 86400) + 'd ago';
    if (d < 86400 * 365) return Math.floor(d / (86400 * 30)) + 'mo ago';
    return Math.floor(d / (86400 * 365)) + 'y ago';
  }
  function surfaced(s) { return s.t ? 'surfaced ' + ago(s.t) : 'never surfaced'; }
  function count(n) { return Number(n).toLocaleString(); }

  // The chart's screen transform, cached: getScreenCTM() forces a synchronous
  // style and layout pass, and pointermove used to ask for it on every frame.
  // A scroll, a resize, a re-render, or the pointer leaving the chart drops
  // it, and it is re-read at most every half second while the pointer stays,
  // so a layout shift nothing announces (the rail toggled from the keyboard)
  // cannot leave it stale for long.
  var ctm = null, ctmInv = null, ctmAt = 0;
  function screenCTM() {
    var now = performance.now();
    if (!ctm || now - ctmAt > 500) {
      var svg = part('.sky-svg');
      ctm = (svg && svg.getScreenCTM()) || null;
      ctmInv = ctm && ctm.inverse();
      ctmAt = now;
    }
    return ctm;
  }
  function dropCTM() { ctm = ctmInv = null; }
  window.addEventListener('resize', dropCTM);
  document.addEventListener('scroll', dropCTM, { capture: true, passive: true });

  function screenPoint(x, y) {
    var m = screenCTM();
    if (!m) return null;
    return { x: m.a * x + m.c * y + m.e, y: m.b * x + m.d * y + m.f, scale: m.a };
  }

  function showTip(s) {
    var tip = part('.sky-tip'), chart = part('.sky-chart');
    if (!tip || !chart) return;
    if (!s) { tip.hidden = true; return; }
    tip.textContent = '';
    tip.style.setProperty('--c', kindVar(s.k));
    tip.appendChild(node('strong', null, s.n));
    if (s.d) tip.appendChild(node('p', null, s.d));
    tip.appendChild(node('span', 'sky-tip-meta', s.k + ' \u00b7 ' + scopeName(s.p) + ' \u00b7 ' + surfaced(s)));
    tip.hidden = false;
    var pt = screenPoint(s.x, s.y);
    if (!pt) return;
    var box = chart.getBoundingClientRect();
    var w = tip.offsetWidth, h = tip.offsetHeight;
    var x = pt.x - box.left - w / 2;
    var y = pt.y - box.top - h - 14;
    if (y < 4) y = pt.y - box.top + 16;
    x = Math.max(4, Math.min(box.width - w - 4, x));
    tip.style.transform = 'translate(' + Math.round(x) + 'px,' + Math.round(y) + 'px)';
  }

  /* ---- Pointer ------------------------------------------------------------ */

  function toUser(clientX, clientY) {
    var m = screenCTM();
    if (!m) return null;
    var inv = ctmInv;
    return { x: inv.a * clientX + inv.c * clientY + inv.e, y: inv.b * clientX + inv.d * clientY + inv.f, scale: m.a };
  }

  // nearest is the star the pointer means: the closest one within reach (in
  // screen pixels, generous for touch). While a filter is on, only the stars it
  // kept are candidates -- a dimmed star is not what anyone is pointing at.
  function nearest(p, px) {
    if (!p) return null;
    var reach = px / (p.scale || 1), best = null, bd = reach * reach;
    var list = pool || stars;
    for (var i = 0; i < list.length; i++) {
      var s = list[i], dx = s.x - p.x, dy = s.y - p.y, d = dx * dx + dy * dy;
      if (d < bd) { bd = d; best = s; }
    }
    return best;
  }

  function polarOf(p) {
    var dx = p.x - geo.c, dy = p.y - geo.c;
    var a = Math.atan2(dx, -dy);
    if (a < 0) a += 2 * Math.PI;
    return { r: Math.sqrt(dx * dx + dy * dy), a: a };
  }
  function wedgeAt(p) {
    var q = polarOf(p);
    if (q.r < geo.core || q.r > geo.rim + 26) return -1;
    for (var i = 0; i < wedges.length; i++) if (q.a >= wedges[i].a0 - 0.004 && q.a <= wedges[i].a1 + 0.004) return wedges[i].i;
    return -1;
  }
  function inCore(p) { return polarOf(p).r <= geo.core; }

  function setHover(id, w) {
    if (id === hover && w === hoverWedge) return;
    hover = id;
    hoverWedge = id ? -1 : w;
    var s = id && byId[id];
    reticle('hover', s || null);
    showTip(s || null);
    paintLight();
    var svg = part('.sky-svg');
    if (svg) svg.style.cursor = s || w >= 0 ? 'pointer' : '';
  }

  var moveFrame = 0, moveEvt = null;
  document.addEventListener('pointermove', function (e) {
    var inChart = e.target && e.target.closest && e.target.closest('#sky .sky-svg');
    if (!inChart) {
      dropCTM();
      if ((hover || hoverWedge >= 0) && cursor < 0) setHover(null, -1);
      return;
    }
    moveEvt = e;
    if (moveFrame) return;
    moveFrame = requestAnimationFrame(function () {
      moveFrame = 0;
      var ev = moveEvt, p = toUser(ev.clientX, ev.clientY);
      if (!p) return;
      var s = nearest(p, ev.pointerType === 'touch' ? 26 : 16);
      setHover(s ? s.id : null, s ? -1 : wedgeAt(p));
    });
  }, { passive: true });
  document.addEventListener('mouseout', function (e) {
    if (!e.relatedTarget && (hover || hoverWedge >= 0) && cursor < 0) setHover(null, -1);
  });
  window.addEventListener('scroll', function () { if (hover && cursor < 0) setHover(null, -1); }, { passive: true });

  /* ---- The readout: selection card and result list ------------------------ */

  function icon(name) {
    var tpl = document.getElementById('sky-icons');
    var src = tpl && tpl.content ? tpl.content.querySelector('.ico-' + name) : null;
    return src ? src.cloneNode(true) : document.createTextNode('');
  }

  function fact(dl, term, value) {
    var row = node('div');
    row.appendChild(node('dt', null, term));
    row.appendChild(node('dd', null, value));
    dl.appendChild(row);
  }

  function card(s) {
    var c = node('article', 'sky-card');
    c.style.setProperty('--c', kindVar(s.k));
    var head = node('header', 'sky-card-head');
    var kind = node('span', 'sky-card-kind');
    kind.appendChild(node('i'));
    kind.appendChild(document.createTextNode(s.k));
    head.appendChild(kind);
    head.appendChild(node('span', 'sky-card-scope', scopeName(s.p)));
    var x = node('button', 'sky-x');
    x.type = 'button';
    x.setAttribute('data-sky-unselect', '');
    x.setAttribute('aria-label', 'Close');
    x.appendChild(icon('x'));
    head.appendChild(x);
    c.appendChild(head);

    var name = node('a', 'sky-card-name', s.n);
    name.href = '/console/memories/' + encodeURIComponent(s.id);
    c.appendChild(name);
    if (s.d) c.appendChild(node('p', 'sky-card-desc', s.d));

    var dl = node('dl', 'sky-card-facts');
    fact(dl, 'Last surfaced', s.t ? ago(s.t) : 'never');
    fact(dl, 'Surfaced', count(s.i) + '\u00d7');
    fact(dl, 'Read by agents', count(s.r) + '\u00d7');
    fact(dl, 'Written', s.c ? ago(s.c) : '\u2014');
    c.appendChild(dl);

    if (s.fav || s.stale) {
      var flags = node('div', 'sky-card-flags');
      if (s.fav) { var f = node('span', 'sky-flag fav'); f.appendChild(icon('star')); f.appendChild(document.createTextNode('Starred')); flags.appendChild(f); }
      if (s.stale) { var g = node('span', 'sky-flag stale'); g.appendChild(icon('timer')); g.appendChild(document.createTextNode('Going stale')); flags.appendChild(g); }
      c.appendChild(flags);
    }
    var open = node('a', 'btn small sky-card-open', 'Open memory');
    open.href = name.href;
    open.appendChild(icon('arrow-right'));
    c.appendChild(open);
    return c;
  }

  function chip(label, drop) {
    var b = node('button', 'sky-chip');
    b.type = 'button';
    b.setAttribute('data-sky-drop', drop);
    b.setAttribute('aria-label', 'Remove filter: ' + label);
    b.appendChild(node('span', null, label));
    b.appendChild(icon('x'));
    return b;
  }

  // The list runs newest first, except when it is a list of what has gone
  // quiet (the stale filter, the stale ring, or never surfaced): then the
  // longest-silent memory leads, because that is the one to decide on.
  function byName(a, b) { return a.n < b.n ? -1 : a.n > b.n ? 1 : 0; }
  function newestFirst(a, b) { return a.t !== b.t ? b.t - a.t : byName(a, b); }
  function oldestFirst(a, b) { return a.t !== b.t ? a.t - b.t : byName(a, b); }

  function results() {
    var quiet = state.stale || state.band >= 3;
    listed = select(state).sort(quiet ? oldestFirst : newestFirst);
    var wrap = node('section', 'sky-results');
    var head = node('div', 'sky-results-head');
    var n = node('span', 'sky-results-n');
    n.appendChild(node('strong', null, count(listed.length)));
    n.appendChild(document.createTextNode(' of ' + count(stars.length)));
    head.appendChild(n);
    if (state.scope !== null) head.appendChild(chip(scopeName(state.scope), 'scope'));
    if (state.band >= 0) head.appendChild(chip(bandLabel(state.band), 'band'));
    if (state.stale) head.appendChild(chip('going stale', 'stale'));
    state.kinds.forEach(function (k) { head.appendChild(chip(k, 'kind:' + k)); });
    if (state.q.trim()) head.appendChild(chip('\u201c' + state.q.trim() + '\u201d', 'q'));
    var clear = node('button', 'sky-clear', 'Clear');
    clear.type = 'button';
    clear.setAttribute('data-sky-clear', '');
    head.appendChild(clear);
    wrap.appendChild(head);

    var scopeRow = state.scope !== null ? part('[data-sky-scope="' + cssString(state.scope) + '"]') : null;
    var href = scopeRow && scopeRow.getAttribute('data-href');
    if (href) {
      var go = node('a', 'sky-results-open', 'Open the ' + scopeName(state.scope) + ' workspace');
      go.href = href;
      go.appendChild(icon('arrow-right'));
      wrap.appendChild(go);
    }

    if (!listed.length) {
      wrap.appendChild(node('p', 'sky-results-empty', 'Nothing in the sky matches. Try fewer words or another filter.'));
      return wrap;
    }
    var rows = node('div', 'sky-rows');
    rows.setAttribute('role', 'list');
    listed.slice(0, LIST_MAX).forEach(function (s, i) {
      var row = node('button', 'sky-row');
      row.type = 'button';
      row.setAttribute('role', 'listitem');
      row.setAttribute('data-sky-row', s.id);
      row.setAttribute('data-i', String(i));
      if (state.sel === s.id) row.setAttribute('aria-current', 'true');
      row.style.setProperty('--c', kindVar(s.k));
      row.appendChild(node('i', 'sky-row-dot' + (s.t ? '' : ' hollow')));
      var main = node('span', 'sky-row-main');
      main.appendChild(node('span', 'sky-row-name', s.n));
      main.appendChild(node('span', 'sky-row-meta', scopeName(s.p) + ' \u00b7 ' + (s.t ? ago(s.t) : 'never surfaced')));
      row.appendChild(main);
      rows.appendChild(row);
    });
    wrap.appendChild(rows);
    if (listed.length > LIST_MAX) {
      wrap.appendChild(node('p', 'sky-results-more', count(listed.length - LIST_MAX) + ' more not listed \u2014 narrow with a search, a ring, or a kind.'));
    }
    return wrap;
  }

  function cssString(v) { return String(v).replace(/["\\]/g, '\\$&').replace(/[\n\r\f]/g, ' '); }

  // drawView rebuilds the card and the list. It runs on a state change or a
  // morph, never on pointer motion, so a row keeps its focus while you point.
  function drawView() {
    var box = part('.sky-view');
    if (!box) return;
    var had = document.activeElement && box.contains(document.activeElement) ? document.activeElement.getAttribute('data-sky-row') : null;
    box.textContent = '';
    if (state.sel && !byId[state.sel]) state.sel = null;
    if (state.sel) box.appendChild(card(byId[state.sel]));
    if (filtering(state)) box.appendChild(results());
    else listed = [];
    if (had) {
      var again = box.querySelector('[data-sky-row="' + cssString(had) + '"]');
      if (again) again.focus({ preventScroll: true });
    }
    markCursor();
  }

  function markCursor() {
    var box = part('.sky-view');
    if (!box) return;
    box.querySelectorAll('.sky-row.active').forEach(function (r) { r.classList.remove('active'); });
    if (cursor < 0) return;
    var row = box.querySelector('.sky-row[data-i="' + cursor + '"]');
    if (row) {
      row.classList.add('active');
      row.scrollIntoView({ block: 'nearest' });
    }
  }

  function syncControls() {
    var root = sky();
    if (!root) return;
    root.querySelectorAll('[data-sky-band]').forEach(function (b) {
      b.setAttribute('aria-pressed', String(+b.getAttribute('data-sky-band') === state.band));
    });
    root.querySelectorAll('[data-sky-scope]').forEach(function (b) {
      b.setAttribute('aria-pressed', String(state.scope !== null && b.getAttribute('data-sky-scope') === state.scope));
    });
    root.querySelectorAll('[data-sky-kind]').forEach(function (b) {
      b.setAttribute('aria-pressed', String(state.kinds.indexOf(b.getAttribute('data-sky-kind')) >= 0));
    });
    var st = root.querySelector('[data-sky-stale]');
    if (st) st.setAttribute('aria-pressed', String(state.stale));
  }

  /* ---- Rendering ---------------------------------------------------------- */

  // render re-derives everything from state. full=false skips the list/card
  // rebuild (pointer-driven previews only restyle the chart).
  function render(full) {
    paintFilter();
    paintLight();
    syncControls();
    if (full) drawView();
    reticle('sel', state.sel ? byId[state.sel] : null);
    var h = hover && byId[hover];
    if (hover && !h) hover = null;
    reticle('hover', h || null);
    if (h) showTip(h);
  }

  function commit() { cursor = -1; render(true); }

  function choose(id) {
    state.sel = id && byId[id] ? id : null;
    render(true);
  }

  function toggleScope(p) { state.scope = state.scope === p ? null : p; commit(); }

  /* ---- Clicks and hovers --------------------------------------------------- */

  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest || !t.closest('#sky')) return;

    if (t.closest('.sky-svg')) {
      var p = toUser(e.clientX, e.clientY);
      if (!p) return;
      var s = nearest(p, e.pointerType === 'touch' ? 26 : 16);
      if (s) {
        if (e.metaKey || e.ctrlKey) { window.open('/console/memories/' + encodeURIComponent(s.id), '_blank', 'noopener'); return; }
        choose(state.sel === s.id ? null : s.id);
        return;
      }
      if (inCore(p)) { state.band = state.band === 0 ? -1 : 0; commit(); return; }
      if (state.sel) { choose(null); return; }
      var w = wedgeAt(p);
      for (var i = 0; i < wedges.length; i++) if (wedges[i].i === w) { toggleScope(wedges[i].p); return; }
      return;
    }

    var b;
    if ((b = t.closest('[data-sky-row]'))) { choose(b.getAttribute('data-sky-row')); return; }
    if ((b = t.closest('[data-sky-unselect]'))) { choose(null); return; }
    if ((b = t.closest('[data-sky-band]'))) {
      var band = +b.getAttribute('data-sky-band');
      state.band = state.band === band ? -1 : band;
      peek = null;
      commit();
      return;
    }
    if ((b = t.closest('[data-sky-scope]'))) { peek = null; toggleScope(b.getAttribute('data-sky-scope')); return; }
    if ((b = t.closest('[data-sky-kind]'))) {
      var k = b.getAttribute('data-sky-kind'), at = state.kinds.indexOf(k);
      state.kinds = at >= 0 ? state.kinds.filter(function (x) { return x !== k; }) : state.kinds.concat([k]);
      peek = null;
      commit();
      return;
    }
    if (t.closest('[data-sky-stale]')) { state.stale = !state.stale; peek = null; commit(); return; }
    if (t.closest('[data-sky-clear]')) {
      state.q = ''; state.kinds = []; state.band = -1; state.stale = false; state.scope = null;
      var q = part('[data-sky-q]');
      if (q) q.value = '';
      commit();
      return;
    }
    if ((b = t.closest('[data-sky-drop]'))) {
      var what = b.getAttribute('data-sky-drop');
      if (what === 'scope') state.scope = null;
      else if (what === 'band') state.band = -1;
      else if (what === 'stale') state.stale = false;
      else if (what === 'q') { state.q = ''; var qi = part('[data-sky-q]'); if (qi) qi.value = ''; }
      else if (what.indexOf('kind:') === 0) {
        var kd = what.slice(5);
        state.kinds = state.kinds.filter(function (x) { return x !== kd; });
      }
      commit();
    }
  });

  document.addEventListener('dblclick', function (e) {
    var t = e.target;
    if (!t || !t.closest || !t.closest('#sky .sky-svg')) return;
    var s = nearest(toUser(e.clientX, e.clientY), 16);
    if (s) location.assign('/console/memories/' + encodeURIComponent(s.id));
  });

  // Pointing at a readout row previews its filter on the chart; clicking it
  // commits. A result row points the reticle at its star.
  document.addEventListener('pointerover', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var b = t.closest('#sky [data-sky-band], #sky [data-sky-scope], #sky [data-sky-kind], #sky [data-sky-stale], #sky [data-sky-row]');
    if (!b) return;
    if (b.hasAttribute('data-sky-row')) {
      var s = byId[b.getAttribute('data-sky-row')];
      if (s) { reticle('hover', s); paintLightFor(s); }
      return;
    }
    var next;
    if (b.hasAttribute('data-sky-band')) next = { band: +b.getAttribute('data-sky-band') };
    else if (b.hasAttribute('data-sky-scope')) next = { scope: b.getAttribute('data-sky-scope') };
    else if (b.hasAttribute('data-sky-kind')) next = { kind: b.getAttribute('data-sky-kind') };
    else next = { stale: true };
    peek = next;
    paintFilter();
    paintLight();
  });
  document.addEventListener('pointerout', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var b = t.closest('#sky [data-sky-band], #sky [data-sky-scope], #sky [data-sky-kind], #sky [data-sky-stale], #sky [data-sky-row]');
    if (!b || (e.relatedTarget && b.contains(e.relatedTarget))) return;
    if (b.hasAttribute('data-sky-row')) {
      if (cursor < 0) { reticle('hover', null); hover = null; paintLight(); }
      return;
    }
    peek = null;
    paintFilter();
    paintLight();
  });
  function paintLightFor(s) {
    hover = s.id;
    paintLight();
  }

  /* ---- Search box ---------------------------------------------------------- */

  var typing = 0;
  document.addEventListener('input', function (e) {
    if (!e.target || !e.target.matches || !e.target.matches('#sky [data-sky-q]')) return;
    state.q = e.target.value;
    if (typing) clearTimeout(typing);
    typing = setTimeout(function () { typing = 0; commit(); }, 70);
  });
  document.addEventListener('keydown', function (e) {
    var t = e.target;
    if (t && t.matches && t.matches('#sky [data-sky-q]')) {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        if (!listed.length) return;
        e.preventDefault();
        var max = Math.min(listed.length, LIST_MAX) - 1;
        cursor = e.key === 'ArrowDown' ? Math.min(max, cursor + 1) : Math.max(-1, cursor - 1);
        markCursor();
        var s = cursor >= 0 ? listed[cursor] : null;
        hover = s ? s.id : null;
        reticle('hover', s);
        showTip(s);
        paintLight();
        return;
      }
      if (e.key === 'Enter') {
        e.preventDefault();
        var pick = listed[cursor >= 0 ? cursor : 0];
        if (pick) choose(pick.id);
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        if (t.value) { t.value = ''; state.q = ''; commit(); }
        else t.blur();
      }
    }
  }, true);
  // Escape lets go of a selected star -- after the shell has had its turn, so
  // closing an open sheet or drawer does not also drop the selection.
  document.addEventListener('keydown', function (e) {
    var t = e.target;
    if (e.key !== 'Escape' || e.defaultPrevented || !state.sel) return;
    if (t === document.body || (t && t.closest && t.closest('#sky'))) choose(null);
  });

  /* ---- Live ------------------------------------------------------------------ */

  // An injection or a read names the memories that just reached an agent; each
  // sends a beam into the core. The morph that follows moves each star to its
  // new ring, and the CSS transition turns that into a glide.
  document.addEventListener('seam:event', function (e) {
    var d = e.detail || {};
    if (d.kind !== 'retrieval.injected' && d.kind !== 'memory.read') return;
    if (document.hidden || calm() || !sky()) return;
    var ids = Array.isArray(d.itemIds) ? d.itemIds : (d.itemId ? [d.itemId] : []);
    var hit = [];
    for (var i = 0; i < ids.length && hit.length < 32; i++) {
      var s = byId[String(ids[i])];
      if (s) hit.push(s);
    }
    if (!hit.length) return;
    pulse(0);
    hit.forEach(function (s, j) { beam(s, j * 45); });
    if (hit.length > 8) pulse(hit.length * 45);
  });

  var watched = null, observer = null;
  function watch() {
    var root = sky();
    if (!root || root === watched || !window.IntersectionObserver) return;
    if (observer) observer.disconnect();
    watched = root;
    observer = new IntersectionObserver(function (entries) {
      var off = !entries[entries.length - 1].isIntersecting;
      if (off !== offscreen) { offscreen = off; paintFilter(); }
    });
    observer.observe(root);
  }

  function refresh(initial) {
    dropCTM();
    collect();
    if (known && !initial && !document.hidden && !calm()) {
      var fresh = stars.filter(function (s) { return !known[s.id]; });
      if (fresh.length <= 12) fresh.forEach(function (s, i) { birth(s, i * 90); });
    }
    known = {};
    stars.forEach(function (s) { known[s.id] = true; });
    watch();
    render(true);
  }

  document.addEventListener('seam:content-updated', function () { refresh(false); });
  refresh(true);
})();
