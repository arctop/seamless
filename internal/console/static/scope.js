/* The project switcher (plan:console-ux-pass UX 27): the "In <project>"
   button a list screen draws, and its popover (templates/scope_shared.html).
   The server draws every choice as a link, so without script the popover is a
   plain menu of links. This script adds, all of it in the page:

   - a field that filters the rows the page already holds, by slug and
     display name, as the owner types: nothing is fetched until a row is
     chosen. Quiet projects stay out of the list until the field has text;
   - Recent: the projects this browser last chose or visited, up to five, in
     localStorage under a key of its own. Its rows are copies of the rows the
     server drew, so their counts are this screen's own; without storage the
     section is simply absent;
   - the keys: p opens it (never while typing in a field, while the palette
     or the shortcut sheet is open, or as the second key of a g-chord), the
     arrows move, Enter chooses, Esc closes (shell.js closes every open menu
     on Esc), and Backspace in an empty field chooses All projects.

   Choosing follows the row's link: data-seam-query, so navigation.js patches
   the view in place. A live refresh morphs the rows back to the server's
   markup and keeps the field (data-live-skip); seam:content-updated redraws
   Recent and the filter over them. */
(function () {
  'use strict';

  var RECENT_KEY = 'seamless-scope-recent';
  var RECENT_MAX = 5;

  function readRecent() {
    try {
      var list = JSON.parse(localStorage.getItem(RECENT_KEY) || '[]');
      if (!Array.isArray(list)) return [];
      return list.filter(function (v) { return typeof v === 'string' && v !== ''; }).slice(0, RECENT_MAX);
    } catch (e) { return []; }
  }
  function remember(value) {
    if (!value) return;
    var list = readRecent().filter(function (v) { return v !== value; });
    list.unshift(value);
    try { localStorage.setItem(RECENT_KEY, JSON.stringify(list.slice(0, RECENT_MAX))); } catch (e) {}
  }

  function slice(list) { return Array.prototype.slice.call(list); }
  function switcher() { return document.querySelector('.main details[data-scope]'); }
  function fieldOf(sw) { return sw ? sw.querySelector('.scope-field input') : null; }
  // The rows the server drew once each: All projects, the main list and the
  // quiet ones. Starred and Recent repeat rows of these.
  function sourceRows(sw) { return slice(sw.querySelectorAll('.scope-list > a.scope-o, .scope-quiet > a.scope-o')); }
  function shown(a) {
    for (var el = a; el && !el.classList.contains('scope-list'); el = el.parentElement) {
      if (el.hidden) return false;
    }
    return true;
  }
  function shownRows(sw) { return slice(sw.querySelectorAll('.scope-list a.scope-o')).filter(shown); }
  // A row's label is its own text, not its count or its hint.
  function labelOf(a) {
    var s = '';
    for (var n = a.firstChild; n; n = n.nextSibling) if (n.nodeType === 3) s += n.nodeValue;
    return s.trim();
  }
  function matches(a, q) {
    var hay = (a.getAttribute('data-scope-v') || '') + ' ' + (a.getAttribute('data-scope-name') || '') + ' ' + labelOf(a);
    return hay.toLowerCase().indexOf(q) !== -1;
  }

  function drawRecent(sw, box, typing) {
    slice(box.querySelectorAll('a.scope-o')).forEach(function (a) { a.remove(); });
    var rows = sourceRows(sw);
    var added = 0;
    readRecent().forEach(function (v) {
      var row = rows.filter(function (a) {
        return a.getAttribute('data-scope-v') === v && !a.hasAttribute('data-scope-locked');
      })[0];
      if (!row) return;
      var copy = row.cloneNode(true);
      copy.id = 'sc-r-' + v;
      copy.hidden = false;
      box.appendChild(copy);
      added++;
    });
    box.hidden = typing || added === 0;
  }

  // The highlighted row, by its id, so a redraw can keep it.
  var active = '';
  function highlight(sw, row) {
    var input = fieldOf(sw);
    slice(sw.querySelectorAll('.scope-list a.scope-o')).forEach(function (a) {
      a.setAttribute('role', 'option');
      a.setAttribute('aria-selected', a === row ? 'true' : 'false');
      a.classList.toggle('active', a === row);
    });
    active = row ? row.id : '';
    if (!input) return;
    if (!row) { input.removeAttribute('aria-activedescendant'); return; }
    input.setAttribute('aria-activedescendant', row.id);
    reveal(sw, row);
  }
  // Scroll the list, never the page, to show the highlighted row.
  function reveal(sw, row) {
    var list = sw.querySelector('.scope-list');
    if (!sw.open || !list) return;
    var r = row.getBoundingClientRect(), l = list.getBoundingClientRect();
    if (r.top < l.top) list.scrollTop -= l.top - r.top;
    else if (r.bottom > l.bottom) list.scrollTop += r.bottom - l.bottom;
  }

  function render(sw, keepActive) {
    var input = fieldOf(sw);
    var raw = input ? input.value.trim() : '';
    var q = raw.toLowerCase();
    var typing = q !== '';
    var list = sw.querySelector('.scope-list');
    if (list) list.setAttribute('role', 'listbox');
    var recent = sw.querySelector('[data-scope-recent]');
    if (recent) drawRecent(sw, recent, typing);
    slice(sw.querySelectorAll('[data-scope-starred], [data-scope-head]')).forEach(function (el) { el.hidden = typing; });
    var quiet = sw.querySelector('[data-scope-quiet]');
    if (quiet) quiet.hidden = !typing;
    var found = 0;
    sourceRows(sw).forEach(function (a) {
      var on = !typing || matches(a, q);
      a.hidden = !on;
      if (on) found++;
    });
    var none = sw.querySelector('[data-scope-none]');
    if (none) {
      none.hidden = !typing || found > 0;
      var said = none.querySelector('q');
      if (said) said.textContent = raw;
    }
    var rows = shownRows(sw);
    var row = null;
    if (keepActive && active) row = rows.filter(function (a) { return a.id === active; })[0] || null;
    if (!row && !typing) row = rows.filter(function (a) { return a.classList.contains('on'); })[0] || null;
    highlight(sw, row || rows[0] || null);
  }

  // On a wide screen the popover opens under the button and shifts left as
  // far as it must to stay inside the page's column (never over the
  // sidebar); on a phone the stylesheet spans it across the title bar
  // instead.
  function place(sw) {
    var pop = sw.querySelector('.scope-pop');
    if (!pop) return;
    pop.style.left = '';
    if (window.matchMedia && window.matchMedia('(max-width: 720px)').matches) return;
    var box = sw.getBoundingClientRect();
    var column = (sw.closest('.main') || document.documentElement).getBoundingClientRect();
    var right = Math.min(column.right, document.documentElement.clientWidth) - 16;
    var left = Math.min(0, right - pop.offsetWidth - box.left);
    pop.style.left = Math.max(column.left + 16 - box.left, left) + 'px';
  }

  function finePointer() { return !window.matchMedia || window.matchMedia('(pointer: fine)').matches; }
  function open(sw, focus) {
    if (!sw.open) sw.open = true;
    place(sw);
    render(sw, false);
    var input = fieldOf(sw);
    if (input && focus) input.focus();
  }

  // The current choice is a visit, and so is a project's own page.
  function init() {
    var page = /^\/console\/projects\/([^/]+)/.exec(location.pathname);
    if (page) { try { remember(decodeURIComponent(page[1])); } catch (e) {} }
    var sw = switcher();
    if (!sw) return;
    remember(sw.getAttribute('data-scope-current'));
    var field = sw.querySelector('.scope-field');
    if (field) field.hidden = false;
    render(sw, true);
    if (sw.open) place(sw);
  }

  // toggle does not bubble: listen in the capture phase. p has already
  // opened, placed and focused it; a click on the button opens it here.
  document.addEventListener('toggle', function (e) {
    var sw = e.target;
    if (!sw.matches || !sw.matches('details[data-scope]')) return;
    var input = fieldOf(sw);
    if (sw.open) {
      if (!input || document.activeElement !== input) open(sw, finePointer());
      return;
    }
    if (input) input.value = '';
    render(sw, false);
  }, true);

  document.addEventListener('input', function (e) {
    var input = e.target;
    if (!input.matches || !input.matches('details[data-scope] .scope-field input')) return;
    render(input.closest('details[data-scope]'), false);
  });

  // Choosing empties the field, so the view the choice patches in shows it
  // empty: the field is the one part a morph leaves alone.
  document.addEventListener('click', function (e) {
    var a = e.target.closest ? e.target.closest('details[data-scope] a.scope-o') : null;
    if (!a) return;
    var input = fieldOf(a.closest('details[data-scope]'));
    if (input) input.value = '';
  });

  document.addEventListener('keydown', function (e) {
    var input = e.target;
    if (!input.matches || !input.matches('details[data-scope] .scope-field input') || e.isComposing) return;
    var sw = input.closest('details[data-scope]');
    var rows = shownRows(sw);
    var at = -1;
    rows.forEach(function (a, i) { if (a.id === active) at = i; });
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!rows.length) return;
      var next = Math.max(0, Math.min(rows.length - 1, at + (e.key === 'ArrowDown' ? 1 : -1)));
      highlight(sw, rows[next]);
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (at >= 0) rows[at].click();
      return;
    }
    if (e.key === 'Backspace' && input.value === '') {
      e.preventDefault();
      var all = sw.querySelector('.scope-list > a.scope-o[data-scope-v=""]');
      if (all) all.click();
    }
  });

  // A key the shell already holds this instant: the second key of a g-chord
  // (shell.js shows .chord-hint while it waits, and g p is Plans), or any key
  // while the palette or the shortcut sheet is open. This listener runs
  // before the shell's (layout.html loads scope.js first), so it steps aside
  // itself, as library.js does for j and k.
  function shellHoldsKey() {
    if (document.querySelector('.chord-hint')) return true;
    var ids = ['cmdk', 'keys'];
    for (var i = 0; i < ids.length; i++) {
      var el = document.getElementById(ids[i]);
      if (el && !el.hidden) return true;
    }
    return false;
  }
  function typing(el) {
    if (!el || !el.tagName) return false;
    var tag = el.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || el.isContentEditable;
  }

  // p opens the switcher on a screen that has one. On a phone the title bar
  // folds its controls behind Filters: those open first.
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'p' || e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented) return;
    if (typing(e.target) || typing(document.activeElement) || shellHoldsKey()) return;
    var sw = switcher();
    if (!sw) return;
    var summary = sw.querySelector('summary');
    if (summary && !summary.getClientRects().length) {
      var fold = document.querySelector('.main [data-filters-toggle][aria-expanded="false"]');
      if (fold) fold.click();
    }
    if (!summary || !summary.getClientRects().length) return;
    e.preventDefault();
    open(sw, true);
  });

  init();
  document.addEventListener('seam:content-updated', init);
})();
