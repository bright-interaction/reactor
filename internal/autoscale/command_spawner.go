package autoscale

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// idPlaceholder is replaced with the worker id in every Stop argv token.
const idPlaceholder = "{id}"

// CommandSpawner starts and stops workers by running operator-provided
// commands -- `docker run`, `kubectl create`, `nomad job run`, anything that
// launches one `reactor worker`. This is the pluggable substrate for EXTERNAL
// orchestration: Reactor deliberately does NOT link the Docker SDK or
// Kubernetes client-go. It shells out to the operator's own CLI, so one
// mechanism covers every cluster manager, adds zero dependencies, and never
// pins the operator to a specific client version. The same controller that
// drives same-host processes drives this unchanged -- it only ever calls
// Spawn/Stop/Running.
//
// The spawn command's stdout (trimmed, first line) becomes the worker id:
// `docker run -d` prints the container id, `kubectl create -o name` prints
// `job.batch/<name>`. That id is substituted for {id} in the stop argv.
// Workers started this way are detached (not child processes), so unlike the
// ProcessSpawner there is no exit reaper: Running() reflects spawn-minus-stop.
// If a container dies on its own, its lease expires, the queue stays deep, and
// the controller spawns a replacement on the next tick -- the drift
// self-corrects through the queue signal.
type CommandSpawner struct {
	// SpawnArgv is the command that starts one worker. Its stdout (trimmed)
	// is taken as the worker id. Must be non-empty.
	SpawnArgv []string
	// SpawnStdin, if set, is fed to the spawn command's stdin. Used by the
	// Kubernetes preset to pipe a Job manifest to `kubectl create -f -`.
	SpawnStdin []byte
	// StopArgv is the command that stops a worker. Every token equal to
	// "{id}" is replaced with the worker id. If empty, Stop just drops the
	// worker from tracking (best effort).
	StopArgv []string
	// Env is the environment for both commands (nil = inherit the parent's).
	Env []string
	// Timeout bounds each spawn/stop command (default 30s).
	Timeout time.Duration
	Log     *slog.Logger

	mu  sync.Mutex
	ids []string // tracked worker ids, newest last
	seq int
}

// NewCommandSpawner builds a command-driven spawner. spawnArgv is required;
// stopArgv may be nil (Stop then only untracks).
func NewCommandSpawner(spawnArgv, stopArgv, env []string, log *slog.Logger) *CommandSpawner {
	if log == nil {
		log = slog.Default()
	}
	return &CommandSpawner{SpawnArgv: spawnArgv, StopArgv: stopArgv, Env: env, Log: log}
}

func (c *CommandSpawner) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 30 * time.Second
}

// Spawn runs the spawn command and tracks the worker by the id it prints.
func (c *CommandSpawner) Spawn(ctx context.Context) (string, error) {
	if len(c.SpawnArgv) == 0 {
		return "", fmt.Errorf("autoscale: CommandSpawner has no spawn command")
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	cmd := exec.CommandContext(cctx, c.SpawnArgv[0], c.SpawnArgv[1:]...)
	if c.Env != nil {
		cmd.Env = c.Env
	}
	if len(c.SpawnStdin) > 0 {
		cmd.Stdin = bytes.NewReader(c.SpawnStdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("autoscale: spawn command failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	c.mu.Lock()
	c.seq++
	id := firstLine(stdout.String())
	if id == "" {
		// Some launchers print nothing useful; synthesize a handle so the
		// controller can still count it. Stop then only untracks.
		id = fmt.Sprintf("cmd-%d", c.seq)
	}
	c.ids = append(c.ids, id)
	c.mu.Unlock()
	c.Log.Info("autoscale: spawned worker", "id", id)
	return id, nil
}

// Stop runs the stop command for id and, on success, drops it from tracking.
// On failure the worker stays tracked so the next tick retries rather than
// silently leaking it.
func (c *CommandSpawner) Stop(ctx context.Context, id string) error {
	if len(c.StopArgv) == 0 {
		c.untrack(id)
		return nil
	}
	argv := substituteID(c.StopArgv, id)
	cctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	if c.Env != nil {
		cmd.Env = c.Env
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	c.Log.Info("autoscale: stopping worker", "id", id)
	if err := cmd.Run(); err != nil {
		c.Log.Warn("autoscale: stop command failed (kept for retry)", "id", id, "err", err, "stderr", strings.TrimSpace(stderr.String()))
		return fmt.Errorf("autoscale: stop command failed: %w", err)
	}
	c.untrack(id)
	return nil
}

// Running returns the number of tracked workers.
func (c *CommandSpawner) Running() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ids)
}

// IDs returns the tracked worker ids, newest last.
func (c *CommandSpawner) IDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.ids))
	copy(out, c.ids)
	return out
}

// StopAll stops every tracked worker.
func (c *CommandSpawner) StopAll(ctx context.Context) {
	for _, id := range c.IDs() {
		_ = c.Stop(ctx, id)
	}
}

func (c *CommandSpawner) untrack(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, v := range c.ids {
		if v == id {
			c.ids = append(c.ids[:i], c.ids[i+1:]...)
			return
		}
	}
}

// substituteID returns a copy of argv with every "{id}" token replaced by id.
func substituteID(argv []string, id string) []string {
	out := make([]string, len(argv))
	for i, tok := range argv {
		out[i] = strings.ReplaceAll(tok, idPlaceholder, id)
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
