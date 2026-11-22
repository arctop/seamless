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
    // Only the sections list scrolls (.nav-scroll), and scroll does not
    // bubble: listen in the capture phase.
    side.addEventListener('scroll', hideTip, { passive: true, capture: true });
    side.addEventListener('click', hideTip);
  }

  /* ---- Sidebar: the sections list scrolls, the head and foot stay ---------- */

  // The brand, the search field, Settings and the account row are pinned;
  // only the sections scroll (layout.html's .nav-scroll). A fade marks each
  // edge with more beyond it: data-nav-above and data-nav-below on the
  // sidebar, which the live morph never touches (only the nav inside it is
  // patched). The list keeps its place across page loads in this tab, and
  // the active section is scrolled into view when it would sit out of sight.
  var NAV_SCROLL = 'seamless-nav-scroll';
  function navScroll() { return side ? side.querySelector('.nav-scroll') : null; }
  function syncFade() {
    var list = navScroll();
    if (!side || !list) return;
    var top = list.scrollTop;
    side.toggleAttribute('data-nav-above', top > 1);
    side.toggleAttribute('data-nav-below', top + list.clientHeight < list.scrollHeight - 1);
  }
  function revealActive() {
    var list = navScroll();
    var active = list && list.querySelector('a.active');
    if (!active) return;
    var box = list.getBoundingClientRect(), r = active.getBoundingClientRect();
    if (r.top < box.top) list.scrollTop -= box.top - r.top + 8;
    else if (r.bottom > box.bottom) list.scrollTop += r.bottom - box.bottom + 8;
  }
  (function () {
    var list = navScroll();
    if (!list) return;
    try {
      var kept = parseInt(sessionStorage.getItem(NAV_SCROLL) || '', 10);
      if (kept > 0) list.scrollTop = kept;
    } catch (e) {}
    revealActive();
    syncFade();
    side.addEventListener('scroll', function (e) {
      if (!e.target.classList || !e.target.classList.contains('nav-scroll')) return;
      syncFade();
      try { sessionStorage.setItem(NAV_SCROLL, String(Math.round(e.target.scrollTop))); } catch (err) {}
    }, { passive: true, capture: true });
    if (window.ResizeObserver) new ResizeObserver(syncFade).observe(list);
    else window.addEventListener('resize', syncFade);
    // A morph can add or remove sections (a feature switched in Settings).
    document.addEventListener('seam:content-updated', syncFade);
  })();

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

  // g was pressed: the hint lists every key the next press can take, as a
  // grid that wraps, and stays until that press (or Esc, or a click). It used
  // to vanish after 1.6 seconds, cut off mid-line.
  var chording = false;
  var chordHint = null;
  function endChord() {
    chording = false;
    if (chordHint) { chordHint.remove(); chordHint = null; }
  }
  function startChord() {
    endChord();
    chording = true;
    chordHint = document.createElement('div');
    chordHint.className = 'chord-hint';
    chordHint.setAttribute('role', 'status');
    var lead = document.createElement('kbd');
    lead.className = 'chord-lead';
    lead.textContent = 'g';
    chordHint.appendChild(lead);
    var list = document.createElement('ul');
    list.className = 'chord-keys';
    list.setAttribute('aria-label', 'then press');
    gotoLinks().forEach(function (a) {
      var item = document.createElement('li');
      var key = document.createElement('kbd');
      key.textContent = a.getAttribute('data-key');
      var label = a.querySelector('.nav-label');
      var name = document.createElement('span');
      name.textContent = label ? label.textContent : (a.getAttribute('data-tip') || '');
      item.appendChild(key);
      item.appendChild(name);
      list.appendChild(item);
    });
    chordHint.appendChild(list);
    document.body.appendChild(chordHint);
  }
  document.addEventListener('pointerdown', function () { if (chording) endChord(); }, true);

  document.addEventListener('keydown', function (e) {
    if (e.defaultPrevented) return;
    if (e.key === 'Escape') {
      endChord();
      if (keys && !keys.hidden) { e.preventDefault(); closeKeys(); return; }
      if (drawerOpen()) { e.preventDefault(); closeDrawer(true); return; }
      return;
    }
    // A modifier pressed alone is not the next key: the hint stays for it.
    if (chording && /^(Shift|Control|Alt|Meta|CapsLock)$/.test(e.key)) return;
    if (e.metaKey || e.ctrlKey || e.altKey || typing(e.target) || overlayOpen()) { endChord(); return; }
    if (chording) {
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

  // Four notes can be set aside (layout.html): the level banner, on a screen
  // above the console level; the update banner, for a day after the daemon
  // upgraded; and the update and backup alerts, while either is in trouble,
  // in one row when both are. "Got it" hides one in this browser, in every
  // tab and on the next visit (localStorage, as the theme), keyed by what it
  // is about: the screen, or "update:" plus the version or the episode, so a
  // newer update or a new streak shows again. The row of both alerts names
  // both keys and hides once both are set aside; each of its lines hides by
  // its own key (data-update-part). Without storage a banner hides for this
  // page only. Re-applied after every morph, which restores what the server
  // rendered.
  var BANNERS = 'seamless-level-banners';
  var BANNER = '[data-level-banner], [data-update-banner]';
  var BANNERS_KEPT = 50; // the newest keys: an older version's note never comes back anyway
  function bannerKeys(b) {
    var keys = b.getAttribute('data-update-banner');
    if (!keys) return [b.getAttribute('data-level-banner')];
    return keys.split(' ').filter(Boolean).map(function (k) { return 'update:' + k; });
  }
  function dismissed() {
    try {
      var list = JSON.parse(read(BANNERS) || '[]');
      return Array.isArray(list) ? list : [];
    } catch (e) { return []; }
  }
  function applyBanners() {
    var gone = dismissed();
    function out(key) { return gone.indexOf(key) !== -1; }
    document.querySelectorAll(BANNER).forEach(function (b) {
      if (bannerKeys(b).every(out)) b.hidden = true;
      b.querySelectorAll('[data-update-part]').forEach(function (part) {
        if (out('update:' + part.getAttribute('data-update-part'))) part.hidden = true;
      });
    });
  }
  document.addEventListener('click', function (e) {
    var x = e.target.closest ? e.target.closest('[data-level-banner-dismiss], [data-update-banner-dismiss]') : null;
    if (!x) return;
    var banner = x.closest(BANNER);
    if (!banner) return;
    var gone = dismissed();
    bannerKeys(banner).forEach(function (id) { if (gone.indexOf(id) === -1) gone.push(id); });
    store(BANNERS, JSON.stringify(gone.slice(-BANNERS_KEPT)));
    banner.hidden = true;
  });
  applyBanners();
  document.addEventListener('seam:content-updated', applyBanners);

  /* ---- Arrival: reveal, count-up, and the panel flashlight ------------------ */

  // html.reveal is set before first paint (layout.html), on the first page a
  // tab shows only, so that page settles in section by section and counts its
  // numbers up; every later page in the tab simply appears. It comes off once
  // the sequence has played, so a live morph that inserts a node later never
  // replays it.
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
      // The sections list scrolls: a section out of its view is reached at
      // the edge it lies beyond.
      var list = target && target.closest('.nav-scroll');
      if (list) {
        var lb = list.getBoundingClientRect();
        to = Math.max(lb.top - sr.top, Math.min(lb.bottom - sr.top, to));
      }
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

  // A tab bar that scrolls sideways (a project's, at phone width) shows its
  // active tab: a page opened on Sessions or Memories must not hide that tab
  // past the bar's edge (UX 24). Only a tab outside the view moves the bar,
  // so a reader's own scrolling stays while the tab they chose is in sight.
  function revealActiveTabs() {
    document.querySelectorAll('.tabs').forEach(function (bar) {
      var on = bar.querySelector('.on');
      if (!on || bar.scrollWidth <= bar.clientWidth) return;
      var b = bar.getBoundingClientRect(), r = on.getBoundingClientRect();
      if (r.left < b.left) bar.scrollLeft += r.left - b.left;
      else if (r.right > b.right) bar.scrollLeft += r.right - b.right;
    });
  }
  revealActiveTabs();
  document.addEventListener('seam:content-updated', revealActiveTabs);

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
    // A count links its screen only where this level's sidebar shows it: the
    // page names those screens in data-checkin-offer (a page drawn before it
    // carried the list links every count, as it always did).
    var offer = box.getAttribute('data-checkin-offer');
    var keys = offer === null ? null : offer.split(' ');
    function offered(key) { return keys === null || keys.indexOf(key) >= 0; }
    var items = [
      [d.memoriesWritten, 'memory written', 'memories written', '/console/memories?sort=recent', 'memories'],
      [d.sessions, 'session started', 'sessions started', '/console/sessions?w=' + w, 'sessions'],
      [d.tasksClosed, 'task closed', 'tasks closed', '/console/tasks', 'tasks'],
      [d.notesWritten, 'note written', 'notes written', '/console/notes', 'notes'],
      [d.proposals, 'proposal to review', 'proposals to review', '/console/gardener', 'gardener'],
      [d.mishaps, 'mishap reported', 'mishaps reported', '/console/mishaps', 'mishaps']
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
      items.forEach(function (it) { body.appendChild(chip(it[0], it[1], it[2], offered(it[4]) ? it[3] : '')); });
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
  // palette reads this list when it opens with an empty query. An event is
  // not: it is one step inside a session, opened in passing, and a run of them
  // would push every page worth reopening out of the eight slots.
  var ENTITY = /^\/console\/(memories|notes|tasks|plans|sessions|projects|labs|trials)\/[^/]+/;
  var KIND = { memories: 'Memory', notes: 'Note', tasks: 'Task', plans: 'Plan', sessions: 'Session', projects: 'Project', labs: 'Lab', trials: 'Trial' };
  // A title that is only a kind and a short id ("Task 4EKWAKDH") never
  // replaces a readable one the list already keeps for that page.
  var BARE = /^(Memory|Note|Task|Plan|Session|Project|Lab|Trial) [0-9A-Z]{8}$/;
  // The kept list, without what it no longer offers (an event kept before
  // events left it).
  function recentList() {
    var list;
    try { list = JSON.parse(read('seamless-recent') || '[]'); } catch (e) { list = []; }
    if (!Array.isArray(list)) return [];
    return list.filter(function (r) { return r && typeof r.href === 'string' && ENTITY.test(r.href); });
  }
  function remember(href, title) {
    var url;
    try { url = new URL(href, location.origin); } catch (e) { return; }
    var m = url.pathname.match(ENTITY);
    if (!m || !title) return;
    title = title.replace(/\s*·\s*Seamless$/, '').trim();
    if (!title) return;
    var list = recentList();
    var kept = list.filter(function (r) { return r.href === url.pathname; })[0];
    if (kept && kept.title && BARE.test(title) && !BARE.test(kept.title)) title = kept.title;
    list = list.filter(function (r) { return r.href !== url.pathname; });
    list.unshift({ href: url.pathname, title: title.slice(0, 120), kind: KIND[m[1]] || '' });
    store('seamless-recent', JSON.stringify(list.slice(0, 8)));
  }
  // On every page, so the palette stops offering what the list no longer
  // keeps even where nothing new is remembered.
  (function tidy() {
    var raw = read('seamless-recent');
    if (raw === null) return;
    var kept = JSON.stringify(recentList().slice(0, 8));
    if (kept !== raw) store('seamless-recent', kept);
  })();
  remember(location.pathname, document.title);
  document.addEventListener('click', function (e) {
    var a = e.target.closest ? e.target.closest('a[href]') : null;
    // A copy button inside a row link copies; it does not open the row.
    if (!a || e.target.closest('.copy-btn')) return;
    var title = a.getAttribute('data-title') || (a.querySelector('.rail-title, strong, .t') || a).textContent;
    remember(a.getAttribute('href'), (title || '').replace(/\s+/g, ' '));
  }, true);

  /* ---- Clock times in this browser's zone ---------------------------------- */

  // A time[data-clock] (the clock and clockDate template helpers) carries its
  // instant in datetime and the daemon's wall clock as its text; here it reads
  // as this browser's own wall clock, naming no zone, as the CLI prints the
  // terminal's. A data-clock="title" one (agoTime, timeAs) keeps its text, a
  // relative age, and trades its UTC title for this browser's date and time.
  // Fragments swapped in later are caught by the observer; a live refresh
  // morphs the server's text and title back onto the nodes it keeps (and drops
  // data-clocked), so every refresh localizes again.
  function two(n) { return (n < 10 ? '0' : '') + n; }
  function localClock(el) {
    if (el.getAttribute('data-clocked')) return;
    var d = new Date(el.getAttribute('datetime'));
    if (isNaN(d.getTime())) return;
    var text = two(d.getHours()) + ':' + two(d.getMinutes());
    var date = d.getFullYear() + '-' + two(d.getMonth() + 1) + '-' + two(d.getDate());
    var kind = el.getAttribute('data-clock');
    if (kind === 'title') el.setAttribute('title', date + ' ' + text);
    else el.textContent = kind === 'date' ? date + ' ' + text : text;
    el.setAttribute('data-clocked', '1');
  }
  function localClocks(scope) {
    if (scope.matches && scope.matches('time[data-clock]')) localClock(scope);
    var els = scope.querySelectorAll ? scope.querySelectorAll('time[data-clock]') : [];
    for (var i = 0; i < els.length; i++) localClock(els[i]);
  }
  localClocks(document);
  document.addEventListener('seam:content-updated', function () { localClocks(document); });
  if (window.MutationObserver && document.body) {
    new MutationObserver(function (records) {
      var left = false;
      for (var i = 0; i < records.length; i++) {
        var added = records[i].addedNodes;
        for (var j = 0; j < added.length; j++) {
          if (added[j].nodeType !== 1) continue;
          localClocks(added[j]);
          // A time left swapped in later (a fragment) counts down too.
          if (!left && (added[j].matches('time[data-left]') || added[j].querySelector('time[data-left]'))) left = true;
        }
      }
      if (left) tickLeft();
    }).observe(document.body, { childList: true, subtree: true });
  }

  /* ---- Confirm in the page: two-step buttons -------------------------------- */

  // A submit button with data-confirm asks before its form posts, in the page
  // instead of a native confirm() dialog. The first press arms it: the button
  // reads data-confirm-label ("Confirm" when absent) and the data-confirm
  // sentence shows beside it as a status line. A second press within six
  // seconds posts. Escape, a press anywhere else or the timeout puts it back.
  // The submit is caught in the capture phase, so the in-place client
  // (navigation.js) and a form's own listeners never see an unconfirmed post.
  // Without script the form posts on the first press, so a route behind a
  // confirm must still be safe to call.
  var armedBtn = null, armedNote = null, armedLabel = '', armedTimer = 0;
  function disarm() {
    if (!armedBtn) return;
    clearTimeout(armedTimer);
    armedBtn.innerHTML = armedLabel;
    armedBtn.removeAttribute('data-armed');
    armedBtn.removeAttribute('aria-describedby');
    if (armedNote && armedNote.parentNode) armedNote.parentNode.removeChild(armedNote);
    armedBtn = armedNote = null;
  }
  function arm(btn) {
    disarm();
    armedLabel = btn.innerHTML;
    btn.textContent = btn.getAttribute('data-confirm-label') || 'Confirm';
    btn.setAttribute('data-armed', '');
    var note = document.createElement('span');
    note.className = 'confirm-note';
    note.id = 'seam-confirm-note';
    note.setAttribute('role', 'status');
    note.textContent = btn.getAttribute('data-confirm');
    btn.insertAdjacentElement('afterend', note);
    btn.setAttribute('aria-describedby', note.id);
    armedBtn = btn;
    armedNote = note;
    armedTimer = setTimeout(disarm, 6000);
  }
  document.addEventListener('submit', function (e) {
    var btn = e.submitter;
    if (!btn || !btn.hasAttribute || !btn.hasAttribute('data-confirm')) return;
    if (btn === armedBtn) { disarm(); return; }
    e.preventDefault();
    e.stopPropagation();
    arm(btn);
  }, true);
  document.addEventListener('click', function (e) {
    if (!armedBtn) return;
    var hit = e.target && e.target.closest ? e.target.closest('button[data-armed]') : null;
    if (hit !== armedBtn) disarm();
  }, true);
  document.addEventListener('keydown', function (e) {
    if (armedBtn && e.key === 'Escape') disarm();
  });

  /* ---- The shared parts (UX 33): hints, switches that save at once, time left */

  function say(message, tone, action) {
    if (window.SeamConsole && window.SeamConsole.flash) window.SeamConsole.flash(message, tone, action);
  }
  function words(el) { return el ? (el.textContent || '').replace(/\s+/g, ' ').trim() : ''; }

  // Hints ({{hint}} and {{hintFor}}, kit.go): an (i) button and its tooltip.
  // CSS opens one on hover and on keyboard focus. A tap or a click pins it
  // open (.is-pinned); the same again, Esc, a tap outside, or another hint
  // opening closes it, so one is open at a time. A hint closed while the
  // pointer or focus is still on it stays closed (.is-shut) until both have
  // left it. Esc is taken in the capture phase, before panel.js's, and only
  // when it closes a hint (preventDefault): one Esc closes the innermost
  // thing. The live morph resets classes to the server's, so the pinned
  // hint, kept here by its tooltip's id, is pinned again after each one.
  var pinnedHint = '';
  function hintAt(el) { return el && el.closest ? el.closest('.hint') : null; }
  function hintTip(h) {
    var b = h.querySelector('.hint-btn');
    return b ? b.getAttribute('aria-describedby') || '' : '';
  }
  function hintOpen(h) {
    if (h.classList.contains('is-shut')) return false;
    return h.classList.contains('is-pinned') || h.matches(':hover') || h.contains(document.activeElement);
  }
  function shutHint(h) {
    h.classList.remove('is-pinned');
    h.classList.add('is-shut');
    if (hintTip(h) === pinnedHint) pinnedHint = '';
  }
  function unpinHint(h) {
    h.classList.remove('is-pinned');
    if (hintTip(h) === pinnedHint) pinnedHint = '';
  }
  function shutOtherHints(except) {
    document.querySelectorAll('.hint').forEach(function (h) {
      if (h !== except && hintOpen(h)) shutHint(h);
    });
  }
  // Keep an open tooltip inside the window, and inside the page's own column
  // when it sits in one (.main): nudged sideways, never clipped and never
  // under the sidebar, which stacks above the column.
  function placeHint(h) {
    var pop = h.querySelector('.hint-pop');
    if (!pop) return;
    pop.style.removeProperty('--hint-dx');
    var r = pop.getBoundingClientRect();
    if (!r.width) return;
    var left = 12, edge = document.documentElement.clientWidth - 12;
    var col = h.closest('.main');
    if (col) {
      var c = col.getBoundingClientRect();
      left = Math.max(left, c.left + 12);
      edge = Math.min(edge, c.right - 12);
    }
    var dx = r.right > edge ? edge - r.right : (r.left < left ? left - r.left : 0);
    if (dx) pop.style.setProperty('--hint-dx', Math.round(dx) + 'px');
  }
  function enterHint(e) {
    var h = hintAt(e.target);
    if (!h || h.contains(e.relatedTarget)) return;
    h.classList.remove('is-shut');
    shutOtherHints(h);
    placeHint(h);
  }
  document.addEventListener('mouseover', enterHint);
  document.addEventListener('focusin', enterHint);
  document.addEventListener('mouseout', function (e) {
    var h = hintAt(e.target);
    if (h && !h.contains(e.relatedTarget) && !h.contains(document.activeElement)) h.classList.remove('is-shut');
  });
  document.addEventListener('focusout', function (e) {
    var h = hintAt(e.target);
    if (h && !h.contains(e.relatedTarget) && !h.matches(':hover')) h.classList.remove('is-shut');
  });
  document.addEventListener('click', function (e) {
    var btn = e.target.closest ? e.target.closest('.hint-btn') : null;
    var h = hintAt(btn);
    if (h) {
      e.preventDefault();
      if (h.classList.contains('is-pinned')) { shutHint(h); return; }
      shutOtherHints(h);
      h.classList.remove('is-shut');
      h.classList.add('is-pinned');
      pinnedHint = hintTip(h);
      placeHint(h);
      return;
    }
    if (hintAt(e.target)) return; // a tap on the tooltip itself keeps it
    document.querySelectorAll('.hint.is-pinned').forEach(unpinHint);
  });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape' || e.defaultPrevented) return;
    var open = Array.prototype.filter.call(document.querySelectorAll('.hint'), hintOpen);
    if (!open.length) return;
    e.preventDefault();
    open.forEach(shutHint);
  }, true);
  document.addEventListener('seam:content-updated', function () {
    if (!pinnedHint) return;
    var h = hintAt(document.getElementById(pinnedHint));
    if (h) h.classList.add('is-pinned');
    else pinnedHint = '';
  });

  // Switches that save at once (D9): a form with data-autosave posts as soon
  // as one of its switches (checkboxes), radios or selects changes. Typing
  // in a text or number field never posts. The post is the form's own
  // submit (requestSubmit), which navigation.js takes as it takes every
  // owner POST inside .main: X-Seam-Follow, the server's redirect followed in
  // place, never a fetch of this script's own. While it is out the control
  // (a radio's whole group) is disabled and aria-busy. navigation.js ends it
  // with seam:submitted. Saved, the toast names the change ("Momentum is
  // off.", by the control's data-autosave-label, aria-label or label), adds
  // the data-autosave-note its row or form carries, when one does ("Agents
  // see it from their next session."), and offers Undo, which posts the
  // values the form held before the change. Not saved
  // (no answer, or the server's ?error=), the control goes back and the
  // toast says why, with Try again. The values a form holds are read when
  // the page is drawn and after every morph, so they are the server's.
  var autoValues = window.WeakMap ? new WeakMap() : null;
  var autoSaving = null;
  function autoField(el) {
    return !!el && (el.type === 'checkbox' || el.type === 'radio' || el.tagName === 'SELECT');
  }
  function formValues(form) {
    var out = [];
    try {
      new FormData(form).forEach(function (value, key) { if (typeof value === 'string') out.push([key, value]); });
    } catch (e) {}
    return out;
  }
  function keepAutoValues() {
    if (!autoValues) return;
    document.querySelectorAll('form[data-autosave]').forEach(function (form) { autoValues.set(form, formValues(form)); });
  }
  // setFormValues puts one field's controls back to the values in list. A
  // control the server drew disabled (the config file decides) is left alone.
  function setFormValues(form, list, name) {
    var mine = list.filter(function (p) { return p[0] === name; }).map(function (p) { return p[1]; });
    Array.prototype.forEach.call(form.elements, function (el) {
      if (el.name !== name || (el.disabled && !el.hasAttribute('data-autosave-busy'))) return;
      if (el.type === 'checkbox' || el.type === 'radio') el.checked = mine.indexOf(el.value) !== -1;
      else if (el.tagName === 'SELECT' && !el.multiple && mine.length) el.value = mine[0];
    });
  }
  function autoLabel(el, form) {
    var named = el.closest('[data-autosave-label]');
    if (named && form.contains(named)) return named.getAttribute('data-autosave-label');
    var box = el.type === 'radio' ? el.closest('[role="radiogroup"], fieldset') : null;
    var from = box || el;
    if (from.getAttribute('aria-label')) return from.getAttribute('aria-label');
    var by = from.getAttribute('aria-labelledby');
    if (by) return by.split(/\s+/).map(function (id) { return words(document.getElementById(id)); }).join(' ').trim();
    if (box && box.tagName === 'FIELDSET') return words(box.querySelector('legend'));
    var label = !box && el.labels && el.labels[0];
    return label ? words(label.querySelector('strong') || label) : '';
  }
  function choiceWords(el) {
    if (el.getAttribute('aria-label')) return el.getAttribute('aria-label');
    var label = el.labels && el.labels[0];
    return words(label) || el.value;
  }
  // autoNote is the sentence a saved change adds after its words: the
  // data-autosave-note of the control's nearest holder inside the form (the
  // form itself included), or none.
  function autoNote(el, form) {
    var noted = el.closest('[data-autosave-note]');
    return noted && form.contains(noted) ? ' ' + noted.getAttribute('data-autosave-note') : '';
  }
  // describe words a field's state, read now, while the page shows it: the
  // toast that follows the save may come after a morph.
  function describe(form, control) {
    var name = autoLabel(control, form);
    var note = autoNote(control, form);
    var field = control.name;
    var choices = {};
    if (control.type === 'radio') {
      Array.prototype.forEach.call(form.elements, function (el) {
        if (el.type === 'radio' && el.name === field) choices[el.value] = choiceWords(el);
      });
    } else if (control.tagName === 'SELECT') {
      Array.prototype.forEach.call(control.options, function (o) { choices[o.value] = words(o); });
    }
    return function (list) {
      var values = list.filter(function (p) { return p[0] === field; }).map(function (p) { return p[1]; });
      if (!name) return 'Saved.' + note;
      if (control.type === 'checkbox') return name + ' is ' + (values.indexOf(control.value) !== -1 ? 'on' : 'off') + '.' + note;
      return name + ': ' + (choices[values[0]] || values[0] || 'none') + '.' + note;
    };
  }
  function autoGroup(form, control) {
    if (control.type !== 'radio') return [control];
    return Array.prototype.filter.call(form.elements, function (el) {
      return el.type === 'radio' && el.name === control.name && !el.disabled;
    });
  }
  // liveForm finds a form again after a morph that drew it anew.
  function liveForm(form) {
    if (form.isConnected) return form;
    if (form.id) return document.getElementById(form.id);
    var action = form.getAttribute('action') || '';
    var same = Array.prototype.filter.call(document.querySelectorAll('form[data-autosave]'), function (f) {
      return (f.getAttribute('action') || '') === action;
    });
    return same.length === 1 ? same[0] : null;
  }
  // liveControl finds the control again: the one changed, or, after a morph
  // drew it anew, the field's control by name.
  function liveControl(form, save) {
    if (save.control.isConnected && save.control.form === form) return save.control;
    var group = Array.prototype.filter.call(form.elements, function (el) { return el.name === save.name && autoField(el); });
    return group.filter(function (el) { return el.checked; })[0] || group[0] || null;
  }
  // focusField gives focus back to a field: a radio group's checked radio,
  // or the control itself.
  function focusField(form, control) {
    var target = control;
    if (control.type === 'radio') {
      target = Array.prototype.filter.call(form.elements, function (el) {
        return el.type === 'radio' && el.name === control.name && el.checked;
      })[0] || control;
    }
    try { target.focus({ preventScroll: true }); } catch (e) {}
  }
  function autosave(form, control, from, to, opts) {
    if (typeof form.requestSubmit !== 'function') return;
    var name = control.name;
    if (autoSaving) {
      // One save at a time: this change goes back, and says so.
      setFormValues(form, from, name);
      say('Not saved. Another change is still saving.', 'error', {
        label: 'Try again', run: function () { retry({ form: form, control: control, name: name, from: from, to: to, undoing: opts.undoing }); }
      });
      return;
    }
    if (opts.replay) {
      setFormValues(form, to, name);
      focusField(form, control);
    }
    var group = autoGroup(form, control);
    var save = {
      form: form, control: control, name: name, from: from, to: to, undoing: !!opts.undoing,
      words: describe(form, control), group: group, sent: false,
      refocus: !!opts.replay || group.indexOf(document.activeElement) !== -1
    };
    autoSaving = save;
    form.requestSubmit();
    if (!save.sent) {
      // navigation.js did not take it: a form the browser found invalid, or
      // one outside .main, which the browser posts itself.
      autoSaving = null;
      return;
    }
    group.forEach(function (el) {
      el.setAttribute('data-autosave-busy', '');
      el.setAttribute('aria-busy', 'true');
      el.disabled = true;
    });
  }
  // replay posts a field's values again from the toast (Undo, Try again):
  // focus leaves the toast for the control it is about.
  function replay(save, from, to, undoing) {
    var form = liveForm(save.form);
    var control = form && liveControl(form, save);
    if (!control) return;
    autosave(form, control, from, to, { replay: true, undoing: undoing });
  }
  function retry(save) { replay(save, save.from, save.to, save.undoing); }
  document.addEventListener('submit', function (e) {
    if (autoSaving && e.target === autoSaving.form) autoSaving.sent = e.defaultPrevented;
  });
  document.addEventListener('change', function (e) {
    var el = e.target;
    var form = el && el.form;
    if (!form || !form.hasAttribute('data-autosave') || !autoField(el)) return;
    var before = autoValues && autoValues.get(form);
    autosave(form, el, before || [], formValues(form), {});
  });
  document.addEventListener('seam:submitted', function (e) {
    var save = autoSaving;
    var d = e.detail || {};
    if (!save || d.form !== save.form) return;
    autoSaving = null;
    save.group.forEach(function (el) {
      // A control the morph drew afresh already carries the server's state.
      if (!el.hasAttribute('data-autosave-busy')) return;
      el.removeAttribute('data-autosave-busy');
      el.removeAttribute('aria-busy');
      el.disabled = false;
    });
    var form = liveForm(save.form);
    if (d.ok) {
      say(save.words(save.to), 'ok', save.undoing ? null : {
        label: 'Undo', run: function () { replay(save, save.to, save.from, true); }
      });
    } else {
      if (form) setFormValues(form, save.from, save.name);
      say('Not saved. ' + (d.error || 'Seamless did not answer.'), 'error', {
        label: 'Try again', run: function () { retry(save); }
      });
    }
    // A disabled control loses focus; give it back to the field.
    var active = document.activeElement;
    var control = form && liveControl(form, save);
    if (save.refocus && control && (!active || active === document.body)) focusField(form, control);
    keepAutoValues();
  });
  keepAutoValues();
  document.addEventListener('seam:content-updated', keepAutoValues);

  // Time left (time[data-left], kit.go timeLeft): the server writes the
  // words, and shell.js counts them down every 30 seconds while the page is
  // in sight, as kit.go's leftWords does: warn from 30 minutes, when renewal
  // warnings start, and "ended" at the end, where it stops. A hidden page
  // keeps no timer (console-streams-held-only-while-visible); shown again,
  // it counts at once. The title, the end in this browser's clock, is the
  // time[data-clock] rule's.
  var LEFT_TICK_MS = 30000;
  var LEFT_WARN_MS = 30 * 60 * 1000;
  var leftTimer = 0;
  function leftWords(ms) {
    if (ms <= 0) return ['ended', ''];
    var tone = ms <= LEFT_WARN_MS ? 'warn' : 'ok';
    var mins = Math.floor(ms / 60000);
    if (mins < 1) return ['under 1 min left', tone];
    if (mins < 60) return [mins + ' min left', tone];
    if (mins < 1440) return [Math.floor(mins / 60) + ' h ' + (mins % 60 ? (mins % 60) + ' m ' : '') + 'left', tone];
    var hours = Math.floor((mins % 1440) / 60);
    return [Math.floor(mins / 1440) + ' d ' + (hours ? hours + ' h ' : '') + 'left', tone];
  }
  function tickLeft() {
    if (leftTimer) { clearTimeout(leftTimer); leftTimer = 0; }
    if (document.visibilityState === 'hidden') return;
    var counting = 0;
    var now = Date.now();
    document.querySelectorAll('time[data-left][datetime]').forEach(function (el) {
      var end = Date.parse(el.getAttribute('datetime'));
      if (isNaN(end)) return;
      var left = leftWords(end - now);
      var text = el.querySelector('span');
      if (text && text.textContent !== left[0]) text.textContent = left[0];
      el.classList.toggle('ok', left[1] === 'ok');
      el.classList.toggle('warn', left[1] === 'warn');
      if (left[1]) counting++;
    });
    if (counting) leftTimer = setTimeout(tickLeft, LEFT_TICK_MS);
  }
  document.addEventListener('visibilitychange', tickLeft);
  document.addEventListener('seam:content-updated', tickLeft);
  tickLeft();
})();
