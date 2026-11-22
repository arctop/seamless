/* Read in place (plan:console-ux-pass UX 31). A plain click on a link to a
   memory, a note, a task, a plan, a session, an event or a trial opens the
   item's ?peek=1 fragment in one wide panel over the page; Esc, Close or the
   browser's Back closes it. The page behind is never reloaded and never
   morphed by the panel: it keeps its scroll, its folds and its live state.
   A modified click, a middle click or a link from outside the console still
   opens the item's own page. A project link is not read here: it goes to the
   project.

   The panel (#read-panel, with its scrim) sits outside .main in layout.html,
   so a live morph of the page behind leaves it as it is, and the page's own
   live refresh goes on while it is open.

   window.SeamPanel = { isOpen, open, close, leave }:
     isOpen()             whether the panel shows.
     open(href, opener)   loads href's ?peek=1 fragment into the panel and
                          returns true (false for a URL it does not read).
                          Focus returns to opener on close. An opener inside
                          the panel is a step deeper (a new history entry).
     close()              closes it, through the history entries it added.
     leave(href)          records this page as the item page's way back,
                          then opens href as a full page.

   Clicks: one bubble-phase listener on the document. It leaves alone a
   click already handled (defaultPrevented: navigation.js's data-seam-query
   links, library.js's rail and reader, scope.js, the copy buttons and every
   other in-place handler loaded before this script), one with a modifier key
   or not the main button (the browser's new tab), one inside a form or on a
   button, and these in-place parts: data-jump anchors, the plan card's
   data-open-step links, the Diary's and Interactions' detail split (which
   owns every click in its region). A link marked data-no-panel (or inside
   one), the panel's own Open full page and the step dossier's "open full"
   navigate as a full page, and record the way back first.

   Keys: a capture-phase listener on the document. While the panel is open it
   takes Esc, j and k only, and skips an event already handled (the palette,
   the menus and the page-about close on Esc first, in capture listeners
   registered before this one), with a modifier, from a text field, or while
   the shortcut sheet, the drawer, a g-chord or an armed confirm holds the
   key (those let go of Esc in the bubble phase).

   History: opening pushes #peek=<href> (one entry, state.seamPanel), so
   Back closes the panel. A link inside the panel pushes another entry, so
   Back steps back through them; k and j replace the entry. Close and Esc go
   back through every entry the panel added. A reload with #peek= reopens
   it without adding an entry (close then strips the hash).

   The way back: before the tab leaves for an item's own page, this page's
   URL and title go to sessionStorage; the item page's back link
   (a[data-item-back], templates/item_shared.html) then reads "Back to <that
   title>" and goes there, again after each in-place morph. With no record
   it keeps its own link (the console sends no Referer).

   Actions inside the panel (Mark done, star, archive...) post through
   fetch with X-Seam-Follow (console-mutation-follow-contract): the answer's
   notice or error shows as a toast, the panel draws the item again, and an
   item that is gone (a deleted note) closes the panel. The page behind
   follows through its own live refresh, never through the panel. */
(function () {
  'use strict';

  var ITEM = /^\/console\/(memories|notes|tasks|plans|sessions|events|trials)\/[^/]+$/;
  var KINDS = { memories: 'Memory', notes: 'Note', tasks: 'Task', plans: 'Plan', sessions: 'Session', events: 'Event', trials: 'Trial' };
  var LISTS = { memories: 'Memories', notes: 'Notes', tasks: 'Tasks', plans: 'Plans', sessions: 'Sessions', events: 'Events', trials: 'Trials' };
  // Links that open the item's own page on purpose: they navigate, and the
  // page they leave becomes the item page's way back.
  var FULL = '[data-no-panel], .dossier-open';
  // Parts with an in-place behaviour of their own that may not cancel the
  // click first: the panel stays out of their way.
  var INPLACE = '[data-jump], [data-open-step], .list-detail-split';
  var BACK_KEY = 'seamless-way-back';
  var HASH = '#peek=';

  function panel() { return document.getElementById('read-panel'); }
  function scrim() { return document.getElementById('read-scrim'); }
  function body() { return document.getElementById('read-panel-body'); }
  function inPanel(el) { var p = panel(); return !!(p && el && p.contains(el)); }
  function isOpen() { var p = panel(); return !!(p && !p.hidden); }

  // itemURL is href as an item the panel reads, or null: same origin, one of
  // the kinds above, the item's own path (never a sub-route such as a
  // session's transcript). The fragment is dropped; the query is kept. An
  // in-page #anchor is never an item, even on an item's own page.
  function itemURL(href) {
    if (!href || href.charAt(0) === '#') return null;
    var url;
    try { url = new URL(href, location.href); } catch (e) { return null; }
    if (url.origin !== location.origin || !ITEM.test(url.pathname)) return null;
    url.hash = '';
    return url;
  }
  function pathOf(url) { return url.pathname + url.search; }
  function kindOf(href) { var m = /^\/console\/([a-z]+)\//.exec(href || ''); return m ? m[1] : ''; }
  function hrefOf(el) {
    if (!el || !el.getAttribute) return '';
    var url = itemURL(el.getAttribute('href') || el.getAttribute('data-href'));
    return url ? pathOf(url) : '';
  }
  // The fragment's URL keeps the link's own query (a plan's ?project=).
  function peekURL(href) {
    var url = new URL(href, location.href);
    url.searchParams.set('peek', '1');
    return url.pathname + url.search;
  }
  function typing(el) {
    if (!el) return false;
    var tag = el.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || !!el.isContentEditable;
  }
  function shown(el) { return !!(el && el.getClientRects && el.getClientRects().length); }

  // Something else holds Esc, j and k this instant: the palette or the
  // shortcut sheet over the panel, the phone drawer, a g-chord waiting for
  // its key, or a confirm armed in the panel. Each lets go of the key itself.
  function othersHoldKeys() {
    var ids = ['cmdk', 'keys'];
    for (var i = 0; i < ids.length; i++) {
      var el = document.getElementById(ids[i]);
      if (el && !el.hidden) return true;
    }
    if (document.documentElement.hasAttribute('data-drawer')) return true;
    if (document.querySelector('.chord-hint')) return true;
    var p = panel();
    return !!(p && p.querySelector('button[data-armed]'));
  }

  /* ---- State ---------------------------------------------------------------- */

  var current = '';   // the href the panel shows
  var from = null;    // the page element focus returns to, and the row marked
  var list = [];      // the hrefs k and j step through
  var index = -1;     // the opener's place in list
  var seq = 0;        // the newest load; an older answer is dropped
  var inflight = null;
  var closing = false;
  var closeTimer = null;

  // The list the panel was opened from, in document order: when the opener is
  // a list row's own link (or the row), the page's rows; otherwise the page's
  // links to items of the same kind. Only what shows counts (a closed tab or
  // fold holds nothing to step to), and each item once.
  function listFrom(opener, href) {
    var main = document.querySelector('.main');
    if (!opener || !main || !main.contains(opener)) return [href];
    var row = opener.closest('.row-link[data-href]');
    var rows = !!row && hrefOf(row) === href;
    var kind = kindOf(href);
    var seen = {};
    var out = [];
    main.querySelectorAll(rows ? '.row-link[data-href]' : 'a[href], .row-link[data-href]').forEach(function (el) {
      if (el.closest(INPLACE + ', ' + FULL) || !shown(el)) return;
      var h = hrefOf(el);
      if (!h || seen[h] || (!rows && kindOf(h) !== kind)) return;
      seen[h] = true;
      out.push(h);
    });
    return out.indexOf(href) >= 0 ? out : [href];
  }

  // findInPage is the page's element for href: its row when it has one,
  // else its first link. It finds again the opener a live morph replaced.
  function findInPage(href) {
    var main = document.querySelector('.main');
    if (!main || !href) return null;
    var hit = null;
    main.querySelectorAll('.row-link[data-href], a[href]').forEach(function (el) {
      if (!hit && hrefOf(el) === href && shown(el)) hit = el;
    });
    return hit;
  }
  // found sets the list from the page's own element for href, for a panel
  // that opens with no click (Back, Forward, a reload).
  function found(href) {
    from = findInPage(href);
    list = listFrom(from, href);
    index = list.indexOf(href);
  }
  function opener() {
    if (from && from.isConnected) return from;
    from = findInPage(index >= 0 ? list[index] : current);
    return from;
  }
  // The row the panel reads from is marked while it is open. A morph resets
  // the page to what the server drew, so the mark goes back on after one.
  function markRow() {
    document.querySelectorAll('[data-panel-current]').forEach(function (el) { el.removeAttribute('data-panel-current'); });
    if (!isOpen()) return;
    var el = opener();
    var row = el && el.closest ? el.closest('.row-link') : null;
    if (row) row.setAttribute('data-panel-current', '');
  }
  function focusTarget(el) {
    if (!el) return null;
    if (el.matches && el.matches('a[href], button')) return el;
    var h = hrefOf(el);
    var links = el.querySelectorAll ? el.querySelectorAll('a[href]') : [];
    for (var i = 0; i < links.length; i++) if (hrefOf(links[i]) === h) return links[i];
    return null;
  }

  /* ---- History -------------------------------------------------------------- */

  // depth is how many entries the panel added below the one the tab is on.
  function depth() {
    var st = history.state;
    return st && st.seamPanel ? (st.seamPanelDepth || 0) : 0;
  }
  // stateFor keeps what the entry's state already says (navigation.js's
  // seamView) and adds the panel's own two fields.
  function stateFor(href, d) {
    var st = {};
    var old = history.state;
    if (old && typeof old === 'object') {
      for (var k in old) if (Object.prototype.hasOwnProperty.call(old, k)) st[k] = old[k];
    }
    st.seamPanel = href;
    st.seamPanelDepth = d;
    return st;
  }
  function hashFor(href) { return location.pathname + location.search + HASH + encodeURIComponent(href); }
  // strip takes the panel out of the entry the tab is on, hash and state.
  function strip() {
    var st = {};
    var old = history.state;
    if (old && typeof old === 'object') {
      for (var k in old) if (Object.prototype.hasOwnProperty.call(old, k) && k !== 'seamPanel' && k !== 'seamPanelDepth') st[k] = old[k];
    }
    try { history.replaceState(st, '', location.pathname + location.search); } catch (e) {}
  }
  function hashHref() {
    if (location.hash.indexOf(HASH) !== 0) return '';
    var raw;
    try { raw = decodeURIComponent(location.hash.slice(HASH.length)); } catch (e) { return ''; }
    var url = itemURL(raw);
    return url ? pathOf(url) : '';
  }
  // A page with the Diary's or Interactions' detail split keeps the #peek=
  // links it always had: they are its own, unless the entry says the panel's.
  function splitPage() { return !!(document.querySelector('.list-detail-split') && document.getElementById('detail-pane')); }

  /* ---- Showing an item ------------------------------------------------------ */

  // show draws href in the panel. how says what history does: push (opened
  // from the page, or a link inside the panel: one more entry), replace (k /
  // j, or a new opener while open), none (Back, Forward, a reload).
  function show(href, how) {
    var p = panel(), s = scrim();
    if (!p) return false;
    if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
    closing = false;
    try {
      if (how === 'push') history.pushState(stateFor(href, depth() + 1), '', hashFor(href));
      else if (how === 'replace') history.replaceState(stateFor(href, depth()), '', hashFor(href));
    } catch (e) {}
    var fresh = href !== current;
    current = href;
    p.setAttribute('aria-label', (KINDS[kindOf(href)] || 'Item') + ', read in place');
    var full = p.querySelector('[data-panel-full]');
    if (full) full.setAttribute('href', href);
    steps();
    if (p.hidden) {
      p.hidden = false;
      if (s) s.hidden = false;
      try { p.focus({ preventScroll: true }); } catch (e) {}
    }
    markRow();
    load(href, !fresh);
    return true;
  }
  function steps() {
    var p = panel();
    if (!p) return;
    var prev = p.querySelector('[data-panel-step="-1"]');
    var next = p.querySelector('[data-panel-step="1"]');
    if (prev) prev.disabled = index <= 0;
    if (next) next.disabled = index < 0 || index >= list.length - 1;
  }

  // load fetches the fragment. A new item starts at its top; the same item
  // drawn again (after an action, or once the stream is back) keeps the
  // panel's scroll. again is such a redraw: an item gone by then (a 404) is
  // left undrawn, for the caller to close.
  function load(href, again) {
    var b = body();
    if (!b) return Promise.resolve(null);
    var mine = ++seq;
    if (inflight) { try { inflight.abort(); } catch (e) {} }
    var stop = window.AbortController ? new AbortController() : null;
    inflight = stop;
    b.setAttribute('aria-busy', 'true');
    return fetch(peekURL(href), { credentials: 'same-origin', headers: { Accept: 'text/html' }, signal: stop ? stop.signal : undefined })
      .then(function (r) {
        var landed = new URL(r.url || location.href, location.href);
        if (landed.pathname === '/console/login') {
          // An expired sign-in is the one navigation a read may make.
          location.assign('/console/login?next=' + encodeURIComponent(location.pathname + location.search));
          return null;
        }
        // A fragment-shaped answer (a stale link's 404) is drawn as it is.
        var html = (r.headers.get('Content-Type') || '').indexOf('text/html') === 0;
        if (!r.ok && !(html && r.status < 500)) throw new Error('http ' + r.status);
        return r.text().then(function (text) { return { status: r.status, text: text }; });
      })
      .then(function (answer) {
        if (!answer || mine !== seq || current !== href) return answer;
        if (again && answer.status === 404) return answer;
        var top = b.scrollTop;
        b.innerHTML = answer.text;
        b.scrollTop = again ? top : 0;
        if (window.IX && window.IX.enhance) { try { window.IX.enhance(b); } catch (e) {} }
        where(b, href);
        return answer;
      })
      .catch(function (error) {
        if (mine !== seq || (error && error.name === 'AbortError')) return null;
        b.textContent = '';
        var line = document.createElement('p');
        line.className = 'read-panel-error';
        line.textContent = 'Could not load this item. The page behind is unchanged, and Open full page shows the item on its own page.';
        b.appendChild(line);
        return null;
      })
      .finally(function () {
        if (mine !== seq) return;
        inflight = null;
        b.removeAttribute('aria-busy');
      });
  }

  // where names the item's place in the header: its project, then its
  // collection (a task in a plan: the plan). The fragment names the project
  // (data-panel-project, "" for the global scope); a plan's link names it in
  // ?project=. A fragment that names none (a stale link's error) shows the
  // collection alone.
  function where(b, href) {
    var p = panel();
    var line = p && p.querySelector('[data-panel-where]');
    if (!line) return;
    line.textContent = '';
    var kind = kindOf(href);
    var root = b.querySelector('[data-panel-project]');
    var known = !!root;
    var project = root ? root.getAttribute('data-panel-project') : '';
    var plan = root ? root.getAttribute('data-panel-plan') : '';
    if (kind === 'plans') {
      try { project = new URL(href, location.href).searchParams.get('project'); } catch (e) { project = null; }
      known = project !== null;
      if (project === 'global') project = '';
    }
    if (known) {
      if (project) {
        var a = document.createElement('a');
        a.href = '/console/projects/' + encodeURIComponent(project);
        a.textContent = project;
        line.appendChild(a);
      } else {
        line.appendChild(document.createTextNode(kind === 'notes' ? 'inbox' : 'global'));
      }
      var sep = document.createElement('span');
      sep.className = 'read-panel-sep';
      sep.setAttribute('aria-hidden', 'true');
      sep.textContent = '›';
      line.appendChild(sep);
    }
    if (kind === 'tasks' && plan) {
      var pl = document.createElement('a');
      pl.href = '/console/plans/' + encodeURIComponent(plan) + '?project=' + encodeURIComponent(project || 'global');
      pl.className = 'mono';
      pl.textContent = 'plan:' + plan;
      line.appendChild(pl);
    } else {
      line.appendChild(document.createTextNode(LISTS[kind] || ''));
    }
  }

  /* ---- The API -------------------------------------------------------------- */

  function open(href, opener) {
    var url = itemURL(href);
    if (!url || !panel()) return false;
    var h = pathOf(url);
    if (opener && inPanel(opener)) {
      if (h === current && isOpen()) return true;
      return show(h, 'push');
    }
    var wasOpen = isOpen();
    from = opener || null;
    list = listFrom(opener, h);
    index = list.indexOf(h);
    return show(h, wasOpen ? 'replace' : 'push');
  }

  function close() {
    if (!isOpen()) return;
    var d = depth();
    if (d > 0) {
      // Back through every entry the panel added; popstate tears it down.
      closing = true;
      try { history.go(-d); } catch (e) { closing = false; }
      if (closing) {
        closeTimer = setTimeout(function () { closeTimer = null; if (closing) { closing = false; teardown(); } }, 600);
        return;
      }
    }
    if (location.hash.indexOf(HASH) === 0 || (history.state && history.state.seamPanel)) strip();
    teardown();
  }

  function teardown() {
    var p = panel(), s = scrim(), b = body();
    if (inflight) { try { inflight.abort(); } catch (e) {} inflight = null; }
    seq++;
    var back = focusTarget(opener());
    var hadFocus = !!(p && (p.contains(document.activeElement) || document.activeElement === document.body));
    if (p) p.hidden = true;
    if (s) s.hidden = true;
    if (b) { b.textContent = ''; b.removeAttribute('aria-busy'); }
    var line = p && p.querySelector('[data-panel-where]');
    if (line) line.textContent = '';
    current = '';
    list = [];
    index = -1;
    markRow();
    from = null;
    // Focus goes back to the link that opened it, without moving the page.
    if (back && hadFocus && typeof back.focus === 'function') {
      try { back.focus({ preventScroll: true }); } catch (e) {}
    }
  }

  function leave(href) {
    rememberWayBack(href);
    location.href = href;
  }

  // stepBy moves through the list the panel was opened from; the page
  // behind scrolls only as far as it takes to keep that row in sight.
  function stepBy(d) {
    var i = index + d;
    if (index < 0 || i < 0 || i >= list.length) return;
    index = i;
    from = findInPage(list[i]);
    if (from && from.scrollIntoView) { try { from.scrollIntoView({ block: 'nearest' }); } catch (e) {} }
    show(list[i], 'replace');
  }

  window.SeamPanel = { isOpen: isOpen, open: open, close: close, leave: leave };

  /* ---- The way back --------------------------------------------------------- */

  function pageTitle() { return (document.title || '').replace(/\s*·\s*Seamless$/, '').trim(); }
  function rememberWayBack(to) {
    var url = itemURL(to);
    var title = pageTitle();
    if (!url || !title) return;
    var here = new URL(location.href);
    if (here.hash.indexOf(HASH) === 0) here.hash = '';
    try {
      sessionStorage.setItem(BACK_KEY, JSON.stringify({ href: here.pathname + here.search + here.hash, title: title.slice(0, 80), to: url.pathname }));
    } catch (e) {}
  }
  function wayBack() {
    try {
      var rec = JSON.parse(sessionStorage.getItem(BACK_KEY) || 'null');
      if (!rec || typeof rec.href !== 'string' || typeof rec.title !== 'string' || typeof rec.to !== 'string') return null;
      if (rec.href.indexOf('/console/') !== 0 || rec.href.indexOf('/console//') === 0 || !rec.title) return null;
      return rec;
    } catch (e) { return null; }
  }
  // The record applies to the item page it was made for, and keeps applying
  // in this document as k / j load the next items in place: it then follows
  // them, so a reload of the item reached that way still finds it.
  var following = false;
  function applyWayBack() {
    var a = document.querySelector('.main a[data-item-back]');
    var rec = a ? wayBack() : null;
    if (!rec) return;
    if (rec.to !== location.pathname) {
      if (!following || !ITEM.test(location.pathname)) return;
      rec.to = location.pathname;
      try { sessionStorage.setItem(BACK_KEY, JSON.stringify(rec)); } catch (e) {}
    }
    if (rec.href === location.pathname + location.search) return;
    following = true;
    a.setAttribute('href', rec.href);
    a.setAttribute('data-way-back', '');
    var label = a.querySelector('span');
    if (label) {
      label.textContent = 'Back to ' + rec.title;
      label.classList.remove('mono');
    }
  }
  applyWayBack();

  /* ---- Clicks --------------------------------------------------------------- */

  document.addEventListener('click', function (e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var t = e.target;
    if (!t || !t.closest) return;
    if (isOpen()) {
      if (t.closest('#read-scrim, #read-panel [data-panel-close]')) { e.preventDefault(); close(); return; }
      var step = t.closest('#read-panel [data-panel-step]');
      if (step) { e.preventDefault(); stepBy(parseInt(step.getAttribute('data-panel-step'), 10) || 0); return; }
    }
    var a = t.closest('a[href]');
    if (!a || a.target || a.hasAttribute('download')) return;
    var url = itemURL(a.getAttribute('href'));
    if (!url || t.closest('form, button, .copy-btn')) return;
    if (a.closest(FULL)) { rememberWayBack(url.href); return; }
    if (a.closest(INPLACE)) return;
    // A link to the page this is already on is left to the browser.
    if (!isOpen() && pathOf(url) === location.pathname + location.search) return;
    e.preventDefault();
    open(url.href, a);
  });

  /* ---- Keys ----------------------------------------------------------------- */

  document.addEventListener('keydown', function (e) {
    if (!isOpen() || e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return;
    if (e.key !== 'Escape' && e.key !== 'j' && e.key !== 'k') return;
    if (typing(e.target) || othersHoldKeys()) return;
    e.preventDefault();
    e.stopPropagation();
    if (e.key === 'Escape') close();
    else if (!e.repeat) stepBy(e.key === 'j' ? 1 : -1);
  }, true);

  /* ---- Actions inside the panel --------------------------------------------- */

  // navigation.js posts only the forms inside .main; the panel posts its own,
  // the same way, and draws the item again instead of the page.
  var posting = false;
  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (e.defaultPrevented || !form || !inPanel(form)) return;
    var submitter = e.submitter;
    var method = (submitter && submitter.hasAttribute('formmethod') ? submitter.formMethod : form.method || 'get').toLowerCase();
    if (method !== 'post') return;
    e.preventDefault();
    if (posting) { if (window.SeamConsole) window.SeamConsole.flash('Another update is still in progress.', 'error'); return; }
    var action = submitter && submitter.hasAttribute('formaction') ? submitter.formAction : form.action;
    var target;
    try { target = new URL(action || location.href, location.href); } catch (err) { return; }
    if (target.origin !== location.origin) return;
    var data;
    try { data = new FormData(form, submitter); } catch (err) {
      data = new FormData(form);
      if (submitter && submitter.name) data.append(submitter.name, submitter.value);
    }
    var payload = new URLSearchParams();
    data.forEach(function (value, key) { if (typeof value === 'string') payload.append(key, value); });
    var acted = current;
    posting = true;
    fetch(target.href, { method: 'POST', body: payload, credentials: 'same-origin', headers: { Accept: 'text/html', 'X-Seam-Follow': '1' } })
      .then(function (r) {
        var named = r.headers.get('X-Seam-Location');
        var landed = named ? new URL(named, location.href) : null;
        if (landed && landed.pathname === '/console/login') { location.assign(landed.href); return null; }
        if (!r.ok) {
          return r.text().then(function (text) {
            var plain = (r.headers.get('Content-Type') || '').indexOf('text/plain') === 0;
            throw new Error(plain && text.trim() ? text.trim().slice(0, 200) : '');
          });
        }
        var notice = landed && landed.searchParams.get('notice');
        var problem = landed && landed.searchParams.get('error');
        if (window.SeamConsole && (notice || problem)) window.SeamConsole.flash(problem || notice, problem ? 'error' : 'ok');
        if (!isOpen() || current !== acted) return null;
        // The item drawn again; one that is gone closes the panel.
        return load(acted, true).then(function (answer) {
          if (answer && answer.status === 404 && current === acted) close();
        });
      })
      .catch(function (error) {
        var said = error && error.message;
        if (window.SeamConsole) window.SeamConsole.flash(said || 'Could not apply that. The item is unchanged.', 'error');
      })
      .finally(function () { posting = false; });
  });

  /* ---- History and the page around it --------------------------------------- */

  window.addEventListener('popstate', function () {
    var st = history.state;
    var h = st && st.seamPanel ? st.seamPanel : '';
    if (closing) {
      closing = false;
      if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
      if (h) strip();
      teardown();
      return;
    }
    if (!h && !splitPage()) h = hashHref();
    if (h && itemURL(h)) {
      var i = list.indexOf(h);
      if (i >= 0) { index = i; from = findInPage(h); } else if (!isOpen()) found(h);
      show(h, 'none');
      return;
    }
    if (isOpen()) teardown();
  });

  // A morph of the page behind (a live refresh, an in-place load) leaves the
  // panel alone; the row mark and the item page's way back go on again. The
  // in-place client names the new URL only after the morph, so the way back
  // follows the item once more when it has.
  document.addEventListener('seam:content-updated', function () {
    markRow();
    applyWayBack();
    setTimeout(applyWayBack, 0);
  });
  // Back in sight after a while, the item may have changed meanwhile.
  document.addEventListener('seam:stream-resumed', function () {
    if (isOpen() && current) load(current, true);
  });

  // A reload with #peek= opens the panel again, adding no entry: the entry
  // before it may belong to the page as it was before the reload.
  (function adopt() {
    var st = history.state;
    var h = st && st.seamPanel ? st.seamPanel : '';
    if (!h && !splitPage()) h = hashHref();
    if (!h || !itemURL(h)) return;
    try { history.replaceState(stateFor(h, 0), '', location.href); } catch (e) {}
    found(h);
    show(h, 'none');
  })();
})();
