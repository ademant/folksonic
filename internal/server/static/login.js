(function () {
  "use strict";
  var msg = document.getElementById("msg");
  function say(t) { msg.textContent = t; }
  function b64uToBuf(s) {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    var bin = atob(s), buf = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
    return buf.buffer;
  }
  function bufToB64u(buf) {
    var bytes = new Uint8Array(buf), s = "";
    for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }
  async function post(path, body) {
    var r = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) });
    var j = await r.json().catch(function () { return {}; });
    if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
    return j;
  }
  // finish stores the session and opens the account page.
  function finish(token, username) {
    try { sessionStorage.setItem("tinysonic.token", token); sessionStorage.setItem("tinysonic.user", username); } catch (e) {}
    say("Signed in as " + username + ".");
    location.href = "account";
  }
  // Single sign-on: begin asks the server for the provider's URL and the browser
  // follows it. The mode and any invitation are kept on the server.
  async function startSSO(body) {
    var j = await post("api/oidc/begin", body);
    location.href = j.url;
  }
  // The identity provider's round trip ends here, with a one-time code in the
  // fragment (never sent to the server) or an error in the query string.
  (function handleSSOReturn() {
    var q = new URLSearchParams(location.search);
    var err = q.get("oidc_error");
    if (err) say(err);
    var hash = new URLSearchParams(location.hash.replace(/^#/, ""));
    var code = hash.get("oidc");
    if (!code) return;
    try { history.replaceState(null, "", location.pathname); } catch (e) {}
    say("Completing sign-in…");
    post("api/oidc/exchange", { code: code, device: "web" })
      .then(function (j) { finish(j.token, j.username); })
      .catch(function (e) { say(e.message || String(e)); });
  })();
  fetch("api/oidc/status").then(function (r) { return r.json(); }).then(function (j) {
    if (!j.enabled) return;
    document.getElementById("sso").hidden = false;
    document.getElementById("registerSSO").hidden = false;
  }).catch(function () {});
  document.getElementById("sso").addEventListener("click", async function () {
    try { say("Redirecting to your identity provider…"); await startSSO({ mode: "signin", client: "web" }); }
    catch (e) { say(e.message || String(e)); }
  });
  document.getElementById("registerSSO").addEventListener("click", async function () {
    var code = document.getElementById("ri").value.trim();
    var name = document.getElementById("ru").value.trim();
    if (!code || !name) { say("Enter the invitation code and a username first."); return; }
    try { say("Redirecting to your identity provider…"); await startSSO({ mode: "invite", client: "web", invite: code, username: name }); }
    catch (e) { say(e.message || String(e)); }
  });
  document.getElementById("passkey").addEventListener("click", async function () {
    var name = document.getElementById("u").value.trim();
    if (!name) { say("Enter your username first."); return; }
    try {
      say("Waiting for your passkey…");
      var begin = await post("api/passkey/login/begin", { username: name });
      var o = begin.options.publicKey;
      o.challenge = b64uToBuf(o.challenge);
      (o.allowCredentials || []).forEach(function (c) { c.id = b64uToBuf(c.id); });
      var cred = await navigator.credentials.get({ publicKey: o });
      var resp = {
        id: bufToB64u(cred.rawId), rawId: bufToB64u(cred.rawId), type: cred.type,
        response: {
          authenticatorData: bufToB64u(cred.response.authenticatorData),
          clientDataJSON: bufToB64u(cred.response.clientDataJSON),
          signature: bufToB64u(cred.response.signature),
          userHandle: cred.response.userHandle ? bufToB64u(cred.response.userHandle) : null
        },
        clientExtensionResults: {}
      };
      var done = await fetch("api/passkey/login/finish?ceremony=" + encodeURIComponent(begin.ceremony), {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(resp)
      });
      var j = await done.json();
      if (!done.ok) throw new Error(j.error || "sign-in failed");
      finish(j.token, j.username);
    } catch (e) { say(e.message || String(e)); }
  });
  // A registration link carries the code as ?invite=. Move it into the form
  // and drop the query from the address bar so it does not linger in history.
  // This runs after the error message above has read its parameter.
  var invite = new URLSearchParams(location.search).get("invite");
  if (invite) {
    document.getElementById("ri").value = invite;
    document.getElementById("reg").open = true;
  }
  if (location.search) { try { history.replaceState(null, "", location.pathname); } catch (e) {} }
  document.getElementById("register").addEventListener("click", async function () {
    var code = document.getElementById("ri").value.trim();
    var name = document.getElementById("ru").value.trim();
    var pw = document.getElementById("rp").value;
    if (!code || !name || !pw) { say("Enter the invitation code, a username and a password, or use a passkey instead."); return; }
    if (pw !== document.getElementById("rp2").value) { say("The passwords do not match."); return; }
    try {
      var j = await post("api/register", { username: name, password: pw, invite: code });
      finish(j.token, j.username);
    } catch (e) { say(e.message || String(e)); }
  });
  // Passkey sign-up: the account is created only after the passkey is verified.
  function prepareCreation(o) {
    o.challenge = b64uToBuf(o.challenge);
    o.user.id = b64uToBuf(o.user.id);
    (o.excludeCredentials || []).forEach(function (c) { c.id = b64uToBuf(c.id); });
    return o;
  }
  document.getElementById("registerPasskey").addEventListener("click", async function () {
    var code = document.getElementById("ri").value.trim();
    var name = document.getElementById("ru").value.trim();
    if (!code || !name) { say("Enter the invitation code and a username first."); return; }
    try {
      say("Create your passkey…");
      var begin = await post("api/register/passkey/begin", { username: name, invite: code });
      var cred = await navigator.credentials.create({ publicKey: prepareCreation(begin.options.publicKey) });
      var resp = {
        id: bufToB64u(cred.rawId), rawId: bufToB64u(cred.rawId), type: cred.type,
        response: {
          attestationObject: bufToB64u(cred.response.attestationObject),
          clientDataJSON: bufToB64u(cred.response.clientDataJSON)
        },
        clientExtensionResults: {}
      };
      if (cred.response.getTransports) resp.response.transports = cred.response.getTransports();
      var done = await fetch("api/register/passkey/finish?ceremony=" + encodeURIComponent(begin.ceremony), {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(resp)
      });
      var j = await done.json();
      if (!done.ok) throw new Error(j.error || "registration failed");
      finish(j.token, j.username);
    } catch (e) { say(e.message || String(e)); }
  });
  document.getElementById("pw").addEventListener("click", async function () {
    var name = document.getElementById("u").value.trim();
    var pw = document.getElementById("p").value;
    try {
      var j = await post("api/login", { username: name, password: pw });
      finish(j.token, j.username);
    } catch (e) { say(e.message || String(e)); }
  });
})();
