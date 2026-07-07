// cron-echo is the smallest workflow that demonstrates a complete
// Reactor run end-to-end:
//
//   1. Daemon receives a trigger (webhook or cron) for this slug.
//   2. Supervisor exec's this binary.
//   3. runtime.Serve handshakes with the host, decodes the trigger
//      payload, calls Run.
//   4. Run executes one Step ("echo") that emits a Logger line and
//      returns a small struct.
//   5. Supervisor records the journal, marks the run succeeded.
//
// Build: reactor workflow build --src examples/cron-echo --slug cron-echo
// Register: reactor workflow register --db ... --slug cron-echo
//
// Trigger via webhook (after seeding a webhook trigger row):
//   curl -X POST http://127.0.0.1:7777/webhook/<token> \
//        -H "X-Webhook-Signature: sha256=<hmac>" \
//        -d '{"hello":"world"}'
package main

import (
	"context"
	"time"

	"github.com/brightinteraction/reactor/sdk"
	"github.com/brightinteraction/reactor/sdk/runtime"
)

// Workflow + Trigger are read by `reactor workflow register` to
// stamp the workflows row. The trigger here is a generic webhook so
// every install can demo it without needing a Stripe / GitHub account.
//
//nolint:gochecknoglobals // SDK contract: package-level declarations.
var (
	Workflow = reactor.Workflow{Slug: "cron-echo", Version: "0.1.0"}
	Trigger  = reactor.WebhookTrigger{
		Path:     "/webhook/cron-echo",
		Provider: "generic",
	}
)

// Payload is the typed trigger body. The runtime JSON-decodes the
// inbound webhook into this; consumers can put anything in the payload.
type Payload struct {
	Hello string `json:"hello"`
}

// Echo is the step output. Tiny because it lands in the journal as a
// JSON blob and we want the home-page table to read cleanly.
type Echo struct {
	Got     string `json:"got"`
	StepRun int64  `json:"step_run_unix"`
}

// Run is the workflow entrypoint. Demonstrates two SDK primitives:
// SideEffect for journaling a non-deterministic value (the wall-clock
// time) so replay is deterministic, and Step for the side-effecting
// "send" call with idempotency + timeout.
func Run(ctx context.Context, flow reactor.Flow, in Payload) error {
	log := flow.Logger()
	log.Info("cron-echo: starting", "hello", in.Hello)

	got := in.Hello
	if got == "" {
		got = "(empty)"
	}

	// Capture the wall-clock once via SideEffect so replay returns the
	// same value (without re-invoking time.Now). This is the primitive
	// the lint message points to whenever `time.Now` appears in a
	// workflow body.
	startUnix, err := reactor.SideEffect(flow, ctx, "captured-time", func() int64 {
		return time.Now().Unix()
	})
	if err != nil {
		return err
	}

	_, err = reactor.Step(flow, ctx, "echo", reactor.StepOpts{
		IdempotencyKey: got,
		Timeout:        5 * time.Second,
	}, func(_ context.Context) (Echo, error) {
		// Closure body runs once per attempt; on replay the cached
		// output (this very struct) is returned without re-invocation.
		return Echo{Got: got, StepRun: startUnix}, nil
	})
	return err
}

func main() {
	runtime.Serve(Workflow, Trigger, Run)
}
