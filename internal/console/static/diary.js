/* The Diary's page script (templates/diary.html, diary.go): walking the days
   from the keyboard, and today's live refresh.

   Keys. Left and right walk to the previous and the next day, t to today.
   Each one clicks the page's own in-place link (data-seam-query), so the
   shared navigation client loads the day, and history works as it does for
   the link. The keys stand down while a g-chord is under way (shell.js owns
   the next key: g then d opens the Diary), while typing, while a dialog is
   open, and inside a chart or the detail pane, whose own keys move their own
   cursor.

   Today is live. The page opts out of the layout's refresh on every event
   (SEAM_NO_LIVE_REFRESH) and refreshes itself from the same stream
   (seam:event, never a second EventSource), only while it shows today, and at
   most once in the entry's data-diary-live milliseconds: a busy fleet sends
   many events a second. An open peek stays open across the refresh: the pane
   is held out of the morph (data-live-skip) and the split's open state is put
   back after it (seam:content-updated). A refresh that fails leaves the page
   as it is, and the next event tries again. */
(function () {
  'use strict';

  window.SEAM_NO_LIVE_REFRESH = true;

  function page() { return document.querySelector('.main .dy-page'); }
  function live() { return document.querySelector('.main .dy-entry[data-diary-live]'); }

  function typing(t) {
    return !!(t && t.closest && t.closest('input, textarea, select, [contenteditable=""], [contenteditable="true"]'));
  }
  function overlayOpen() {
    var cmdk = document.getElementById('cmdk');
    var keys = document.getElementById('keys');
    return !!((cmdk && !cmdk.hidden) || (keys && !keys.hidden));
  }
  function selecting() {
    var s = window.getSelection && window.getSelection();
    return !!(s && s.type === 'Range' && String(s).trim());
  }

  // follow clicks one of the page's own day links, if it has that one (the
  // next day is not offered on today).
  function follow(selector) {
    var a = document.querySelector('.main ' + selector);
    if (!a) return false;
    a.click();
    return true;
  }

  // Capture: this listener runs before shell.js ends a g-chord, so it can see
  // the chord's hint and leave the key to it.
  document.addEventListener('keydown', function (e) {
    if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || e.shiftKey || e.repeat) return;
    if (!page() || typing(e.target) || overlayOpen()) return;
    if (document.querySelector('.chord-hint')) return;
    if (e.target && e.target.closest && e.target.closest('.chart, #detail-pane, details.menu[open]')) return;
    var done = false;
    switch (e.key) {
      case 'ArrowLeft': done = follow('[data-diary-prev]'); break;
      case 'ArrowRight': done = follow('[data-diary-next]'); break;
      case 't': done = follow('[data-diary-today]'); break;
      default: return;
    }
    if (done) e.preventDefault();
  }, true);

  var timer = null;
  var busy = false;
  var last = 0;

  function gap() {
    var el = live();
    var n = el ? parseInt(el.getAttribute('data-diary-live'), 10) : 0;
    return n > 0 ? n : 15000;
  }
  function schedule(wait) {
    if (!timer) timer = setTimeout(refresh, wait);
  }
  function refresh() {
    timer = null;
    if (!live() || busy) return;
    if (document.hidden || selecting() || overlayOpen()) { schedule(3000); return; }
    busy = true;
    last = Date.now();
    var pane = document.getElementById('detail-pane');
    if (pane && !pane.hidden) pane.setAttribute('data-live-skip', '');
    fetch(location.href, { credentials: 'same-origin', headers: { 'X-Seam-Live': '1' } })
      .then(function (r) {
        if (!r.ok) throw new Error('http ' + r.status);
        return r.text();
      })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, 'text/html');
        if (!doc.querySelector('.sidebar')) {
          // An expired sign-in is the one navigation a data refresh may make.
          location.assign('/console/login?next=' + encodeURIComponent(location.pathname + location.search));
          return;
        }
        if (window.SeamConsole) window.SeamConsole.patchPage(doc, 'diary');
      })
      .catch(function () {})
      .finally(function () {
        busy = false;
        var held = document.getElementById('detail-pane');
        if (held) held.removeAttribute('data-live-skip');
      });
  }

  document.addEventListener('seam:event', function () {
    if (!live()) return;
    schedule(Math.max(2000, gap() - (Date.now() - last)));
  });

  // The morph syncs the split's classes to the server's, which never renders
  // a pane open: an open peek puts its own class back.
  document.addEventListener('seam:content-updated', function () {
    var pane = document.getElementById('detail-pane');
    var split = document.querySelector('.main .dy-split');
    if (pane && split && !pane.hidden) split.classList.add('detail-open');
  });
})();
