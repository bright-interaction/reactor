// Reactor WebAuthn ceremony driver. Loaded as an external asset (the dashboard
// CSP is `script-src 'self'`, so no inline script / onclick is possible). Wires
// any [data-webauthn] button to a passkey register or assert ceremony.
(function () {
  'use strict';

  // base64url <-> ArrayBuffer. WebAuthn JSON uses unpadded base64url.
  function b64uToBuf(s) {
    s = s.replace(/-/g, '+').replace(/_/g, '/');
    var pad = s.length % 4;
    if (pad) { s += '===='.slice(pad); }
    var bin = atob(s);
    var buf = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) { buf[i] = bin.charCodeAt(i); }
    return buf.buffer;
  }
  function bufToB64u(buf) {
    var bytes = new Uint8Array(buf);
    var bin = '';
    for (var i = 0; i < bytes.length; i++) { bin += String.fromCharCode(bytes[i]); }
    return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function showError(msg) {
    var el = document.getElementById('webauthn-error');
    if (el) { el.textContent = msg; el.hidden = false; }
    else { window.alert(msg); }
  }
  function clearError() {
    var el = document.getElementById('webauthn-error');
    if (el) { el.hidden = true; el.textContent = ''; }
  }

  async function postJSON(url, body) {
    var res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
      body: JSON.stringify(body || {})
    });
    var data = {};
    try { data = await res.json(); } catch (e) { /* non-JSON error body */ }
    return { res: res, data: data };
  }

  function decodeCreation(options) {
    var pk = options.publicKey;
    pk.challenge = b64uToBuf(pk.challenge);
    pk.user.id = b64uToBuf(pk.user.id);
    if (pk.excludeCredentials) {
      pk.excludeCredentials.forEach(function (c) { c.id = b64uToBuf(c.id); });
    }
    return pk;
  }
  function decodeRequest(options) {
    var pk = options.publicKey;
    pk.challenge = b64uToBuf(pk.challenge);
    if (pk.allowCredentials) {
      pk.allowCredentials.forEach(function (c) { c.id = b64uToBuf(c.id); });
    }
    return pk;
  }

  function encodeAttestation(cred) {
    return {
      id: cred.id,
      rawId: bufToB64u(cred.rawId),
      type: cred.type,
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
      response: {
        attestationObject: bufToB64u(cred.response.attestationObject),
        clientDataJSON: bufToB64u(cred.response.clientDataJSON)
      }
    };
  }
  function encodeAssertion(cred) {
    var r = cred.response;
    return {
      id: cred.id,
      rawId: bufToB64u(cred.rawId),
      type: cred.type,
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
      response: {
        authenticatorData: bufToB64u(r.authenticatorData),
        clientDataJSON: bufToB64u(r.clientDataJSON),
        signature: bufToB64u(r.signature),
        userHandle: r.userHandle ? bufToB64u(r.userHandle) : null
      }
    };
  }

  async function runRegister(btn) {
    clearError();
    var begin = await postJSON(btn.dataset.begin, {});
    if (!begin.res.ok) { showError(begin.data.error || 'Could not start.'); return; }
    var pk = decodeCreation(begin.data.options);
    var cred;
    try { cred = await navigator.credentials.create({ publicKey: pk }); }
    catch (e) { showError('Passkey registration was cancelled or failed.'); return; }
    var nameSrc = btn.dataset.nameSource ? document.getElementById(btn.dataset.nameSource) : null;
    var finish = await postJSON(btn.dataset.finish, {
      challengeId: begin.data.challengeId,
      name: nameSrc ? nameSrc.value : '',
      response: encodeAttestation(cred)
    });
    if (finish.res.ok && finish.data.redirect) { window.location = finish.data.redirect; }
    else { showError(finish.data.error || 'Registration failed.'); }
  }

  async function runAssert(btn) {
    clearError();
    var begin = await postJSON(btn.dataset.begin, {});
    if (!begin.res.ok) { showError(begin.data.error || 'Could not start.'); return; }
    var pk = decodeRequest(begin.data.options);
    var cred;
    try { cred = await navigator.credentials.get({ publicKey: pk }); }
    catch (e) { showError('Passkey check was cancelled or failed.'); return; }
    var finish = await postJSON(btn.dataset.finish, {
      challengeId: begin.data.challengeId,
      response: encodeAssertion(cred)
    });
    if (finish.res.ok && finish.data.redirect) { window.location = finish.data.redirect; }
    else { showError(finish.data.error || 'Passkey check failed.'); }
  }

  document.addEventListener('DOMContentLoaded', function () {
    var buttons = document.querySelectorAll('[data-webauthn]');
    if (!window.PublicKeyCredential) {
      buttons.forEach(function (btn) {
        btn.disabled = true;
        btn.title = 'This browser does not support passkeys.';
      });
      return;
    }
    buttons.forEach(function (btn) {
      btn.addEventListener('click', function (e) {
        e.preventDefault();
        btn.disabled = true;
        var done = function () { btn.disabled = false; };
        var run = btn.dataset.webauthn === 'register' ? runRegister : runAssert;
        run(btn).finally(done);
      });
    });
  });
})();
