package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bayuh85/atmo-mem/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mem.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return st
}

func TestSaveGetSearchDeleteRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	when := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	auth, err := st.Save(ctx, store.Memory{
		SessionID: "ses_1",
		Project:   "demo",
		Kind:      "decision",
		Title:     "authentication middleware",
		Content:   "JWT checks live in the gateway.",
		Tags:      []string{"auth", "jwt"},
		CreatedAt: when,
		UpdatedAt: when,
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth.ID == "" || auth.CreatedAt.UnixMilli() != when.UnixMilli() {
		t.Fatalf("saved memory %+v", auth)
	}

	other, err := st.Save(ctx, store.Memory{
		Project:   "demo",
		Kind:      "fact",
		Title:     "logging format",
		Content:   "The authentication middleware is mentioned only in passing.",
		CreatedAt: when.Add(-time.Hour),
		UpdatedAt: when.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(ctx, store.Memory{
		Project:   "other",
		Kind:      "decision",
		Title:     "authentication middleware",
		Content:   "This belongs to another project.",
		CreatedAt: when,
		UpdatedAt: when,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(ctx, store.Memory{
		Project:   "demo",
		Kind:      "bugfix",
		Title:     "größe umlaut",
		Content:   "Unicode round trip.",
		CreatedAt: when,
		UpdatedAt: when,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.Get(ctx, auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != auth.Title || len(got.Tags) != 2 || got.Tags[0] != "auth" {
		t.Fatalf("get %+v", got)
	}

	hits, err := st.Search(ctx, store.BuildMatch("authentication middleware", store.MatchAll), "demo", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected at least 2 hits, got %d", len(hits))
	}
	if hits[0].Memory.ID != auth.ID {
		t.Fatalf("title match should rank first, got %q (score %v) before %q", hits[0].Memory.Title, hits[0].Score, hits[1].Memory.Title)
	}
	for _, h := range hits {
		if h.Memory.Project != "demo" {
			t.Fatalf("project leak: %+v", h.Memory)
		}
		if h.Memory.ID == other.ID && hits[0].Memory.ID == other.ID {
			t.Fatal("content-only match outranked the title")
		}
	}

	kindHits, err := st.Search(ctx, store.BuildMatch("authentication", store.MatchAll), "demo", "fact", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(kindHits) != 1 || kindHits[0].Memory.ID != other.ID {
		t.Fatalf("kind filter %+v", kindHits)
	}

	uni, err := st.Search(ctx, store.BuildMatch("größe", store.MatchAll), "demo", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(uni) != 1 {
		t.Fatalf("unicode search returned %d", len(uni))
	}

	tagHits, err := st.Search(ctx, store.BuildMatch("jwt", store.MatchAll), "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tagHits) != 1 || tagHits[0].Memory.ID != auth.ID {
		t.Fatalf("tag search %+v", tagHits)
	}

	if err := st.Delete(ctx, auth.ID); err != nil {
		t.Fatal(err)
	}
	_, err = st.Get(ctx, auth.ID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	left, err := st.Search(ctx, store.BuildMatch("jwt", store.MatchAll), "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("fts still has deleted memory: %+v", left)
	}
	if err := st.Delete(ctx, auth.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestFTSCountMatchesRows(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := st.Save(ctx, store.Memory{
			Kind: "fact", Title: fmt.Sprintf("row %d", i), Content: "body",
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.CountFTS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("fts count %d", n)
	}
}

func TestReopenPersistsAndMigrationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "mem.db")
	ctx := context.Background()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := st.Save(ctx, store.Memory{Kind: "fact", Title: "kept", Content: "across process"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.Get(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "across process" {
		t.Fatalf("content %q", got.Content)
	}
	hits, err := st.Search(ctx, store.BuildMatch("kept", store.MatchAll), "", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits %d", len(hits))
	}
}

func TestListOrderAndSessionSummary(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if _, err := st.Save(ctx, store.Memory{
			Project:   "demo",
			SessionID: "ses_a",
			Kind:      "observation",
			Title:     fmt.Sprintf("n%d", i),
			Content:   "x",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := st.List(ctx, "demo", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Title != "n2" || list[1].Title != "n1" {
		t.Fatalf("list %+v", list)
	}
	sessMem, err := st.BySession(ctx, "ses_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessMem) != 3 || sessMem[0].Title != "n0" {
		t.Fatalf("session order %+v", sessMem)
	}
	ended := base.Add(time.Hour)
	if err := st.SetSessionSummary(ctx, "ses_a", "demo", "shipped", ended); err != nil {
		t.Fatal(err)
	}
	sess, err := st.GetSession(ctx, "ses_a")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Summary != "shipped" || sess.EndedAt == nil || !sess.EndedAt.Equal(ended) {
		t.Fatalf("session %+v", sess)
	}
	if _, err := st.GetSession(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
}

func TestQuotesInContentAreSearchable(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if _, err := st.Save(ctx, store.Memory{
		Kind:    "fact",
		Title:   `He said "ship it"`,
		Content: "quote 'ok' and backslash \\ end",
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := st.Search(ctx, store.BuildMatch("ship", store.MatchAll), "", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits %d", len(hits))
	}
}

func TestConcurrentSave(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const n = 20
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.Save(ctx, store.Memory{
				Kind:    "fact",
				Title:   fmt.Sprintf("note %d", i),
				Content: "concurrent body",
			})
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	list, err := st.List(ctx, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n {
		t.Fatalf("saved %d", len(list))
	}
	fts, err := st.CountFTS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fts != n {
		t.Fatalf("fts %d", fts)
	}
}
