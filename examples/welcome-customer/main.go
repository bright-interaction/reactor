// welcome-customer is the substantive demo workflow. It exercises the
// full SDK surface so an operator running the demo can see every
// Reactor feature working together:
//
//   1. Step with retry policy + idempotency key + timeout.
//   2. vault.MustGet for an external API credential.
//   3. reactor.Permanent to surface non-retryable errors.
//   4. AwaitSignal as the human-in-the-loop primitive (with token
//      derivation so the workflow can put a "click here to approve"
//      URL in an outbound email before suspending).
//   5. Sleep(72h) demonstrating the long-suspend / scheduler-resume
//      flow.
//   6. A second Step that consumes both the signal payload and the
//      sleep boundary.
//
// Build:    reactor workflow build --src examples/welcome-customer --slug welcome-customer
// Register: reactor workflow register --db ... --slug welcome-customer
// Trigger:  POST /webhook/<token> with {"customer_id":"cus_demo","email":"a@b"}
// Approve:  POST /signal/<token> with {"approved":true,"approver":"alice"}
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bright-interaction/reactor/sdk"
	"github.com/bright-interaction/reactor/sdk/runtime"
	"github.com/bright-interaction/reactor/sdk/vault"
)

//nolint:gochecknoglobals // SDK contract: package-level Workflow + Trigger declarations.
var (
	Workflow = reactor.Workflow{Slug: "welcome-customer", Version: "1.0.0"}
	Trigger  = reactor.WebhookTrigger{
		Path:     "/webhook/welcome-customer",
		Provider: "generic",
	}
)

// Payload is what the trigger body decodes into. Every field is
// optional so the demo never crashes on missing data.
type Payload struct {
	CustomerID string `json:"customer_id"`
	Email      string `json:"email"`
	Plan       string `json:"plan"`
}

// Customer is the typed step output. Tests + the home-page table read
// the journal's output_jsonb column as JSON, so keeping fields snake-
// case via json tags makes the dashboard surface tidy.
type Customer struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Plan  string `json:"plan"`
}

// approval is the typed output of the AwaitSignal-to-Step bridge step.
// json:"-" on the raw bytes keeps it out of the workflow's output
// without losing the Go-side typing.
type approval struct {
	Approved bool   `json:"approved"`
	Approver string `json:"approver"`
	Note     string `json:"note,omitempty"`
}

// Run is the workflow entrypoint. The runtime calls this once per
// trigger; durability + replay are handled at the Flow boundary so the
// body reads as straight-line code.
func Run(ctx context.Context, flow reactor.Flow, in Payload) error {
	log := flow.Logger().With("customer_id", in.CustomerID)

	// Default a customer id so the demo run is non-empty even when the
	// trigger body is bare {}.
	if in.CustomerID == "" {
		in.CustomerID = "cus_demo"
	}
	if in.Email == "" {
		in.Email = in.CustomerID + "@example.com"
	}
	if in.Plan == "" {
		in.Plan = "starter"
	}

	// Step 1: fetch the customer record. Demonstrates retry + idem key
	// + timeout. The closure deliberately surfaces a Permanent error
	// when the customer id has the magic "cus_invalid" sentinel so the
	// DLQ + status-page paths have something to render.
	customer, err := reactor.Step(flow, ctx, "fetch-customer", reactor.StepOpts{
		IdempotencyKey: in.CustomerID,
		RetryPolicy: reactor.ExpBackoff{
			Max:  5,
			Base: 200 * time.Millisecond,
			Cap:  5 * time.Second,
		},
		Timeout: 15 * time.Second,
	}, func(ctx context.Context) (Customer, error) {
		_ = vault.MustGet("crm-api-key") // would carry an Authorization header in real code
		if in.CustomerID == "cus_invalid" {
			return Customer{}, reactor.Permanent(errors.New("customer not found"))
		}
		return Customer{ID: in.CustomerID, Email: in.Email, Plan: in.Plan}, nil
	})
	if err != nil {
		return fmt.Errorf("fetch-customer: %w", err)
	}
	log.Info("customer fetched", "email", customer.Email, "plan", customer.Plan)

	// Step 2: send the welcome email. The closure logs an "approve"
	// link that the operator can click to fire the AwaitSignal below;
	// SignalToken is deterministic so the URL is stable across retries.
	approveToken := flow.SignalToken("manager-approve")
	if _, err := reactor.Step(flow, ctx, "send-welcome", reactor.StepOpts{
		IdempotencyKey: "welcome:" + customer.ID,
		Timeout:        10 * time.Second,
	}, func(ctx context.Context) (struct{}, error) {
		_ = vault.MustGet("resend-api-key")
		log.Info("would send welcome email",
			"to", customer.Email,
			"approve_url", "POST /signal/"+approveToken)
		return struct{}{}, nil
	}); err != nil {
		return fmt.Errorf("send-welcome: %w", err)
	}

	// Step 3: human-in-the-loop. AwaitSignal blocks the run. The
	// supervisor persists a schedule, suspends the subprocess, and the
	// scheduler resumes when an external POST /signal/<token> fires
	// or the timeout expires. 7-day timeout demonstrates that the
	// signal survives host restarts.
	log.Info("awaiting manager approval", "token", approveToken)
	sig, err := flow.AwaitSignal(ctx, "manager-approve", 7*24*time.Hour)
	if err != nil {
		return fmt.Errorf("manager-approve: %w", err)
	}

	// Step 4: persist the approval decision. The signal's bytes are
	// JSON; we run a Step around the decode so the journal captures
	// who approved + when. This step has no idempotency key because
	// the input changes per signal payload.
	approved, err := reactor.Step(flow, ctx, "record-approval", reactor.StepOpts{
		Timeout: 5 * time.Second,
	}, func(ctx context.Context) (approval, error) {
		// flow.AwaitSignal returns Signal{Data: rawJSON}. The runtime's
		// generic input decoder doesn't apply here because the signal
		// payload arrives mid-run, not as the trigger body.
		var a approval
		if len(sig.Data) > 0 {
			if err := json.Unmarshal(sig.Data, &a); err != nil {
				return approval{}, reactor.Permanent(fmt.Errorf("approval JSON: %w", err))
			}
		}
		log.Info("approval recorded", "approved", a.Approved, "by", a.Approver)
		return a, nil
	})
	if err != nil {
		return err
	}
	if !approved.Approved {
		return reactor.Permanent(errors.New("workflow rejected by " + approved.Approver))
	}

	// Step 5: long sleep. The scheduler suspends + resumes; in the
	// demo the operator can advance the clock by editing the
	// schedules row to fast-forward.
	if err := flow.Sleep(ctx, "wait-3-days", 72*time.Hour); err != nil {
		return fmt.Errorf("wait: %w", err)
	}

	// Step 6: send the day-3 nudge. Closes the demo loop.
	if _, err := reactor.Step(flow, ctx, "send-nudge", reactor.StepOpts{
		IdempotencyKey: "nudge:" + customer.ID,
		Timeout:        10 * time.Second,
	}, func(ctx context.Context) (struct{}, error) {
		_ = vault.MustGet("resend-api-key")
		log.Info("would send day-3 nudge",
			"to", customer.Email,
			"approver", approved.Approver)
		return struct{}{}, nil
	}); err != nil {
		return fmt.Errorf("send-nudge: %w", err)
	}
	return nil
}

func main() {
	runtime.Serve(Workflow, Trigger, Run)
}
