package knowledge

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed seed/*.md
var seedFS embed.FS

// nowFnPtr is the package-level clock, held in an atomic.Pointer so
// SetClockForTest can swap it from parallel tests under -race without
// triggering the detector. Initialised to time.Now in init below.
var nowFnPtr atomic.Pointer[func() time.Time]

func init() {
	def := func() time.Time { return time.Now() }
	nowFnPtr.Store(&def)
}

func nowFn() time.Time {
	return (*nowFnPtr.Load())()
}

// Entry is one corpus item: the parsed frontmatter + the body markdown
// + the on-disk path the body was read from.
type Entry struct {
	Frontmatter Frontmatter
	Body        string
	Path        string
}

// Committer is the git surface the Store calls after every successful
// add / supersede / promote. Mirrors codegen.Committer's signature so
// the same GitCommitter instance can serve both packages.
type Committer interface {
	Commit(ctx context.Context, dir, slug, version string) error
}

// noopCommitter is the default when callers don't pass one. Useful for
// tests + for installations where REACTOR_GIT_BACKED=false.
type noopCommitter struct{}

func (noopCommitter) Commit(_ context.Context, _, _, _ string) error { return nil }

// ErrNotFound is returned when an entry id doesn't resolve to a file.
var ErrNotFound = errors.New("knowledge: entry not found")

// ErrRedacted is returned when an Add or Revise hits the PII redactor.
// Wraps the formatted finding list so callers can surface details.
type ErrRedacted struct {
	Findings []RedactionFinding
	Summary  string
}

func (e *ErrRedacted) Error() string { return e.Summary }

// Store is the knowledge corpus. One instance per process; safe for
// concurrent reads but every write takes the write side of the RWMutex.
type Store struct {
	Root      string    // ~/.reactor/knowledge by convention
	Git       Committer // optional; nil → noopCommitter
	Redactor  *Redactor // optional; nil → NewRedactor()
	StaleDays float64   // 0 → 90 by default

	mu sync.RWMutex
}

// New returns a Store rooted at root. Creates root with mode 0700 if
// it doesn't exist. Pre-seeds the corpus on a fresh install by walking
// the embedded seed/ directory.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("knowledge: root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("knowledge: mkdir root: %w", err)
	}
	s := &Store{Root: root, Git: noopCommitter{}, Redactor: NewRedactor(), StaleDays: 90}
	if err := s.seedIfEmpty(); err != nil {
		return nil, err
	}
	return s, nil
}

// seedIfEmpty walks the embedded seed FS and writes any entries whose
// id isn't already present in Root. Idempotent: re-running on a
// populated corpus is a no-op.
func (s *Store) seedIfEmpty() error {
	return fs.WalkDir(seedFS, "seed", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		raw, err := seedFS.ReadFile(path)
		if err != nil {
			return err
		}
		fm, _, err := parseFrontmatter(raw)
		if err != nil {
			return fmt.Errorf("knowledge: seed %s: %w", path, err)
		}
		dest := filepath.Join(s.Root, fm.Topic, fm.ID+".md")
		if _, err := os.Stat(dest); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		// Re-encode so the on-disk shape is canonical and the seed time
		// reflects the install.
		fm.CreatedAt = nowFn().UTC()
		if fm.LastValidatedAt.IsZero() {
			fm.LastValidatedAt = fm.CreatedAt
		}
		_, body, err := parseFrontmatter(raw)
		if err != nil {
			return err
		}
		out := encodeFrontmatter(fm, string(body))
		return os.WriteFile(dest, out, 0o600)
	})
}

// Add validates the entry through the redactor, derives an id if empty,
// stamps timestamps, writes to <Root>/<Topic>/<ID>.md, and commits via
// Git. The caller-passed Frontmatter wins on Title + Topic + Sources +
// Tags + CreatedBy; everything else is overwritten by Add.
func (s *Store) Add(ctx context.Context, e Entry) (Entry, error) {
	if e.Frontmatter.Topic == "" {
		return Entry{}, errors.New("knowledge: topic required")
	}
	if e.Frontmatter.Title == "" {
		return Entry{}, errors.New("knowledge: title required")
	}
	if e.Body == "" {
		return Entry{}, errors.New("knowledge: body required")
	}
	if findings := s.Redactor.Scan(e.Body); len(findings) > 0 {
		return Entry{}, &ErrRedacted{Findings: findings, Summary: s.Redactor.Format(findings)}
	}

	if e.Frontmatter.ID == "" {
		id, err := newID(e.Frontmatter.Topic)
		if err != nil {
			return Entry{}, err
		}
		e.Frontmatter.ID = id
	}
	now := nowFn().UTC()
	e.Frontmatter.CreatedAt = now
	if e.Frontmatter.CreatedBy == "" {
		e.Frontmatter.CreatedBy = "operator"
	}
	if e.Frontmatter.LastValidatedAt.IsZero() {
		e.Frontmatter.LastValidatedAt = now
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.Root, e.Frontmatter.Topic)
	// Path-traversal guard: Topic is caller-controlled (HTTP + MCP add) and
	// becomes a filesystem path segment. A value like "../../../etc/cron.d"
	// would MkdirAll + WriteFile an attacker-bodied .md outside the knowledge
	// root. Reject anything that resolves outside s.Root.
	if !withinRoot(s.Root, dir) {
		return Entry{}, fmt.Errorf("knowledge: invalid topic %q (escapes the knowledge root)", e.Frontmatter.Topic)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Entry{}, fmt.Errorf("knowledge: mkdir topic: %w", err)
	}
	dest := filepath.Join(dir, e.Frontmatter.ID+".md")
	if !withinRoot(s.Root, dest) {
		return Entry{}, fmt.Errorf("knowledge: invalid id %q (escapes the knowledge root)", e.Frontmatter.ID)
	}
	if _, err := os.Stat(dest); err == nil {
		return Entry{}, fmt.Errorf("knowledge: id %q already exists; use Supersede to replace", e.Frontmatter.ID)
	}
	out := encodeFrontmatter(e.Frontmatter, e.Body)
	if err := os.WriteFile(dest, out, 0o600); err != nil {
		return Entry{}, fmt.Errorf("knowledge: write: %w", err)
	}
	e.Path = dest
	if err := s.commit(ctx, dir, "add: "+e.Frontmatter.ID); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// withinRoot reports whether path stays inside root after cleaning. It guards
// caller-controlled path segments (knowledge Topic/ID) that use "../" to escape
// the knowledge directory.
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Get returns the entry with id (no topic needed; the store walks).
// Returns ErrNotFound if no file matches.
func (s *Store) Get(ctx context.Context, id string) (Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getLocked(id)
}

func (s *Store) getLocked(id string) (Entry, error) {
	var found Entry
	var hit bool
	err := filepath.WalkDir(s.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		base := strings.TrimSuffix(filepath.Base(path), ".md")
		if base != id {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fm, body, err := parseFrontmatter(raw)
		if err != nil {
			return err
		}
		found = Entry{Frontmatter: fm, Body: string(body), Path: path}
		hit = true
		return fs.SkipAll
	})
	if err != nil {
		return Entry{}, err
	}
	if !hit {
		return Entry{}, ErrNotFound
	}
	return found, nil
}

// List returns every entry, optionally filtered to one topic. Sorted by
// (topic asc, id asc) for deterministic output.
func (s *Store) List(ctx context.Context, topic string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Entry
	root := s.Root
	if topic != "" {
		root = filepath.Join(s.Root, topic)
		if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fm, body, err := parseFrontmatter(raw)
		if err != nil {
			return fmt.Errorf("knowledge: parse %s: %w", path, err)
		}
		out = append(out, Entry{Frontmatter: fm, Body: string(body), Path: path})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Frontmatter.Topic != out[j].Frontmatter.Topic {
			return out[i].Frontmatter.Topic < out[j].Frontmatter.Topic
		}
		return out[i].Frontmatter.ID < out[j].Frontmatter.ID
	})
	return out, nil
}

// Search returns the top-N BM25 hits across the corpus.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	entries, err := s.List(ctx, "")
	if err != nil {
		return nil, err
	}
	return rankEntries(entries, query, limit, s.StaleDays), nil
}

// Supersede creates a new entry that points back at oldID via the
// supersedes frontmatter. The old entry stays on disk; git history is
// the audit. Returns the new entry.
func (s *Store) Supersede(ctx context.Context, oldID string, newEntry Entry, reason string) (Entry, error) {
	old, err := s.Get(ctx, oldID)
	if err != nil {
		return Entry{}, err
	}
	if newEntry.Frontmatter.Topic == "" {
		newEntry.Frontmatter.Topic = old.Frontmatter.Topic
	}
	if newEntry.Frontmatter.Title == "" {
		newEntry.Frontmatter.Title = old.Frontmatter.Title
	}
	newEntry.Frontmatter.Supersedes = append(newEntry.Frontmatter.Supersedes, oldID)
	if reason != "" {
		newEntry.Frontmatter.Sources = append(newEntry.Frontmatter.Sources, "supersede-reason:"+reason)
	}
	return s.Add(ctx, newEntry)
}

// PromoteGold flips the gold flag on an existing entry. Used by the
// operator from the dashboard to weight an entry higher in prompts.
func (s *Store) PromoteGold(ctx context.Context, id string, gold bool) (Entry, error) {
	return s.mutate(ctx, id, "promote-gold:"+id, func(e *Entry) {
		e.Frontmatter.Gold = gold
	})
}

// MarkStale forces last_validated_at past the stale window so the
// entry is deprioritised in search until an operator re-validates.
func (s *Store) MarkStale(ctx context.Context, id string) (Entry, error) {
	return s.mutate(ctx, id, "mark-stale:"+id, func(e *Entry) {
		past := nowFn().AddDate(-1, 0, 0).UTC()
		e.Frontmatter.LastValidatedAt = past
	})
}

// IncrementCitation bumps the citation count. Called by the prompt
// assembler when an entry lands in a generated workflow's prompt. Does
// NOT commit (citation noise would balloon git history).
func (s *Store) IncrementCitation(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.getLocked(id)
	if err != nil {
		return err
	}
	e.Frontmatter.CitationCount++
	out := encodeFrontmatter(e.Frontmatter, e.Body)
	return os.WriteFile(e.Path, out, 0o600)
}

func (s *Store) mutate(ctx context.Context, id, msg string, fn func(*Entry)) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.getLocked(id)
	if err != nil {
		return Entry{}, err
	}
	fn(&e)
	out := encodeFrontmatter(e.Frontmatter, e.Body)
	if err := os.WriteFile(e.Path, out, 0o600); err != nil {
		return Entry{}, err
	}
	if err := s.commit(ctx, filepath.Dir(e.Path), msg); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (s *Store) commit(ctx context.Context, dir, msg string) error {
	if s.Git == nil {
		return nil
	}
	return s.Git.Commit(ctx, dir, "knowledge", msg)
}

// newID returns "<topic-prefix>_<8 hex>" using the first letter of the
// topic. Predictable shape so operators reading git logs see at a glance
// which topic an id belongs to.
func newID(topic string) (string, error) {
	prefix := "k"
	for _, r := range topic {
		if r >= 'a' && r <= 'z' {
			prefix = string(r)
			break
		}
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("knowledge: id rand: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}

// SetClockForTest swaps the package clock atomically. Restore by
// deferring the returned restore func. Test-only; no callers in
// production code.
func SetClockForTest(fn func() time.Time) (restore func()) {
	prev := nowFnPtr.Load()
	nowFnPtr.Store(&fn)
	return func() { nowFnPtr.Store(prev) }
}

// visibleToTenant reports whether an entry is readable by a viewer scoped to
// tenant. An empty scope is the admin/global view and sees everything; an
// entry with no tenant is shared material and is visible to everyone.
func visibleToTenant(e Entry, tenant string) bool {
	if tenant == "" {
		return true
	}
	return e.Frontmatter.Tenant == "" || e.Frontmatter.Tenant == tenant
}

// ListForTenant is List scoped to a viewer. Pass "" for the unscoped
// admin view. Use this for anything reachable by a member: the corpus mixes
// shared playbooks with per-tenant post-mortems, and List returns both.
func (s *Store) ListForTenant(ctx context.Context, topic, tenant string) ([]Entry, error) {
	all, err := s.List(ctx, topic)
	if err != nil {
		return nil, err
	}
	if tenant == "" {
		return all, nil
	}
	out := all[:0]
	for _, e := range all {
		if visibleToTenant(e, tenant) {
			out = append(out, e)
		}
	}
	return out, nil
}

// GetForTenant is Get scoped to a viewer. Returns ErrNotFound (never a
// permission error) when the entry belongs to another tenant, so guessing an
// id cannot confirm that it exists.
func (s *Store) GetForTenant(ctx context.Context, id, tenant string) (Entry, error) {
	e, err := s.Get(ctx, id)
	if err != nil {
		return Entry{}, err
	}
	if !visibleToTenant(e, tenant) {
		return Entry{}, ErrNotFound
	}
	return e, nil
}
