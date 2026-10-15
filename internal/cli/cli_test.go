package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atmorouter/atmo-mem/internal/cli"
)

func isolateEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"ATMO_MEM_DB", "ATMO_MEM_PROJECT", "ATMO_MEM_BASE_URL", "ATMO_MEM_API_KEY",
		"ATMO_MEM_MODEL", "ATMO_MEM_TIMEOUT", "OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_MODEL",
	} {
		t.Setenv(key, "")
	}
}

func TestHelpVersionAndUnknown(t *testing.T) {
	isolateEnv(t)
	var out, errOut bytes.Buffer
	if code := cli.Run([]string{"help"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("help code %d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "atmo-mem recall") && !strings.Contains(out.String(), "atmo-mem recall [query]") {
		t.Fatalf("help missing recall:\n%s", out.String())
	}

	out.Reset()
	if code := cli.Run([]string{"version"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("version: %s", errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "atmo-mem") || !strings.Contains(text, "https://atmorouter.dev/v1") || !strings.Contains(text, "zai/glm-5.3") {
		t.Fatalf("version output:\n%s", text)
	}
	if strings.Contains(text, "sk-") {
		t.Fatalf("version leaked a key:\n%s", text)
	}
	if !strings.Contains(text, "api key: missing") {
		t.Fatalf("version output:\n%s", text)
	}

	errOut.Reset()
	if code := cli.Run([]string{"nope"}, strings.NewReader(""), &out, &errOut); code == 0 {
		t.Fatal("expected unknown command to fail")
	}
}

func TestCLIRoundTrip(t *testing.T) {
	isolateEnv(t)
	db := filepath.Join(t.TempDir(), "mem.db")
	var out, errOut bytes.Buffer
	code := cli.Run([]string{
		"save", "--db", db, "--json",
		"--title", "Use FTS5",
		"--content", "SQLite FTS5 indexes memories.",
		"--kind", "decision",
		"--tags", "sqlite,search",
		"--project", "atmo",
		"--session", "ses_cli",
	}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("save: %s", errOut.String())
	}
	var saved struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" {
		t.Fatalf("output %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	code = cli.Run([]string{"search", "--db", db, "--project", "atmo", "--json", "fts"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("search: %s", errOut.String())
	}
	if !strings.Contains(out.String(), saved.ID) {
		t.Fatalf("search output %s", out.String())
	}

	out.Reset()
	code = cli.Run([]string{"recall", "--db", db, "--project", "atmo", "memory index"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("recall: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "Project memory for atmo") || !strings.Contains(out.String(), "Use FTS5") {
		t.Fatalf("recall output:\n%s", out.String())
	}

	out.Reset()
	code = cli.Run([]string{"get", "--db", db, saved.ID}, strings.NewReader(""), &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "SQLite FTS5") {
		t.Fatalf("get code %d\n%s\n%s", code, out.String(), errOut.String())
	}

	out.Reset()
	code = cli.Run([]string{"delete", "--db", db, saved.ID}, strings.NewReader(""), &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "deleted "+saved.ID) {
		t.Fatalf("delete %d %s %s", code, out.String(), errOut.String())
	}

	out.Reset()
	errOut.Reset()
	code = cli.Run([]string{"save", "--db", db, "--content", "missing title"}, strings.NewReader(""), &out, &errOut)
	if code == 0 || !strings.Contains(errOut.String(), "title") {
		t.Fatalf("expected title error, code %d err %s", code, errOut.String())
	}
}

func TestCLISummarize(t *testing.T) {
	isolateEnv(t)
	var gotModel string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"## Outcome\nShipped the indexer.\n"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("ATMO_MEM_API_KEY", "test-key")
	t.Setenv("ATMO_MEM_BASE_URL", srv.URL+"/v1")
	t.Setenv("ATMO_MEM_MODEL", "zai/glm-5.3")

	db := filepath.Join(t.TempDir(), "mem.db")
	var out, errOut bytes.Buffer
	code := cli.Run([]string{
		"summarize", "--db", db, "--json",
		"--project", "demo",
		"--notes", "We stored memories in SQLite and exposed them over MCP.",
	}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("summarize: %s", errOut.String())
	}
	if gotAuth != "Bearer test-key" || gotModel != "zai/glm-5.3" {
		t.Fatalf("auth %q model %q", gotAuth, gotModel)
	}
	if !strings.Contains(out.String(), "Shipped the indexer") {
		t.Fatalf("output %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	code = cli.Run([]string{"search", "--db", db, "--project", "demo", "--kind", "summary", "indexer"}, strings.NewReader(""), &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "summary") {
		t.Fatalf("search code %d\n%s\n%s", code, out.String(), errOut.String())
	}
}

func TestMCPCommand(t *testing.T) {
	isolateEnv(t)
	db := filepath.Join(t.TempDir(), "mem.db")
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	errOut := &bytes.Buffer{}
	done := make(chan int, 1)
	go func() {
		done <- cli.Run([]string{"mcp", "--db", db}, serverIn, serverOut, errOut)
	}()

	enc := json.NewEncoder(clientOut)
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "cli", "version": "0"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(clientIn)
	var initResp map[string]any
	if err := dec.Decode(&initResp); err != nil {
		t.Fatal(err)
	}
	if initResp["error"] != nil {
		t.Fatalf("initialize %#v stderr %s", initResp, errOut.String())
	}
	_ = clientOut.Close()
	code := <-done
	if code != 0 {
		t.Fatalf("mcp exit %d stderr %s", code, errOut.String())
	}
}
