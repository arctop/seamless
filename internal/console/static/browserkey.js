/* The browser key: what makes a console unlock this browser's alone
   (internal/console/browserproof), and under passkey sign-in the console
   sign-in too (internal/console/signin.go): the login page sets the key and
   the worker up before it signs in, so every console request after it
   carries the proof.

   A browser sends its cookies to every port of the host that set them, so the
   unlock cookie alone could be read by any program that opens this browser on
   another local port. IndexedDB belongs to one origin (scheme, host and port),
   so the key lives there: an ECDSA P-256 key made non-extractable, which
   script can use but never read. The daemon stores an unlock's hash over the
   cookie and this key's public half, and counts it only for a request whose
   proof the key signed.

   The page and the service worker (sw.js, which imports this file) share the
   storage and the proof below. In the page it also:
     - sets the key and the worker up before an unlock begins (ready()), so the
       begin, and every request after it, carries a proof;
     - asks again when a page was drawn without one (a hard reload skips the
       worker; the layout marks such a page data-browser-proof="missing"): once
       the worker controls the page, the view is fetched again in place.
   Both ask the console for the worker again first, and wait while a new one
   takes the page: after an upgrade the old worker would sign with the old
   daemon's code. The daemon serves this file and the worker no-cache, so a
   new worker imports this file as the new daemon serves it.
   A browser that cannot hold the key (no service worker: an insecure page or
   some private windows) is told so, and nothing is unlocked. */
(function (scope) {
  'use strict';

  var DB = 'seamless-console', STORE = 'keys', ID = 'browser';
  var HEADER = 'Seamless-Browser-Proof';

  // One request against the key store: fn(store) returns the IDBRequest whose
  // result the promise resolves with once its transaction commits.
  function withStore(mode, fn) {
    return new Promise(function (resolve, reject) {
      var open = indexedDB.open(DB, 1);
      open.onupgradeneeded = function () { open.result.createObjectStore(STORE); };
      open.onerror = function () { reject(open.error); };
      open.onsuccess = function () {
        var db = open.result, tx, req;
        try {
          tx = db.transaction(STORE, mode);
          req = fn(tx.objectStore(STORE));
        } catch (e) { db.close(); reject(e); return; }
        tx.oncomplete = function () { db.close(); resolve(req.result); };
        tx.onerror = tx.onabort = function () { db.close(); reject(tx.error); };
      };
    });
  }

  function b64url(buf) {
    var bytes = new Uint8Array(buf), bin = '';
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  // load resolves with the stored key ({ privateKey, publicKey }), or null.
  function load() {
    return withStore('readonly', function (s) { return s.get(ID); }).then(function (k) {
      return k && k.privateKey && typeof k.publicKey === 'string' ? k : null;
    });
  }

  // create makes the key once. add, never put: of two tabs racing here the
  // first key stays, so the page and the worker always sign with the same one.
  function create() {
    return crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']).then(function (pair) {
      return crypto.subtle.exportKey('spki', pair.publicKey).then(function (spki) {
        var key = { privateKey: pair.privateKey, publicKey: b64url(spki) };
        return withStore('readwrite', function (s) { return s.add(key, ID); }).then(function () { return key; }, load);
      });
    }).then(function (key) {
      if (!key) throw new Error('This browser could not keep its unlock key.');
      return key;
    });
  }

  // proof is the header value for a request sent now: the time, signed.
  function proof(key) {
    var ts = Math.floor(Date.now() / 1000);
    var msg = new TextEncoder().encode('seamless browser proof v1\n' + ts);
    return crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, key.privateKey, msg).then(function (sig) {
      return 'v1.' + ts + '.' + key.publicKey + '.' + b64url(sig);
    });
  }

  scope.SeamBrowserKey = { HEADER: HEADER, load: load, proof: proof };
  if (typeof window === 'undefined' || scope !== window) return; // the service worker needs only the above

  function supported() {
    return !!(window.isSecureContext && navigator.serviceWorker && window.crypto && crypto.subtle && window.indexedDB);
  }

  // WAIT bounds how long a page waits for the worker to sign its requests:
  // long enough to fetch and install a new worker over a slow network.
  var WAIT = 10000;

  // current resolves true once this page is controlled by the newest worker
  // the console serves, or false after ms. After an upgrade the worker still
  // running is the old one, with the old daemon's copy of this file, so it
  // first asks the console for the worker again (reg.update()). A newer worker
  // installing, waiting or activating takes this page itself (sw.js:
  // skipWaiting, then clients.claim), so the page waits for that
  // controllerchange. With no newer worker, any controller is the newest. An
  // active worker that does not control the page yet (a hard reload skips it)
  // is asked to claim it. A newer worker that fails to install leaves the
  // active one the newest. A failed update check changes nothing: the active
  // worker is the newest the browser could get.
  function current(reg, ms) {
    var sw = navigator.serviceWorker;
    return new Promise(function (resolve) {
      var settled = false, checked = false, asked = false, newer = null, taken = false, timer = null;
      function settle(ok) {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        sw.removeEventListener('controllerchange', changed);
        resolve(ok);
      }
      // pending is the registration's worker that has not taken the page yet.
      function pending() {
        if (reg.installing || reg.waiting) return reg.installing || reg.waiting;
        return reg.active && reg.active.state === 'activating' ? reg.active : null;
      }
      // taken records a controllerchange since the newer worker began to
      // activate: one before then was some other worker's. The page's
      // controller and reg.active are one object for one worker, so the
      // comparison below also sees a change this page heard before it looked.
      function check() {
        if (settled || !checked) return;
        var next = pending();
        if (next) {
          if (next !== newer) {
            newer = next;
            next.addEventListener('statechange', check);
          }
          if (next.state !== 'activating') taken = false;
          return;
        }
        if (newer && newer.state === 'redundant') newer = null;
        var ctl = sw.controller;
        if (ctl && (!newer || taken || ctl === reg.active)) { settle(true); return; }
        // No controller yet, or still the old one: the active worker is asked
        // to take the page.
        if (!asked && reg.active) {
          asked = true;
          reg.active.postMessage({ type: 'claim' });
        }
      }
      function changed() {
        if (newer) taken = true;
        check();
      }
      sw.addEventListener('controllerchange', changed);
      timer = setTimeout(function () { settle(false); }, ms);
      reg.update().catch(function () {}).then(function () {
        checked = true;
        check();
      });
    });
  }

  // ready sets up the key and the worker, and resolves once this page's
  // requests carry the newest worker's proofs. It rejects with words for the
  // owner.
  function ready() {
    if (!supported()) {
      return Promise.reject(new Error('This browser cannot hold a console unlock here: it needs a secure page (https, or localhost) that allows service workers, which some private windows do not.'));
    }
    return load().then(function (k) { return k || create(); }).then(function () {
      return navigator.serviceWorker.register('/console/sw.js', { scope: '/console/' });
    }).then(function () {
      return navigator.serviceWorker.ready;
    }).then(function (reg) {
      return current(reg, WAIT);
    }).then(function (ok) {
      if (!ok) throw new Error('The console\'s service worker did not start in this browser: reload the page and try again.');
    });
  }

  // recover runs once per page load: a page drawn without a proof is fetched
  // again in place once the newest worker can sign, and left as drawn
  // otherwise.
  function recover() {
    if (document.documentElement.getAttribute('data-browser-proof') !== 'missing' || !supported()) return;
    navigator.serviceWorker.getRegistration('/console/').then(function (reg) {
      if (!reg || !reg.active) return false;
      return load().then(function (k) { return k ? current(reg, WAIT) : false; });
    }).then(function (ok) {
      if (ok && window.SeamConsole && window.SeamConsole.load) {
        window.SeamConsole.load(location.href, { history: 'replace', source: 'query' });
      }
    }).catch(function () {});
  }

  scope.SeamBrowserKey.ready = ready;
  scope.SeamBrowserKey.supported = supported;
  recover();
})(self);
