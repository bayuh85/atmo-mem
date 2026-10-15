// Package memory is the domain layer for saving, searching, recalling, and summarizing memories.
package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/bayuh85/atmo-mem/internal/llm"
	"github.com/bayuh85/atmo-mem/internal/store"
)

const (
	KindObservation = "observation"
	KindDecision    = "decision"
	KindBugfix      = "bugfix"
	KindPreference  = "preference"
	KindFact        = "fact"
	KindSummary     = "summary"
	KindTask        = "task"

	defaultSearchLimit = 10
	maxSearchLimit     = 50
	defaultRecallLimit = 8
	maxRecallLimit     = 30
	defaultListLimit   = 20
	maxListLimit       = 100

	maxTitleRunes   = 300
	maxContentRunes = 200_000
	maxProjectRunes = 200
	maxTags         = 20
	maxTagRunes     = 40
	maxSessionRunes = 80
	maxPromptRunes  = 80_000
)

// Kinds is the accepted set, in documentation order.
func Kinds() []string {
	return []string{
		KindObservation,
		KindDecision,
		KindBugfix,
		KindPreference,
		KindFact,
		KindSummary,
		KindTask,
	}
}

// InputError is a caller mistake (missing title, unknown kind, and so on).
type InputError struct{ Message string }

func (e *InputError) Error() string { return e.Message }

func inputErr(format string, args ...any) error {
	return &InputError{Message: fmt.Sprintf(format, args...)}
}

// Service coordinates the store and the optional LLM.
type Service struct {
	Store   *store.Store
	LLM     llm.Completer
	Project string
	Now     func() time.Time
}

// New returns a service. model may be nil when summarization is unused.
func New(st *store.Store, model llm.Completer, project string) *Service {
	return &Service{
		Store:   st,
		LLM:     model,
		Project: strings.TrimSpace(project),
		Now:     time.Now,
	}
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func (s *Service) project(explicit string) string {
	if p := strings.TrimSpace(explicit); p != "" {
		return p
	}
	return s.Project
}

// SaveInput is a new memory before validation.
type SaveInput struct {
	SessionID string
	Project   string
	Kind      string
	Title     string
	Content   string
	Tags      []string
}

// Save validates and stores a memory.
func (s *Service) Save(ctx context.Context, in SaveInput) (store.Memory, error) {
	mem, err := s.prepare(in, s.now())
	if err != nil {
		return store.Memory{}, err
	}
	saved, err := s.Store.Save(ctx, mem)
	if err != nil {
		return store.Memory{}, err
	}
	if saved.SessionID != "" {
		if err := s.Store.TouchSession(ctx, saved.SessionID, saved.Project, saved.CreatedAt); err != nil {
			return store.Memory{}, err
		}
	}
	return saved, nil
}

// SearchInput is a keyword search.
type SearchInput struct {
	Query   string
	Project string
	Kind    string
	Limit   int
}

// Search runs an AND prefix search.
func (s *Service) Search(ctx context.Context, in SearchInput) ([]store.Memory, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, inputErr("query is required")
	}
	match := store.BuildMatch(in.Query, store.MatchAll)
	if match == "" {
		return nil, inputErr("query has no searchable terms")
	}
	kind, err := optionalKind(in.Kind)
	if err != nil {
		return nil, err
	}
	project := s.project(in.Project)
	if err := validateProject(project); err != nil {
		return nil, err
	}
	hits, err := s.Store.Search(ctx, match, project, kind, clamp(in.Limit, defaultSearchLimit, maxSearchLimit))
	if err != nil {
		return nil, err
	}
	out := make([]store.Memory, len(hits))
	for i, h := range hits {
		out[i] = h.Memory
	}
	return out, nil
}

// RecallInput asks for memories relevant to a task.
type RecallInput struct {
	Query   string
	Project string
	Limit   int
}

// RecallResult is ranked memory plus a text block an agent can read directly.
type RecallResult struct {
	Memories []store.Memory `json:"memories"`
	Context  string         `json:"context"`
}

// Recall ranks project memories for a task.
// An empty query returns recent memories. The newest summary for the project
// is pinned first so the previous session stays visible.
func (s *Service) Recall(ctx context.Context, in RecallInput) (RecallResult, error) {
	limit := clamp(in.Limit, defaultRecallLimit, maxRecallLimit)
	project := s.project(in.Project)
	if err := validateProject(project); err != nil {
		return RecallResult{}, err
	}
	now := s.now()
	match := store.BuildMatch(in.Query, store.MatchAny)

	var mems []store.Memory
	if match == "" {
		listed, err := s.Store.List(ctx, project, "", limit)
		if err != nil {
			return RecallResult{}, err
		}
		mems = listed
	} else {
		fetch := limit * 5
		if fetch < 20 {
			fetch = 20
		}
		hits, err := s.Store.Search(ctx, match, project, "", fetch)
		if err != nil {
			return RecallResult{}, err
		}
		mems = orderHits(hits, now)
		if len(mems) > limit {
			mems = mems[:limit]
		}
	}

	summary, ok, err := s.Store.LatestByKind(ctx, project, KindSummary)
	if err != nil {
		return RecallResult{}, err
	}
	if ok {
		mems = pinFront(mems, summary, limit)
	}
	if mems == nil {
		mems = []store.Memory{}
	}
	return RecallResult{
		Memories: mems,
		Context:  FormatRecall(project, strings.TrimSpace(in.Query), mems),
	}, nil
}

// ListInput filters recent memories.
type ListInput struct {
	Project string
	Kind    string
	Limit   int
}

// List returns recent memories, newest first.
func (s *Service) List(ctx context.Context, in ListInput) ([]store.Memory, error) {
	kind, err := optionalKind(in.Kind)
	if err != nil {
		return nil, err
	}
	project := s.project(in.Project)
	if err := validateProject(project); err != nil {
		return nil, err
	}
	mems, err := s.Store.List(ctx, project, kind, clamp(in.Limit, defaultListLimit, maxListLimit))
	if err != nil {
		return nil, err
	}
	if mems == nil {
		mems = []store.Memory{}
	}
	return mems, nil
}

// Get loads one memory.
func (s *Service) Get(ctx context.Context, memoryID string) (store.Memory, error) {
	memoryID = strings.TrimSpace(memoryID)
	if memoryID == "" {
		return store.Memory{}, inputErr("id is required")
	}
	return s.Store.Get(ctx, memoryID)
}

// Delete removes one memory.
func (s *Service) Delete(ctx context.Context, memoryID string) error {
	memoryID = strings.TrimSpace(memoryID)
	if memoryID == "" {
		return inputErr("id is required")
	}
	return s.Store.Delete(ctx, memoryID)
}

// SummarizeInput is the material for one session summary.
type SummarizeInput struct {
	Transcript string
	SessionID  string
	Project    string
	Title      string
}

// SummaryResult is the stored summary memory and its text.
type SummaryResult struct {
	Memory  store.Memory `json:"memory"`
	Summary string       `json:"summary"`
}

const summarySystemPrompt = `You write durable memory for a coding agent. You will receive notes from a work session, and sometimes earlier memories saved during that same session. Produce a summary the agent can trust in a later session.

Use this markdown shape. Omit a section only when it would be empty.

## Outcome
What changed or what was learned, in a few sentences.

## Decisions
Choices that future work should not reopen, each with the reason if the notes include one.

## State
Concrete leftovers: files, commands, errors, APIs, configuration, or constraints worth keeping.

## Follow-ups
Unfinished work, in a short list.

Rules:
- Use only information present in the notes and memories. Do not guess.
- Prefer exact names over paraphrases.
- Stay under 400 words.
- Do not add a title, preamble, or closing line.`

// Summarize compresses a transcript and any memories already saved for the
// session, then stores the result as a summary memory.
func (s *Service) Summarize(ctx context.Context, in SummarizeInput) (SummaryResult, error) {
	if s.LLM == nil {
		return SummaryResult{}, fmt.Errorf("summarize: %w", llm.ErrNoAPIKey)
	}
	sessionID := strings.TrimSpace(in.SessionID)
	if err := validateSession(sessionID); err != nil {
		return SummaryResult{}, err
	}
	project := s.project(in.Project)
	if err := validateProject(project); err != nil {
		return SummaryResult{}, err
	}
	transcript := strings.TrimSpace(in.Transcript)

	var prior []store.Memory
	if sessionID != "" {
		var err error
		prior, err = s.Store.BySession(ctx, sessionID)
		if err != nil {
			return SummaryResult{}, err
		}
	}
	if transcript == "" && len(prior) == 0 {
		return SummaryResult{}, inputErr("nothing to summarize; pass a transcript or a session_id that has memories")
	}

	user := summaryUserPrompt(project, transcript, prior)
	raw, err := s.LLM.Complete(ctx, []llm.Message{
		{Role: "system", Content: summarySystemPrompt},
		{Role: "user", Content: user},
	})
	if err != nil {
		return SummaryResult{}, fmt.Errorf("summarize: %w", err)
	}
	summary := stripFence(raw)
	if summary == "" {
		return SummaryResult{}, errors.New("summarize: model returned an empty summary")
	}

	now := s.now()
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = summaryTitle(project, now)
	}
	saved, err := s.Save(ctx, SaveInput{
		SessionID: sessionID,
		Project:   project,
		Kind:      KindSummary,
		Title:     title,
		Content:   summary,
		Tags:      []string{"summary"},
	})
	if err != nil {
		return SummaryResult{}, err
	}
	if sessionID != "" {
		if err := s.Store.SetSessionSummary(ctx, sessionID, project, summary, now); err != nil {
			return SummaryResult{}, err
		}
	}
	return SummaryResult{Memory: saved, Summary: summary}, nil
}

func summaryUserPrompt(project, transcript string, prior []store.Memory) string {
	var b strings.Builder
	if project != "" {
		fmt.Fprintf(&b, "Project: %s\n\n", project)
	}
	if len(prior) > 0 {
		b.WriteString("Memories already saved in this session:\n")
		for _, m := range prior {
			fmt.Fprintf(&b, "\n- [%s] %s\n%s\n", m.Kind, m.Title, m.Content)
		}
		b.WriteString("\n")
	}
	if transcript != "" {
		b.WriteString("Session notes:\n")
		b.WriteString(transcript)
		b.WriteString("\n")
	}
	text := b.String()
	runes := []rune(text)
	if len(runes) <= maxPromptRunes {
		return text
	}
	return string(runes[:maxPromptRunes]) + "\n\n[truncated]\n"
}

func summaryTitle(project string, now time.Time) string {
	day := now.UTC().Format("2006-01-02")
	if project == "" {
		return "Session summary " + day
	}
	return "Session summary " + day + " (" + project + ")"
}

func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	rest := s[3:]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 {
		return s
	}
	rest = strings.TrimSpace(rest[nl+1:])
	rest = strings.TrimSuffix(rest, "```")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return strings.TrimSpace(s)
	}
	return rest
}

// FormatRecall renders memories as a compact context block.
func FormatRecall(project, query string, mems []store.Memory) string {
	var b strings.Builder
	if project != "" {
		fmt.Fprintf(&b, "Project memory for %s\n", project)
	} else {
		b.WriteString("Project memory\n")
	}
	if query != "" {
		fmt.Fprintf(&b, "Query: %s\n", query)
	} else {
		b.WriteString("Recent memories\n")
	}
	if len(mems) == 0 {
		b.WriteString("\nNo memories stored yet.\n")
		return b.String()
	}
	for _, m := range mems {
		fmt.Fprintf(&b, "\n[%s] %s (%s, %s)\n%s\n",
			m.Kind, m.Title, m.ID, m.CreatedAt.UTC().Format("2006-01-02"), strings.TrimSpace(m.Content))
		if len(m.Tags) > 0 {
			fmt.Fprintf(&b, "tags: %s\n", strings.Join(m.Tags, ", "))
		}
	}
	return b.String()
}

// RankScore combines FTS5 bm25 (more negative is a better match) with a small
// recency bonus. Higher is better. The bonus is intentionally small so a
// clearly better match beats a merely newer one.
func RankScore(bm25 float64, created, now time.Time) float64 {
	age := now.Sub(created).Hours()
	if age < 0 {
		age = 0
	}
	bonus := 0.35 * math.Exp(-age/168.0)
	return -bm25 + bonus
}

func orderHits(hits []store.Hit, now time.Time) []store.Memory {
	sort.SliceStable(hits, func(i, j int) bool {
		return RankScore(hits[i].Score, hits[i].Memory.CreatedAt, now) >
			RankScore(hits[j].Score, hits[j].Memory.CreatedAt, now)
	})
	out := make([]store.Memory, len(hits))
	for i, h := range hits {
		out[i] = h.Memory
	}
	return out
}

func pinFront(mems []store.Memory, summary store.Memory, limit int) []store.Memory {
	out := make([]store.Memory, 0, len(mems)+1)
	out = append(out, summary)
	for _, m := range mems {
		if m.ID == summary.ID {
			continue
		}
		out = append(out, m)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *Service) prepare(in SaveInput, now time.Time) (store.Memory, error) {
	title := strings.TrimSpace(in.Title)
	content := strings.TrimSpace(in.Content)
	if title == "" {
		return store.Memory{}, inputErr("title is required")
	}
	if content == "" {
		return store.Memory{}, inputErr("content is required")
	}
	if err := rejectNUL("title", title); err != nil {
		return store.Memory{}, err
	}
	if err := rejectNUL("content", content); err != nil {
		return store.Memory{}, err
	}
	if runeLen(title) > maxTitleRunes {
		return store.Memory{}, inputErr("title exceeds %d characters", maxTitleRunes)
	}
	if runeLen(content) > maxContentRunes {
		return store.Memory{}, inputErr("content exceeds %d characters", maxContentRunes)
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind == "" {
		kind = KindObservation
	}
	if !validKind(kind) {
		return store.Memory{}, inputErr("unknown kind %q (want %s)", kind, strings.Join(Kinds(), ", "))
	}
	project := s.project(in.Project)
	if err := validateProject(project); err != nil {
		return store.Memory{}, err
	}
	sessionID := strings.TrimSpace(in.SessionID)
	if err := validateSession(sessionID); err != nil {
		return store.Memory{}, err
	}
	tags, err := normalizeTags(in.Tags)
	if err != nil {
		return store.Memory{}, err
	}
	return store.Memory{
		SessionID: sessionID,
		Project:   project,
		Kind:      kind,
		Title:     title,
		Content:   content,
		Tags:      tags,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func optionalKind(kind string) (string, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		return "", nil
	}
	if !validKind(kind) {
		return "", inputErr("unknown kind %q (want %s)", kind, strings.Join(Kinds(), ", "))
	}
	return kind, nil
}

func validKind(kind string) bool {
	for _, k := range Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func validateProject(project string) error {
	if err := rejectNUL("project", project); err != nil {
		return err
	}
	if runeLen(project) > maxProjectRunes {
		return inputErr("project exceeds %d characters", maxProjectRunes)
	}
	if strings.ContainsAny(project, "\r\n") {
		return inputErr("project must be a single line")
	}
	return nil
}

func validateSession(id string) error {
	if id == "" {
		return nil
	}
	if runeLen(id) > maxSessionRunes {
		return inputErr("session_id exceeds %d characters", maxSessionRunes)
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ':' {
			continue
		}
		return inputErr("session_id %q has unsupported characters", id)
	}
	return nil
}

func normalizeTags(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if tag == "" {
			continue
		}
		if runeLen(tag) > maxTagRunes {
			return nil, inputErr("tag %q exceeds %d characters", tag, maxTagRunes)
		}
		if strings.ContainsAny(tag, ",\r\n\t ") || strings.ContainsRune(tag, 0) {
			return nil, inputErr("tag %q cannot contain spaces, commas, or control characters", tag)
		}
		for _, r := range tag {
			if unicode.IsControl(r) {
				return nil, inputErr("tag %q cannot contain control characters", tag)
			}
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
		if len(out) > maxTags {
			return nil, inputErr("at most %d tags", maxTags)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func rejectNUL(name, v string) error {
	if strings.ContainsRune(v, 0) {
		return inputErr("%s contains a NUL byte", name)
	}
	return nil
}

func runeLen(s string) int { return len([]rune(s)) }

func clamp(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
