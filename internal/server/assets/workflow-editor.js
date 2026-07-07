// workflow-editor.js: the n8n-style editor controller. Served from /assets so
// it satisfies CSP (script-src 'self'; no inline JS, no inline handlers).
//
// Two jobs:
//   1. Visual <-> Code view toggle on the workflow detail page.
//   2. Click a node in the visual DAG -> slide-in drawer on the right that
//      fetches just that node's Go source (GET .../node/{step}/code), lets the
//      operator edit it, and saves it back (POST same URL). The server splices
//      the snippet into the full file and runs the validator, so a bad edit is
//      rejected with the compiler's message shown inline in the drawer.
(function () {
    var editor = document.getElementById("wf-editor");
    if (!editor) return;

    var slug = editor.getAttribute("data-slug");
    var editable = editor.getAttribute("data-editable") === "true";

    // --- view toggle ---------------------------------------------------------
    var tabs = editor.querySelectorAll(".wf-tab");
    function showView(name) {
        editor.querySelectorAll(".wf-view").forEach(function (v) {
            v.hidden = v.getAttribute("data-view") !== name;
        });
        tabs.forEach(function (t) {
            t.classList.toggle("is-active", t.getAttribute("data-view") === name);
        });
    }
    tabs.forEach(function (t) {
        t.addEventListener("click", function () { showView(t.getAttribute("data-view")); });
    });

    // --- drawer --------------------------------------------------------------
    var drawer = document.getElementById("wf-drawer");
    var backdrop = document.getElementById("wf-drawer-backdrop");
    var title = document.getElementById("wf-drawer-title");
    var meta = document.getElementById("wf-drawer-meta");
    var dataflow = document.getElementById("wf-drawer-dataflow");
    var codeBox = document.getElementById("wf-drawer-code");
    var status = document.getElementById("wf-drawer-status");
    var applyBtn = document.getElementById("wf-drawer-apply");
    var current = null;

    function esc(s) {
        return String(s == null ? "" : s).replace(/[&<>"]/g, function (c) {
            return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
        });
    }
    function pretty(raw) {
        if (raw == null) return "";
        try { return JSON.stringify(typeof raw === "string" ? JSON.parse(raw) : raw, null, 2); }
        catch (e) { return String(raw); }
    }
    // insertAtCursor drops text into the code box where the caret is, so a
    // non-dev can reference an upstream step without retyping its name.
    function insertAtCursor(text) {
        if (codeBox.disabled) return;
        var s = codeBox.selectionStart, e = codeBox.selectionEnd;
        codeBox.value = codeBox.value.slice(0, s) + text + codeBox.value.slice(e);
        codeBox.selectionStart = codeBox.selectionEnd = s + text.length;
        codeBox.focus();
    }

    // renderDataflow draws the "available data" + "feeds into" panels: the n8n
    // move of showing what flows into a node (with real sample output from the
    // last run) so you can wire it without reading the whole program.
    function renderDataflow(data) {
        var up = data.upstream || [], down = data.downstream || [];
        var h = "";
        h += '<div class="wf-df-sec"><div class="wf-df-hdr">Available data <span class="muted">' +
            (up.length ? "from " + up.length + " upstream step" + (up.length > 1 ? "s" : "") : "") + "</span></div>";
        if (!up.length) {
            h += '<p class="wf-df-empty">This is a starting node. Its input is the trigger payload.</p>';
        } else {
            up.forEach(function (u) {
                var hasOut = u.output != null && u.output !== "null" && u.output !== "";
                h += '<div class="wf-df-node">';
                h += '<div class="wf-df-noderow"><button type="button" class="wf-df-chip" data-ref="' + esc(u.name) + '" title="Insert this step name at the cursor"><span class="wf-df-dot wf-k-' + esc(u.kind || "step") + '"></span>' + esc(u.name) + '</button>';
                if (u.status) h += '<span class="wf-df-status is-' + esc(u.status) + '">' + esc(u.status) + "</span>";
                h += "</div>";
                if (hasOut) {
                    h += '<details class="wf-df-data"><summary>sample output</summary><pre>' + esc(pretty(u.output)) + "</pre></details>";
                } else {
                    h += '<p class="wf-df-nodata muted">No sample yet. Run the workflow once to capture real data here.</p>';
                }
                h += "</div>";
            });
        }
        h += "</div>";
        if (down.length) {
            h += '<div class="wf-df-sec"><div class="wf-df-hdr">Feeds into</div><div class="wf-df-chips">';
            down.forEach(function (d) {
                h += '<span class="wf-df-chip is-static"><span class="wf-df-dot wf-k-' + esc(d.kind || "step") + '"></span>' + esc(d.name) + "</span>";
            });
            h += "</div></div>";
        }
        dataflow.innerHTML = h;
        dataflow.querySelectorAll(".wf-df-chip[data-ref]").forEach(function (chip) {
            chip.addEventListener("click", function () { insertAtCursor(chip.getAttribute("data-ref")); });
        });
    }

    function setStatus(msg, kind) {
        status.textContent = msg || "";
        status.className = "wf-drawer-status" + (kind ? " is-" + kind : "");
    }

    function openDrawer() {
        drawer.classList.add("is-open");
        drawer.setAttribute("aria-hidden", "false");
        if (backdrop) backdrop.hidden = false;
    }
    function closeDrawer() {
        drawer.classList.remove("is-open");
        drawer.setAttribute("aria-hidden", "true");
        if (backdrop) backdrop.hidden = true;
        current = null;
        document.dispatchEvent(new CustomEvent("reactor:drawer-close"));
    }

    function loadNode(step) {
        current = step;
        title.textContent = step;
        meta.textContent = "Loading this node...";
        codeBox.value = "";
        codeBox.disabled = true;
        applyBtn.disabled = true;
        if (dataflow) dataflow.innerHTML = "";
        setStatus("");
        openDrawer();

        fetch("/workflows/" + encodeURIComponent(slug) + "/node/" + encodeURIComponent(step) + "/code", {
            headers: { "Accept": "application/json" }
        }).then(function (r) {
            if (r.status === 404) {
                meta.textContent = "This node has no editable Go source (it may be a structural or trigger node). Use the Code view to edit the whole file.";
                return null;
            }
            if (!r.ok) { return r.text().then(function (t) { throw new Error(t || ("HTTP " + r.status)); }); }
            return r.json();
        }).then(function (data) {
            if (!data) return;
            codeBox.value = data.code || "";
            if (dataflow) renderDataflow(data);
            meta.textContent = "From " + (data.source || "main.go") + ". Edits splice back into the full workflow and re-run go vet + lint + build.";
            if (editable && data.editable) {
                codeBox.disabled = false;
                applyBtn.disabled = false;
            } else {
                codeBox.disabled = true;
                applyBtn.disabled = true;
                meta.textContent = "Read-only: the code validator is not wired on this instance.";
            }
        }).catch(function (err) {
            meta.textContent = "Could not load this node.";
            setStatus(String(err.message || err), "err");
        });
    }

    function applyNode() {
        if (!current) return;
        applyBtn.disabled = true;
        setStatus("Validating + rebuilding...", "busy");
        var form = new URLSearchParams();
        form.set("body", codeBox.value);
        fetch("/workflows/" + encodeURIComponent(slug) + "/node/" + encodeURIComponent(current) + "/code", {
            method: "POST",
            headers: { "Content-Type": "application/x-www-form-urlencoded" },
            body: form.toString()
        }).then(function (r) {
            if (r.ok) {
                setStatus("Saved. Reloading...", "ok");
                window.location.reload();
                return;
            }
            return r.text().then(function (t) {
                applyBtn.disabled = false;
                setStatus(t || ("HTTP " + r.status), "err");
            });
        }).catch(function (err) {
            applyBtn.disabled = false;
            setStatus(String(err.message || err), "err");
        });
    }

    document.addEventListener("reactor:node-tap", function (e) {
        if (e.detail && e.detail.id) loadNode(e.detail.id);
    });

    var closeBtn = document.getElementById("wf-drawer-close");
    var cancelBtn = document.getElementById("wf-drawer-cancel");
    if (closeBtn) closeBtn.addEventListener("click", closeDrawer);
    if (cancelBtn) cancelBtn.addEventListener("click", closeDrawer);
    if (backdrop) backdrop.addEventListener("click", closeDrawer);
    if (applyBtn) applyBtn.addEventListener("click", applyNode);
    document.addEventListener("keydown", function (e) {
        if (e.key === "Escape" && drawer.classList.contains("is-open")) closeDrawer();
    });

    // Soft tab support in the code box so editing stays comfortable.
    if (codeBox) {
        codeBox.addEventListener("keydown", function (e) {
            if (e.key === "Tab") {
                e.preventDefault();
                var s = codeBox.selectionStart, en = codeBox.selectionEnd;
                codeBox.value = codeBox.value.slice(0, s) + "\t" + codeBox.value.slice(en);
                codeBox.selectionStart = codeBox.selectionEnd = s + 1;
            }
        });
    }
})();
