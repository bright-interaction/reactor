package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/brightinteraction/reactor/internal/codegen"
	"github.com/brightinteraction/reactor/internal/knowledge"
)

// cmdKnowledge dispatches the knowledge subcommand group:
// add / list / search / show / supersede / promote / stale.
//
// All subcommands operate on the on-disk corpus at <root>/knowledge/.
// The same Store is what the daemon's MCP server + codegen prompt
// assembler read, so a CLI add takes effect for the next generate
// without restart.
func cmdKnowledge(ctx context.Context, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("knowledge: missing subcommand (add|list|search|show|supersede|promote|stale)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return cmdKnowledgeAdd(ctx, rest)
	case "list":
		return cmdKnowledgeList(ctx, rest)
	case "search":
		return cmdKnowledgeSearch(ctx, rest)
	case "show":
		return cmdKnowledgeShow(ctx, rest)
	case "supersede":
		return cmdKnowledgeSupersede(ctx, rest)
	case "promote":
		return cmdKnowledgePromote(ctx, rest)
	case "stale":
		return cmdKnowledgeStale(ctx, rest)
	default:
		_ = log
		return fmt.Errorf("knowledge: unknown subcommand %q", sub)
	}
}

// openStore returns a Store rooted at <root>/knowledge with a real
// git committer wired so add / supersede / promote operations land as
// commits the same way codegen-generated workflows do. Falls back to
// the package's noop committer (silent skip) when git isn't available
// at the root path; the GitCommitter walks up looking for .git itself.
func openStore(root string) (*knowledge.Store, error) {
	if root == "" {
		return nil, errors.New("knowledge: missing --root (and HOME unset)")
	}
	s, err := knowledge.New(filepath.Join(root, "knowledge"))
	if err != nil {
		return nil, err
	}
	s.Git = &codegen.GitCommitter{}
	return s, nil
}

func cmdKnowledgeAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("knowledge add", flag.ContinueOnError)
	root := fs.String("root", defaultRoot(), "Reactor state root")
	topic := fs.String("topic", "", "topic (required)")
	title := fs.String("title", "", "title (required)")
	body := fs.String("body", "", "body markdown (use --body-file for long content)")
	bodyFile := fs.String("body-file", "", "path to body markdown file (overrides --body)")
	tagsRaw := fs.String("tags", "", "comma-separated tags")
	sourcesRaw := fs.String("sources", "", "comma-separated source URLs / refs")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if *topic == "" || *title == "" {
		return errors.New("knowledge add: --topic and --title required")
	}
	bodyStr := *body
	if *bodyFile != "" {
		raw, err := os.ReadFile(*bodyFile)
		if err != nil {
			return fmt.Errorf("knowledge add: read body-file: %w", err)
		}
		bodyStr = string(raw)
	}
	if bodyStr == "" {
		return errors.New("knowledge add: --body or --body-file required")
	}

	s, err := openStore(*root)
	if err != nil {
		return err
	}
	e, err := s.Add(ctx, knowledge.Entry{
		Frontmatter: knowledge.Frontmatter{
			Topic:     *topic,
			Title:     *title,
			Tags:      splitCSV(*tagsRaw),
			Sources:   splitCSV(*sourcesRaw),
			CreatedBy: "operator",
		},
		Body: bodyStr,
	})
	if err != nil {
		return err
	}
	fmt.Printf("added %s -> %s\n", e.Frontmatter.ID, e.Path)
	return nil
}

func cmdKnowledgeList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("knowledge list", flag.ContinueOnError)
	root := fs.String("root", defaultRoot(), "Reactor state root")
	topic := fs.String("topic", "", "filter to one topic")
	asJSON := fs.Bool("json", false, "emit JSON")
	gold := fs.Bool("gold", false, "show only gold entries")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	entries, err := s.List(ctx, *topic)
	if err != nil {
		return err
	}
	if *gold {
		filtered := entries[:0]
		for _, e := range entries {
			if e.Frontmatter.Gold {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	if *asJSON {
		if entries == nil {
			entries = []knowledge.Entry{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	if len(entries) == 0 {
		fmt.Println("(no entries)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "ID\tTOPIC\tTITLE\tGOLD\tCITES\tCREATED_BY")
	for _, e := range entries {
		gold := ""
		if e.Frontmatter.Gold {
			gold = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			e.Frontmatter.ID, e.Frontmatter.Topic,
			truncate(e.Frontmatter.Title, 60), gold,
			e.Frontmatter.CitationCount, e.Frontmatter.CreatedBy)
	}
	return nil
}

func cmdKnowledgeSearch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("knowledge search", flag.ContinueOnError)
	root := fs.String("root", defaultRoot(), "Reactor state root")
	limit := fs.Int("limit", 5, "max hits to return")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("knowledge search: missing query")
	}
	q := strings.Join(fs.Args(), " ")
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	hits, err := s.Search(ctx, q, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		if hits == nil {
			hits = []knowledge.Hit{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(hits)
	}
	if len(hits) == 0 {
		fmt.Println("(no hits)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "SCORE\tID\tTOPIC\tTITLE")
	for _, h := range hits {
		fmt.Fprintf(tw, "%.2f\t%s\t%s\t%s\n",
			h.Score, h.Entry.Frontmatter.ID, h.Entry.Frontmatter.Topic,
			truncate(h.Entry.Frontmatter.Title, 60))
	}
	return nil
}

func cmdKnowledgeShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("knowledge show", flag.ContinueOnError)
	root := fs.String("root", defaultRoot(), "Reactor state root")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("knowledge show: missing <id>")
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	e, err := s.Get(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("# %s\n\n", e.Frontmatter.Title)
	fmt.Printf("id: %s\ntopic: %s\ngold: %t\ncitation_count: %d\ncreated_by: %s\nsources: %v\ntags: %v\n\n---\n\n",
		e.Frontmatter.ID, e.Frontmatter.Topic, e.Frontmatter.Gold,
		e.Frontmatter.CitationCount, e.Frontmatter.CreatedBy,
		e.Frontmatter.Sources, e.Frontmatter.Tags)
	fmt.Println(e.Body)
	return nil
}

func cmdKnowledgeSupersede(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("knowledge supersede", flag.ContinueOnError)
	root := fs.String("root", defaultRoot(), "Reactor state root")
	bodyFile := fs.String("new-body-file", "", "path to new body markdown")
	reason := fs.String("reason", "", "free-form reason for supersede")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("knowledge supersede: missing <old-id>")
	}
	if *bodyFile == "" {
		return errors.New("knowledge supersede: --new-body-file required")
	}
	if *reason == "" {
		return errors.New("knowledge supersede: --reason required")
	}
	raw, err := os.ReadFile(*bodyFile)
	if err != nil {
		return fmt.Errorf("knowledge supersede: read body-file: %w", err)
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	revised, err := s.Supersede(ctx, fs.Arg(0), knowledge.Entry{
		Frontmatter: knowledge.Frontmatter{CreatedBy: "operator"},
		Body:        string(raw),
	}, *reason)
	if err != nil {
		return err
	}
	fmt.Printf("revised %s -> %s\n", fs.Arg(0), revised.Frontmatter.ID)
	return nil
}

func cmdKnowledgePromote(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("knowledge promote", flag.ContinueOnError)
	root := fs.String("root", defaultRoot(), "Reactor state root")
	off := fs.Bool("off", false, "demote (set gold=false) instead of promote")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("knowledge promote: missing <id>")
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	e, err := s.PromoteGold(ctx, fs.Arg(0), !*off)
	if err != nil {
		return err
	}
	fmt.Printf("%s gold=%t\n", e.Frontmatter.ID, e.Frontmatter.Gold)
	return nil
}

func cmdKnowledgeStale(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("knowledge stale", flag.ContinueOnError)
	root := fs.String("root", defaultRoot(), "Reactor state root")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("knowledge stale: missing <id>")
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	e, err := s.MarkStale(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("%s marked stale (last_validated_at -> %s)\n", e.Frontmatter.ID, e.Frontmatter.LastValidatedAt)
	return nil
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
