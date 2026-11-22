/* Command palette: Cmd/Ctrl-K, any [data-cmdk-open] trigger, or "/" on a page
   with no filter of its own opens a search-or-jump overlay on every console
   page. It answers in three layers:

   - instantly, from the page itself: the sidebar's sections ("Jump to"), a few
     actions, and the recently opened entities shell.js remembers;
   - after two characters, from GET /console/search?format=json&fast=1 -- the
     same route the Search page uses, with fast=1 so a query per keystroke never
     triggers a remote embedding call (see console/search.go).

   The server's groups follow the page's rules: each kind is fetched one past
   what it shows, starred matches take their slots first, and a cut kind says
   so (group.more) with the page that shows the rest (group.moreHref). The
   palette shows the count as "5+" and ends the group with "More in Search".
   Each group head wears its kind's accent (group.accent, muted for the
   retired memories), the same one the Search page's chips and rows wear.

   Inert when #cmdk is absent (the login page has no palette). */
(function () {
  var overlay = document.getElementById('cmdk');
  if (!overlay) return;

  var panel = overlay.querySelector('.cmdk');
  var input = document.getElementById('cmdk-input');
  var list = document.getElementById('cmdk-list');
  var allLink = document.getElementById('cmdk-all');

  var MIN_CHARS = 2;      // matches the server's floor (store's ftsQuery drops 1-char tokens)
  var DEBOUNCE_MS = 180;
  var PER_GROUP = 5;

  var rows = [];          // flat list of {href, run} in render order, for arrow keys
  var sel = -1;
  var timer = null;
  var inflight = null;    // AbortController for the outstanding fetch
  var restoreFocus = null;

  function isOpen() { return !overlay.hidden; }

  function searchHref(q) {
    return '/console/search?q=' + encodeURIComponent(q || '');
  }

  // The search scopes the console level offers, rendered on the sidebar nav
  // (it is re-rendered whenever the level changes the screen set). The server's
  // JSON is level-blind by contract, so the palette -- a presentation surface --
  // narrows the groups it shows here instead.
  function visibleScopes() {
    var nav = document.querySelector('nav.nav[data-scopes]');
    if (!nav) return null;
    return (nav.getAttribute('data-scopes') || '').split(' ').filter(Boolean);
  }
  // A group belongs to its scope: the archived and superseded memories are
  // memories (group.scope); an older answer without it names its kind.
  function shown(g, scopes) {
    return !scopes || scopes.indexOf(g.scope || g.kind) !== -1;
  }
  function scopeHint() {
    var scopes = visibleScopes();
    if (!scopes || !scopes.length) return 'Keep typing to search.';
    var words = scopes.slice();
    if (words.length > 1) words[words.length - 1] = 'and ' + words[words.length - 1];
    return 'Keep typing to search ' + words.join(words.length > 2 ? ', ' : ' ') + '.';
  }

  /* ---- Local sources: pages, actions, recents ------------------------------ */

  // Pages come from the sidebar, so a feature switched off in Settings -- or a
  // screen above the console level -- is not offered here either. The pages
  // with no nav row (Search, Context) and the Settings sections ride along as
  // hidden [data-jump] links inside the same nav, rendered from the registries.
  function pages() {
    var out = [];
    document.querySelectorAll('nav.nav a[href]').forEach(function (a) {
      var label = a.querySelector('.nav-label');
      out.push({
        title: a.getAttribute('data-jump') || (label ? label.textContent : a.getAttribute('data-tip') || a.getAttribute('href')),
        hint: a.getAttribute('data-hint') || '',
        href: a.getAttribute('href'),
        key: a.getAttribute('data-key'),
        icon: a.querySelector('svg')
      });
    });
    return out;
  }
  function actions() {
    var out = [];
    var theme = document.getElementById('theme-toggle');
    if (theme) {
      // The theme on screen, which under System is the OS's, not the attribute's.
      var onScreen = window.SeamTheme ? window.SeamTheme.effective() : document.documentElement.getAttribute('data-theme');
      out.push({ title: 'Switch to ' + (onScreen === 'dark' ? 'light' : 'dark') + ' theme', run: function () { theme.click(); } });
    }
    if (window.SeamShell) {
      out.push({ title: 'Collapse or expand the sidebar', hint: '[', run: function () { window.SeamShell.toggleSidebar(); } });
      out.push({ title: 'Keyboard shortcuts', hint: '?', run: function () { window.SeamShell.openKeys(); } });
    }
    // A navigation, not a run(): the level is chosen on its own Settings page.
    out.push({ title: 'Change experience level', href: '/console/settings?s=experience' });
    // A lock is set on each project's own page, which exists only under
    // peer_auth; Privacy's Locked card says what locking needs here and links
    // the projects ready for it. Offered while the level shows Privacy.
    if (document.querySelector('nav.nav a[href="/console/privacy"]')) {
      out.push({ title: 'Lock a project', href: '/console/privacy#pv-fence-locked' });
    }
    return out;
  }
  function recents() {
    var raw;
    try { raw = JSON.parse(localStorage.getItem('seamless-recent') || '[]'); } catch (e) { raw = []; }
    if (!Array.isArray(raw)) return [];
    var here = location.pathname;
    return raw.filter(function (r) { return r && r.href && r.title && r.href !== here; }).slice(0, 5);
  }
  // A forgiving match: every query character in order, word starts preferred.
  function score(text, q) {
    text = (text || '').toLowerCase();
    q = q.toLowerCase();
    if (!q) return 1;
    var at = text.indexOf(q);
    if (at === 0) return 4;
    if (at > 0) return text.charAt(at - 1) === ' ' ? 3 : 2;
    var i = 0;
    for (var j = 0; j < text.length && i < q.length; j++) if (text.charAt(j) === q.charAt(i)) i++;
    return i === q.length ? 1 : 0;
  }
  function localGroups(q) {
    var groups = [];
    var jump = pages().map(function (p) { return { p: p, s: score(p.title, q) }; })
      .filter(function (x) { return x.s > 0; })
      .sort(function (a, b) { return b.s - a.s; })
      .map(function (x) { return x.p; });
    var acts = actions().filter(function (a) { return score(a.title, q) > 0; });
    if (!q) {
      var rec = recents();
      if (rec.length) groups.push({ label: 'Recent', icon: 'history', rows: rec.map(function (r) { return { title: r.title, meta: r.kind, href: r.href }; }) });
      groups.push({ label: 'Jump to', icon: 'compass', rows: jump.map(pageRow) });
      if (acts.length) groups.push({ label: 'Actions', icon: 'zap', rows: acts.map(actionRow) });
      return groups;
    }
    if (jump.length) groups.push({ label: 'Jump to', icon: 'compass', rows: jump.slice(0, 4).map(pageRow) });
    if (acts.length) groups.push({ label: 'Actions', icon: 'zap', rows: acts.slice(0, 3).map(actionRow) });
    return groups;
  }
  function pageRow(p) {
    return { title: p.title, meta: p.hint || '', key: p.key ? 'g ' + p.key : '', href: p.href, svg: p.icon };
  }
  function actionRow(a) {
    return { title: a.title, key: a.hint || '', run: a.run, href: a.href };
  }

  /* ---- Open / close -------------------------------------------------------- */

  function open() {
    if (isOpen()) return;
    restoreFocus = document.activeElement;
    overlay.hidden = false;
    input.value = '';
    allLink.setAttribute('href', searchHref(''));
    render(localGroups(''), null);
    input.focus();
  }

  function close() {
    if (!isOpen()) return;
    if (inflight) { inflight.abort(); inflight = null; }
    if (timer) { clearTimeout(timer); timer = null; }
    overlay.hidden = true;
    panel.classList.remove('loading');
    list.innerHTML = '';
    rows = [];
    sel = -1;
    if (restoreFocus && typeof restoreFocus.focus === 'function') {
      try { restoreFocus.focus(); } catch (e) {}
    }
    restoreFocus = null;
  }

  function setSelected(i) {
    var opts = list.querySelectorAll('.cmdk-opt');
    if (!opts.length) { sel = -1; input.removeAttribute('aria-activedescendant'); return; }
    if (i < 0) i = opts.length - 1;
    if (i >= opts.length) i = 0;
    sel = i;
    opts.forEach(function (o, n) {
      var on = n === i;
      o.classList.toggle('selected', on);
      o.setAttribute('aria-selected', on ? 'true' : 'false');
    });
    var cur = opts[i];
    input.setAttribute('aria-activedescendant', cur.id);
    if (cur.scrollIntoView) cur.scrollIntoView({ block: 'nearest' });
  }

  function note(cls, text) {
    var d = document.createElement('li');
    d.className = cls;
    d.setAttribute('role', 'presentation');
    d.textContent = text;
    list.appendChild(d);
  }

  /* render paints local groups, then (when present) the server's groups. Every
     field goes in via textContent EXCEPT snippetHtml, which is the ONE innerHTML
     in this file: it is server-generated HTML from console/search.go's
     highlightSnippet, which escapes the item text before substituting <mark>.
     The page icons are cloned nodes from the sidebar, never strings. */
  function render(local, remote, status) {
    rows = [];
    sel = -1;
    input.removeAttribute('aria-activedescendant');
    list.innerHTML = '';

    (local || []).forEach(function (g) { group(g.label, g.rows.length, g.rows, 'local'); });

    if (remote && remote.groups && remote.groups.length) {
      var scopes = visibleScopes();
      remote.groups.forEach(function (g) {
        if (!shown(g, scopes)) return;
        var items = (g.rows || []).slice(0, PER_GROUP).map(function (r) {
          var bits = [];
          if (r.favorite) bits.push('starred');
          if (r.retired) bits.push(r.supersededBy ? r.retired + ' by ' + r.supersededBy.name : r.retired);
          if (r.identifier && r.identifier !== r.title) bits.push(r.identifier);
          if (r.matchedId) bits.push(r.matchedId);
          bits.push(r.project || 'global');
          bits.push(r.age);
          return { title: r.title, desc: r.description || '', snippetHtml: r.snippetHtml, meta: bits.join(' · '), href: r.href };
        });
        // A cut kind: its count is a floor, and its last row opens the page
        // that shows the rest.
        if (g.more && g.moreHref) items.push({ title: 'More in Search', meta: g.label, href: g.moreHref, more: true });
        group(g.label, g.count + (g.more ? '+' : ''), items, 'remote', g);
      });
    }
    if (status) note(status.cls, status.text);
    if (rows.length) setSelected(0);
  }

  function group(label, count, items, source, g) {
    if (!items.length) return;
    var head = document.createElement('li');
    head.className = 'cmdk-group' + (source === 'local' ? ' local' : '');
    head.setAttribute('role', 'presentation');
    if (g && g.accent) head.setAttribute('data-search-kind', g.accent);
    if (g && g.retired) head.setAttribute('data-retired', '');
    head.textContent = label + ' ';
    if (source !== 'local') {
      var n = document.createElement('span');
      n.className = 'n';
      n.textContent = count;
      head.appendChild(n);
    }
    list.appendChild(head);

    items.forEach(function (r) {
      var li = document.createElement('li');
      li.className = 'cmdk-opt' + (source === 'local' ? ' cmdk-local' : '') + (r.more ? ' cmdk-more' : '');
      li.id = 'cmdk-opt-' + rows.length;
      li.setAttribute('role', 'option');
      li.setAttribute('aria-selected', 'false');

      if (r.svg) {
        var ic = r.svg.cloneNode(true);
        ic.setAttribute('class', 'ico cmdk-ico');
        li.appendChild(ic);
      }
      var t = document.createElement('span');
      t.className = 't';
      t.textContent = r.title;
      li.appendChild(t);

      if (r.snippetHtml || r.desc) {
        var d = document.createElement('span');
        d.className = 'd';
        if (r.snippetHtml) d.innerHTML = r.snippetHtml; // server-escaped; see the note above
        else d.textContent = r.desc;
        li.appendChild(d);
      }
      if (r.meta) {
        var meta = document.createElement('span');
        meta.className = 'meta';
        meta.textContent = r.meta;
        li.appendChild(meta);
      }
      if (r.key) {
        var k = document.createElement('kbd');
        k.className = 'cmdk-key';
        k.textContent = r.key;
        li.appendChild(k);
      }

      var idx = rows.length;
      li.addEventListener('mousemove', function () { if (sel !== idx) setSelected(idx); });
      li.addEventListener('click', function (e) { choose(rows[idx], e.metaKey || e.ctrlKey); });
      list.appendChild(li);
      rows.push({ href: r.href, run: r.run });
    });
  }

  function choose(row, newTab) {
    if (!row) return;
    if (row.run) { close(); row.run(); return; }
    go(row.href, newTab);
  }

  function go(href, newTab) {
    if (!href) return;
    if (newTab) { window.open(href, '_blank'); return; }
    close();
    location.href = href;
  }

  function run(q) {
    if (inflight) inflight.abort();
    var ctl = new AbortController();
    inflight = ctl;
    panel.classList.add('loading');
    fetch(searchHref(q) + '&format=json&fast=1', {
      headers: { Accept: 'application/json' },
      signal: ctl.signal
    }).then(function (r) {
      if (!r.ok) throw new Error('search failed: ' + r.status);
      return r.json();
    }).then(function (data) {
      if (ctl !== inflight) return; // superseded by a newer keystroke
      panel.classList.remove('loading');
      var local = localGroups(q);
      var scopes = visibleScopes();
      var none = !(data.groups || []).some(function (g) { return shown(g, scopes); });
      render(local, data, none ? { cls: 'cmdk-empty', text: local.length ? 'No knowledge or work matches "' + q + '".' : 'No results for "' + q + '".' } : null);
    }).catch(function (err) {
      if (err && err.name === 'AbortError') return;
      if (ctl !== inflight) return;
      panel.classList.remove('loading');
      // Surface it: a silently empty palette reads as "nothing matched".
      render(localGroups(q), null, { cls: 'cmdk-error', text: 'Search is unavailable right now.' });
    });
  }

  input.addEventListener('input', function () {
    var q = input.value.trim();
    allLink.setAttribute('href', searchHref(q));
    if (timer) clearTimeout(timer);
    if (inflight) { inflight.abort(); inflight = null; }
    panel.classList.remove('loading');
    var local = localGroups(q);
    if (q.length < MIN_CHARS) {
      render(local, null, q ? { cls: 'cmdk-empty', text: scopeHint() } : null);
      return;
    }
    render(local, null, null);
    panel.classList.add('loading');
    timer = setTimeout(function () { run(q); }, DEBOUNCE_MS);
  });

  input.addEventListener('keydown', function (e) {
    if (e.key === 'ArrowDown') { e.preventDefault(); setSelected(sel + 1); return; }
    if (e.key === 'ArrowUp') { e.preventDefault(); setSelected(sel - 1); return; }
    if (e.key === 'Enter') {
      e.preventDefault();
      var q = input.value.trim();
      if (sel >= 0 && rows[sel]) { choose(rows[sel], e.metaKey || e.ctrlKey); }
      else if (q) { go(searchHref(q), e.metaKey || e.ctrlKey); }
      return;
    }
    // Trap Tab inside the dialog. Only two stops exist (the input and the
    // see-all link), so either direction lands on the other one.
    if (e.key === 'Tab') {
      e.preventDefault();
      allLink.focus();
    }
  });

  allLink.addEventListener('keydown', function (e) {
    if (e.key === 'Tab') { e.preventDefault(); input.focus(); }
  });

  // Backdrop click closes; a click inside the panel must not.
  overlay.addEventListener('click', function (e) {
    if (!panel.contains(e.target)) close();
  });

  // Any element can open the palette (the sidebar's search field, the phone
  // bar's search button); delegated so a morph-replaced trigger keeps working.
  document.addEventListener('click', function (e) {
    var trigger = e.target.closest ? e.target.closest('[data-cmdk-open]') : null;
    if (!trigger) return;
    e.preventDefault();
    open();
  });

  function typingTarget(el) {
    if (!el) return false;
    var tag = el.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || el.isContentEditable;
  }

  // The page's own filter, when it has one (library rails, Sessions, Projects,
  // the Search page's query). "/" means "narrow what I am looking at" first.
  function pageFilter() {
    var el = document.querySelector('.main [data-page-filter], .main .lib-query input[name="q"]');
    return el && el.offsetParent !== null ? el : null;
  }

  /* Capture phase: Escape must reach us before the detail pane's document-level
     handler (layout.html), or closing the palette over an open peek pane would
     also tear the pane down. stopPropagation keeps that from happening. */
  document.addEventListener('keydown', function (e) {
    if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
      e.preventDefault();
      if (isOpen()) close(); else open();
      return;
    }
    if (isOpen() && e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      close();
      return;
    }
    if (e.key === '/' && !isOpen() && !typingTarget(e.target) &&
        !e.metaKey && !e.ctrlKey && !e.altKey) {
      var keysSheet = document.getElementById('keys');
      if (keysSheet && !keysSheet.hidden) return;
      e.preventDefault();
      e.stopPropagation();
      var filter = pageFilter();
      if (filter) { filter.focus(); if (filter.select) filter.select(); }
      else open();
    }
  }, true);
})();
