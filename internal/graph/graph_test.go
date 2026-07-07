package graph

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sample() *Graph {
	g := New()
	g.AddNode(Node{ID: "workflow:welcome-customer", Kind: KindWorkflow, Label: "welcome-customer", Attrs: map[string]any{"status": "succeeded"}})
	g.AddNode(Node{ID: "credential:resend-api-key", Kind: KindCredential, Label: "resend-api-key", Attrs: map[string]any{"provider": "shared-secret"}})
	g.AddNode(Node{ID: "knowledge:h_timeout", Kind: KindKnowledge, Label: "Always pass context.Context with a timeout", Attrs: map[string]any{"topic": "http-clients", "gold": true}})
	g.AddEdge(Edge{From: "workflow:welcome-customer", To: "credential:resend-api-key", Kind: EdgeUses})
	g.AddEdge(Edge{From: "knowledge:h_timeout", To: "workflow:welcome-customer", Kind: EdgeCitedBy})
	return g
}

func TestNeighborsExpandsBothDirections(t *testing.T) {
	t.Parallel()
	g := sample()
	sub := g.Neighbors("workflow:welcome-customer", 1)
	wantNodes := map[string]bool{
		"workflow:welcome-customer": false,
		"credential:resend-api-key": false,
		"knowledge:h_timeout":       false,
	}
	for _, n := range sub.Nodes {
		if _, ok := wantNodes[n.ID]; ok {
			wantNodes[n.ID] = true
		}
	}
	for id, seen := range wantNodes {
		if !seen {
			t.Errorf("missing node %s in 1-hop neighborhood", id)
		}
	}
	if len(sub.Edges) != 2 {
		t.Fatalf("expected 2 edges, got %d", len(sub.Edges))
	}
}

func TestNeighborsRespectsEdgeKind(t *testing.T) {
	t.Parallel()
	g := sample()
	sub := g.Neighbors("workflow:welcome-customer", 1, EdgeUses)
	for _, e := range sub.Edges {
		if e.Kind != EdgeUses {
			t.Fatalf("edge kind filter leaked %q", e.Kind)
		}
	}
	// h_timeout is reachable only via CITED_BY which is filtered out, so it should not appear.
	for _, n := range sub.Nodes {
		if n.ID == "knowledge:h_timeout" {
			t.Fatal("h_timeout should not be in USES-only neighborhood")
		}
	}
}

func TestQueryRanksRelevantNodes(t *testing.T) {
	t.Parallel()
	g := sample()
	sub := g.Query("timeout context", 5)
	if len(sub.Nodes) == 0 {
		t.Fatal("expected at least one hit")
	}
	if sub.Nodes[0].ID != "knowledge:h_timeout" {
		t.Fatalf("expected knowledge:h_timeout as top hit, got %q", sub.Nodes[0].ID)
	}
}

func TestRemoveNodeDropsEdges(t *testing.T) {
	t.Parallel()
	g := sample()
	g.RemoveNode("credential:resend-api-key")
	if _, ok := g.Get("credential:resend-api-key"); ok {
		t.Fatal("node still present after RemoveNode")
	}
	if len(g.Outbound("workflow:welcome-customer", EdgeUses)) != 0 {
		t.Fatal("USES edge from workflow not removed when target dropped")
	}
}

func TestSerializeRoundTripsViaJSON(t *testing.T) {
	t.Parallel()
	g := sample()
	var buf bytes.Buffer
	if err := g.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	var sub Subgraph
	if err := json.Unmarshal(buf.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	if len(sub.Nodes) != 3 || len(sub.Edges) != 2 {
		t.Fatalf("serialized shape mismatch: %d nodes, %d edges", len(sub.Nodes), len(sub.Edges))
	}
}

func TestFormatSubgraphRendersClaudeShape(t *testing.T) {
	t.Parallel()
	g := sample()
	sub := g.Neighbors("workflow:welcome-customer", 1)
	out := FormatSubgraph(sub)
	if !strings.Contains(out, "NODE workflow:welcome-customer") {
		t.Errorf("missing node line: %q", out)
	}
	if !strings.Contains(out, "EDGE workflow:welcome-customer USES credential:resend-api-key") {
		t.Errorf("missing edge line: %q", out)
	}
	if !strings.Contains(out, "status=succeeded") {
		t.Errorf("missing attr in node line: %q", out)
	}
}

func TestStatsCountsByKind(t *testing.T) {
	t.Parallel()
	g := sample()
	s := g.Stats()
	if s["nodes_total"] != 3 {
		t.Errorf("nodes_total = %d, want 3", s["nodes_total"])
	}
	if s["nodes_workflow"] != 1 || s["nodes_credential"] != 1 || s["nodes_knowledge"] != 1 {
		t.Errorf("kind counts off: %+v", s)
	}
	if s["edges_total"] != 2 {
		t.Errorf("edges_total = %d, want 2", s["edges_total"])
	}
}
