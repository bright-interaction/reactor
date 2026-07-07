package registry

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateDAGAcceptsCanonicalShape(t *testing.T) {
	t.Parallel()
	good := []byte(`{
		"slug": "send-welcome",
		"version": "0.1.0",
		"steps": [
			{"name":"fetch", "kind":"step", "timeout_seconds": 5},
			{"name":"sleep", "kind":"sleep", "depends_on":["fetch"]},
			{"name":"notify","kind":"step", "depends_on":["sleep"], "idempotency_key":"x:welcome"}
		]
	}`)
	if err := ValidateDAG(good); err != nil {
		t.Fatalf("expected canonical shape to validate: %v", err)
	}
}

func TestValidateDAGRejectsBadSlug(t *testing.T) {
	t.Parallel()
	bad := []byte(`{"slug": "../escape", "version": "0.1.0", "steps": []}`)
	err := ValidateDAG(bad)
	var dse *DAGSchemaError
	if !errors.As(err, &dse) {
		t.Fatalf("expected DAGSchemaError, got %v", err)
	}
	found := false
	for _, is := range dse.Issues {
		if is.Path == "/slug" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected slug issue, got %v", dse.Issues)
	}
}

func TestValidateDAGRejectsBadKind(t *testing.T) {
	t.Parallel()
	bad := []byte(`{"slug": "ok", "version": "0.1.0", "steps": [{"name":"a","kind":"explode"}]}`)
	err := ValidateDAG(bad)
	var dse *DAGSchemaError
	if !errors.As(err, &dse) {
		t.Fatalf("expected DAGSchemaError, got %v", err)
	}
}

func TestValidateDAGRejectsDuplicateStepName(t *testing.T) {
	t.Parallel()
	bad := []byte(`{"slug":"ok","version":"0.1.0","steps":[{"name":"a","kind":"step"},{"name":"a","kind":"step"}]}`)
	err := ValidateDAG(bad)
	var dse *DAGSchemaError
	if !errors.As(err, &dse) {
		t.Fatalf("expected DAGSchemaError, got %v", err)
	}
	if !strings.Contains(err.Error(), "duplicate step name") {
		t.Fatalf("expected duplicate-name error, got %v", err)
	}
}

func TestValidateDAGRejectsParseError(t *testing.T) {
	t.Parallel()
	if err := ValidateDAG([]byte("not json")); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestValidateDAGRejectsEmpty(t *testing.T) {
	t.Parallel()
	if err := ValidateDAG(nil); err == nil {
		t.Fatal("expected empty-body error")
	}
}
