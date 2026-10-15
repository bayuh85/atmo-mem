package memory_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bayuh85/atmo-mem/internal/llm"
	"github.com/bayuh85/atmo-mem/internal/memory"
	"github.com/bayuh85/atmo-mem/internal/store"
)

func testService(t *testing.T, model llm.Completer) *memory.Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := memory.New(st, model, "demo")
	svc.Now = func() time.Time {
		return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	}
	return svc
}

func TestSaveSearchRecall(t *testing.T) {
	svc := testService(t, nil)
	ctx := context.Background()
	saved, err := svc.Save(ctx, memory.SaveInput{
		Title:     "Authentication middleware",
		Content:   "JWT checks live in the gateway.",
		Kind:      "Decision",
		Tags:      []string{"Auth", "auth", "jwt"},
		SessionID: "ses_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Kind != memory.KindDecision || saved.Project != "demo" || len(saved.Tags) != 2 {
		t.Fatalf("%+v", saved)
	}
	if _, err := svc.Save(ctx, memory.SaveInput{Title: "CSS", Content: "margin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(ctx, memory.SaveInput{
		Project: "other",
		Title:   "Authentication middleware",
		Content: "not this project",
		Kind:    memory.KindFact,
	}); err != nil {
		t.Fatal(err)
	}

	found, err := svc.Search(ctx, memory.SearchInput{Query: "auth middleware", Project: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != saved.ID {
		t.Fatalf("%+v", found)
	}

	recalled, err := svc.Recall(ctx, memory.RecallInput{Query: "gateway jwt", Project: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recalled.Memories) == 0 || recalled.Memories[0].ID != saved.ID {
		t.Fatalf("%+v", recalled.Memories)
	}
	if !strings.Contains(recalled.Context, "Project memory for demo") || !strings.Contains(recalled.Context, saved.ID) {
		t.Fatalf("context:\n%s", recalled.Context)
	}

	if _, err := svc.Save(ctx, memory.SaveInput{Title: " ", Content: "x"}); err == nil {
		t.Fatal("expected title error")
	}
	if _, err := svc.Save(ctx, memory.SaveInput{Title: "t", Content: "c", Kind: "nope"}); err == nil {
		t.Fatal("expected kind error")
	}
}

func TestRecallPinsLatestSummary(t *testing.T) {
	svc := testService(t, nil)
	ctx := context.Background()
	if _, err := svc.Save(ctx, memory.SaveInput{
		Kind:    memory.KindFact,
		Title:   "cookie banner",
		Content: "unrelated cookies",
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := svc.Save(ctx, memory.SaveInput{
		Kind:    memory.KindSummary,
		Title:   "Session summary",
		Content: "Shipped the indexer.",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A summary in another project must not leak.
	if _, err := svc.Save(ctx, memory.SaveInput{
		Project: "other",
		Kind:    memory.KindSummary,
		Title:   "Other summary",
		Content: "Payroll export.",
	}); err != nil {
		t.Fatal(err)
	}

	recalled, err := svc.Recall(ctx, memory.RecallInput{Query: "ziggurat", Project: "demo", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(recalled.Memories) != 1 || recalled.Memories[0].ID != summary.ID {
		t.Fatalf("pinned %+v", recalled.Memories)
	}

	empty, err := svc.Recall(ctx, memory.RecallInput{Project: "empty-proj"})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Memories) != 0 || !strings.Contains(empty.Context, "No memories stored yet") {
		t.Fatalf("%+v\n%s", empty.Memories, empty.Context)
	}
}

func TestSummarizeStoresMemoryAndSession(t *testing.T) {
	model := &scriptedLLM{reply: "```markdown\n## Outcome\nShipped the indexer.\n\n## Follow-ups\nNone.\n```"}
	svc := testService(t, model)
	ctx := context.Background()
	prior, err := svc.Save(ctx, memory.SaveInput{
		SessionID: "ses_9",
		Kind:      memory.KindDecision,
		Title:     "Use FTS5",
		Content:   "SQLite FTS5 indexes memories.",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Summarize(ctx, memory.SummarizeInput{
		SessionID:  "ses_9",
		Transcript: "We stored memories in SQLite and exposed them over MCP.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Memory.Kind != memory.KindSummary {
		t.Fatalf("kind %s", res.Memory.Kind)
	}
	if !strings.Contains(res.Summary, "## Outcome") || strings.Contains(res.Summary, "```") {
		t.Fatalf("summary %q", res.Summary)
	}
	if res.Memory.Title != "Session summary 2026-10-15 (demo)" {
		t.Fatalf("title %q", res.Memory.Title)
	}
	if len(model.got) != 2 {
		t.Fatalf("messages %d", len(model.got))
	}
	if !strings.Contains(model.got[0].Content, "## Follow-ups") || !strings.Contains(model.got[0].Content, "Do not guess") {
		t.Fatalf("system prompt drifted: %s", model.got[0].Content)
	}
	if !strings.Contains(model.got[1].Content, "exposed them over MCP") || !strings.Contains(model.got[1].Content, prior.Title) {
		t.Fatalf("user prompt %s", model.got[1].Content)
	}
	sess, err := svc.Store.GetSession(ctx, "ses_9")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Summary != res.Summary || sess.EndedAt == nil {
		t.Fatalf("session %+v", sess)
	}

	found, err := svc.Search(ctx, memory.SearchInput{Query: "indexer", Kind: memory.KindSummary})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != res.Memory.ID {
		t.Fatalf("%+v", found)
	}
}

func TestSummarizeFailuresDoNotSave(t *testing.T) {
	ctx := context.Background()
	svc := testService(t, nil)
	_, err := svc.Summarize(ctx, memory.SummarizeInput{Transcript: "notes"})
	if !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("err %v", err)
	}

	model := &scriptedLLM{err: errors.New("upstream down")}
	svc = testService(t, model)
	_, err = svc.Summarize(ctx, memory.SummarizeInput{Transcript: "notes"})
	if err == nil || !strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("err %v", err)
	}
	list, err := svc.List(ctx, memory.ListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("saved on failure: %+v", list)
	}

	model.err = nil
	model.reply = "   "
	if _, err := svc.Summarize(ctx, memory.SummarizeInput{Transcript: "notes"}); err == nil {
		t.Fatal("expected empty summary error")
	}
	if _, err := svc.Summarize(ctx, memory.SummarizeInput{}); err == nil {
		t.Fatal("expected nothing to summarize")
	}
}

func TestRankScorePrefersRelevance(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	older := now.Add(-30 * 24 * time.Hour)
	newer := now.Add(-time.Hour)
	strongOld := memory.RankScore(-10, older, now)
	weakNew := memory.RankScore(-0.1, newer, now)
	if strongOld <= weakNew {
		t.Fatalf("strong old %v should beat weak new %v", strongOld, weakNew)
	}
	sameOld := memory.RankScore(-2, older, now)
	sameNew := memory.RankScore(-2, newer, now)
	if sameNew <= sameOld {
		t.Fatalf("newer %v should beat older %v when bm25 ties", sameNew, sameOld)
	}
}

func TestDeleteAndGet(t *testing.T) {
	svc := testService(t, nil)
	ctx := context.Background()
	saved, err := svc.Save(ctx, memory.SaveInput{Title: "temp", Content: "gone soon"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, saved.ID)
	if err != nil || got.Content != "gone soon" {
		t.Fatalf("%v %+v", err, got)
	}
	if err := svc.Delete(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, saved.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if err := svc.Delete(ctx, ""); err == nil {
		t.Fatal("expected id error")
	}
}

type scriptedLLM struct {
	reply string
	err   error
	got   []llm.Message
}

func (s *scriptedLLM) Complete(_ context.Context, messages []llm.Message) (string, error) {
	s.got = append([]llm.Message(nil), messages...)
	if s.err != nil {
		return "", s.err
	}
	return s.reply, nil
}
