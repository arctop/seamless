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
   as it is, and the next event tries again.

   A peek keeps the day in place: the link clicked stays where it was on
   screen when the pane opens and the entry becomes a box of its own. */
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
  // The layout lets its stream go while the tab is out of sight and catches
  // up once it is open again (seam:stream-resumed): events went by unseen,
  // so today refreshes then too, by the same rule.
  document.addEventListener('seam:stream-resumed', function () {
    if (!live()) return;
    schedule(Math.max(2000, gap() - (Date.now() - last)));
  });

  // The morph syncs the split's classes to the server's, which never renders
  // a pane open: an open peek puts its own class back.
  document.addEventListener('seam:content-updated', function () {
    var pane = document.getElementById('detail-pane');
    var split = document.querySelector('.main .dy-split');
    if (pane && split && !pane.hidden) split.classList.add('detail-open');
    watch();
  });

  // Opening a peek caps the day's entry into a box of its own that starts
  // scrolled to its top, so the link just clicked, far down the day, would
  // jump out of sight. Its place on screen is noted as it is clicked (in the
  // capture phase, before the layout's pane loader runs), and once the pane
  // has opened the box is scrolled to put it back there, or as near as the
  // box allows. A modified click opens a tab instead and is left alone.
  var kept = null;
  document.addEventListener('click', function (e) {
    var a = e.target && e.target.closest ? e.target.closest('.main .dy-page [data-peek]') : null;
    var pane = document.getElementById('detail-pane');
    if (!a || !pane || !pane.hidden || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
    kept = { el: a, top: a.getBoundingClientRect().top, at: Date.now() };
  }, true);
  function restore() {
    var k = kept;
    kept = null;
    if (!k || !k.el.isConnected || Date.now() - k.at > 10000) return;
    var lp = k.el.closest('.list-pane');
    if (!lp || lp.scrollHeight <= lp.clientHeight) return;
    var box = lp.getBoundingClientRect(), r = k.el.getBoundingClientRect();
    var lo = Math.max(box.top, 0) + 8, hi = Math.min(box.bottom, window.innerHeight) - r.height - 8;
    lp.scrollTop += r.top - Math.max(lo, Math.min(k.top, hi));
  }
  // The pane's opening is the split gaining detail-open; the morph keeps the
  // split's node, and a page drawn anew is watched again on content-updated.
  var watched = null;
  var observer = window.MutationObserver ? new MutationObserver(function () {
    if (kept && watched && watched.classList.contains('detail-open')) restore();
  }) : null;
  function watch() {
    var split = document.querySelector('.main .dy-split');
    if (!observer || !split || split === watched) return;
    observer.disconnect();
    watched = split;
    observer.observe(split, { attributes: true, attributeFilter: ['class'] });
  }
  watch();
})();
