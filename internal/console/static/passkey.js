/* The WebAuthn helpers the console's passkey ceremonies share
   (window.SeamPasskey): the lock page's lock change, Settings > Backups'
   restore, and under passkey sign-in the login page and the Add a passkey
   page. Each ceremony is two JSON POSTs around navigator.credentials.get (or
   create, to add a passkey) -- begin answers the options, the page asks the
   authenticator, finish sends its answer back -- and a change the daemon
   applies at startup waits for the restart by watching /healthz name a new
   instance. */
(function () {
  'use strict';

  function b64urlToBuf(s) {
    s = s.replace(/-/g, '+').replace(/_/g, '/');
    while (s.length % 4) s += '=';
    var bin = atob(s), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out.buffer;
  }
  function bufToB64url(buf) {
    var bytes = new Uint8Array(buf), bin = '';
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }
  // requestOptions decodes the server's navigator.credentials.get options.
  function requestOptions(pk) {
    if (window.PublicKeyCredential && PublicKeyCredential.parseRequestOptionsFromJSON) {
      return PublicKeyCredential.parseRequestOptionsFromJSON(pk);
    }
    pk.challenge = b64urlToBuf(pk.challenge);
    (pk.allowCredentials || []).forEach(function (c) { c.id = b64urlToBuf(c.id); });
    return pk;
  }
  // creationOptions decodes the server's navigator.credentials.create options.
  function creationOptions(pk) {
    if (window.PublicKeyCredential && PublicKeyCredential.parseCreationOptionsFromJSON) {
      return PublicKeyCredential.parseCreationOptionsFromJSON(pk);
    }
    pk.challenge = b64urlToBuf(pk.challenge);
    pk.user.id = b64urlToBuf(pk.user.id);
    (pk.excludeCredentials || []).forEach(function (c) { c.id = b64urlToBuf(c.id); });
    return pk;
  }
  // toJSON encodes the browser's assertion, or a new passkey's attestation,
  // the way the server reads it.
  function toJSON(cred) {
    if (cred.toJSON) return cred.toJSON();
    var r = cred.response;
    var out = {
      id: cred.id, rawId: bufToB64url(cred.rawId), type: cred.type,
      authenticatorAttachment: cred.authenticatorAttachment || null,
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
    };
    if (r.attestationObject) {
      out.response = { clientDataJSON: bufToB64url(r.clientDataJSON), attestationObject: bufToB64url(r.attestationObject) };
      if (r.getTransports) out.response.transports = r.getTransports();
      return out;
    }
    out.response = {
      clientDataJSON: bufToB64url(r.clientDataJSON), authenticatorData: bufToB64url(r.authenticatorData),
      signature: bufToB64url(r.signature)
    };
    if (r.userHandle) out.response.userHandle = bufToB64url(r.userHandle);
    return out;
  }
  // assert runs the authenticator over a begin's options.
  function assert(begun) {
    return navigator.credentials.get({ publicKey: requestOptions(begun.options.publicKey) });
  }
  // create makes a new passkey over a begin's creation options.
  function create(begun) {
    return navigator.credentials.create({ publicKey: creationOptions(begun.options.publicKey) });
  }
  // post sends one ceremony step and resolves with its JSON answer, or
  // rejects with the server's own refusal.
  function post(path, fields) {
    return fetch(path, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Accept': 'application/json', 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams(fields).toString()
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) throw new Error(data.error || ('request failed (' + res.status + ')'));
        return data;
      });
    });
  }
  // instance is the daemon process /healthz answers from, '' while none does.
  function instance() {
    return fetch('/healthz', { cache: 'no-store' }).then(function (res) {
      return res.ok ? res.json() : {};
    }).then(function (h) { return h.instance || ''; }).catch(function () { return ''; });
  }
  // waitForRestart resolves true once /healthz answers from a process other
  // than before, polling once a second up to tries times.
  function waitForRestart(before, tries) {
    if (tries <= 0) return Promise.resolve(false);
    return new Promise(function (resolve) { setTimeout(resolve, 1000); }).then(instance).then(function (now) {
      return now && now !== before ? true : waitForRestart(before, tries - 1);
    });
  }
  function supported() { return !!(window.PublicKeyCredential && navigator.credentials); }
  // refusal words a failed ceremony for the page's status line.
  function refusal(err) {
    if (err && err.name === 'NotAllowedError') return 'Cancelled, or the authenticator timed out. Nothing changed.';
    return err && err.message ? err.message : 'Something went wrong. Nothing changed.';
  }

  window.SeamPasskey = {
    assert: assert, create: create, creationOptions: creationOptions, instance: instance, post: post, refusal: refusal,
    requestOptions: requestOptions, supported: supported, toJSON: toJSON, waitForRestart: waitForRestart
  };
})();
