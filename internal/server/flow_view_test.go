package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

func TestRunFlowDiagram(t *testing.T) {
	t.Parallel()
	dag := []byte(`{"steps":[
		{"name":"fetch","kind":"step","uses":["http"]},
		{"name":"transform","kind":"step","depends_on":["fetch"]},
		{"name":"save","kind":"step","depends_on":["transform"]}
	]}`)
	start := time.Now().UTC()
	steps := []journal.StepRow{
		{StepName: "fetch", Attempt: 1, Status: "succeeded", OutputJSONB: json.RawMessage(`{"rows":3}`),
			StartedAt: start, FinishedAt: start.Add(120 * time.Millisecond)},
		{StepName: "transform", Attempt: 1, Status: "failed", ErrorText: "boom: bad row",
			StartedAt: start.Add(120 * time.Millisecond), FinishedAt: start.Add(130 * time.Millisecond)},
		// 'save' never ran -> pending.
	}

	html := runFlowDiagram(dag, steps)
	if html == "" {
		t.Fatal("expected flow HTML, got empty")
	}

	// Status overlay: succeeded / failed / pending classes present.
	for _, want := range []string{"flow-succeeded", "flow-failed", "flow-pending"} {
		if !strings.Contains(html, want) {
			t.Errorf("flow missing status class %s", want)
		}
	}
	// Data peek: the output data + the error are rendered (JSON is HTML-escaped
	// inside the <pre>, so match the key + value, not the raw quotes).
	if !strings.Contains(html, "rows") || !strings.Contains(html, "<pre>") {
		t.Error("flow should show fetch's output data (where data ends up)")
	}
	if !strings.Contains(html, "boom: bad row") {
		t.Error("flow should show the failed step's error")
	}
	// Lineage: transform shows it depends on fetch.
	if !strings.Contains(html, "from fetch") {
		t.Error("flow should show data lineage (from fetch)")
	}
	// Topological order: fetch before transform before save.
	fi, ti, si := strings.Index(html, "fetch"), strings.Index(html, "transform"), strings.Index(html, "save")
	if !(fi < ti && ti < si) {
		t.Fatalf("steps not in topological order: fetch=%d transform=%d save=%d", fi, ti, si)
	}
}

func TestRunFlowDiagramEmptyDAG(t *testing.T) {
	t.Parallel()
	if got := runFlowDiagram(nil, nil); got != "" {
		t.Fatalf("nil DAG should yield no diagram, got %q", got)
	}
	if got := runFlowDiagram([]byte(`{"steps":[]}`), nil); got != "" {
		t.Fatalf("empty DAG should yield no diagram, got %q", got)
	}
}
