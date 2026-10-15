// Package mcp implements the Model Context Protocol over stdio for atmo-mem.
//
// Messages are newline-delimited JSON-RPC 2.0. stdout is reserved for protocol
// messages; logs go to the injected logger.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/bayuh85/atmo-mem/internal/memory"
)

const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeNotInitialized = -32002

	maxMessageBytes = 8 << 20

	defaultProtocol = "2025-03-26"
)

// Server serves memory tools over an MCP stdio session.
type Server struct {
	Mem     *memory.Service
	Version string
	Log     *log.Logger

	initialized bool
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcErrorBody) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErrorBody   `json:"error,omitempty"`
}

// Serve reads requests until in reaches EOF or ctx is cancelled between messages.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReaderSize(in, 64*1024)
	writer := bufio.NewWriterSize(out, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := readMessage(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		resp := s.dispatch(ctx, line)
		if resp == nil {
			continue
		}
		if err := writeMessage(writer, resp); err != nil {
			return err
		}
	}
}

func (s *Server) dispatch(ctx context.Context, line []byte) (resp *response) {
	defer func() {
		if rec := recover(); rec != nil {
			s.logf("panic: %v", rec)
			resp = &response{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &rpcErrorBody{Code: codeInternal, Message: "internal error"},
			}
		}
	}()

	if len(line) == 0 {
		return nil
	}
	if line[0] == '[' {
		return errorResponse(nil, codeInvalidRequest, "batches are not supported")
	}

	var req request
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&req); err != nil {
		return errorResponse(nil, codeParseError, "parse error")
	}
	// Reject a second JSON value on the same line.
	if dec.More() {
		return errorResponse(idOrNull(req.ID), codeParseError, "parse error")
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return errorResponse(idOrNull(req.ID), codeInvalidRequest, "jsonrpc must be \"2.0\"")
	}
	if req.Method == "" {
		return errorResponse(req.ID, codeInvalidRequest, "method is required")
	}

	result, rpcErr, respond := s.handle(ctx, req)
	if !respond {
		return nil
	}
	if rpcErr != nil {
		return &response{JSONRPC: "2.0", ID: idOrNull(req.ID), Error: rpcErr}
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcErrorBody, bool) {
	switch req.Method {
	case "initialize":
		if !hasID(req.ID) {
			return nil, &rpcErrorBody{Code: codeInvalidRequest, Message: "initialize requires an id"}, true
		}
		result, err := s.initialize(req.Params)
		if err != nil {
			return nil, &rpcErrorBody{Code: codeInvalidParams, Message: err.Error()}, true
		}
		s.initialized = true
		return result, nil, true
	case "notifications/initialized", "initialized":
		s.initialized = true
		return nil, nil, false
	case "ping":
		if !hasID(req.ID) {
			return nil, nil, false
		}
		return map[string]any{}, nil, true
	case "notifications/cancelled":
		return nil, nil, false
	default:
		if strings.HasPrefix(req.Method, "notifications/") {
			return nil, nil, false
		}
	}

	if !s.initialized && req.Method != "ping" {
		if !hasID(req.ID) {
			return nil, nil, false
		}
		return nil, &rpcErrorBody{Code: codeNotInitialized, Message: "server not initialized"}, true
	}

	switch req.Method {
	case "tools/list":
		return map[string]any{"tools": s.toolDefs()}, nil, true
	case "tools/call":
		result, err := s.callTool(ctx, req.Params)
		if err != nil {
			var body *rpcErrorBody
			if errors.As(err, &body) {
				return nil, body, true
			}
			return nil, &rpcErrorBody{Code: codeInternal, Message: err.Error()}, true
		}
		return result, nil, true
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil, true
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil, true
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil, true
	case "logging/setLevel":
		return map[string]any{}, nil, true
	default:
		if !hasID(req.ID) {
			return nil, nil, false
		}
		return nil, &rpcErrorBody{Code: codeMethodNotFound, Message: "method not found: " + req.Method}, true
	}
}

func (s *Server) initialize(params json.RawMessage) (map[string]any, error) {
	var body struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if len(bytes.TrimSpace(params)) > 0 && !bytes.Equal(bytes.TrimSpace(params), []byte("null")) {
		if err := json.Unmarshal(params, &body); err != nil {
			return nil, err
		}
	}
	proto := body.ProtocolVersion
	if !supportedProtocol(proto) {
		proto = defaultProtocol
	}
	version := s.Version
	if version == "" {
		version = "dev"
	}
	if body.ClientInfo.Name != "" {
		s.logf("client %s %s", body.ClientInfo.Name, body.ClientInfo.Version)
	}
	return map[string]any{
		"protocolVersion": proto,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    "atmo-mem",
			"version": version,
			"title":   "atmo-mem",
		},
		"instructions": instructions,
	}, nil
}

func supportedProtocol(v string) bool {
	switch v {
	case "2024-11-05", "2025-03-26", "2025-06-18":
		return true
	default:
		return false
	}
}

const instructions = `atmo-mem stores durable project memory across coding sessions.
- memory_recall: load memories relevant to the current task. Call this at the start of work. An empty query returns recent project memory, with the latest summary pinned first.
- memory_save: record a decision, bugfix, preference, fact, task, or observation that would be expensive to rediscover.
- memory_search: keyword search. Longer words are prefix-matched, so "auth" finds "authentication".
- memory_list, memory_get, memory_delete: browse or remove memories.
- session_summarize: compress the session into a summary memory. This calls the configured LLM and needs ATMO_MEM_API_KEY. Save, search, and recall stay offline.
Pass project and session_id when you know them. ATMO_MEM_PROJECT is the default project.`

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

func errorResponse(id json.RawMessage, code int, message string) *response {
	return &response{
		JSONRPC: "2.0",
		ID:      idOrNull(id),
		Error:   &rpcErrorBody{Code: code, Message: message},
	}
}

func hasID(id json.RawMessage) bool {
	s := bytes.TrimSpace(id)
	return len(s) > 0 && !bytes.Equal(s, []byte("null"))
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if hasID(id) {
		return id
	}
	return json.RawMessage("null")
}

func readMessage(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		frag, err := r.ReadSlice('\n')
		if len(buf)+len(frag) > maxMessageBytes {
			return nil, fmt.Errorf("mcp message exceeds %d bytes", maxMessageBytes)
		}
		buf = append(buf, frag...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		trimmed := bytes.TrimSpace(buf)
		if len(trimmed) == 0 && err == nil {
			buf = buf[:0]
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(trimmed) == 0 {
				return nil, io.EOF
			}
			if errors.Is(err, io.EOF) && len(trimmed) > 0 {
				return trimmed, nil
			}
			return nil, err
		}
		return trimmed, nil
	}
}

func writeMessage(w *bufio.Writer, resp *response) error {
	payload, err := marshalCompact(resp)
	if err != nil {
		return err
	}
	if bytes.Contains(payload, []byte("\n")) {
		return errors.New("mcp response contained a newline")
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}

func marshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func marshalPretty(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}
