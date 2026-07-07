package server

import (
	"strings"
	"testing"
)

const dataflowDAG = `{
  "steps": [
    {"name": "fetch", "kind": "step"},
    {"name": "transform", "kind": "step", "depends_on": ["fetch"]},
    {"name": "notify", "kind": "step", "depends_on": ["transform"]},
    {"name": "log", "kind": "side_effect", "depends_on": ["transform"]}
  ],
  "edges": [
    {"from": "fetch", "to": "transform"},
    {"from": "transform", "to": "notify"},
    {"from": "transform", "to": "log"}
  ]
}`

func names(cs []nodeConn) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return strings.Join(out, ",")
}

func TestDagConnections_Middle(t *testing.T) {
	up, down := dagConnections([]byte(dataflowDAG), "transform")
	if names(up) != "fetch" {
		t.Fatalf("upstream = %q, want fetch", names(up))
	}
	if names(down) != "notify,log" {
		t.Fatalf("downstream = %q, want notify,log", names(down))
	}
}

func TestDagConnections_Source(t *testing.T) {
	up, down := dagConnections([]byte(dataflowDAG), "fetch")
	if len(up) != 0 {
		t.Fatalf("source node should have no upstream, got %q", names(up))
	}
	if names(down) != "transform" {
		t.Fatalf("downstream = %q, want transform", names(down))
	}
}

func TestDagConnections_KindCarried(t *testing.T) {
	up, _ := dagConnections([]byte(dataflowDAG), "notify")
	if len(up) != 1 || up[0].Name != "transform" || up[0].Kind != "step" {
		t.Fatalf("expected upstream transform(step), got %+v", up)
	}
}

func TestDagConnections_BadJSON(t *testing.T) {
	up, down := dagConnections([]byte("not json"), "x")
	if up != nil || down != nil {
		t.Fatalf("bad json should yield nil, nil")
	}
}
