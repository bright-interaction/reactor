package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/brightinteraction/reactor/internal/runtime/journal"
)

// node_dataflow.go surfaces, for one node in the visual editor, the dataflow
// around it: which steps feed into it (upstream), which it feeds (downstream),
// and -- the part that makes this feel like n8n -- the ACTUAL output each
// upstream step produced on the workflow's most recent run. That lets a
// non-dev see the real shape of the data available to the node before they
// edit it, instead of guessing from the code.

// nodeConn is one connected step, optionally carrying the upstream step's last
// recorded output so the drawer can show real data.
type nodeConn struct {
	Name   string          `json:"name"`
	Kind   string          `json:"kind,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
	Status string          `json:"status,omitempty"`
}

// dagShape is the subset of dag.json we need to walk connections.
type dagShape struct {
	Steps []struct {
		Name      string   `json:"name"`
		Kind      string   `json:"kind"`
		DependsOn []string `json:"depends_on"`
	} `json:"steps"`
	Edges []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"edges"`
}

// dagConnections returns the upstream (feeds-in) and downstream (feeds-out)
// step names for step, derived from depends_on plus explicit edges. When a DAG
// declares neither for a step, callers fall back to source order.
func dagConnections(dagBytes []byte, step string) (upstream, downstream []nodeConn) {
	var d dagShape
	if err := json.Unmarshal(dagBytes, &d); err != nil {
		return nil, nil
	}
	kindOf := map[string]string{}
	for _, s := range d.Steps {
		kindOf[s.Name] = s.Kind
	}
	upSeen, downSeen := map[string]bool{}, map[string]bool{}
	addUp := func(name string) {
		if name == "" || name == step || upSeen[name] {
			return
		}
		upSeen[name] = true
		upstream = append(upstream, nodeConn{Name: name, Kind: kindOf[name]})
	}
	addDown := func(name string) {
		if name == "" || name == step || downSeen[name] {
			return
		}
		downSeen[name] = true
		downstream = append(downstream, nodeConn{Name: name, Kind: kindOf[name]})
	}
	for _, s := range d.Steps {
		if s.Name == step {
			for _, dep := range s.DependsOn {
				addUp(dep)
			}
		}
		for _, dep := range s.DependsOn {
			if dep == step {
				addDown(s.Name)
			}
		}
	}
	for _, e := range d.Edges {
		if e.To == step {
			addUp(e.From)
		}
		if e.From == step {
			addDown(e.To)
		}
	}
	return upstream, downstream
}

// latestStepOutputs returns a map of step name -> last recorded output (and
// status) from the workflow's most recent run, plus that run's id. Returns an
// empty map (not an error) when there is no journal, no run, or no steps yet --
// the editor degrades to showing connections without sample data.
func (s *Server) latestStepOutputs(ctx context.Context, slug string) (map[string]nodeConn, string) {
	out := map[string]nodeConn{}
	if s.Journal == nil {
		return out, ""
	}
	wfID, err := s.Journal.WorkflowIDBySlug(ctx, slug)
	if err != nil {
		return out, ""
	}
	runs, err := s.Journal.ListRuns(ctx, journal.RunFilter{WorkflowID: wfID, Limit: 1})
	if err != nil || len(runs) == 0 {
		return out, ""
	}
	runID := runs[0].ID
	steps, err := s.Journal.ListSteps(ctx, runID)
	if err != nil {
		return out, runID
	}
	// ListSteps is chronological; the last row for a step name wins so we show
	// the final attempt's output.
	for _, st := range steps {
		out[st.StepName] = nodeConn{Name: st.StepName, Output: st.OutputJSONB, Status: st.Status}
	}
	return out, runID
}

// nodeDataflow assembles the connections for step, enriching each upstream
// entry with sample output from the latest run when available.
func (s *Server) nodeDataflow(ctx context.Context, slug, dir, step string) (upstream, downstream []nodeConn, runID string) {
	dagBytes, _ := os.ReadFile(filepath.Join(dir, "dag.json"))
	upstream, downstream = dagConnections(dagBytes, step)
	outputs, runID := s.latestStepOutputs(ctx, slug)
	for i := range upstream {
		if o, ok := outputs[upstream[i].Name]; ok {
			upstream[i].Output = o.Output
			upstream[i].Status = o.Status
		}
	}
	return upstream, downstream, runID
}
