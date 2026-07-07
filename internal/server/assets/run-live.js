// run-live.js: the live run view. Served from /assets (CSP blocks inline JS).
// While a run is executing it streams log lines over SSE from /runs/{id}/tail
// into the #run-log pane, and polls /runs/{id}/status to know when the run has
// finished, then re-renders the page so the final timeline + step outputs +
// terminal status show. No-op once the run is already terminal.
(function () {
  var el = document.getElementById("run-live");
  if (!el) return;
  var id = el.getAttribute("data-run-id");
  var status = el.getAttribute("data-status");
  if (!id) return;
  var terminal = { succeeded: 1, failed: 1, failed_dlq: 1, cancelled: 1 };
  if (terminal[status]) return; // nothing live to do

  var log = document.getElementById("run-log");

  // Live logs over SSE. The server replays the buffered history on each
  // connection, so clearing the pane on (re)connect keeps it idempotent: no
  // duplicate lines if the browser reconnects.
  var es = null;
  try {
    es = new EventSource("/runs/" + encodeURIComponent(id) + "/tail");
  } catch (e) { es = null; }
  if (es && log) {
    es.addEventListener("open", function () { log.textContent = ""; });
    es.onmessage = function (ev) {
      log.textContent += (log.textContent ? "\n" : "") + ev.data;
      log.scrollTop = log.scrollHeight;
    };
    // onerror: EventSource auto-reconnects; the status poll owns finalization.
  }

  // Finalize: poll the authoritative status. When the run is terminal, stop
  // streaming and reload so the server re-renders the final state.
  var done = false;
  function finish() {
    if (done) return;
    done = true;
    if (es) es.close();
    window.location.reload();
  }
  var poll = setInterval(function () {
    fetch("/runs/" + encodeURIComponent(id) + "/status", { headers: { Accept: "application/json" } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        if (d && d.terminal) {
          clearInterval(poll);
          finish();
        }
      })
      .catch(function () { /* transient; keep polling */ });
  }, 2000);
})();
