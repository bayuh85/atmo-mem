package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atmorouter/atmo-mem/internal/llm"
	"github.com/atmorouter/atmo-mem/internal/mcp"
	"github.com/atmorouter/atmo-mem/internal/memory"
	"github.com/atmorouter/atmo-mem/internal/store"
)

func startServer(t *testing.T, model llm.Completer) *rpcClient {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := memory.New(st, model, "demo")
	svc.Now = func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) }

	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	srv := &mcp.Server{Mem: svc, Version: "test"}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(context.Background(), serverIn, serverOut)
	}()

	c := &rpcClient{t: t, w: clientOut, dec: json.NewDecoder(clientIn)}
	t.Cleanup(func() {
		_ = clientOut.Close()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not exit")
		}
	})
	return c
}

type rpcClient struct {
	t   *testing.T
	w   io.Writer
	dec *json.Decoder
	id  int
}

func (c *rpcClient) notify(method string, params any) {
	c.t.Helper()
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *rpcClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.id++
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	return c.recv()
}

func (c *rpcClient) sendRaw(line string) {
	c.t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(c.w, line)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			c.t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out writing")
	}
}

func (c *rpcClient) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	c.sendRaw(string(b) + "\n")
}

func (c *rpcClient) recv() map[string]any {
	c.t.Helper()
	type result struct {
		msg map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var msg map[string]any
		err := c.dec.Decode(&msg)
		ch <- result{msg, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			c.t.Fatalf("recv: %v", r.err)
		}
		return r.msg
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out waiting for response")
		return nil
	}
}

func (c *rpcClient) initialize() {
	c.t.Helper()
	res := c.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	if res["error"] != nil {
		c.t.Fatalf("initialize error: %#v", res["error"])
	}
	result, _ := res["result"].(map[string]any)
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "atmo-mem" {
		c.t.Fatalf("serverInfo %#v", info)
	}
	if result["protocolVersion"] != "2024-11-05" {
		c.t.Fatalf("protocol %#v", result["protocolVersion"])
	}
	instr, _ := result["instructions"].(string)
	if instr == "" {
		c.t.Fatal("missing instructions")
	}
	c.notify("notifications/initialized", map[string]any{})
}

func TestProtocolAndMemoryTools(t *testing.T) {
	c := startServer(t, &scriptedLLM{reply: "## Outcome\nRemember the port.\n"})

	early := c.call("tools/list", map[string]any{})
	errObj, _ := early["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("expected not-initialized error")
	}

	c.initialize()

	// A notification must not consume the next response.
	c.notify("notifications/cancelled", map[string]any{})
	pong := c.call("ping", map[string]any{})
	if pong["error"] != nil || pong["result"] == nil {
		t.Fatalf("ping %#v", pong)
	}

	listed := c.call("tools/list", map[string]any{})
	result := mustResult(t, listed)
	tools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		names[tool["name"].(string)] = true
		if tool["inputSchema"] == nil {
			t.Fatalf("tool %v missing schema", tool["name"])
		}
	}
	for _, name := range []string{"memory_save", "memory_search", "memory_recall", "memory_list", "memory_get", "memory_delete", "session_summarize"} {
		if !names[name] {
			t.Fatalf("missing tool %s in %#v", name, names)
		}
	}

	saved := mustResult(t, c.call("tools/call", map[string]any{
		"name": "memory_save",
		"arguments": map[string]any{
			"title":      "Authentication middleware",
			"content":    "JWT checks live in the gateway.\nSecond line.",
			"kind":       "decision",
			"tags":       "auth, jwt",
			"session_id": "ses_1",
		},
	}))
	if saved["isError"] == true {
		t.Fatalf("save error %#v", saved)
	}
	text, _ := contentText(saved)
	if text == "" || !json.Valid([]byte(text)) {
		t.Fatalf("save text %q", text)
	}
	var payload struct {
		Memory struct {
			ID string `json:"id"`
		} `json:"memory"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Memory.ID == "" {
		t.Fatal("missing id")
	}

	found := mustResult(t, c.call("tools/call", map[string]any{
		"name":      "memory_search",
		"arguments": map[string]any{"query": "auth gateway"},
	}))
	if !strings.Contains(contentTextMust(t, found), payload.Memory.ID) {
		t.Fatalf("search %#v", found)
	}

	recalled := mustResult(t, c.call("tools/call", map[string]any{
		"name":      "memory_recall",
		"arguments": map[string]any{"query": "jwt"},
	}))
	if !strings.Contains(contentTextMust(t, recalled), "Project memory for demo") {
		t.Fatalf("recall %#v", recalled)
	}

	got := mustResult(t, c.call("tools/call", map[string]any{
		"name":      "memory_get",
		"arguments": map[string]any{"id": payload.Memory.ID},
	}))
	if !strings.Contains(contentTextMust(t, got), "Second line.") {
		t.Fatalf("get lost the newline: %#v", got)
	}

	bad := mustResult(t, c.call("tools/call", map[string]any{
		"name":      "memory_save",
		"arguments": map[string]any{"title": ""},
	}))
	if bad["isError"] != true {
		t.Fatalf("expected tool error, got %#v", bad)
	}
	if bad["error"] != nil {
		t.Fatal("tool failure should not be a JSON-RPC error")
	}

	sum := mustResult(t, c.call("tools/call", map[string]any{
		"name": "session_summarize",
		"arguments": map[string]any{
			"session_id": "ses_1",
			"transcript": "We kept JWT checks in the gateway.",
		},
	}))
	if sum["isError"] == true {
		t.Fatalf("summarize %#v", sum)
	}
	if !strings.Contains(contentTextMust(t, sum), "Remember the port") {
		t.Fatalf("summary %#v", sum)
	}

	deleted := mustResult(t, c.call("tools/call", map[string]any{
		"name":      "memory_delete",
		"arguments": map[string]any{"id": payload.Memory.ID},
	}))
	if deleted["isError"] == true {
		t.Fatalf("delete %#v", deleted)
	}

	missing := c.call("nope", map[string]any{})
	if missing["error"] == nil {
		t.Fatal("expected method not found")
	}

	c.sendRaw("{}\n")
	invalid := c.recv()
	if invalid["error"] == nil {
		t.Fatalf("expected invalid request: %#v", invalid)
	}
	c.sendRaw("{\n")
	parsed := c.recv()
	errBody, _ := parsed["error"].(map[string]any)
	if errBody == nil {
		t.Fatalf("expected parse error: %#v", parsed)
	}
	// The session still answers after a bad line.
	if c.call("ping", map[string]any{})["result"] == nil {
		t.Fatal("server died after parse error")
	}
}

func mustResult(t *testing.T, msg map[string]any) map[string]any {
	t.Helper()
	if msg["error"] != nil {
		t.Fatalf("rpc error: %#v", msg["error"])
	}
	result, _ := msg["result"].(map[string]any)
	if result == nil {
		t.Fatalf("missing result: %#v", msg)
	}
	return result
}

func contentTextMust(t *testing.T, result map[string]any) string {
	t.Helper()
	text, ok := contentText(result)
	if !ok {
		t.Fatalf("no text content in %#v", result)
	}
	return text
}

func contentText(result map[string]any) (string, bool) {
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return "", false
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text, text != ""
}

type scriptedLLM struct {
	reply string
}

func (s *scriptedLLM) Complete(context.Context, []llm.Message) (string, error) {
	return s.reply, nil
}
