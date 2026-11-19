(function () {
  "use strict";
  var msg = document.getElementById("msg");
  var input = document.getElementById("link");
  var copy = document.getElementById("copy");
  function say(t) { msg.textContent = t; }

  // The token is in the fragment, which the browser never sends to the server.
  // It is taken out of the address bar here, so it is not left in the history.
  var link = "";
  var m = /^#token=([^&]+)$/.exec(location.hash);
  if (m) {
    link = location.origin + location.pathname + "#token=" + m[1];
    try { history.replaceState(null, "", location.pathname); } catch (e) {}
    input.value = link;
    copy.disabled = false;
  } else {
    say("This page has no pairing code. Create a new code on the account page.");
  }

  copy.addEventListener("click", async function () {
    if (!link) return;
    try {
      await navigator.clipboard.writeText(link);
      say("Link copied.");
    } catch (e) {
      input.select();
      say("Select the link and copy it.");
    }
  });
})();
