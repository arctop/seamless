/* Library-style screens (Memories / Notes / Tasks / Plans / Labs / Trials /
   Gardener): a grouped rail on the left and a document reader on the right.
   Every rail item is a real server-rendered link; this module upgrades clicks
   to an in-place reader swap (?reader=1 fragment + history.pushState of the
   item's canonical URL) so the document and rail keep their scroll positions
   and the switch is instant. j / k move the selection. The delegated handlers
   stay inert without #lib-reader, but can enhance one inserted by an in-place
   mutation response later. */
(function () {
  'use strict';

  // Re-queried lazily: the SSE live client can morph the rail in place.
  function items() { return Array.prototype.slice.call(document.querySelectorAll('.lib-rail .rail-item')); }
  function readerEl() { return document.getElementById('lib-reader'); }
  function basePath() {
    var el = readerEl();
    return el ? el.getAttribute('data-base') || '' : '';
  }
  function pathOf(href) {
    try { return new URL(href, location.origin).pathname; } catch (e) { return href; }
  }
  // The group header is sticky inside the rail, so "visible" starts below it --
  // an item scrolled to the rail's top edge would sit hidden under its header.
  function stickyTop(a) {
    var group = a.closest('.rail-group');
    var head = group && group.querySelector('.rail-group-hd');
    return head ? head.offsetHeight : 0;
  }
  // Scroll the rail's list (never the page) to show the item. Stepping (j/k,
  // clicks) moves only as far as needed; a fresh page centers the selection so
  // it never parks against an edge with its neighbors out of view.
  function revealInRail(a, center) {
    var disclosure = a.closest ? a.closest('details') : null;
    if (disclosure) disclosure.open = true;
    var rail = a.closest('.rail-scroll') || a.closest('.lib-rail');
    if (!rail) return;
    var r = rail.getBoundingClientRect(), b = a.getBoundingClientRect();
    if (center) {
      if (rail.scrollHeight > rail.clientHeight) rail.scrollTop += (b.top - r.top) - (r.height - b.height) / 2;
      return;
    }
    var top = r.top + 8 + stickyTop(a);
    if (b.top < top) rail.scrollTop += b.top - top;
    else if (b.bottom > r.bottom - 8) rail.scrollTop += b.bottom - r.bottom + 8;
  }
  function mark(href) {
    var p = pathOf(href);
    var sel = null;
    items().forEach(function (a) {
      var on = pathOf(a.getAttribute('href')) === p;
      a.classList.toggle('selected', on);
      if (on) { a.setAttribute('aria-current', 'page'); sel = a; }
      else { a.removeAttribute('aria-current'); }
    });
    return sel;
  }
  function selectedIndex(list) {
    var idx = -1;
    list.forEach(function (a, i) { if (a.classList.contains('selected')) idx = i; });
    return idx;
  }
  function updateReaderNav() {
    var el = readerEl();
    if (!el) return;
    var nav = el.querySelector('.reader-nav');
    if (!nav) return;
    var list = items(), idx = selectedIndex(list), sel = idx >= 0 ? list[idx] : null;
    var noun = el.getAttribute('aria-label') || 'Item';
    var context = sel && sel.getAttribute('data-context');
    nav.querySelector('.reader-location').textContent = noun + (context ? ' / ' + context : '');
    nav.querySelector('.reader-index').textContent = idx >= 0 ? (idx + 1) + ' of ' + list.length : list.length + ' items';
    var prev = nav.querySelector('[data-reader-step="-1"]');
    var next = nav.querySelector('[data-reader-step="1"]');
    prev.disabled = idx <= 0;
    next.disabled = idx < 0 || idx >= list.length - 1;
  }
  function ensureReaderNav() {
    var el = readerEl();
    if (!el) return;
    var nav = el.querySelector('.reader-nav');
    if (!nav) {
      nav = document.createElement('nav');
      nav.id = 'reader-nav';
      nav.className = 'reader-nav';
      nav.setAttribute('aria-label', 'Reader navigation');
      nav.setAttribute('data-live-skip', '');

      var copy = document.createElement('div');
      copy.className = 'reader-nav-copy';
      var locationLabel = document.createElement('span');
      locationLabel.className = 'reader-location';
      var indexLabel = document.createElement('span');
      indexLabel.className = 'reader-index';
      copy.appendChild(locationLabel);
      copy.appendChild(indexLabel);

      var actions = document.createElement('div');
      actions.className = 'reader-nav-actions';
      var hint = document.createElement('span');
      hint.className = 'reader-keyhint';
      hint.innerHTML = '<kbd>k</kbd> previous <kbd>j</kbd> next';
      var prev = document.createElement('button');
      prev.className = 'reader-step';
      prev.type = 'button';
      prev.textContent = '\u2190';
      prev.title = 'Previous item (k)';
      prev.setAttribute('aria-label', 'Previous item');
      prev.setAttribute('data-reader-step', '-1');
      var next = document.createElement('button');
      next.className = 'reader-step';
      next.type = 'button';
      next.textContent = '\u2192';
      next.title = 'Next item (j)';
      next.setAttribute('aria-label', 'Next item');
      next.setAttribute('data-reader-step', '1');
      actions.appendChild(hint);
      actions.appendChild(prev);
      actions.appendChild(next);
      nav.appendChild(copy);
      nav.appendChild(actions);
      el.insertBefore(nav, el.firstChild);
    }
    updateReaderNav();
  }
  function stepSelection(delta) {
    var list = items();
    if (!list.length) return;
    var idx = selectedIndex(list);
    var next = idx === -1 ? 0 : idx + delta;
    if (next < 0 || next >= list.length) return;
    load(list[next].getAttribute('href'), true);
  }
  // The page draws its sort and grouping links (data-lib-here) for the item
  // open when it loaded. After a swap they follow the item now in the reader,
  // each keeping its own query, so a sort never snaps the reader back; the
  // star's way back (data-lib-next) is the URL the reader is on, filters and
  // all. A later live refresh draws the same links from the server.
  function followReader(href) {
    var here;
    try { here = new URL(href, location.origin); } catch (e) { return; }
    document.querySelectorAll('.lib-page a[data-lib-here]').forEach(function (a) {
      try { a.setAttribute('href', here.pathname + new URL(a.getAttribute('href'), location.origin).search); } catch (e) {}
    });
    var el = readerEl();
    if (el) el.querySelectorAll('input[data-lib-next]').forEach(function (input) { input.value = here.pathname + here.search; });
  }
  function load(href, push) {
    var url = href + (href.indexOf('?') >= 0 ? '&' : '?') + 'reader=1';
    fetch(url, { credentials: 'same-origin' })
      .then(function (r) { if (!r.ok) throw new Error('http ' + r.status); return r.text(); })
      .then(function (html) {
        var el = readerEl();
        if (!el) return;
        el.innerHTML = html;
        if (window.IX && window.IX.enhance) { try { window.IX.enhance(el); } catch (e) {} }
        if (push) { try { history.pushState({ lib: 1 }, '', href); } catch (e) {} }
        followReader(href);
        var sel = mark(href);
        if (sel) {
          var t = sel.getAttribute('data-title');
          if (t) document.title = t + ' · Seamless';
          revealInRail(sel);
        }
        ensureReaderNav();
      })
      .catch(function () {
        // Keep the current reader intact. A fetch failure is not permission to
        // turn a data update into a document navigation.
        if (window.SeamConsole) window.SeamConsole.flash('Could not update the reader. The current data is unchanged.', 'error');
      });
  }

  document.addEventListener('click', function (e) {
    var step = e.target.closest ? e.target.closest('[data-reader-step]') : null;
    if (step) {
      e.preventDefault();
      if (!step.disabled) stepSelection(parseInt(step.getAttribute('data-reader-step'), 10));
      return;
    }
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button === 1) return;
    var a = e.target.closest ? e.target.closest('a[href]') : null;
    if (!a) return;
    // A copy button inside a row copies; it does not open the row. The button
    // sits inside the anchor, so ask the click's target, not the anchor.
    if (e.target.closest('.copy-btn')) return;
    var inRail = !!a.closest('.lib-rail') && a.classList.contains('rail-item');
    // Inside the reader, links to siblings of the same entity (e.g. a
    // supersession neighbor) swap in place too; everything else navigates.
    var el = readerEl();
    var base = basePath();
    var inReader = el && el.contains(a) && base && pathOf(a.getAttribute('href')).indexOf(base + '/') === 0;
    if (!inRail && !inReader) return;
    e.preventDefault();
    load(a.getAttribute('href'), true);
  });

  // Browser Back/Forward across swapped selections.
  window.addEventListener('popstate', function () {
    // A filter/sort history step needs the whole server-rendered view patched;
    // the shared navigation client owns it. Reader-only history stays cheap.
    if (window.SeamConsole && window.SeamConsole.needsView(location.href)) return;
    var base = basePath();
    if (base && location.pathname.indexOf(base) === 0) load(location.pathname + location.search, false);
  });

  // j / k step the selection through the rail (list order, across groups),
  // Gmail-style. ("/" -> the page-local filter is search.js's, which owns the
  // key so the palette and the filter never both answer it.) Arrow keys are
  // left alone so they keep scrolling the document.
  document.addEventListener('keydown', function (e) {
    if (e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented) return;
    var ae = document.activeElement;
    if (ae && ae.matches && ae.matches('input, textarea, select, [contenteditable=""], [contenteditable="true"]')) return;
    if (e.key !== 'j' && e.key !== 'k') return;
    if (!items().length) return;
    e.preventDefault();
    stepSelection(e.key === 'j' ? 1 : -1);
  });

  function enhanceReader(initial) {
    if (!readerEl()) return;
    // The list URL auto-opens a default selection server-side; pin its
    // canonical URL so a live refresh cannot choose a different item mid-read.
    // Later full-view patches already canonicalize through navigation.js.
    if (initial) {
      var layout = document.querySelector('.lib-layout');
      var auto = layout && layout.getAttribute('data-auto-url');
      if (auto) { try { history.replaceState(null, '', auto); } catch (e) {} }
    }
    var selected = items().filter(function (a) { return a.classList.contains('selected'); })[0];
    if (selected) revealInRail(selected, initial);
    ensureReaderNav();
  }

  enhanceReader(true);
  document.addEventListener('seam:content-updated', function () { enhanceReader(false); });

  // Live refreshes morph the server-rendered page and can remove this small
  // JS-owned navigation strip. Restore it after any such replacement; the
  // guard makes the observer settle after the single insertion.
  if (window.MutationObserver) {
    var main = document.querySelector('.main');
    if (main) new MutationObserver(function () {
      if (readerEl() && !readerEl().querySelector('.reader-nav')) ensureReaderNav();
    }).observe(main, { childList: true, subtree: true });
  }
})();
