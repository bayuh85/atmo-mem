package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bayuh85/atmo-mem/internal/llm"
)

func TestCompleteSendsOpenAIShape(t *testing.T) {
	var gotPath, gotAuth, gotUA, gotType string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"## Outcome\nDone.\n"}}]}`)
	}))
	defer srv.Close()

	c := llm.New(srv.URL+"/v1/", "test-key", "zai/glm-5.3", "atmo-mem/test", 5*time.Second)
	out, err := c.Complete(context.Background(), []llm.Message{
		{Role: "system", Content: "rules"},
		{Role: "user", Content: "notes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "## Outcome\nDone.\n" {
		t.Fatalf("content %q", out)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path %s", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth %q", gotAuth)
	}
	if gotUA != "atmo-mem/test" || !strings.Contains(gotType, "application/json") {
		t.Fatalf("headers ua=%q type=%q", gotUA, gotType)
	}
	if body["model"] != "zai/glm-5.3" {
		t.Fatalf("model %#v", body["model"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages %#v", body["messages"])
	}
}

func TestCompleteContentPartsAndErrors(t *testing.T) {
	parts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]}}]}`)
	}))
	defer parts.Close()
	c := llm.New(parts.URL+"/v1", "k", "m", "", time.Second)
	out, err := c.Complete(context.Background(), []llm.Message{{Role: "user", Content: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if out != "alpha\nbeta" {
		t.Fatalf("parts %q", out)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer denied.Close()
	c = llm.New(denied.URL+"/v1", "super-secret", "zai/glm-5.3", "", time.Second)
	_, err = c.Complete(context.Background(), []llm.Message{{Role: "user", Content: "x"}})
	if err == nil {
		t.Fatal("expected status error")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked the key: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error %v", err)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `not-json`)
	}))
	defer broken.Close()
	c = llm.New(broken.URL+"/v1", "k", "m", "", time.Second)
	_, err = c.Complete(context.Background(), []llm.Message{{Role: "user", Content: "x"}})
	if err == nil || !strings.Contains(err.Error(), "not-json") {
		t.Fatalf("error %v", err)
	}
}

func TestCompleteRequiresKeyAndHonorsCancel(t *testing.T) {
	called := false
	c := llm.New("http://127.0.0.1:9/v1", "", "m", "", time.Second)
	c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("should not dial")
	})}
	_, err := c.Complete(context.Background(), []llm.Message{{Role: "user", Content: "x"}})
	if !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("err %v", err)
	}
	if called {
		t.Fatal("request was sent without a key")
	}

	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer blocked.Close()
	c = llm.New(blocked.URL+"/v1", "k", "m", "", time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Complete(ctx, []llm.Message{{Role: "user", Content: "x"}})
	if err == nil {
		t.Fatal("expected cancel error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
