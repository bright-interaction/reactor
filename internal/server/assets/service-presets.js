// service-presets.js: one-click pre-fill of the new-credential form from the
// service catalog (HubSpot, Airtable, Notion, ...). Served from /assets (CSP
// blocks inline JS). The operator still pastes their own key; the preset fills
// the suggested vault key name + service + tells them where to get the key.
(function () {
  var form = document.getElementById("credential-form");
  if (!form) return;
  function set(name, val) {
    var el = form.querySelector('[name="' + name + '"]');
    if (el) el.value = val == null ? "" : val;
  }
  var note = document.getElementById("svc-preset-note");
  var buttons = document.querySelectorAll(".svc-preset");
  buttons.forEach(function (btn) {
    btn.addEventListener("click", function () {
      set("name", btn.getAttribute("data-key-name"));
      set("service", btn.getAttribute("data-service"));
      // API keys are static secrets; default the provider to manual (no
      // auto-rotate) so the row is created without a rotation schedule.
      var prov = form.querySelector('[name="provider"]');
      if (prov) prov.value = "manual";
      if (note) note.textContent = btn.getAttribute("data-hint") || "";
      buttons.forEach(function (b) { b.classList.toggle("is-active", b === btn); });
      var val = form.querySelector('[name="value"]');
      if (val) { val.focus(); val.scrollIntoView({ block: "center", behavior: "smooth" }); }
    });
  });
})();
