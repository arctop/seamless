/* Settings > Backups (templates/settings_backups.html). The section works
   without this script -- every control is a form or a data-seam-query link --
   and this adds what a page cannot do alone:

     - the folder field's live check (GET /console/settings/backups/folder,
       a look that never writes) and the suggestion chips that fill it;
     - the passkey ceremony a restore needs under peer_auth with a project
       locked (static/passkey.js): begin, the authenticator, then the restore;
     - following a restore through its steps and the restart that finishes it,
       by re-drawing the section in place (never a reload) and watching
       /healthz name a new daemon;
     - refreshing the section when a backup.* event arrives, since the
       Settings page turns the general live refresh off.

   State the morph would drop (the folder the owner chose, a ceremony in
   flight) is held here and re-applied on seam:content-updated. Inert on
   every other section. */
(function () {
  'use strict';

  var SECTION = '/console/settings?s=backups';
  var chosenDir = null; // the folder typed or picked and not saved yet
  var ceremony = false; // a passkey ceremony is in flight
  var tracker = null;   // the restore or backup being followed: {mode, timer}
  var refreshTimer = null;
  var checkTimer = null;
  var checkSeq = 0;

  function section() { return document.getElementById('backups'); }
  function main() { return document.querySelector('.main'); }
  function form() { return document.querySelector('[data-backups-form]'); }
  function dirInput() { return document.querySelector('[data-backups-dir]'); }

  function busy(on) {
    var m = main();
    if (!m) return;
    if (on) m.setAttribute('aria-busy', 'true');
    else m.removeAttribute('aria-busy');
  }
  function say(selector, msg) {
    var el = document.querySelector(selector);
    if (el) el.textContent = msg;
  }

  /* ---- Drawing the section again, in place ------------------------------- */

  // redraw fetches this view and morphs it in (navigation.js's patchPage),
  // quietly: while Seamless restarts a fetch fails, and that is expected.
  function redraw() {
    if (!window.SeamConsole || !window.SeamConsole.patchPage) return Promise.resolve(false);
    return fetch(location.href, {
      credentials: 'same-origin', cache: 'no-store',
      headers: { Accept: 'text/html', 'X-Seam-Partial': '1' }
    }).then(function (res) {
      var final = new URL(res.url || location.href, location.href);
      if (final.pathname === '/console/login') { location.assign(final.href); return false; }
      if (!res.ok) return false;
      return res.text().then(function (html) {
        var doc = new DOMParser().parseFromString(html, 'text/html');
        if (!doc.getElementById('backups')) return false;
        return window.SeamConsole.patchPage(doc, 'backups');
      });
    }).catch(function () { return false; });
  }

  // safe reports whether a refresh may run now: nothing typed and unsaved, no
  // ceremony in flight.
  function safe() {
    var f = form();
    return !ceremony && !(f && f.classList.contains('is-dirty'));
  }

  function scheduleRefresh(delay) {
    if (refreshTimer) clearTimeout(refreshTimer);
    refreshTimer = setTimeout(function () {
      refreshTimer = null;
      if (!section()) return;
      if (document.hidden || !safe()) { scheduleRefresh(1500); return; }
      redraw();
    }, delay);
  }

  // A backup.* event: the section shows what it changed.
  document.addEventListener('seam:event', function (e) {
    var kind = e.detail && e.detail.kind;
    if (typeof kind !== 'string' || kind.indexOf('backup.') !== 0 || !section()) return;
    if (tracker && tracker.mode === 'restart') return; // the restart wait redraws when it is done
    scheduleRefresh(600);
  });

  /* ---- Following a restore, and a backup under way ----------------------- */

  function stopTracking() {
    if (tracker && tracker.timer) clearTimeout(tracker.timer);
    tracker = null;
    busy(false);
  }

  // poll redraws every ms until the section says something else.
  function poll(mode, ms) {
    tracker = { mode: mode, timer: null };
    var mine = tracker;
    (function next() {
      mine.timer = setTimeout(function () {
        if (tracker !== mine) return;
        if (document.hidden || !safe()) { next(); return; }
        redraw().then(function () { if (tracker === mine) next(); });
      }, ms);
    })();
  }

  // waitRestart is the restart that finishes a staged restore: the page
  // marks itself busy, waits for /healthz to answer from a new daemon, then
  // draws the section again. It also redraws now and then, in case the
  // restart came before the first look.
  function waitRestart(manual) {
    var pk = window.SeamPasskey;
    tracker = { mode: 'restart', timer: null };
    var mine = tracker;
    if (!manual) busy(true);
    say('[data-backups-wait]', manual ? '' : 'Seamless is restarting to finish the restore…');
    var tries = manual ? 720 : 180;
    var before = null;
    function settle() {
      if (tracker !== mine) return;
      redraw().then(function (drawn) {
        if (tracker !== mine) return;
        if (!drawn) { mine.timer = setTimeout(settle, 1000); return; }
        follow(); // the section says where the restore is now
      });
    }
    function next() {
      if (tracker !== mine) return;
      if (--tries <= 0) {
        busy(false);
        say('[data-backups-wait]', 'Seamless has not come back yet. Check that it is running, then open this section again.');
        tracker = null;
        return;
      }
      mine.timer = setTimeout(function () {
        if (tracker !== mine) return;
        var look = pk ? pk.instance() : Promise.resolve('');
        look.then(function (now) {
          if (tracker !== mine) return;
          if (before === null) { before = now; next(); return; }
          if (now && now !== before) { settle(); return; }
          if (tries % 5 === 0) {
            // Still the same daemon, or none: look at the section itself.
            redraw().then(function (drawn) {
              if (tracker !== mine) return;
              var s = section();
              if (drawn && s && s.getAttribute('data-restore-state') !== 'pending') { follow(); return; }
              next();
            });
            return;
          }
          next();
        });
      }, manual ? 5000 : 1000);
    }
    next();
  }

  // follow starts, keeps or stops following what the section says is under
  // way.
  function follow() {
    var s = section();
    var state = s ? s.getAttribute('data-restore-state') : '';
    var manual = s && s.getAttribute('data-restore-restart') === 'manual';
    var want = '';
    if (state === 'staging' || state === 'archiving') want = 'steps';
    else if (state === 'pending') want = 'restart';
    else if (s && s.hasAttribute('data-backup-running')) want = 'backup';
    if (tracker && tracker.mode === want) {
      if (want === 'restart' && !manual) busy(true);
      return;
    }
    stopTracking();
    if (want === 'steps') poll('steps', 1500);
    else if (want === 'backup') poll('backup', 10000);
    else if (want === 'restart') waitRestart(manual);
  }

  /* ---- The folder field --------------------------------------------------- */

  function markChip() {
    var input = dirInput();
    document.querySelectorAll('[data-backups-chip]').forEach(function (chip) {
      chip.classList.toggle('is-current', !!input && chip.getAttribute('data-backups-chip') === input.value);
    });
  }

  function checkFolder() {
    var input = dirInput();
    var line = document.querySelector('[data-backups-check]');
    if (!input || !line) return;
    var seq = ++checkSeq;
    fetch('/console/settings/backups/folder?dir=' + encodeURIComponent(input.value), {
      credentials: 'same-origin', headers: { Accept: 'application/json' }
    }).then(function (res) {
      return res.json().then(function (data) {
        if (!res.ok) throw new Error(data.error || 'request failed');
        return data;
      });
    }).then(function (data) {
      if (seq !== checkSeq) return;
      line.textContent = data.text || '';
      line.setAttribute('data-usable', data.usable ? 'true' : 'false');
    }).catch(function () {
      if (seq !== checkSeq) return;
      line.textContent = 'The folder could not be checked just now.';
      line.removeAttribute('data-usable');
    });
  }

  document.addEventListener('input', function (e) {
    var input = e.target;
    if (!input.matches || !input.matches('[data-backups-dir]')) return;
    chosenDir = input.value === input.defaultValue ? null : input.value;
    markChip();
    if (checkTimer) clearTimeout(checkTimer);
    checkTimer = setTimeout(checkFolder, 350);
  });

  document.addEventListener('click', function (e) {
    var chip = e.target.closest ? e.target.closest('[data-backups-chip]') : null;
    var input = dirInput();
    if (!chip || !input) return;
    input.value = chip.getAttribute('data-backups-chip');
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.focus();
  });

  /* ---- The restore's passkey ceremony ------------------------------------ */

  function wireRestore() {
    var f = document.querySelector('[data-backups-restore][data-passkey]');
    if (!f || f._seamWired) return;
    f._seamWired = true;
    // On the form itself, so it runs before the shared POST path, which
    // stands down for a submit already handled.
    f.addEventListener('submit', function (e) {
      e.preventDefault();
      var pk = window.SeamPasskey;
      var go = f.querySelector('[data-backups-go]');
      var picked = f.querySelector('input[name="mode"]:checked');
      var ref = f.querySelector('input[name="ref"]');
      function status(msg) { var el = f.querySelector('[data-backups-status]'); if (el) el.textContent = msg; }
      if (!pk || !pk.supported()) { status('This browser cannot use a passkey here: restore from a terminal instead.'); return; }
      if (!picked || !ref) { status('Choose what to restore first.'); return; }
      var fields = { ref: ref.value, mode: picked.value };
      ceremony = true;
      if (go) go.disabled = true;
      status('Follow your browser’s prompt.');
      pk.post('/console/settings/backups/restore/begin', fields).then(function (begun) {
        return pk.assert(begun).then(function (cred) {
          status('Approved. Starting the restore…');
          return pk.post('/console/settings/backups/restore', {
            ref: fields.ref, mode: fields.mode, id: begun.id, response: JSON.stringify(pk.toJSON(cred))
          });
        });
      }).then(function () {
        ceremony = false;
        // The section without the confirm card: its progress card now.
        if (window.SeamConsole && window.SeamConsole.load) {
          return window.SeamConsole.load(SECTION + '#backups', { history: 'replace', source: 'query' });
        }
        return redraw();
      }).catch(function (err) {
        ceremony = false;
        if (go) go.disabled = false;
        status(pk.refusal(err));
      });
    });
  }

  /* ---- After every draw --------------------------------------------------- */

  function enhance(event) {
    if (!section()) { stopTracking(); return; }
    var input = dirInput();
    if (input && chosenDir !== null) {
      if (input.defaultValue === chosenDir) {
        chosenDir = null; // saved
      } else if (input.value !== chosenDir) {
        input.value = chosenDir; // the morph put the saved folder back
        input.dispatchEvent(new Event('input', { bubbles: true }));
      }
    }
    markChip();
    wireRestore();
    if (ceremony) {
      var go = document.querySelector('[data-backups-restore] [data-backups-go]');
      if (go) go.disabled = true;
    }
    reveal(!!event);
    follow();
  }

  // reveal brings a card that has just appeared into view: the confirm card
  // in its row (focused, for the keyboard), and the progress card or result
  // banner at the top of the section after a restore starts or finishes.
  // An in-place load lands on its URL's #anchor (navigation.js), but these
  // cards mostly appear in a redraw -- a backup.* event, the steps, the
  // restart -- which lands nowhere, and an anchor never moves focus.
  var shown = {};
  function reveal(updated) {
    ['backup-restore', 'backup-progress', 'backup-result', 'backup-failed'].forEach(function (id) {
      var el = document.getElementById(id);
      var fresh = !!el && !shown[id];
      shown[id] = !!el;
      if (!fresh || !updated) return;
      try { el.scrollIntoView({ block: 'nearest' }); } catch (e) { el.scrollIntoView(); }
      if (id === 'backup-restore') {
        try { el.focus({ preventScroll: true }); } catch (e) { el.focus(); }
      }
    });
  }

  enhance();
  document.addEventListener('seam:content-updated', enhance);
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden && section() && refreshTimer === null && tracker === null) follow();
  });
})();
