// oauth-callback.js: runs on the page the OAuth provider redirects back to.
// In a popup (the Integrations Connect flow) it tells the opener the result and
// closes itself. Otherwise it falls back to the Integrations page. Served from
// /assets so it satisfies CSP (no inline JS).
(function () {
  var el = document.getElementById("oauth-result");
  if (!el) return;
  var ok = el.getAttribute("data-ok") === "1";
  var name = el.getAttribute("data-name") || "";
  var err = el.getAttribute("data-err") || "";

  if (window.opener && !window.opener.closed) {
    try {
      window.opener.postMessage({ type: "reactor-oauth", ok: ok, name: name, err: err }, location.origin);
    } catch (e) { /* opener gone */ }
    window.close();
    return;
  }
  // Standalone (not a popup): land on the Integrations page with the result.
  var q = ok ? "ok=" + encodeURIComponent(name) : "err=" + encodeURIComponent(err || "oauth failed");
  window.location.replace("/integrations?" + q);
})();
