// integrations.js: the Make-style Connect flow. Served from /assets (CSP).
// Clicking Connect opens a sized popup, the same-origin form posts into it and
// redirects to the provider's consent screen; when the provider redirects back,
// oauth-callback.js posts a message here and we refresh to show the new state.
(function () {
  document.querySelectorAll('form[data-popup]').forEach(function (f) {
    f.addEventListener("submit", function () {
      // Open the named window first so the form (target="reactor-oauth") posts
      // into a real popup. Not preventing default: the POST proceeds.
      window.open("", "reactor-oauth", "width=640,height=760,menubar=no,toolbar=no,location=yes");
    });
  });

  window.addEventListener("message", function (e) {
    if (e.origin !== location.origin) return;
    if (!e.data || e.data.type !== "reactor-oauth") return;
    if (e.data.ok) {
      window.location.reload();
    } else {
      window.location.search = "?err=" + encodeURIComponent(e.data.err || "connection failed");
    }
  });
})();
