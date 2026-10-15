/* Console shell: the chrome around every page -- the collapsible sidebar, the
   phone drawer, the g-chord shortcuts and the ? sheet, flash banners surfaced
   as toasts, and the recents list the command palette offers. Everything here
   is a per-browser convenience (localStorage), never server state, and every
   piece degrades to the plain server-rendered page without it.

   The shortcut map is read from the sidebar's own links (a[data-key]) at the
   moment a key is pressed, so a feature toggled off in Settings takes its
   shortcut with it and nothing here needs to know the section list. */
(function () {
  'use strict';

  var root = document.documentElement;

  function store(key, value) {
    try {
      if (value === null) localStorage.removeItem(key);
      else localStorage.setItem(key, value);
    } catch (e) {}
  }
  function read(key) {
    try { return localStorage.getItem(key); } catch (e) { return null; }
  }
  function typing(el) {
    if (!el) return false;
    var tag = el.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || el.isContentEditable;
  }
  function overlayOpen() {
    var ids = ['cmdk', 'keys'];
    for (var i = 0; i < ids.length; i++) {
      var el = document.getElementById(ids[i]);
      if (el && !el.hidden) return true;
    }
    return false;
  }
  function phone() { return window.matchMedia && window.matchMedia('(max-width: 720px)').matches; }

  /* ---- Sidebar: collapse to an icon rail (desktop) ------------------------ */

  var toggle = document.getElementById('side-toggle');
  function collapsed() { return root.getAttribute('data-sidebar') === 'collapsed'; }
  function syncToggle() {
    if (!toggle) return;
    var c = collapsed();
    toggle.setAttribute('aria-expanded', c ? 'false' : 'true');
    toggle.setAttribute('aria-label', c ? 'Expand sidebar' : 'Collapse sidebar');
    toggle.setAttribute('title', (c ? 'Expand' : 'Collapse') + ' sidebar ([)');
  }
  function setCollapsed(on) {
    if (on) root.setAttribute('data-sidebar', 'collapsed');
    else root.removeAttribute('data-sidebar');
    store('seamless-sidebar', on ? 'collapsed' : null);
    syncToggle();
  }
  if (toggle) toggle.addEventListener('click', function () { setCollapsed(!collapsed()); });
  syncToggle();

  // Collapsed, a section is an icon; its name (and g-chord) appears beside it on
  // hover or focus. Fixed-positioned so the sidebar's own scroll box cannot clip it.
  var tip = null;
  function hideTip() { if (tip) { tip.remove(); tip = null; } }
  function showTip(el) {
    if (!collapsed() || phone()) return;
    var label = el.getAttribute('data-tip');
    if (!label) return;
    hideTip();
    tip = document.createElement('div');
    tip.className = 'side-tip';
    tip.setAttribute('aria-hidden', 'true');
    tip.textContent = label;
    var key = el.getAttribute('data-key');
    if (key) {
      var k = document.createElement('kbd');
      k.textContent = 'g ' + key;
      tip.appendChild(k);
    }
    document.body.appendChild(tip);
    var r = el.getBoundingClientRect();
    tip.style.left = Math.round(r.right + 10) + 'px';
    tip.style.top = Math.round(r.top + r.height / 2) + 'px';
  }
  var side = document.getElementById('sidebar');
  if (side) {
    side.addEventListener('mouseover', function (e) {
      var el = e.target.closest && e.target.closest('[data-tip]');
      if (el && side.contains(el)) showTip(el);
    });
    side.addEventListener('mouseout', function (e) {
      var el = e.target.closest && e.target.closest('[data-tip]');
      if (el && !el.contains(e.relatedTarget)) hideTip();
    });
    side.addEventListener('focusin', function (e) {
      var el = e.target.closest && e.target.closest('[data-tip]');
      if (el) showTip(el);
    });
    side.addEventListener('focusout', hideTip);
    side.addEventListener('scroll', hideTip, { passive: true });
    side.addEventListener('click', hideTip);
  }

  /* ---- Phone drawer -------------------------------------------------------- */

  var sidebar = document.getElementById('sidebar');
  var opener = document.getElementById('drawer-open');
  var closer = document.getElementById('drawer-close');
  var scrim = document.getElementById('drawer-scrim');
  function drawerOpen() { return root.hasAttribute('data-drawer'); }
  function openDrawer() {
    if (!sidebar) return;
    root.setAttribute('data-drawer', 'open');
    if (scrim) scrim.hidden = false;
    if (opener) opener.setAttribute('aria-expanded', 'true');
    var first = sidebar.querySelector('.nav a.active') || sidebar.querySelector('.nav a');
    if (first) { try { first.focus(); } catch (e) {} }
  }
  function closeDrawer(restore) {
    if (!drawerOpen()) return;
    root.removeAttribute('data-drawer');
    if (scrim) scrim.hidden = true;
    if (opener) {
      opener.setAttribute('aria-expanded', 'false');
      if (restore) { try { opener.focus(); } catch (e) {} }
    }
  }
  if (opener) opener.addEventListener('click', openDrawer);
  if (closer) closer.addEventListener('click', function () { closeDrawer(true); });
  if (scrim) scrim.addEventListener('click', function () { closeDrawer(true); });
  if (sidebar) sidebar.addEventListener('click', function (e) {
    // A section link navigates away; the palette trigger opens over the page.
    if (e.target.closest && e.target.closest('.nav a, [data-cmdk-open]')) closeDrawer(false);
  });
  window.addEventListener('resize', function () { if (!phone()) closeDrawer(false); });

  /* ---- Shortcut sheet (?) -------------------------------------------------- */

  var keys = document.getElementById('keys');
  var keysRestore = null;
  function gotoLinks() {
    return Array.prototype.slice.call(document.querySelectorAll('nav.nav a[data-key]'));
  }
  function fillGoto() {
    var dl = document.getElementById('keys-goto');
    if (!dl) return;
    dl.textContent = '';
    gotoLinks().forEach(function (a) {
      var row = document.createElement('div');
      var dt = document.createElement('dt');
      var kbd = document.createElement('kbd');
      kbd.textContent = a.getAttribute('data-key');
      dt.appendChild(kbd);
      var dd = document.createElement('dd');
      var label = a.querySelector('.nav-label');
      dd.textContent = label ? label.textContent : a.getAttribute('data-tip') || a.getAttribute('href');
      row.appendChild(dt);
      row.appendChild(dd);
      dl.appendChild(row);
    });
  }
  function openKeys() {
    if (!keys || !keys.hidden) return;
    fillGoto();
    keysRestore = document.activeElement;
    keys.hidden = false;
    var close = keys.querySelector('[data-keys-close]');
    if (close) close.focus();
  }
  function closeKeys() {
    if (!keys || keys.hidden) return;
    keys.hidden = true;
    if (keysRestore && typeof keysRestore.focus === 'function') { try { keysRestore.focus(); } catch (e) {} }
    keysRestore = null;
  }
  if (keys) keys.addEventListener('click', function (e) {
    if (e.target === keys || (e.target.closest && e.target.closest('[data-keys-close]'))) closeKeys();
  });
  // Any [data-keys-open] control (Settings -> Experience) opens the sheet;
  // delegated so a morph-replaced button keeps working.
  document.addEventListener('click', function (e) {
    if (e.target.closest && e.target.closest('[data-keys-open]')) { e.preventDefault(); openKeys(); }
  });
  window.SeamShell = { openKeys: openKeys, toggleSidebar: function () { setCollapsed(!collapsed()); } };

  /* ---- g-chords ------------------------------------------------------------ */

  var chordTimer = null;
  var chordHint = null;
  function endChord() {
    if (chordTimer) { clearTimeout(chordTimer); chordTimer = null; }
    if (chordHint) { chordHint.remove(); chordHint = null; }
  }
  function startChord() {
    endChord();
    chordHint = document.createElement('div');
    chordHint.className = 'chord-hint';
    chordHint.setAttribute('role', 'status');
    var lead = document.createElement('kbd');
    lead.textContent = 'g';
    chordHint.appendChild(lead);
    var list = document.createElement('span');
    list.textContent = gotoLinks().map(function (a) {
      var label = a.querySelector('.nav-label');
      return a.getAttribute('data-key') + ' ' + (label ? label.textContent : '');
    }).join('  ·  ');
    chordHint.appendChild(list);
    document.body.appendChild(chordHint);
    chordTimer = setTimeout(endChord, 1600);
  }

  document.addEventListener('keydown', function (e) {
    if (e.defaultPrevented) return;
    if (e.key === 'Escape') {
      if (keys && !keys.hidden) { e.preventDefault(); closeKeys(); return; }
      if (drawerOpen()) { e.preventDefault(); closeDrawer(true); return; }
      endChord();
      return;
    }
    if (e.metaKey || e.ctrlKey || e.altKey || typing(e.target) || overlayOpen()) return;
    if (chordTimer) {
      var key = e.key.toLowerCase();
      var hit = gotoLinks().filter(function (a) { return a.getAttribute('data-key') === key; })[0];
      endChord();
      if (hit) {
        e.preventDefault();
        location.href = hit.getAttribute('href');
      }
      return;
    }
    if (e.key === 'g') { e.preventDefault(); startChord(); return; }
    if (e.key === '?') { e.preventDefault(); openKeys(); return; }
    if (e.key === '[' && !phone()) { e.preventDefault(); setCollapsed(!collapsed()); }
  });

  /* ---- Menus (details.menu) and the page-purpose disclosure ----------------- */

  // A <details> menu stays open across the in-place morph (navigation.js keeps
  // a DETAILS element's open state), so choosing an item must close it here.
  // Outside clicks and Escape close any open menu or (i) disclosure too.
  function closeFloating(except) {
    document.querySelectorAll('details.menu[open], details.page-about[open]').forEach(function (d) {
      if (d !== except) d.open = false;
    });
  }
  document.addEventListener('click', function (e) {
    var inside = e.target.closest ? e.target.closest('details.menu, details.page-about') : null;
    if (inside && e.target.closest('.menu-list a')) { inside.open = false; return; }
    closeFloating(inside);
  });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    var open = document.querySelector('details.menu[open], details.page-about[open]');
    if (!open) return;
    e.preventDefault();
    open.open = false;
    var summary = open.querySelector('summary');
    if (summary) summary.focus();
  }, true);

  /* ---- Phone title bars: the Filters toggle --------------------------------- */

  // Held here, not in an attribute alone: the live morph resets attributes to
  // what the server rendered, so the open state is re-applied after each patch.
  var filtersOpen = false;
  function syncFilters() {
    document.querySelectorAll('[data-filters-toggle]').forEach(function (b) {
      b.setAttribute('aria-expanded', filtersOpen ? 'true' : 'false');
      b.classList.toggle('on', filtersOpen);
    });
    document.querySelectorAll('.mv2-controls').forEach(function (c) {
      if (filtersOpen) c.setAttribute('data-open', '');
      else c.removeAttribute('data-open');
    });
  }
  document.addEventListener('click', function (e) {
    var b = e.target.closest ? e.target.closest('[data-filters-toggle]') : null;
    if (!b) return;
    filtersOpen = !filtersOpen;
    syncFilters();
  });
  document.addEventListener('seam:content-updated', syncFilters);

  /* ---- Flash banners as toasts --------------------------------------------- */

  // A mutation redirects back with ?notice= / ?error=; the server renders the
  // banner (the no-JS answer). With script, a success notice becomes a toast and
  // leaves the page, and both params leave the URL, so a later live refresh --
  // which re-fetches location.href -- cannot replay a message about an action
  // that already happened. An error banner stays on the page as well.
  function surfaceFlash() {
    var main = document.querySelector('.main');
    if (!main) return;
    var shown = false;
    main.querySelectorAll('[data-flash]').forEach(function (el) {
      var tone = el.getAttribute('data-flash');
      var text = (el.textContent || '').trim();
      if (text && window.SeamConsole && !shown) {
        window.SeamConsole.flash(text, tone === 'error' ? 'error' : 'ok');
        shown = true;
      }
      if (tone !== 'error') el.remove();
    });
    try {
      var url = new URL(location.href);
      if (url.searchParams.has('notice') || url.searchParams.has('error')) {
        url.searchParams.delete('notice');
        url.searchParams.delete('error');
        history.replaceState(history.state, '', url.pathname + url.search + url.hash);
      }
    } catch (e) {}
  }
  surfaceFlash();
  document.addEventListener('seam:content-updated', surfaceFlash);

  /* ---- Banners: "Got it" --------------------------------------------------- */

  // Two notes can be set aside (layout.html): the level banner, on a screen
  // above the console level, and the update banner, for a day after the
  // daemon upgraded. "Got it" hides one for this tab only -- a banner is not
  // durable state, so sessionStorage -- keyed by what it is about: the screen,
  // or "update:" plus the version, so a newer update shows again. Re-applied
  // after every morph, which restores what the server rendered.
  var BANNERS = 'seamless-level-banners';
  var BANNER = '[data-level-banner], [data-update-banner]';
  function bannerKey(b) {
    var version = b.getAttribute('data-update-banner');
    return version ? 'update:' + version : b.getAttribute('data-level-banner');
  }
  function dismissed() {
    try {
      var list = JSON.parse(sessionStorage.getItem(BANNERS) || '[]');
      return Array.isArray(list) ? list : [];
    } catch (e) { return []; }
  }
  function applyBanners() {
    var gone = dismissed();
    document.querySelectorAll(BANNER).forEach(function (b) {
      if (gone.indexOf(bannerKey(b)) !== -1) b.hidden = true;
    });
  }
  document.addEventListener('click', function (e) {
    var x = e.target.closest ? e.target.closest('[data-level-banner-dismiss], [data-update-banner-dismiss]') : null;
    if (!x) return;
    var banner = x.closest(BANNER);
    if (!banner) return;
    var id = bannerKey(banner);
    var gone = dismissed();
    if (gone.indexOf(id) === -1) gone.push(id);
    try { sessionStorage.setItem(BANNERS, JSON.stringify(gone)); } catch (e2) {}
    banner.hidden = true;
  });
  applyBanners();
  document.addEventListener('seam:content-updated', applyBanners);

  /* ---- Arrival: reveal, count-up, and the panel flashlight ------------------ */

  // html.reveal is set before first paint (layout.html) so the page settles in
  // section by section; it comes off once the sequence has played, so a live
  // morph that inserts a node later never replays it.
  var calm = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  setTimeout(function () { root.classList.remove('reveal'); }, 1100);

  // Headline numbers count up from zero on arrival: "24%", "~297", "1,284".
  // Only a whole number with an optional prefix/suffix is animated; anything
  // else (an em dash, "no data") is left exactly as the server wrote it.
  var COUNTED = '.ov2-vital-num > strong, .now-tape-cell > strong, .project-summary-copy > strong, .mv2-count > strong, .stat .value';
  function countUp() {
    if (calm) return;
    document.querySelectorAll(COUNTED).forEach(function (el) {
      var text = (el.textContent || '').trim();
      var m = text.match(/^([^\d-]*)(\d{1,3}(?:,\d{3})*|\d+)([^\d]*)$/);
      if (!m) return;
      var end = parseInt(m[2].replace(/,/g, ''), 10);
      if (!isFinite(end) || end < 2) return;
      var comma = m[2].indexOf(',') !== -1;
      var t0 = null;
      var dur = Math.min(1100, 520 + end * 2);
      function fmt(n) { var s = String(n); return comma ? s.replace(/\B(?=(\d{3})+(?!\d))/g, ',') : s; }
      function frame(t) {
        if (t0 === null) t0 = t;
        var p = Math.min(1, (t - t0) / dur);
        var eased = 1 - Math.pow(1 - p, 4);
        el.textContent = m[1] + fmt(Math.round(end * eased)) + m[3];
        if (p < 1) requestAnimationFrame(frame);
        else el.textContent = text;
      }
      el.textContent = m[1] + '0' + m[3];
      requestAnimationFrame(frame);
    });
  }
  if (root.classList.contains('reveal')) countUp();

  var LIT = '.ov2-vital, .project-summary-card, .ov2-panel, .card, .now-zone, .context-scope-card, .retrieval-panel, .search-results';
  var litFrame = 0, litEvent = null;
  document.addEventListener('pointermove', function (e) {
    litEvent = e;
    if (litFrame) return;
    litFrame = requestAnimationFrame(function () {
      litFrame = 0;
      var ev = litEvent;
      var el = ev && ev.target && ev.target.closest ? ev.target.closest(LIT) : null;
      if (!el) return;
      var r = el.getBoundingClientRect();
      el.style.setProperty('--mx', Math.round(ev.clientX - r.left) + 'px');
      el.style.setProperty('--my', Math.round(ev.clientY - r.top) + 'px');
    });
  }, { passive: true });

  /* ---- The Seam and the sky ------------------------------------------------ */

  // Every event the SSE stream carries (layout.html re-dispatches it as
  // seam:event) becomes a spark that runs down the Seam from the daemon's orb
  // to the section it belongs to, and lights that section's icon, and it rings
  // the orb and the live dots once. The same events feed --activity, a
  // decaying event rate the sky brightens with. Purely ambient: nothing here
  // changes data, and reduced motion or a hidden tab skips the sparks and the
  // rings (the activity level still settles). Between events nothing moves.
  var seam = document.getElementById('seam');
  var reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)');
  var ROUTES = [
    [/^retrieval\./, '/console/retrieval', 'flow'],
    [/^(memory\.written|note\.written|trial\.recorded)$/, null, 'ok'],
    [/^(agent\.mishap|hook\.error)$/, '/console/', 'danger'],
    [/^memory\./, '/console/memories', 'brand'],
    [/^note\./, '/console/notes', 'brand'],
    [/^session\./, '/console/sessions', 'brand'],
    [/^task\./, '/console/tasks', 'brand'],
    [/^(plan\.|subagent\.)/, '/console/plans', 'violet'],
    [/^gardener\./, '/console/gardener', 'pop'],
    [/^recall\.miss$/, '/console/retrieval', 'pop'],
    [/^trial\./, '/console/trials', 'ok'],
    [/^(tool\.call|hook\.prompt)$/, '/console/interactions', 'brand'],
    [/^project\./, '/console/projects', 'violet'],
    [/^update\./, '/console/settings', 'warn']
  ];
  var WRITE_HOME = { 'memory.written': '/console/memories', 'note.written': '/console/notes', 'trial.recorded': '/console/trials' };
  function routeOf(kind) {
    for (var i = 0; i < ROUTES.length; i++) {
      if (ROUTES[i][0].test(kind)) return { href: ROUTES[i][1] || WRITE_HOME[kind], tone: ROUTES[i][2] };
    }
    return { href: '/console/now', tone: 'brand' };
  }
  function navLink(href) {
    var links = document.querySelectorAll('nav.nav a[href]');
    for (var i = 0; i < links.length; i++) if (links[i].getAttribute('href') === href) return links[i];
    return null;
  }
  var live = 0;
  function spark(kind) {
    if (!seam || document.hidden || (reduce && reduce.matches) || live >= 7 || !seam.animate) return;
    var route = routeOf(kind || '');
    var target = navLink(route.href) || navLink('/console/now');
    var tone = 'var(--' + route.tone + ')';
    var el = document.createElement('span');
    el.className = 'spark';
    el.style.setProperty('--spark', tone);
    seam.appendChild(el);
    live++;
    var horizontal = seam.offsetWidth > seam.offsetHeight;
    seam.classList.toggle('horizontal', horizontal);
    var from, to, frames;
    if (horizontal) {
      from = 0; to = seam.offsetWidth * (0.35 + Math.random() * 0.6);
      frames = [
        { transform: 'translateX(' + from + 'px) scale(.6)', opacity: 0 },
        { opacity: 1, offset: 0.12 },
        { transform: 'translateX(' + to + 'px) scale(1)', opacity: 1, offset: 0.82 },
        { transform: 'translateX(' + to + 'px) scale(2.4)', opacity: 0 }
      ];
    } else {
      var orb = document.querySelector('.sidebar .brand .dot');
      var sr = seam.getBoundingClientRect();
      from = orb ? orb.getBoundingClientRect().top + 5 - sr.top : 30;
      to = target ? target.getBoundingClientRect().top + target.offsetHeight / 2 - sr.top : sr.height * 0.5;
      frames = [
        { transform: 'translateY(' + from + 'px) scale(.6)', opacity: 0 },
        { opacity: 1, offset: 0.1 },
        { transform: 'translateY(' + to + 'px) scale(1)', opacity: 1, offset: 0.84 },
        { transform: 'translateY(' + to + 'px) scale(2.6)', opacity: 0 }
      ];
    }
    var dist = Math.abs(to - from);
    var anim = el.animate(frames, { duration: Math.min(1500, 520 + dist * 0.9), easing: 'cubic-bezier(.3,.6,.25,1)' });
    anim.onfinish = function () {
      el.remove();
      live--;
      if (target && !horizontal) {
        target.style.setProperty('--land', tone);
        target.classList.remove('landed');
        void target.offsetWidth;
        target.classList.add('landed');
      }
    };
  }

  // The rings are CSS (transform and opacity only, so the compositor plays
  // them alone). An event that lands mid-ring is absorbed by it; the class
  // comes off when the ring ends or is cut short.
  function ping() {
    if (document.hidden || (reduce && reduce.matches)) return;
    document.querySelectorAll('a.brand .dot.on, .live-dot').forEach(function (el) {
      if (!el.closest('.live.off')) el.classList.add('ping');
    });
  }
  function unping(e) {
    if (e.animationName === 'orb-halo' || e.animationName === 'live-ping') e.target.classList.remove('ping');
  }
  document.addEventListener('animationend', unping);
  document.addEventListener('animationcancel', unping);

  // --activity: events decay with a ~20s half-life; the rate maps onto 0..1
  // so a steady trickle glows faintly and a burst lights the sky. It lives on
  // :root, where every write restyles the whole document (~16 ms on a full
  // Overview), so the level is quantised to twentieths and written only when
  // it changes: a few writes as each event settles, not one every 3 seconds.
  // The consumers' opacity transitions smooth the steps.
  var rate = 0, rateAt = Date.now(), level = '0.00';
  function decay() {
    var now = Date.now();
    rate *= Math.pow(0.5, (now - rateAt) / 20000);
    rateAt = now;
  }
  function publish() {
    decay();
    var next = (Math.round((1 - Math.exp(-rate / 6)) * 20) / 20).toFixed(2);
    if (next === level) return;
    level = next;
    root.style.setProperty('--activity', next);
  }
  document.addEventListener('seam:event', function (e) {
    decay();
    rate += 1;
    publish();
    spark(e.detail && e.detail.kind);
    ping();
  });
  setInterval(publish, 3000);

  /* ---- Timelines open on "now" --------------------------------------------- */

  // A horizontally scrolling timeline (the capture calendar) reads oldest to
  // newest; on a narrow screen it must open on its newest end, not a year ago.
  // Only the first render is pinned, so a reader's own scrolling is kept.
  // Tracked in a WeakSet, not an attribute: the live morph strips attributes
  // the server did not render, which would re-pin on every refresh.
  var pinned = window.WeakSet ? new WeakSet() : null;
  function pinScrollEnds() {
    document.querySelectorAll('[data-scroll-end]').forEach(function (el) {
      if (pinned && pinned.has(el)) return;
      el.scrollLeft = el.scrollWidth;
      if (pinned) pinned.add(el);
    });
  }
  pinScrollEnds();
  document.addEventListener('seam:content-updated', pinScrollEnds);

  /* ---- Check-in: "since you were last here" -------------------------------- */

  // The stamp is when the owner last stopped LOOKING (a hidden tab or a page
  // they left), so returning to the Overview hours later answers "what changed
  // while I was away?" with the server's own counts. Browsing between pages
  // keeps re-stamping, so a quick round trip never shows a stale digest.
  var SEEN = 'seamless-last-seen';
  var MIN_AWAY = 5 * 60 * 1000;
  var MAX_AWAY = 89 * 24 * 3600 * 1000;
  var answered = null; // the stamp the visible digest answers
  function markSeen() { store(SEEN, String(Date.now())); }
  window.addEventListener('pagehide', markSeen);
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'hidden') markSeen();
    else checkin();
  });

  function awayLabel(ms) {
    var m = Math.round(ms / 60000);
    if (m < 60) return m + 'm ago';
    var h = Math.round(m / 60);
    if (h < 48) return h + 'h ago';
    return Math.round(h / 24) + 'd ago';
  }
  function windowFor(ms) {
    var h = ms / 3600000;
    return h <= 24 ? '24h' : h <= 168 ? '7d' : h <= 720 ? '30d' : 'all';
  }
  function chip(n, one, many, href) {
    var tag = document.createElement(href ? 'a' : 'span');
    tag.className = 'checkin-chip';
    if (href) tag.href = href;
    var strong = document.createElement('strong');
    strong.textContent = n;
    tag.appendChild(strong);
    tag.appendChild(document.createTextNode(' ' + (n === 1 ? one : many)));
    return tag;
  }
  function renderCheckin(box, d, away) {
    var body = box.querySelector('.checkin-body');
    if (!body) return;
    body.textContent = '';
    var lead = document.createElement('span');
    lead.className = 'checkin-lead';
    var when = document.createElement('strong');
    when.textContent = awayLabel(away);
    var w = windowFor(away);
    var items = [
      [d.memoriesWritten, 'memory written', 'memories written', '/console/memories?sort=recent'],
      [d.sessions, 'session started', 'sessions started', '/console/sessions?w=' + w],
      [d.tasksClosed, 'task closed', 'tasks closed', '/console/tasks'],
      [d.notesWritten, 'note written', 'notes written', '/console/notes'],
      [d.proposals, 'proposal to review', 'proposals to review', '/console/gardener'],
      [d.mishaps, 'mishap reported', 'mishaps reported', '']
    ].filter(function (it) { return it[0] > 0; });
    if (!items.length) {
      lead.appendChild(document.createTextNode('Quiet since you were last here '));
      lead.appendChild(when);
      lead.appendChild(document.createTextNode(' \u2014 nothing new was recorded.'));
      body.appendChild(lead);
    } else {
      lead.appendChild(document.createTextNode('Since you were last here '));
      lead.appendChild(when);
      body.appendChild(lead);
      items.forEach(function (it) { body.appendChild(chip(it[0], it[1], it[2], it[3])); });
    }
    box.hidden = false;
  }
  function checkin() {
    var box = document.getElementById('checkin');
    if (!box) return;
    var last = parseInt(read(SEEN), 10);
    if (!isFinite(last) || last === answered) return;
    var away = Date.now() - last;
    if (away < MIN_AWAY || away > MAX_AWAY) return;
    answered = last;
    fetch('/console/since?t=' + last, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d) renderCheckin(box, d, away); })
      .catch(function () {});
  }
  document.addEventListener('click', function (e) {
    var x = e.target.closest ? e.target.closest('[data-checkin-dismiss]') : null;
    if (!x) return;
    var box = document.getElementById('checkin');
    if (box) box.hidden = true;
  });
  checkin();

  /* ---- Recents for the palette --------------------------------------------- */

  // An entity page, or a rail/reader link to one, is worth offering again. The
  // palette reads this list when it opens with an empty query.
  var ENTITY = /^\/console\/(memories|notes|tasks|plans|sessions|projects|labs|trials|events)\/[^/]+/;
  var KIND = { memories: 'Memory', notes: 'Note', tasks: 'Task', plans: 'Plan', sessions: 'Session', projects: 'Project', labs: 'Lab', trials: 'Trial', events: 'Event' };
  function remember(href, title) {
    var url;
    try { url = new URL(href, location.origin); } catch (e) { return; }
    var m = url.pathname.match(ENTITY);
    if (!m || !title) return;
    title = title.replace(/\s*·\s*Seamless$/, '').trim();
    if (!title) return;
    var list;
    try { list = JSON.parse(read('seamless-recent') || '[]'); } catch (e) { list = []; }
    if (!Array.isArray(list)) list = [];
    list = list.filter(function (r) { return r && r.href !== url.pathname; });
    list.unshift({ href: url.pathname, title: title.slice(0, 120), kind: KIND[m[1]] || '' });
    store('seamless-recent', JSON.stringify(list.slice(0, 8)));
  }
  remember(location.pathname, document.title);
  document.addEventListener('click', function (e) {
    var a = e.target.closest ? e.target.closest('a[href]') : null;
    if (!a || a.closest('.copy-btn')) return;
    var title = a.getAttribute('data-title') || (a.querySelector('.rail-title, strong, .t') || a).textContent;
    remember(a.getAttribute('href'), (title || '').replace(/\s+/g, ' '));
  }, true);
})();
