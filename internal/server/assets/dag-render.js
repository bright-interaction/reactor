// dag-render.js: reads the dag.json shape from a <script type="application/json"
// id="dag-data"> island and renders it into the #dag-canvas div via cytoscape.
// Layout: top-down breadthfirst so workflow steps read like a vertical pipeline.
//
// Step kinds get distinct colours so the DAG conveys at-a-glance shape:
//   step          -> green
//   sleep         -> grey
//   await_signal  -> amber
//   side_effect   -> blue
//
// Clicking a node dispatches an "reactor:node-tap" event (detail.id = the step
// name) that workflow-editor.js listens for to open the code-editing drawer. We
// keep the two scripts decoupled via the event rather than sharing globals.
(function () {
    var data = document.getElementById("dag-data");
    var canvas = document.getElementById("dag-canvas");
    if (!data || !canvas || typeof cytoscape !== "function") return;

    var dag;
    try { dag = JSON.parse(data.textContent || "{}"); } catch (e) { return; }
    var steps = (dag.steps || dag.nodes || []);
    if (!steps.length) return;

    function nodeId(s, i) { return s.name || s.id || ("n" + i); }

    var nodes = steps.map(function (s, i) {
        return {
            data: {
                id: nodeId(s, i),
                label: (s.name || s.id || "(unnamed)") + "\n" + (s.kind || "step"),
                kind: s.kind || "step"
            }
        };
    });

    var edges = [];
    var seen = {};
    steps.forEach(function (s, i) { seen[nodeId(s, i)] = true; });
    steps.forEach(function (s, i) {
        (s.depends_on || []).forEach(function (d) {
            if (seen[d]) edges.push({ data: { source: d, target: nodeId(s, i) } });
        });
    });
    // If no explicit depends_on, draw a linear chain so the layout has structure.
    if (edges.length === 0 && steps.length > 1) {
        for (var i = 1; i < steps.length; i++) {
            edges.push({ data: { source: nodeId(steps[i - 1], i - 1), target: nodeId(steps[i], i) } });
        }
    }

    var color = {
        step: "#15803D",
        sleep: "#A3A3A3",
        await_signal: "#D97706",
        side_effect: "#4F46E5"
    };

    // Pull a couple of colours from the live theme so the graph matches light
    // and dark mode at render time.
    var css = getComputedStyle(document.documentElement);
    function v(name, fb) { return (css.getPropertyValue(name) || "").trim() || fb; }
    var accent = v("--accent", "#171717");
    var edgeColor = v("--border-strong", "#D4D4D4");

    var cy = cytoscape({
        container: canvas,
        elements: { nodes: nodes, edges: edges },
        layout: { name: "breadthfirst", directed: true, padding: 16, spacingFactor: 1.4 },
        style: [
            {
                selector: "node",
                style: {
                    "background-color": function (ele) { return color[ele.data("kind")] || color.step; },
                    "label": "data(label)",
                    "text-wrap": "wrap",
                    "text-valign": "center",
                    "text-halign": "center",
                    "color": "#fff",
                    "font-size": 11,
                    "font-family": "ui-monospace, SFMono-Regular, monospace",
                    "width": 116,
                    "height": 52,
                    "shape": "round-rectangle",
                    "border-width": 0
                }
            },
            {
                selector: "node.wf-selected",
                style: { "border-width": 3, "border-color": accent }
            },
            {
                selector: "edge",
                style: {
                    "width": 2,
                    "line-color": edgeColor,
                    "target-arrow-color": edgeColor,
                    "target-arrow-shape": "triangle",
                    "curve-style": "bezier"
                }
            }
        ],
        wheelSensitivity: 0.2
    });

    window.__reactorCy = cy;

    // Hover affordance + click-to-edit.
    cy.on("mouseover", "node", function () { canvas.style.cursor = "pointer"; });
    cy.on("mouseout", "node", function () { canvas.style.cursor = "default"; });
    cy.on("tap", "node", function (evt) {
        cy.nodes().removeClass("wf-selected");
        evt.target.addClass("wf-selected");
        document.dispatchEvent(new CustomEvent("reactor:node-tap", {
            detail: { id: evt.target.id() }
        }));
    });

    // Let the drawer clear the selection highlight when it closes.
    document.addEventListener("reactor:drawer-close", function () {
        cy.nodes().removeClass("wf-selected");
    });
})();
