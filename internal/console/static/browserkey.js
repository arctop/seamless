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

  // controlled resolves true once the worker controls this page, asking an
  // active worker to claim it, or false after ms.
  function controlled(reg, ms) {
    if (navigator.serviceWorker.controller) return Promise.resolve(true);
    return new Promise(function (resolve) {
      var settled = false;
      function settle(ok) { if (!settled) { settled = true; resolve(ok); } }
      navigator.serviceWorker.addEventListener('controllerchange', function () { settle(true); });
      if (reg && reg.active) reg.active.postMessage({ type: 'claim' });
      setTimeout(function () { settle(!!navigator.serviceWorker.controller); }, ms);
    });
  }

  // ready sets up the key and the worker, and resolves once this page's
  // requests carry proofs. It rejects with words for the owner.
  function ready() {
    if (!supported()) {
      return Promise.reject(new Error('This browser cannot hold a console unlock here: it needs a secure page (https, or localhost) that allows service workers, which some private windows do not.'));
    }
    return load().then(function (k) { return k || create(); }).then(function () {
      return navigator.serviceWorker.register('/console/sw.js', { scope: '/console/' });
    }).then(function () {
      return navigator.serviceWorker.ready;
    }).then(function (reg) {
      return controlled(reg, 5000);
    }).then(function (ok) {
      if (!ok) throw new Error('The console\'s service worker did not start in this browser: reload the page and try again.');
    });
  }

  // recover runs once per page load: a page drawn without a proof is fetched
  // again in place once the worker can sign, and left as drawn otherwise.
  function recover() {
    if (document.documentElement.getAttribute('data-browser-proof') !== 'missing' || !supported()) return;
    navigator.serviceWorker.getRegistration('/console/').then(function (reg) {
      if (!reg || !reg.active) return false;
      return load().then(function (k) { return k ? controlled(reg, 3000) : false; });
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
