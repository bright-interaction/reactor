// Package graph maintains an in-memory graph over Reactor's runtime state
// (workflows, credentials, triggers, recent runs, dlq items) and the
// knowledge corpus. The graph is the AI Environment Lens: a single MCP
// query returns the subgraph relevant to a brief, replacing the 5+
// roundtrips an AI would otherwise need to walk the database manually.
//
// The graph is rebuilt incrementally on writes (workflow CRUD, credential
// CRUD, knowledge add) and serialised to ~/.reactor/graph.json so external
// tools can consume it. JSON shape is self-describing; no graphify
// dependency.
package graph

import (
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Common node kinds. Defined as constants so MCP / dashboard / tests
// share the same vocabulary.
const (
	KindWorkflow    = "workflow"
	KindCredential  = "credential"
	KindTrigger     = "trigger"
	KindRun         = "run"
	KindDLQItem     = "dlq-item"
	KindKnowledge   = "knowledge"
	KindPostMortem  = "post-mortem"
)

// Common edge kinds.
const (
	EdgeUses        = "USES"        // workflow USES credential
	EdgeFires       = "FIRES"       // trigger FIRES workflow
	EdgeBelongsTo   = "BELONGS_TO"  // run BELONGS_TO workflow
	EdgeFrom        = "FROM"        // dlq-item FROM run
	EdgeDerivedFrom = "DERIVED_FROM" // post-mortem DERIVED_FROM run
	EdgeCitedBy     = "CITED_BY"    // knowledge CITED_BY workflow / run
	EdgeSupersedes  = "SUPERSEDES"  // knowledge SUPERSEDES knowledge
)

// Node is a single graph vertex. ID has the shape "<kind>:<local-id>"
// so subgraphs can be flattened without losing kind context. Attrs is a
// loose map so callers can stash domain-specific fields without
// forcing a schema migration.
type Node struct {
	ID    string         `json:"id"`
	Kind  string         `json:"kind"`
	Label string         `json:"label"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Edge is a directed link between two nodes. Kind identifies the
// relationship; Attrs carries optional metadata (e.g. created_at on a
// grant).
type Edge struct {
	From  string         `json:"from"`
	To    string         `json:"to"`
	Kind  string         `json:"kind"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Graph is the in-memory store. Safe for concurrent reads; write paths
// take the mu write side.
type Graph struct {
	mu    sync.RWMutex
	nodes map[string]Node
	out   map[string][]Edge // outbound edges by from-id
	in    map[string][]Edge // inbound  edges by to-id
}

// New returns an empty Graph.
func New() *Graph {
	return &Graph{
		nodes: map[string]Node{},
		out:   map[string][]Edge{},
		in:    map[string][]Edge{},
	}
}

// Subgraph is a self-contained slice of nodes + edges. Returned by
// Query and Neighbors. Suitable to JSON-marshal for an MCP tool reply.
type Subgraph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// AddNode inserts or replaces a node. Replacing keeps existing edges.
func (g *Graph) AddNode(n Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[n.ID] = n
}

// AddEdge inserts e. Caller is responsible for not duplicating; we
// don't dedupe because some edges legitimately repeat with different
// attrs (e.g. multiple CITED_BY edges from one knowledge node).
func (g *Graph) AddEdge(e Edge) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.out[e.From] = append(g.out[e.From], e)
	g.in[e.To] = append(g.in[e.To], e)
}

// RemoveNode drops the node and every edge touching it. Used by the
// builder when a workflow / credential / knowledge entry is deleted.
func (g *Graph) RemoveNode(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.nodes, id)
	for _, e := range g.out[id] {
		g.in[e.To] = removeEdge(g.in[e.To], e)
	}
	delete(g.out, id)
	for _, e := range g.in[id] {
		g.out[e.From] = removeEdge(g.out[e.From], e)
	}
	delete(g.in, id)
}

func removeEdge(edges []Edge, target Edge) []Edge {
	out := edges[:0]
	for _, e := range edges {
		if e.From == target.From && e.To == target.To && e.Kind == target.Kind {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Get returns a node by id and whether it was found.
func (g *Graph) Get(id string) (Node, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	n, ok := g.nodes[id]
	return n, ok
}

// Outbound returns edges whose From == id, optionally filtered by
// edgeKinds (empty = all).
func (g *Graph) Outbound(id string, edgeKinds ...string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return filterEdges(g.out[id], edgeKinds)
}

// Inbound returns edges whose To == id, optionally filtered.
func (g *Graph) Inbound(id string, edgeKinds ...string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return filterEdges(g.in[id], edgeKinds)
}

func filterEdges(in []Edge, kinds []string) []Edge {
	if len(kinds) == 0 {
		out := make([]Edge, len(in))
		copy(out, in)
		return out
	}
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	out := make([]Edge, 0, len(in))
	for _, e := range in {
		if want[e.Kind] {
			out = append(out, e)
		}
	}
	return out
}

// Neighbors returns the subgraph reachable from startID within depth
// steps, treating edges as undirected for the walk. edgeKinds (if any)
// constrain which edges count for the walk.
func (g *Graph) Neighbors(startID string, depth int, edgeKinds ...string) Subgraph {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if depth < 1 {
		depth = 1
	}
	visited := map[string]bool{startID: true}
	frontier := []string{startID}
	var sub Subgraph
	if root, ok := g.nodes[startID]; ok {
		sub.Nodes = append(sub.Nodes, root)
	} else {
		return sub
	}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, cur := range frontier {
			for _, e := range filterEdges(g.out[cur], edgeKinds) {
				sub.Edges = append(sub.Edges, e)
				if !visited[e.To] {
					visited[e.To] = true
					if n, ok := g.nodes[e.To]; ok {
						sub.Nodes = append(sub.Nodes, n)
					}
					next = append(next, e.To)
				}
			}
			for _, e := range filterEdges(g.in[cur], edgeKinds) {
				sub.Edges = append(sub.Edges, e)
				if !visited[e.From] {
					visited[e.From] = true
					if n, ok := g.nodes[e.From]; ok {
						sub.Nodes = append(sub.Nodes, n)
					}
					next = append(next, e.From)
				}
			}
		}
		frontier = next
	}
	return sub
}

// Query scores every node by BM25 over its label + attr-string-values
// against the query, returns the top-N as a Subgraph along with their
// 1-hop neighborhood. Uses the same tokeniser as the knowledge package
// so search ergonomics match.
func (g *Graph) Query(query string, limit int) Subgraph {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if limit <= 0 {
		limit = 10
	}
	hits := scoreNodes(g.nodes, query)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	visited := map[string]bool{}
	var sub Subgraph
	for _, h := range hits {
		if !visited[h.ID] {
			visited[h.ID] = true
			sub.Nodes = append(sub.Nodes, h)
		}
		for _, e := range g.out[h.ID] {
			sub.Edges = append(sub.Edges, e)
			if !visited[e.To] {
				visited[e.To] = true
				if n, ok := g.nodes[e.To]; ok {
					sub.Nodes = append(sub.Nodes, n)
				}
			}
		}
	}
	return sub
}

// Serialize writes the full graph as JSON to w. Format is the same
// {"nodes": [...], "edges": [...]} shape that Subgraph uses, so the
// dump and a query response are interchangeable for downstream tools.
func (g *Graph) Serialize(w io.Writer) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	all := Subgraph{
		Nodes: make([]Node, 0, len(g.nodes)),
	}
	for _, n := range g.nodes {
		all.Nodes = append(all.Nodes, n)
	}
	sort.SliceStable(all.Nodes, func(i, j int) bool {
		return all.Nodes[i].ID < all.Nodes[j].ID
	})
	for _, edges := range g.out {
		all.Edges = append(all.Edges, edges...)
	}
	sort.SliceStable(all.Edges, func(i, j int) bool {
		if all.Edges[i].From != all.Edges[j].From {
			return all.Edges[i].From < all.Edges[j].From
		}
		if all.Edges[i].To != all.Edges[j].To {
			return all.Edges[i].To < all.Edges[j].To
		}
		return all.Edges[i].Kind < all.Edges[j].Kind
	})
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(all)
}

// Stats returns counts per kind. Useful for the dashboard summary +
// MCP introspection.
func (g *Graph) Stats() map[string]int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := map[string]int{
		"nodes_total": len(g.nodes),
	}
	for _, n := range g.nodes {
		out["nodes_"+n.Kind]++
	}
	edgeCount := 0
	for _, edges := range g.out {
		edgeCount += len(edges)
	}
	out["edges_total"] = edgeCount
	return out
}

// FormatSubgraph renders a Subgraph in the Claude-friendly shape
// described in the plan:
//   NODE workflow:welcome-customer [status=succeeded ...]
//   EDGE workflow:foo USES credential:bar
// Lighter than JSON for inline prompt injection and easier to skim.
// promptSafe renders an untrusted string as a single-line, quoted Go literal
// so it cannot break out of the prompt region it is embedded in.
//
// Every value this function guards is writable by someone who is not the
// operator running codegen: node labels come from workflow slugs and CREDENTIAL
// NAMES, and attr values include dead-letter error_text, which is arbitrary
// free text chosen by whoever authored the failing workflow. Those strings were
// interpolated raw into the codegen prompt, whose output is compiled and
// executed on the host with no human approval step, and node selection is BM25
// over label+attrs against the brief, so stuffing likely brief terms into a
// long error_text reliably buys inclusion. A newline plus a fenced-block
// terminator was enough to append attacker instructions to the prompt.
//
// strconv.Quote handles the quote, backslash, backtick and newline cases in one
// pass; the length cap keeps one hostile node from crowding out the real slice.
func promptSafe(v string) string {
	const max = 300
	if len(v) > max {
		v = v[:max] + "...(truncated)"
	}
	return strconv.Quote(v)
}

func FormatSubgraph(s Subgraph) string {
	var b strings.Builder
	for _, n := range s.Nodes {
		b.WriteString("NODE ")
		b.WriteString(n.ID)
		if n.Label != "" && n.Label != n.ID {
			b.WriteString(" ")
			b.WriteString(promptSafe(n.Label))
		}
		if len(n.Attrs) > 0 {
			b.WriteString(" [")
			keys := make([]string, 0, len(n.Attrs))
			for k := range n.Attrs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for i, k := range keys {
				if i > 0 {
					b.WriteString(" ")
				}
				b.WriteString(k)
				b.WriteString("=")
				switch v := n.Attrs[k].(type) {
				case string:
					b.WriteString(promptSafe(v))
				default:
					b.WriteString(promptSafe(toString(v)))
				}
			}
			b.WriteString("]")
		}
		b.WriteString("\n")
	}
	for _, e := range s.Edges {
		b.WriteString("EDGE ")
		b.WriteString(e.From)
		b.WriteString(" ")
		b.WriteString(e.Kind)
		b.WriteString(" ")
		b.WriteString(e.To)
		b.WriteString("\n")
	}
	return b.String()
}

func toString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
