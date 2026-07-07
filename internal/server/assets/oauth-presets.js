// oauth-presets.js: one-click fill of the OAuth provider form from a preset
// (Google / Microsoft / GitHub). Served from /assets (CSP blocks inline JS).
// The operator still supplies their own client id + secret; the preset only
// fills the endpoints + default scopes so nobody has to look up URLs.
(function () {
  var form = document.getElementById("oauth-provider-form");
  if (!form) return;
  function set(name, val) {
    var el = form.querySelector('[name="' + name + '"]');
    if (el) el.value = val == null ? "" : val;
  }
  var note = document.getElementById("oauth-preset-note");
  var buttons = document.querySelectorAll(".oauth-preset");
  buttons.forEach(function (btn) {
    btn.addEventListener("click", function () {
      set("provider_id", btn.getAttribute("data-provider-id"));
      set("name", btn.getAttribute("data-name"));
      set("auth_url", btn.getAttribute("data-auth-url"));
      set("token_url", btn.getAttribute("data-token-url"));
      set("scopes", btn.getAttribute("data-scopes"));
      var en = form.querySelector('[name="enabled"]');
      if (en) en.checked = true;
      if (note) note.textContent = btn.getAttribute("data-note") || "";
      buttons.forEach(function (b) { b.classList.toggle("is-active", b === btn); });
      var cid = form.querySelector('[name="client_id"]');
      if (cid) { cid.focus(); cid.scrollIntoView({ block: "center", behavior: "smooth" }); }
    });
  });
})();
