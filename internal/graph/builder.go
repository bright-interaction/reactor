package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/bright-interaction/reactor/internal/credentials"
	"github.com/bright-interaction/reactor/internal/knowledge"
	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// Builder reads from the journal + credentials repo + knowledge store
// and produces a freshly-rebuilt Graph. Used by the daemon at startup
// and by the dispatcher's CRUD hook for incremental updates.
type Builder struct {
	Journal     *journal.Journal
	Credentials *credentials.Repo
	Knowledge   *knowledge.Store

	// RecentRunLimit caps how many recent runs become run nodes. Default 50.
	RecentRunLimit int
}

// Build returns a fully-populated Graph from a snapshot of the data.
// Concurrency: the Graph returned is owned by the caller; the journal
// + credentials reads are point-in-time, so a builder run that races
// with a write may miss the new row but won't corrupt anything.
func (b *Builder) Build(ctx context.Context) (*Graph, error) {
	g := New()
	if b == nil {
		return g, nil
	}

	if b.RecentRunLimit == 0 {
		b.RecentRunLimit = 50
	}

	// Workflows + their direct outbound edges.
	if b.Journal != nil {
		wfs, err := b.Journal.ListWorkflows(ctx)
		if err != nil {
			return nil, fmt.Errorf("graph: list workflows: %w", err)
		}
		for _, wf := range wfs {
			g.AddNode(Node{
				ID:    KindWorkflow + ":" + wf.Slug,
				Kind:  KindWorkflow,
				Label: wf.Slug,
				Attrs: map[string]any{
					"id":          wf.ID,
					"sdk_version": wf.SDKVersion,
					"updated_at":  wf.UpdatedAt.UTC().Format(time.RFC3339),
				},
			})
		}

		// Triggers FIRES workflow. Single ListTriggers scan + map back
		// to slug via wfs (avoids N + 1 round-trips).
		tgs, err := b.Journal.ListTriggers(ctx)
		if err == nil {
			for _, tg := range tgs {
				wfSlug := lookupSlug(wfs, tg.WorkflowID)
				if wfSlug == "" {
					continue
				}
				tNode := Node{
					ID:    KindTrigger + ":" + tg.ID,
					Kind:  KindTrigger,
					Label: string(tg.Kind),
					Attrs: map[string]any{
						"workflow_id": tg.WorkflowID,
						"kind":        string(tg.Kind),
						"provider":    tg.Provider,
						"state":       tg.State,
					},
				}
				if tg.LastFiredAt != nil && !tg.LastFiredAt.IsZero() {
					tNode.Attrs["last_fired_at"] = tg.LastFiredAt.UTC().Format(time.RFC3339)
				}
				g.AddNode(tNode)
				g.AddEdge(Edge{
					From: tNode.ID,
					To:   KindWorkflow + ":" + wfSlug,
					Kind: EdgeFires,
				})
			}
		}

		// Recent runs BELONGS_TO workflow.
		runs, err := b.Journal.ListRecentRuns(ctx, b.RecentRunLimit)
		if err == nil {
			for _, r := range runs {
				wfSlug := lookupSlug(wfs, r.WorkflowID)
				rid := KindRun + ":" + r.ID
				g.AddNode(Node{
					ID:    rid,
					Kind:  KindRun,
					Label: r.ID,
					Attrs: map[string]any{
						"workflow_id":  r.WorkflowID,
						"trigger_kind": r.TriggerKind,
						"status":       r.Status,
						"started_at":   r.StartedAt.UTC().Format(time.RFC3339),
					},
				})
				if wfSlug != "" {
					g.AddEdge(Edge{
						From: rid,
						To:   KindWorkflow + ":" + wfSlug,
						Kind: EdgeBelongsTo,
					})
				}
			}
		}

		// DLQ items FROM run.
		dlqs, err := b.Journal.ListDeadLetterItems(ctx, 100, 0)
		if err == nil {
			for _, d := range dlqs {
				did := KindDLQItem + ":" + d.ID
				g.AddNode(Node{
					ID:    did,
					Kind:  KindDLQItem,
					Label: d.StepName,
					Attrs: map[string]any{
						"run_id":     d.RunID,
						"step_name":  d.StepName,
						"error_text": d.ErrorText,
					},
				})
				g.AddEdge(Edge{
					From: did,
					To:   KindRun + ":" + d.RunID,
					Kind: EdgeFrom,
				})
			}
		}

		// Workflow USES credential, via grants.
		grants, err := b.Journal.ListGrants(ctx)
		if err == nil {
			for _, gr := range grants {
				wfSlug := lookupSlug(wfs, gr.WorkflowID)
				if wfSlug == "" {
					continue
				}
				g.AddEdge(Edge{
					From: KindWorkflow + ":" + wfSlug,
					To:   KindCredential + ":" + gr.CredentialID,
					Kind: EdgeUses,
					Attrs: map[string]any{
						"granted_at": gr.GrantedAt.UTC().Format(time.RFC3339),
						"by":         gr.GrantedBy,
					},
				})
			}
		}
	}

	// Credentials.
	if b.Credentials != nil {
		creds, err := b.Credentials.List(ctx)
		if err == nil {
			for _, c := range creds {
				attrs := map[string]any{
					"name":        c.Name,
					"service":     c.Service,
					"provider":    c.Provider,
					"auto_rotate": c.AutoRotate,
				}
				if !c.LastRotatedAt.IsZero() {
					attrs["last_rotated_at"] = c.LastRotatedAt.UTC().Format(time.RFC3339)
				}
				if c.LastRotationError != "" {
					attrs["last_error"] = c.LastRotationError
				}
				g.AddNode(Node{
					ID:    KindCredential + ":" + c.ID,
					Kind:  KindCredential,
					Label: c.Name,
					Attrs: attrs,
				})
			}
		}
	}

	// Knowledge entries + supersedes edges.
	if b.Knowledge != nil {
		entries, err := b.Knowledge.List(ctx, "")
		if err == nil {
			for _, e := range entries {
				attrs := map[string]any{
					"topic":          e.Frontmatter.Topic,
					"title":          e.Frontmatter.Title,
					"gold":           e.Frontmatter.Gold,
					"citation_count": e.Frontmatter.CitationCount,
					"created_by":     e.Frontmatter.CreatedBy,
				}
				kind := KindKnowledge
				if e.Frontmatter.Topic == "post-mortems" {
					kind = KindPostMortem
				}
				kid := kind + ":" + e.Frontmatter.ID
				g.AddNode(Node{
					ID:    kid,
					Kind:  kind,
					Label: e.Frontmatter.Title,
					Attrs: attrs,
				})
				for _, oldID := range e.Frontmatter.Supersedes {
					g.AddEdge(Edge{
						From: kid,
						To:   KindKnowledge + ":" + oldID,
						Kind: EdgeSupersedes,
					})
				}
			}
		}
	}

	return g, nil
}

func lookupSlug(wfs []journal.Workflow, id string) string {
	for _, w := range wfs {
		if w.ID == id {
			return w.Slug
		}
	}
	return ""
}
