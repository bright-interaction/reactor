package supervisor

import (
	"os"
	"strings"
	"testing"
)

// TestChildEnvWithholdsSecrets is the regression test for the ship-blocker
// where a workflow subprocess inherited the daemon's full environment and
// could read REACTOR_MASTER_KEY + REACTOR_DB_URL to decrypt the whole
// vault. The child must get the system allowlist + input + extra only.
func TestChildEnvWithholdsSecrets(t *testing.T) {
	t.Setenv("REACTOR_MASTER_KEY", "super-secret-key")
	t.Setenv("REACTOR_DB_URL", "postgres://user:pw@db/reactor")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-xxx")
	t.Setenv("PATH", "/usr/bin:/bin")

	env := childEnv([]byte(`{"x":1}`), []string{"FF_TEST_RECORD=/tmp/rec"})

	has := func(prefix string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, prefix) {
				return true
			}
		}
		return false
	}

	for _, leaked := range []string{"REACTOR_MASTER_KEY=", "REACTOR_DB_URL=", "ANTHROPIC_API_KEY="} {
		if has(leaked) {
			t.Errorf("child env leaked secret %q", leaked)
		}
	}
	if !has("PATH=") {
		t.Error("child env dropped PATH (allowlisted system var)")
	}
	if !has("REACTOR_INPUT={") {
		t.Error("child env missing REACTOR_INPUT")
	}
	if !has("FF_TEST_RECORD=") {
		t.Error("child env missing injected ExtraEnv")
	}
}

// TestChildEnvOmitsEmptyInput verifies the input var is only added when
// non-empty so the workflow's LookupEnv("REACTOR_INPUT") path stays
// well-defined.
func TestChildEnvOmitsEmptyInput(t *testing.T) {
	env := childEnv(nil, nil)
	for _, kv := range env {
		if strings.HasPrefix(kv, "REACTOR_INPUT=") {
			t.Fatalf("expected no REACTOR_INPUT for empty input, got %q", kv)
		}
	}
	_ = os.Environ
}
