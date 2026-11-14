/* The console's service worker: it adds this browser's proof
   (browserkey.js, internal/console/browserproof) to the console's own
   requests, so a console unlock, and under passkey sign-in the console
   sign-in itself (internal/console/signin.go), works only in the browser that
   holds the key.

   It runs in a browser that set a key up for an unlock or for signing in --
   under passkey sign-in that is every signed-in browser -- and it signs:
     - every request a console page makes (fetch, the live feed), sent
       same-origin so a redirect never carries the proof off this origin;
     - a page load or a link followed (a GET or HEAD navigation), with redirects
       left to the browser, which loads each step as a new navigation.
   It never re-sends any other navigation: a form posted from another site
   would then look same-origin to the console's origin check (security.go),
   so a posted form goes as the browser sent it, without a proof. The
   console's own forms post through a page fetch (navigation.js), which is
   signed. Outside /console/, the static files, and a browser with no key, it
   stays out of the way. */
'use strict';

importScripts('/console/static/browserkey.js');

var key = null;

function browserKey() {
  if (key) return Promise.resolve(key);
  return self.SeamBrowserKey.load().then(function (k) {
    key = k;
    return k;
  });
}

self.addEventListener('install', function () { self.skipWaiting(); });
self.addEventListener('activate', function (event) { event.waitUntil(self.clients.claim()); });
self.addEventListener('message', function (event) {
  if (event.data && event.data.type === 'claim') event.waitUntil(self.clients.claim());
});

// rebuilt is req again with headers, sent same-origin. A navigation's redirect
// stays the browser's to follow; a page's fetch keeps its own settings. A body
// is read out and sent as bytes, which every browser accepts the same way.
function rebuilt(req, headers, navigation) {
  var init = { method: req.method, headers: headers, credentials: 'same-origin', mode: 'same-origin' };
  if (navigation) {
    init.redirect = 'manual';
  } else {
    init.credentials = req.credentials;
    init.cache = req.cache;
    init.redirect = req.redirect;
    init.signal = req.signal;
  }
  if (req.method === 'GET' || req.method === 'HEAD') return Promise.resolve(new Request(req.url, init));
  return req.arrayBuffer().then(function (body) {
    init.body = body;
    return new Request(req.url, init);
  });
}

function signed(req, navigation) {
  return browserKey().then(function (k) {
    return k ? self.SeamBrowserKey.proof(k) : null;
  }).catch(function () { return null; }).then(function (proof) {
    if (!proof) return fetch(req);
    var headers = new Headers(req.headers);
    headers.set(self.SeamBrowserKey.HEADER, proof);
    return rebuilt(req, headers, navigation).then(function (again) { return fetch(again); });
  });
}

self.addEventListener('fetch', function (event) {
  var req = event.request;
  var url = new URL(req.url);
  if (url.origin !== self.location.origin || url.pathname.indexOf('/console/') !== 0) return;
  if (url.pathname.indexOf('/console/static/') === 0 || url.pathname === '/console/sw.js') return;
  if (req.headers.has(self.SeamBrowserKey.HEADER)) return;
  var navigation = req.mode === 'navigate';
  if (navigation && req.method !== 'GET' && req.method !== 'HEAD') return;
  if (!navigation && !event.clientId) return;
  event.respondWith(signed(req, navigation));
});
