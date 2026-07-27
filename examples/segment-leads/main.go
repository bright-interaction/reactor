// segment-leads shows the block arsenal shaping data with no external calls: it
// dedupes incoming leads by email, routes each into a segment with a Switch,
// groups them, and returns the counts. Pure transforms (sdk/blocks) compose
// inside one Step; nothing here does IO, so it is deterministic and replays
// cleanly.
//
// Build:    reactor workflow build --src examples/segment-leads --slug segment-leads
// Register: reactor workflow register --db ... --slug segment-leads
// Trigger:  POST /webhook/<token> with
//
//	{"leads":[{"email":"a@x.com","score":90},{"email":"a@x.com","score":90},{"email":"b@x.com","score":50}]}
package main

import (
	"context"

	"github.com/bright-interaction/reactor/sdk"
	"github.com/bright-interaction/reactor/sdk/blocks"
	"github.com/bright-interaction/reactor/sdk/runtime"
)

//nolint:gochecknoglobals // SDK contract: package-level declarations.
var (
	Workflow = reactor.Workflow{Slug: "segment-leads", Version: "0.1.0"}
	Trigger  = reactor.WebhookTrigger{Path: "/webhook/segment-leads", Provider: "generic"}
)

// Lead is one inbound lead.
type Lead struct {
	Email string `json:"email"`
	Score int    `json:"score"`
}

// Payload is the webhook body.
type Payload struct {
	Leads []Lead `json:"leads"`
}

// Summary is the journaled step output.
type Summary struct {
	Unique int `json:"unique"`
	Hot    int `json:"hot"`
	Warm   int `json:"warm"`
	Cold   int `json:"cold"`
}

func Run(ctx context.Context, flow reactor.Flow, in Payload) error {
	_, err := reactor.Step(flow, ctx, "segment", reactor.StepOpts{
		IdempotencyKey: "segment",
	}, func(_ context.Context) (Summary, error) {
		// Dedupe by email, keeping the first occurrence.
		uniq := blocks.UniqueBy(in.Leads, func(l Lead) string { return l.Email })

		// Route each lead to a named segment, then bucket by it.
		byseg := blocks.GroupBy(uniq, func(l Lead) string {
			return blocks.Switch(l, []blocks.Case[Lead]{
				{When: func(l Lead) bool { return l.Score >= 80 }, Label: "hot"},
				{When: func(l Lead) bool { return l.Score >= 40 }, Label: "warm"},
			}, "cold")
		})

		return Summary{
			Unique: len(uniq),
			Hot:    len(byseg["hot"]),
			Warm:   len(byseg["warm"]),
			Cold:   len(byseg["cold"]),
		}, nil
	})
	return err
}

func main() {
	runtime.Serve(Workflow, Trigger, Run)
}
