package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DAGSchemaError is the structured error returned by ValidateDAG. The
// dashboard's POST handler surfaces these as a 422 with a machine-
// readable issue list so the editor UI can highlight problems inline.
type DAGSchemaError struct {
	Issues []DAGIssue
}

// DAGIssue pinpoints one validation failure. Path uses JSON-Pointer-ish
// shape ("/steps/3/kind") so a future inline-error UI can map it to a
// position in the textarea.
type DAGIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e *DAGSchemaError) Error() string {
	if len(e.Issues) == 0 {
		return "dag.json: validation failed"
	}
	parts := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		parts[i] = fmt.Sprintf("%s: %s", is.Path, is.Message)
	}
	return "dag.json: " + strings.Join(parts, "; ")
}

// allowedKinds mirrors the JSON Schema's enum. Kept package-local so a
// new kind requires a deliberate edit here + in the SDK + in the lint.
var allowedKinds = map[string]bool{
	"step":         true,
	"sleep":        true,
	"await_signal": true,
	"side_effect":  true,
}

var slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidateDAG checks src against the dag.json shape:
//   - Top-level object with required slug, version, steps
//   - slug matches ^[a-z][a-z0-9-]*$
//   - steps is a (possibly empty) array of {name, kind, depends_on?, idempotency_key?, timeout_seconds?}
//   - kind is one of step / sleep / await_signal / side_effect
//
// Returns *DAGSchemaError on validation failure with a non-empty Issues
// slice; returns a plain error on JSON parse failure.
func ValidateDAG(src []byte) error {
	if len(src) == 0 {
		return errors.New("dag.json: empty body")
	}
	var raw map[string]any
	if err := json.Unmarshal(src, &raw); err != nil {
		return fmt.Errorf("dag.json: parse: %w", err)
	}

	var issues []DAGIssue
	add := func(path, msg string) { issues = append(issues, DAGIssue{Path: path, Message: msg}) }

	slug, _ := raw["slug"].(string)
	if slug == "" {
		add("/slug", "required string")
	} else if !slugRe.MatchString(slug) {
		add("/slug", `must match ^[a-z][a-z0-9-]*$ (becomes a filesystem path)`)
	}
	if v, _ := raw["version"].(string); v == "" {
		add("/version", "required string")
	}

	stepsAny, hasSteps := raw["steps"]
	if !hasSteps {
		add("/steps", "required array")
	} else {
		steps, ok := stepsAny.([]any)
		if !ok {
			add("/steps", "must be an array")
		} else {
			seen := map[string]bool{}
			for i, raw := range steps {
				path := fmt.Sprintf("/steps/%d", i)
				step, ok := raw.(map[string]any)
				if !ok {
					add(path, "must be an object")
					continue
				}
				name, _ := step["name"].(string)
				if name == "" {
					add(path+"/name", "required string")
				} else if seen[name] {
					add(path+"/name", fmt.Sprintf("duplicate step name %q", name))
				} else {
					seen[name] = true
				}
				kind, _ := step["kind"].(string)
				if kind == "" {
					add(path+"/kind", "required string")
				} else if !allowedKinds[kind] {
					add(path+"/kind", fmt.Sprintf("must be one of step / sleep / await_signal / side_effect, got %q", kind))
				}
				if dep, ok := step["depends_on"]; ok {
					arr, ok := dep.([]any)
					if !ok {
						add(path+"/depends_on", "must be an array of strings")
					} else {
						for di, d := range arr {
							if _, ok := d.(string); !ok {
								add(fmt.Sprintf("%s/depends_on/%d", path, di), "must be a string")
							}
						}
					}
				}
				if idem, ok := step["idempotency_key"]; ok {
					if _, ok := idem.(string); !ok {
						add(path+"/idempotency_key", "must be a string")
					}
				}
				if to, ok := step["timeout_seconds"]; ok {
					if n, ok := to.(float64); !ok || n < 0 {
						add(path+"/timeout_seconds", "must be a non-negative number")
					}
				}
			}
		}
	}

	if len(issues) > 0 {
		return &DAGSchemaError{Issues: issues}
	}
	return nil
}
