package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/reactor/internal/migrate"
	"github.com/bright-interaction/reactor/internal/runtime/journal"
	"github.com/bright-interaction/reactor/internal/vault"
	_ "modernc.org/sqlite"
)

// buildTestWorkflow compiles the testworkflow binary into a tempdir and
// returns its path. Cached across tests via t.Cleanup.
func buildTestWorkflow(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "testworkflow")
	cmd := exec.Command("go", "build", "-o", out, "./testworkflow")
	cmd.Dir = "."
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build testworkflow: %v", err)
	}
	return out
}

func newTestSupervisorEnv(t *testing.T, runID string) (*Supervisor, *journal.Journal, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	url := "sqlite://" + dbPath

	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := migrate.Up(context.Background(), silent, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	j := journal.New(db, journal.EngineSQLite)
	if err := j.CreateWorkflow(context.Background(), "wf_test", "test-replay", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("create wf: %v", err)
	}
	if err := j.CreateRun(context.Background(), runID, "wf_test", "manual", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("create run: %v", err)
	}

	v, err := vault.NewStore(vault.NewMemoryBackend(), bytesN(32, 0xAB))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	if err := v.Put(context.Background(), "never-used", []byte("placeholder")); err != nil {
		t.Fatalf("vault put: %v", err)
	}

	logBuf := &slog.Logger{}
	_ = logBuf
	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))

	sup := &Supervisor{
		WorkflowSlug:     "test-replay",
		RunID:            runID,
		Mode:             "live",
		Journal:          j,
		Vault:            v,
		Log:              logger,
		SuspendThreshold: time.Second,
		// Durable-execution tests pre-date the per-workflow ACL; opt
		// them into the legacy empty-table-permissive mode so the
		// testworkflow's vault.MustGet("never-used") still resolves
		// without seeding a grant. The dedicated ACL tests in
		// acl_test.go cover the strict default.
		ACLPermissive: true,
	}
	return sup, j, func() { db.Close() }
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

func bytesN(n int, b byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// ffTestEnv builds an ExtraEnv slice for the testworkflow from key/value
// pairs. The supervisor's child-env allowlist (childEnv) deliberately
// strips the parent's environment so a workflow can't read the vault
// master key, so test instrumentation (FF_TEST_*) must be injected via
// ExtraEnv, its documented purpose, rather than os.Setenv. Empty values
// are skipped since os.Getenv can't distinguish unset from set-to-empty.
func ffTestEnv(kv ...string) []string {
	out := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			continue
		}
		out = append(out, kv[i]+"="+kv[i+1])
	}
	return out
}

// TestSupervisorHappyPath exercises the full pipe: spawn workflow, run two
// steps to completion, verify journal has both, run terminates succeeded.
func TestSupervisorHappyPath(t *testing.T) {
	binary := buildTestWorkflow(t)
	recordPath := filepath.Join(t.TempDir(), "calls.txt")

	sup, j, closeDB := newTestSupervisorEnv(t, "run_happy")
	defer closeDB()
	sup.BinaryPath = binary

	// Inject test instrumentation via ExtraEnv; the child-env allowlist
	// strips the parent process environment.
	sup.ExtraEnv = ffTestEnv("FF_TEST_RECORD", recordPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := sup.Run(ctx)
	if err != nil {
		t.Fatalf("supervisor run: %v", err)
	}
	if status != "succeeded" {
		t.Fatalf("status = %s, want succeeded", status)
	}
	verifyRecord(t, recordPath, []string{"fetch", "send"})

	// Journal should have both step rows succeeded.
	out, err := j.FindCachedOutput(context.Background(), "run_happy", "fetch", "k")
	if err != nil {
		t.Fatalf("fetch cache: %v", err)
	}
	if !strings.Contains(string(out), "fetched-customer") {
		t.Fatalf("fetch output = %s", out)
	}
	if _, err := j.FindCachedOutput(context.Background(), "run_happy", "send", "k"); err != nil {
		t.Fatalf("send cache: %v", err)
	}
}

// TestSupervisorReplayAfterCrash is the durable-execution proof: the
// workflow os.Exit(7)s during the fetch step (the SDK does NOT send
// step_end before the crash because os.Exit bypasses defers). The supervisor
// observes EOF and marks the run failed. We then re-run with the same
// run_id; the second supervisor reads the journal: fetch is "running" not
// "succeeded", so it re-executes (and fetch records a SECOND line). After
// the second run completes both steps, calls.txt contains:
//
//	fetch
//	fetch    <-- re-executed because previous attempt didn't reach step_end
//	send     <-- only ran once because fetch had to re-execute first
//
// This proves the journal correctly distinguishes incomplete-step from
// completed-step on restart.
//
// In week 4 we'll add idempotency-key short-circuit so the second fetch
// can be skipped if the SAME idempotency key was already recorded as
// running (i.e. attempt-deduplication), but the v0 contract is "running
// rows are inconclusive; re-execute on restart". The test asserts the
// per-step journal status moves to succeeded by the second run.
func TestSupervisorReplayAfterCrash(t *testing.T) {
	binary := buildTestWorkflow(t)
	recordPath := filepath.Join(t.TempDir(), "calls.txt")

	sup1, j, closeDB := newTestSupervisorEnv(t, "run_replay")
	defer closeDB()
	sup1.BinaryPath = binary

	// First run: crash inside fetch.
	sup1.ExtraEnv = ffTestEnv("FF_TEST_RECORD", recordPath, "FF_TEST_FAIL_AT", "fetch")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status1, _ := sup1.Run(ctx)
	if status1 != "failed" {
		t.Fatalf("first run status = %s, want failed", status1)
	}

	// Calls so far: only "fetch" (the closure ran, then os.Exit(7)).
	verifyRecord(t, recordPath, []string{"fetch"})

	// Second run: restart, this time without the crash flag.
	sup2 := *sup1 // share Journal + Vault
	sup2.ExtraEnv = ffTestEnv("FF_TEST_RECORD", recordPath)
	status2, err := sup2.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if status2 != "succeeded" {
		t.Fatalf("second run status = %s, want succeeded", status2)
	}

	// fetch ran twice (once in each subprocess); send ran once.
	verifyRecord(t, recordPath, []string{"fetch", "fetch", "send"})

	// Journal: fetch should now have a succeeded attempt; send too.
	if _, err := j.FindCachedOutput(context.Background(), "run_replay", "fetch", "k"); err != nil {
		t.Fatalf("fetch cache after restart: %v", err)
	}
	if _, err := j.FindCachedOutput(context.Background(), "run_replay", "send", "k"); err != nil {
		t.Fatalf("send cache after restart: %v", err)
	}
}

// TestSupervisorLongSleepSuspendAndResume is the week-4 proof: a workflow
// with a Sleep beyond the supervisor's SuspendThreshold is upgraded to a
// schedule row, the subprocess exits cleanly, the run is marked suspended.
// The scheduler then ticks with a clock past wake_at, re-spawns the run,
// the workflow replays fetch from journal, hits the Sleep frame again,
// the supervisor sees wait <= 0 and acks immediately, send executes, run
// succeeds.
//
// This is the test that turns "Sleep(72h)" from a placeholder into a real
// durable primitive. Without it, anything longer than SuspendThreshold
// would either pin a goroutine or fail.
func TestSupervisorLongSleepSuspendAndResume(t *testing.T) {
	binary := buildTestWorkflow(t)
	recordPath := filepath.Join(t.TempDir(), "calls.txt")

	sup, j, closeDB := newTestSupervisorEnv(t, "run_long_sleep")
	defer closeDB()
	sup.BinaryPath = binary
	sup.SuspendThreshold = 100 * time.Millisecond // anything past this suspends

	sup.ExtraEnv = ffTestEnv(
		"FF_TEST_RECORD", recordPath,
		"FF_TEST_SLEEP", "1h", // guaranteed to exceed the threshold
	)

	// First run: should suspend at the Sleep boundary.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status1, err := sup.Run(ctx)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if status1 != "suspended" {
		t.Fatalf("first run status = %s, want suspended", status1)
	}
	verifyRecord(t, recordPath, []string{"fetch", "sleep"}) // fetch ran, sleep
	// closure ran (it just records before calling flow.Sleep)

	// Scheduler ticks with a clock past wake_at.
	advanced := time.Now().Add(2 * time.Hour)
	resumed := *sup
	resumed.Now = func() time.Time { return advanced }
	sched := &Scheduler{
		Journal:            j,
		Vault:              sup.Vault,
		Log:                sup.Log,
		Now:                func() time.Time { return advanced },
		BinaryPath:         func(_ string) (string, error) { return binary, nil },
		TickInterval:       time.Hour, // doesn't matter; we call Tick directly
		Batch:              10,
		SupervisorTemplate: resumed,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("scheduler tick: %v", err)
	}

	// After scheduler tick, the run should be succeeded. Calls:
	//   fetch (run 1)
	//   sleep (run 1)
	//   fetch (run 2 replay path: closure NOT called because cache hit)  <- not recorded
	//   sleep (run 2 replay: closure IS called, then flow.Sleep returns ack instantly)
	//   send  (run 2 actual execution)
	verifyRecord(t, recordPath, []string{"fetch", "sleep", "sleep", "send"})

	// Both real steps cached.
	if _, err := j.FindCachedOutput(ctx, "run_long_sleep", "fetch", "k"); err != nil {
		t.Fatalf("fetch not cached: %v", err)
	}
	if _, err := j.FindCachedOutput(ctx, "run_long_sleep", "send", "k"); err != nil {
		t.Fatalf("send not cached: %v", err)
	}
}

// TestSupervisorReplayUsesCacheOnSecondRun verifies the strongest property:
// when a step DID succeed in run 1 and we re-run, that step's closure is
// NOT executed again, the journal output is replayed verbatim. This is the
// idempotency replay guarantee the durable-execution model promises.
func TestSupervisorReplayUsesCacheOnSecondRun(t *testing.T) {
	binary := buildTestWorkflow(t)
	recordPath := filepath.Join(t.TempDir(), "calls.txt")

	sup1, j, closeDB := newTestSupervisorEnv(t, "run_cached")
	defer closeDB()
	sup1.BinaryPath = binary

	// First run: complete both steps successfully then force a non-zero
	// exit AFTER both succeeded so we have a complete journal but the run
	// is marked failed. Restarting should replay both steps from cache
	// without invoking either closure.
	sup1.ExtraEnv = ffTestEnv("FF_TEST_RECORD", recordPath, "FF_TEST_FAIL_AFTER_SEND", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status1, _ := sup1.Run(ctx)
	if status1 != "failed" {
		t.Fatalf("first run status = %s, want failed (post-send return error)", status1)
	}
	verifyRecord(t, recordPath, []string{"fetch", "send"})

	// Drop the failure flag and restart with the same run_id.
	sup2 := *sup1
	sup2.ExtraEnv = ffTestEnv("FF_TEST_RECORD", recordPath)
	status2, err := sup2.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if status2 != "succeeded" {
		t.Fatalf("second run status = %s, want succeeded (cache replay)", status2)
	}

	// CRITICAL: calls file must STILL be just [fetch, send], because the
	// second run replayed both from the journal without invoking either
	// closure. This is the durable-execution promise.
	verifyRecord(t, recordPath, []string{"fetch", "send"})

	// Sanity: cached outputs are intact.
	out, err := j.FindCachedOutput(context.Background(), "run_cached", "fetch", "k")
	if err != nil || !strings.Contains(string(out), "fetched-customer") {
		t.Fatalf("fetch cache stale: out=%s err=%v", out, err)
	}
}

// TestSupervisorAwaitSignalSuspendAndResume drives the full week-7 signal
// flow: workflow blocks on AwaitSignal, supervisor persists the schedule
// + suspends, an external "delivery" via journal.FireSignal sets the
// payload, scheduler tick re-spawns the workflow, AwaitSignal returns the
// payload, send step executes, run completes succeeded.
func TestSupervisorAwaitSignalSuspendAndResume(t *testing.T) {
	binary := buildTestWorkflow(t)
	recordPath := filepath.Join(t.TempDir(), "calls.txt")

	sup, j, closeDB := newTestSupervisorEnv(t, "run_signal")
	defer closeDB()
	sup.BinaryPath = binary
	// Exercise the SECURE signal-token path: with a signing key wired, the
	// token is HMAC(perRunKey, name) delivered to the workflow over Hello, not
	// derivable from the (semi-public) runID alone.
	sup.SignalSigningKey = []byte("test-signal-signing-key-0123456789")

	sup.ExtraEnv = ffTestEnv("FF_TEST_RECORD", recordPath, "FF_TEST_AWAIT_SIGNAL", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First run: should suspend at the AwaitSignal boundary.
	status1, err := sup.Run(ctx)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if status1 != "suspended" {
		t.Fatalf("first run status = %s, want suspended", status1)
	}

	// The recorded await line includes the token; assert format and
	// confirm the supervisor wrote a matching schedule row.
	lines := readRecord(t, recordPath)
	if len(lines) < 2 || lines[0] != "fetch" || !strings.HasPrefix(lines[1], "await:sig_") {
		t.Fatalf("expected fetch + await:sig_..., got %v", lines)
	}
	token := strings.TrimPrefix(lines[1], "await:")
	perRunKey := perRunSignalKey(sup.SignalSigningKey, "run_signal")
	if expect := DeriveSignalToken(perRunKey, "run_signal", "approval"); token != expect {
		t.Fatalf("token mismatch: workflow=%s supervisor=%s", token, expect)
	}
	// The token must NOT be derivable from the (semi-public) runID alone.
	if legacy := DeriveSignalToken("", "run_signal", "approval"); token == legacy {
		t.Fatalf("signal token %s is forgeable from runID (HMAC signing not applied)", token)
	}

	sched, err := j.FindLatestSignalSchedule(ctx, "run_signal", "wait-approval")
	// Step name in the SDK defaults to signal name if the workflow side
	// passes name as both step name and signal name. Look up by what
	// pipeflow actually sends: step_name = signal_name = "approval".
	if err != nil {
		sched, err = j.FindLatestSignalSchedule(ctx, "run_signal", "approval")
	}
	if err != nil {
		t.Fatalf("schedule not persisted: %v", err)
	}
	if sched.SignalToken != token {
		t.Fatalf("schedule token = %s, want %s", sched.SignalToken, token)
	}

	// External delivery.
	if _, _, err := j.FireSignal(ctx, token, []byte(`{"approved":true}`)); err != nil {
		t.Fatalf("fire signal: %v", err)
	}

	// Scheduler tick re-spawns the workflow.
	resumed := *sup
	sched2 := &Scheduler{
		Journal:            j,
		Vault:              sup.Vault,
		Log:                sup.Log,
		BinaryPath:         func(_ string) (string, error) { return binary, nil },
		TickInterval:       time.Hour,
		Batch:              10,
		SupervisorTemplate: resumed,
		Now:                func() time.Time { return time.Now().Add(time.Second) },
	}
	if err := sched2.Tick(ctx); err != nil {
		t.Fatalf("scheduler tick: %v", err)
	}

	// After resume, send must have executed and the run must be succeeded.
	info, err := j.GetRun(ctx, "run_signal")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if info.Status != "succeeded" {
		t.Fatalf("run status = %s, want succeeded", info.Status)
	}

	// Recording shape: fetch, await, signal:{...}, send. Across the two
	// runs the await line appears twice (once per spawn) because the
	// workflow records BEFORE calling AwaitSignal; on resume the host
	// short-circuits AwaitSignal so the closure that runs the recordCall
	// is invoked again before the wire reply.
	finalLines := readRecord(t, recordPath)
	wantContains := []string{"fetch", `signal:{"approved":true}`, "send"}
	for _, w := range wantContains {
		found := false
		for _, l := range finalLines {
			if l == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %q in calls: %v", w, finalLines)
		}
	}
}

// TestSupervisorPermanentErrorMovesToDLQ proves the dead-letter path.
// The send step returns reactor.Permanent(err); the supervisor records
// the step as failed, writes a dead_letter row, and marks the run as
// failed_dlq instead of failed.
func TestSupervisorPermanentErrorMovesToDLQ(t *testing.T) {
	binary := buildTestWorkflow(t)
	recordPath := filepath.Join(t.TempDir(), "calls.txt")

	sup, j, closeDB := newTestSupervisorEnv(t, "run_dlq")
	defer closeDB()
	sup.BinaryPath = binary

	sup.ExtraEnv = ffTestEnv("FF_TEST_RECORD", recordPath, "FF_TEST_PERMANENT_AT", "send")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := sup.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if status != "failed_dlq" {
		t.Fatalf("status = %s, want failed_dlq", status)
	}

	items, err := j.ListDeadLetterItems(ctx, 10, 0)
	if err != nil {
		t.Fatalf("list dead-letter: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d dead_letter rows, want 1", len(items))
	}
	got := items[0]
	if got.RunID != "run_dlq" || got.StepName != "send" || !strings.Contains(got.ErrorText, "smtp permanent") {
		t.Fatalf("dead_letter row shape wrong: %+v", got)
	}

	info, err := j.GetRun(ctx, "run_dlq")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if info.Status != "failed_dlq" {
		t.Fatalf("run status = %s, want failed_dlq", info.Status)
	}
}

// readRecord returns the lines of the FF_TEST_RECORD file or nil when empty.
func readRecord(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	out := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(out) == 1 && out[0] == "" {
		return nil
	}
	return out
}

// verifyRecord reads the record file and asserts its lines match want.
func verifyRecord(t *testing.T, path string, want []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if len(lines) != len(want) {
		t.Fatalf("calls = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("call %d = %q, want %q (full=%v)", i, lines[i], want[i], lines)
		}
	}
}
