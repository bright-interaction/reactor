// Theme toggle + nav dropdown behavior. Served from /assets (CSP blocks inline
// scripts). Applies the persisted light/dark choice before paint; with no
// choice the CSS prefers-color-scheme media query handles auto.
(function () {
  var KEY = 'reactor-theme';
  var root = document.documentElement;
  function sysDark() { return window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches; }
  function effective() {
    var s = localStorage.getItem(KEY);
    return (s === 'light' || s === 'dark') ? s : (sysDark() ? 'dark' : 'light');
  }
  function apply(t) {
    if (t === 'light' || t === 'dark') root.setAttribute('data-theme', t);
    else root.removeAttribute('data-theme');
  }
  // Apply the saved choice immediately (script is in <head>, before paint).
  var saved = localStorage.getItem(KEY);
  if (saved) apply(saved);

  document.addEventListener('DOMContentLoaded', function () {
    var btn = document.getElementById('theme-toggle');
    if (btn) {
      var sync = function () { btn.setAttribute('title', effective() === 'dark' ? 'Switch to light' : 'Switch to dark'); };
      sync();
      btn.addEventListener('click', function () {
        var next = effective() === 'dark' ? 'light' : 'dark';
        localStorage.setItem(KEY, next);
        apply(next);
        sync();
      });
    }
    // Close nav dropdowns on outside click or Escape.
    document.addEventListener('click', function (e) {
      document.querySelectorAll('details.navdrop[open]').forEach(function (d) {
        if (!d.contains(e.target)) d.removeAttribute('open');
      });
    });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') {
        document.querySelectorAll('details.navdrop[open]').forEach(function (d) { d.removeAttribute('open'); });
      }
    });
  });
})();
