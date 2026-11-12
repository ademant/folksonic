(function () {
  "use strict";
  var token = null, username = "";
  try { token = sessionStorage.getItem("tinysonic.token"); username = sessionStorage.getItem("tinysonic.user") || ""; } catch (e) {}
  if (!token) { location.replace("login"); return; }

  var notice = document.getElementById("notice");
  function say(t) { notice.textContent = t; }
  function $(id) { return document.getElementById(id); }
  function el(tag, text, cls) {
    var n = document.createElement(tag);
    if (text !== undefined && text !== null) n.textContent = text;
    if (cls) n.className = cls;
    return n;
  }
  function when(s) {
    var d = new Date(s);
    return isNaN(d) ? (s || "") : d.toLocaleDateString();
  }
  function signedOut() {
    try { sessionStorage.removeItem("tinysonic.token"); sessionStorage.removeItem("tinysonic.user"); } catch (e) {}
    location.replace("login");
  }
  // raw sends an authenticated request and returns the response; api parses JSON.
  function raw(method, path, body, accept) {
    var h = { "Authorization": "Bearer " + token };
    if (accept) h["Accept"] = accept;
    var opts = { method: method, headers: h };
    if (body !== undefined) { h["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
    return fetch("api/" + path, opts).then(function (r) {
      if (r.status === 401) { signedOut(); throw new Error("signed out"); }
      return r;
    });
  }
  async function api(method, path, body) {
    var r = await raw(method, path, body);
    var j = await r.json().catch(function () { return {}; });
    if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
    return j;
  }
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

  $("who").textContent = username ? "Signed in as " + username : "";
  $("signout").addEventListener("click", async function () {
    try { await raw("POST", "logout"); } catch (e) {}
    signedOut();
  });

  // Messages from an identity-provider round trip arrive as query parameters.
  var params = new URLSearchParams(location.search);
  if (params.get("oidc_error")) say(params.get("oidc_error"));
  else if (params.get("linked")) say("Identity linked.");
  if (location.search) { try { history.replaceState(null, "", location.pathname); } catch (e) {} }

  // API keys
  // Keys unused the longest come first, so the stale ones are easy to spot.
  function lastActivity(k) { return k.lastUsedAt || k.createdAt || ""; }
  function renderKeys(keys) {
    var list = $("keys");
    list.textContent = "";
    if (!keys.length) list.appendChild(el("li", "No API keys yet.", "meta"));
    keys = keys.slice().sort(function (a, b) { return lastActivity(a) < lastActivity(b) ? -1 : lastActivity(a) > lastActivity(b) ? 1 : 0; });
    keys.forEach(function (k) {
      var li = el("li");
      var info = el("div");
      info.appendChild(el("strong", k.label));
      info.appendChild(el("span", "  " + k.prefix + "…", "meta"));
      info.appendChild(el("div", "Created " + when(k.createdAt) +
        (k.lastUsedAt ? " · last used " + when(k.lastUsedAt) : " · never used") +
        (k.expiresAt ? " · expires " + when(k.expiresAt) : ""), "meta"));
      li.appendChild(info);
      var b = el("button", "Revoke", "danger small");
      b.addEventListener("click", async function () {
        try { await api("DELETE", "keys/" + k.id); say("Key revoked."); await loadKeys(); } catch (e) { say(e.message); }
      });
      li.appendChild(b);
      list.appendChild(li);
    });
  }
  async function loadKeys() { renderKeys((await api("GET", "keys")).keys || []); }

  // Devices (#101): paired apps, and pairings waiting for approval.
  function renderPending(list) {
    var box = $("pendingBox"), ul = $("pending");
    ul.textContent = "";
    box.hidden = !list.length;
    list.forEach(function (p) {
      var li = el("li");
      var info = el("div");
      info.appendChild(el("strong", p.name));
      info.appendChild(el("div", "Requested " + when(p.createdAt) + " · expires " + when(p.expiresAt), "meta"));
      li.appendChild(info);
      var ok = el("button", "Approve", "small");
      ok.addEventListener("click", async function () {
        try { await api("POST", "pair/pending/" + p.id + "/approve"); say("Device approved."); await refreshDevices(); } catch (e) { say(e.message); }
      });
      var no = el("button", "Deny", "danger small");
      no.addEventListener("click", async function () {
        try { await api("POST", "pair/pending/" + p.id + "/deny"); say("Pairing denied."); await refreshDevices(); } catch (e) { say(e.message); }
      });
      li.appendChild(ok);
      li.appendChild(no);
      ul.appendChild(li);
    });
  }
  function renderDevices(list) {
    var ul = $("devices");
    ul.textContent = "";
    if (!list.length) ul.appendChild(el("li", "No paired devices yet.", "meta"));
    list.forEach(function (d) {
      var li = el("li");
      var info = el("div");
      info.appendChild(el("strong", d.name));
      info.appendChild(el("div", "Paired " + when(d.createdAt) +
        (d.lastUsedAt ? " · last used " + when(d.lastUsedAt) : " · not used yet"), "meta"));
      li.appendChild(info);
      var b = el("button", "Remove", "danger small");
      b.addEventListener("click", async function () {
        try { await api("DELETE", "devices/" + d.id); say("Device removed."); await refreshDevices(); } catch (e) { say(e.message); }
      });
      li.appendChild(b);
      ul.appendChild(li);
    });
  }
  async function refreshDevices() {
    var pending = (await api("GET", "pair/pending")).pending || [];
    renderPending(pending);
    renderDevices((await api("GET", "devices")).devices || []);
  }
  $("revokeAllKeys").addEventListener("click", async function () {
    if (!confirm("Revoke every API key on this account? Apps and players using them will have to pair again.")) return;
    try {
      var keys = (await api("GET", "keys")).keys || [];
      for (var i = 0; i < keys.length; i++) await api("DELETE", "keys/" + keys[i].id);
      say("All keys revoked.");
      await loadKeys();
    } catch (e) { say(e.message); }
  });
  $("createKey").addEventListener("click", async function () {
    var label = $("keyLabel").value.trim();
    var days = parseInt($("keyExpiry").value, 10) || 0;
    try {
      var j = await api("POST", "keys", { label: label, expires_days: days });
      $("newKeyValue").textContent = j.apiKey;
      $("newKey").style.display = "block";
      say("Key created.");
      $("keyLabel").value = "";
      await loadKeys();
    } catch (e) { say(e.message); }
  });
  $("copyKey").addEventListener("click", function () {
    var v = $("newKeyValue").textContent;
    if (navigator.clipboard) navigator.clipboard.writeText(v).then(function () { say("Copied."); }, function () { say("Select the key and copy it by hand."); });
  });
  $("dismissKey").addEventListener("click", function () {
    $("newKeyValue").textContent = "";
    $("newKey").style.display = "none";
  });

  // Pairing QR: SVG first, PNG when the SVG cannot be fetched or shown.
  var qrURL = null;
  function showQR(blob) {
    if (qrURL) URL.revokeObjectURL(qrURL);
    qrURL = URL.createObjectURL(blob);
    $("qrImg").src = qrURL;
    $("qr").style.display = "block";
  }
  async function pairingImage(accept) {
    var r = await raw("POST", "pair/qr", undefined, accept);
    if (!r.ok) throw new Error((await r.json().catch(function () { return {}; })).error || ("HTTP " + r.status));
    return r.blob();
  }
  $("showQR").addEventListener("click", async function () {
    try {
      var blob;
      try { blob = await pairingImage("image/svg+xml"); } catch (e) { blob = await pairingImage("image/png"); }
      showQR(blob);
      say("Scan the code in the app within five minutes.");
    } catch (e) { say(e.message); }
  });
  $("qrImg").addEventListener("error", async function () {
    if (qrURL && $("qrImg").dataset.png !== "1") {
      try { showQR(await pairingImage("image/png")); $("qrImg").dataset.png = "1"; } catch (e) { say(e.message); }
    }
  });

  // Passkeys
  function renderPasskeys(list) {
    var ul = $("passkeys");
    ul.textContent = "";
    if (!list.length) ul.appendChild(el("li", "No passkeys yet.", "meta"));
    list.forEach(function (p) {
      var li = el("li");
      var info = el("div");
      info.appendChild(el("strong", p.label || "Unnamed passkey"));
      info.appendChild(el("div", "Added " + when(p.createdAt), "meta"));
      li.appendChild(info);
      var b = el("button", "Remove", "danger small");
      b.addEventListener("click", async function () {
        try { await api("DELETE", "passkeys/" + p.id); say("Passkey removed."); await loadPasskeys(); } catch (e) { say(e.message); }
      });
      li.appendChild(b);
      ul.appendChild(li);
    });
  }
  async function loadPasskeys() { renderPasskeys((await api("GET", "passkeys")).passkeys || []); }
  function prepareCreation(o) {
    o.challenge = b64uToBuf(o.challenge);
    o.user.id = b64uToBuf(o.user.id);
    (o.excludeCredentials || []).forEach(function (c) { c.id = b64uToBuf(c.id); });
    return o;
  }
  $("addPasskey").addEventListener("click", async function () {
    var label = $("pkLabel").value.trim();
    try {
      say("Create your passkey…");
      var begin = await api("POST", "passkey/register/begin");
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
      var r = await raw("POST", "passkey/register/finish?ceremony=" + encodeURIComponent(begin.ceremony) + "&label=" + encodeURIComponent(label), resp);
      var j = await r.json().catch(function () { return {}; });
      if (!r.ok) throw new Error(j.error || "registration failed");
      $("pkLabel").value = "";
      say("Passkey added.");
      await loadPasskeys();
    } catch (e) { say(e.message || String(e)); }
  });

  // Single sign-on: shown only when the server has it configured.
  function renderIdentities(list) {
    var ul = $("identities");
    ul.textContent = "";
    if (!list.length) ul.appendChild(el("li", "No identities linked.", "meta"));
    list.forEach(function (i) {
      var li = el("li");
      var info = el("div");
      info.appendChild(el("strong", i.issuer));
      info.appendChild(el("div", "Linked " + when(i.createdAt), "meta"));
      li.appendChild(info);
      var b = el("button", "Unlink", "danger small");
      b.addEventListener("click", async function () {
        try { await api("DELETE", "oidc/identities/" + i.id); say("Identity unlinked."); await loadIdentities(); } catch (e) { say(e.message); }
      });
      li.appendChild(b);
      ul.appendChild(li);
    });
  }
  async function loadIdentities() { renderIdentities((await api("GET", "oidc/identities")).identities || []); }
  $("linkIdentity").addEventListener("click", async function () {
    try {
      var j = await api("POST", "oidc/begin", { mode: "link", client: "web" });
      location.href = j.url;
    } catch (e) { say(e.message); }
  });

  // Load everything; each section fails on its own so one problem does not hide the rest.
  [loadKeys, loadPasskeys, refreshDevices].forEach(function (f) { f().catch(function (e) { say(e.message); }); });
  // Pairings appear while a phone is waiting; refresh while the page is in view.
  setInterval(function () {
    if (document.visibilityState === "visible") refreshDevices().catch(function () {});
  }, 5000);
  // The library page is for librarians; its stats endpoint answers 403 otherwise.
  raw("GET", "library/stats").then(function (r) { $("librarySection").hidden = !r.ok; }).catch(function () {});
  fetch("api/oidc/status").then(function (r) { return r.json(); }).then(function (j) {
    if (!j.enabled) return;
    $("ssoSection").hidden = false;
    loadIdentities().catch(function (e) { say(e.message); });
  }).catch(function () {});
})();
