package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strings"

	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// This file renders a run's execution as a flow diagram: the workflow's step
// graph (from dag.json) laid out top-to-bottom in topological order, each node
// coloured by the run's actual per-step status, with the duration and an
// expandable peek of the data that step produced (output_jsonb). It is
// dependency-free (server-rendered HTML + the dashboard's own CSS) and shows
// "how it ran, where the data ended up, and how it flowed".

// flowNode is one step in the diagram, after overlaying run data on the DAG.
type flowNode struct {
	Name      string
	Kind      string
	Uses      []string
	DependsOn []string
	Status    string // succeeded | failed | running | suspended | cancelled | pending
	Output    json.RawMessage
	Error     string
	DurMS     int64
	level     int
}

// runFlowDiagram builds the flow HTML for a run. Returns "" when the DAG can't
// be parsed or has no steps, so the caller can fall back to the plain timeline.
func runFlowDiagram(dagJSON []byte, steps []journal.StepRow) string {
	nodes := parseDAGNodes(dagJSON)
	if len(nodes) == 0 {
		return ""
	}
	overlayRunStatus(nodes, steps)
	assignLevels(nodes)

	// Group by level, stable order within a level.
	maxLevel := 0
	for _, n := range nodes {
		if n.level > maxLevel {
			maxLevel = n.level
		}
	}
	levels := make([][]*flowNode, maxLevel+1)
	order := make([]*flowNode, 0, len(nodes))
	for _, n := range nodes {
		order = append(order, n)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].Name < order[j].Name })
	for _, n := range order {
		levels[n.level] = append(levels[n.level], n)
	}

	var b strings.Builder
	b.WriteString(`<h2>Flow</h2>`)
	b.WriteString(flowSummary(order))
	b.WriteString(`<div class="flow">`)
	for li, row := range levels {
		if len(row) == 0 {
			continue
		}
		b.WriteString(`<div class="flow-row">`)
		for _, n := range row {
			b.WriteString(renderFlowNode(n))
		}
		b.WriteString(`</div>`)
		if li < len(levels)-1 {
			b.WriteString(`<div class="flow-conn"></div>`)
		}
	}
	b.WriteString(`</div>`)
	return b.String()
}

func flowSummary(nodes []*flowNode) string {
	var ran, ok, failed int
	var totalMS int64
	for _, n := range nodes {
		if n.Status != "pending" {
			ran++
		}
		switch n.Status {
		case "succeeded":
			ok++
		case "failed", "failed_dlq":
			failed++
		}
		totalMS += n.DurMS
	}
	return fmt.Sprintf(`<p class="muted">%d steps &middot; %d succeeded &middot; %d failed &middot; %d not run &middot; %s active compute</p>`,
		len(nodes), ok, failed, len(nodes)-ran, fmtDurMS(totalMS))
}

func renderFlowNode(n *flowNode) string {
	status := n.Status
	if status == "" {
		status = "pending"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="flow-node flow-%s">`, template.HTMLEscapeString(status))
	b.WriteString(`<div class="flow-node-body">`)

	dur := ""
	if n.DurMS > 0 {
		dur = `<span class="flow-dur">` + fmtDurMS(n.DurMS) + `</span>`
	}
	fmt.Fprintf(&b, `<div class="flow-node-head"><span class="flow-name">%s</span><span class="flow-status">%s%s</span></div>`,
		template.HTMLEscapeString(n.Name), dur, template.HTMLEscapeString(status))

	// Meta: kind, what it touches, and where its inputs come from (lineage).
	var meta []string
	if n.Kind != "" {
		meta = append(meta, template.HTMLEscapeString(n.Kind))
	}
	if len(n.Uses) > 0 {
		meta = append(meta, "uses "+template.HTMLEscapeString(strings.Join(n.Uses, ", ")))
	}
	if len(n.DependsOn) > 0 {
		meta = append(meta, "from "+template.HTMLEscapeString(strings.Join(n.DependsOn, ", ")))
	}
	if len(meta) > 0 {
		b.WriteString(`<div class="flow-meta">` + strings.Join(meta, " &middot; ") + `</div>`)
	}

	// Data peek: where the data ends up. Failed steps show the error instead.
	switch {
	case n.Error != "":
		b.WriteString(`<details class="flow-data"><summary>error</summary><pre class="flow-err">` +
			template.HTMLEscapeString(truncate(n.Error, 1200)) + `</pre></details>`)
	case len(n.Output) > 0 && string(n.Output) != "null":
		b.WriteString(`<details class="flow-data"><summary>output</summary><pre>` +
			template.HTMLEscapeString(truncate(prettyJSON(string(n.Output)), 4000)) + `</pre></details>`)
	case status == "pending":
		b.WriteString(`<div class="flow-meta muted">not run</div>`)
	default:
		b.WriteString(`<div class="flow-meta muted">no output</div>`)
	}

	b.WriteString(`</div></div>`)
	return b.String()
}

// parseDAGNodes reads the dag.json into flow nodes, tolerating both the
// steps[]+depends_on shape and the nodes[]/edges[] shape.
func parseDAGNodes(src []byte) map[string]*flowNode {
	var dag struct {
		Steps []struct {
			Name      string   `json:"name"`
			Kind      string   `json:"kind"`
			DependsOn []string `json:"depends_on"`
			Uses      []string `json:"uses"`
		} `json:"steps"`
		Nodes []struct {
			ID    string   `json:"id"`
			Name  string   `json:"name"`
			Kind  string   `json:"kind"`
			Label string   `json:"label"`
			Uses  []string `json:"uses"`
		} `json:"nodes"`
		Edges []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"edges"`
	}
	if len(src) == 0 || json.Unmarshal(src, &dag) != nil {
		return nil
	}
	nodes := map[string]*flowNode{}
	for _, s := range dag.Steps {
		if s.Name == "" {
			continue
		}
		nodes[s.Name] = &flowNode{Name: s.Name, Kind: s.Kind, Uses: s.Uses, DependsOn: s.DependsOn}
	}
	for _, n := range dag.Nodes {
		name := n.ID
		if name == "" {
			name = n.Name
		}
		if name == "" {
			continue
		}
		if _, ok := nodes[name]; !ok {
			kind := n.Kind
			if n.Label != "" && kind == "" {
				kind = n.Label
			}
			nodes[name] = &flowNode{Name: name, Kind: kind, Uses: n.Uses}
		}
	}
	// edges[] add to the target's depends_on.
	for _, e := range dag.Edges {
		if e.To == "" || e.From == "" {
			continue
		}
		if nodes[e.To] == nil {
			nodes[e.To] = &flowNode{Name: e.To}
		}
		nodes[e.To].DependsOn = appendUnique(nodes[e.To].DependsOn, e.From)
	}
	return nodes
}

// overlayRunStatus stamps each node with the run's actual per-step result (the
// latest attempt wins). Nodes with no matching step row stay "pending".
func overlayRunStatus(nodes map[string]*flowNode, steps []journal.StepRow) {
	for i := range steps {
		s := steps[i]
		n := nodes[s.StepName]
		if n == nil {
			// A step the DAG didn't declare (dynamic step): add it so it shows.
			n = &flowNode{Name: s.StepName}
			nodes[s.StepName] = n
		}
		n.Status = s.Status
		n.Output = s.OutputJSONB
		n.Error = s.ErrorText
		if !s.StartedAt.IsZero() && !s.FinishedAt.IsZero() {
			n.DurMS = s.FinishedAt.Sub(s.StartedAt).Milliseconds()
		}
	}
	for _, n := range nodes {
		if n.Status == "" {
			n.Status = "pending"
		}
	}
}

// assignLevels computes each node's topological depth (longest path from a
// root) for the row layout. Cycles (shouldn't happen in a DAG) are bounded by
// the node count so this always terminates.
func assignLevels(nodes map[string]*flowNode) {
	var depth func(name string, seen map[string]bool) int
	depth = func(name string, seen map[string]bool) int {
		n := nodes[name]
		if n == nil || len(n.DependsOn) == 0 {
			return 0
		}
		if seen[name] || len(seen) > len(nodes) {
			return 0
		}
		seen[name] = true
		max := 0
		for _, dep := range n.DependsOn {
			if d := depth(dep, seen) + 1; d > max {
				max = d
			}
		}
		delete(seen, name)
		return max
	}
	for name, n := range nodes {
		n.level = depth(name, map[string]bool{})
	}
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func fmtDurMS(ms int64) string {
	switch {
	case ms <= 0:
		return "0ms"
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%.1fm", float64(ms)/60000)
	}
}
