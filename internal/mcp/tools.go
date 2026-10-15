package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/atmorouter/atmo-mem/internal/memory"
	"github.com/atmorouter/atmo-mem/internal/store"
)

type tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Call        func(ctx context.Context, args json.RawMessage) (any, error)
}

func (s *Server) toolDefs() []map[string]any {
	tools := s.tools()
	out := make([]map[string]any, len(tools))
	for i, t := range tools {
		out[i] = map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.Schema,
		}
	}
	return out
}

func (s *Server) tools() []tool {
	return []tool{
		{
			Name:        "memory_save",
			Description: "Store a durable memory for future sessions. Use for decisions, bugfixes, preferences, facts, tasks, and observations that would be expensive to rediscover.",
			Schema: obj(map[string]any{
				"title":      str("Short label. Specific names beat vague titles."),
				"content":    str("The fact, decision, or observation itself."),
				"kind":       kindProp("Memory kind. Defaults to observation."),
				"tags":       tagsProp(),
				"project":    str("Project name. Defaults to ATMO_MEM_PROJECT when set."),
				"session_id": str("Optional session id (letters, digits, and . _ : -)."),
			}, []string{"title", "content"}),
			Call: s.callSave,
		},
		{
			Name:        "memory_search",
			Description: "Keyword search over saved memories. Words of 3 or more characters are prefix-matched, so \"auth\" finds \"authentication\". All words must match. Use memory_recall when you want ranked context for a task.",
			Schema: obj(map[string]any{
				"query":   str("Search text."),
				"project": str("Limit to one project. Defaults to ATMO_MEM_PROJECT."),
				"kind":    kindProp("Limit to one kind."),
				"limit":   intProp("Maximum hits. Default 10, maximum 50."),
			}, []string{"query"}),
			Call: s.callSearch,
		},
		{
			Name:        "memory_recall",
			Description: "Retrieve memories relevant to a task, ranked for the current project. Call this at the start of a session and before work that may depend on earlier decisions. An empty query returns recent project memory. The newest summary is included even when it does not match the query.",
			Schema: obj(map[string]any{
				"query":   str("What you are about to work on. May be empty."),
				"project": str("Project name. Defaults to ATMO_MEM_PROJECT."),
				"limit":   intProp("Maximum memories. Default 8, maximum 30."),
			}, nil),
			Call: s.callRecall,
		},
		{
			Name:        "memory_list",
			Description: "List recent memories, newest first.",
			Schema: obj(map[string]any{
				"project": str("Limit to one project. Defaults to ATMO_MEM_PROJECT."),
				"kind":    kindProp("Limit to one kind."),
				"limit":   intProp("Maximum rows. Default 20, maximum 100."),
			}, nil),
			Call: s.callList,
		},
		{
			Name:        "memory_get",
			Description: "Fetch one memory by id.",
			Schema:      obj(map[string]any{"id": str("Memory id returned by memory_save.")}, []string{"id"}),
			Call:        s.callGet,
		},
		{
			Name:        "memory_delete",
			Description: "Permanently delete one memory by id.",
			Schema:      obj(map[string]any{"id": str("Memory id to delete.")}, []string{"id"}),
			Call:        s.callDelete,
		},
		{
			Name:        "session_summarize",
			Description: "Compress session notes and memories already saved for that session into a summary memory, using the configured OpenAI-compatible LLM. Requires ATMO_MEM_API_KEY. Save, search, and recall do not.",
			Schema: obj(map[string]any{
				"transcript": str("Session notes or a transcript excerpt."),
				"session_id": str("When set, memories saved with this id are included and the session record is updated."),
				"project":    str("Project name. Defaults to ATMO_MEM_PROJECT."),
				"title":      str("Optional title. Defaults to a dated session summary title."),
			}, nil),
			Call: s.callSummarize,
		},
	}
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, error) {
	var body struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := unmarshalParams(params, &body); err != nil {
		return nil, &rpcErrorBody{Code: codeInvalidParams, Message: err.Error()}
	}
	if body.Name == "" {
		return nil, &rpcErrorBody{Code: codeInvalidParams, Message: "tool name is required"}
	}
	if s.Mem == nil {
		return nil, errors.New("memory service is not configured")
	}
	for _, t := range s.tools() {
		if t.Name != body.Name {
			continue
		}
		result, err := t.Call(ctx, body.Arguments)
		if err != nil {
			var rpc *rpcErrorBody
			if errors.As(err, &rpc) {
				return nil, rpc
			}
			s.logf("tool %s: %v", body.Name, err)
			return toolError(err), nil
		}
		return toolOK(result), nil
	}
	return nil, &rpcErrorBody{Code: codeInvalidParams, Message: "unknown tool " + body.Name}
}

func (s *Server) callSave(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Title     string   `json:"title"`
		Content   string   `json:"content"`
		Kind      string   `json:"kind"`
		Tags      flexTags `json:"tags"`
		Project   string   `json:"project"`
		SessionID string   `json:"session_id"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if s.Mem == nil {
		return nil, errors.New("memory service is not configured")
	}
	mem, err := s.Mem.Save(ctx, memory.SaveInput{
		Title:     args.Title,
		Content:   args.Content,
		Kind:      args.Kind,
		Tags:      []string(args.Tags),
		Project:   args.Project,
		SessionID: args.SessionID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"memory": mem}, nil
}

func (s *Server) callSearch(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Query   string `json:"query"`
		Project string `json:"project"`
		Kind    string `json:"kind"`
		Limit   int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	mems, err := s.Mem.Search(ctx, memory.SearchInput{
		Query: args.Query, Project: args.Project, Kind: args.Kind, Limit: args.Limit,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"memories": nonNil(mems)}, nil
}

func (s *Server) callRecall(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Query   string `json:"query"`
		Project string `json:"project"`
		Limit   int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	res, err := s.Mem.Recall(ctx, memory.RecallInput{Query: args.Query, Project: args.Project, Limit: args.Limit})
	if err != nil {
		return nil, err
	}
	res.Memories = nonNil(res.Memories)
	return res, nil
}

func (s *Server) callList(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Project string `json:"project"`
		Kind    string `json:"kind"`
		Limit   int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	mems, err := s.Mem.List(ctx, memory.ListInput{Project: args.Project, Kind: args.Kind, Limit: args.Limit})
	if err != nil {
		return nil, err
	}
	return map[string]any{"memories": nonNil(mems)}, nil
}

func (s *Server) callGet(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	mem, err := s.Mem.Get(ctx, args.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"memory": mem}, nil
}

func (s *Server) callDelete(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := s.Mem.Delete(ctx, args.ID); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": args.ID}, nil
}

func (s *Server) callSummarize(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Transcript string `json:"transcript"`
		SessionID  string `json:"session_id"`
		Project    string `json:"project"`
		Title      string `json:"title"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	res, err := s.Mem.Summarize(ctx, memory.SummarizeInput{
		Transcript: args.Transcript,
		SessionID:  args.SessionID,
		Project:    args.Project,
		Title:      args.Title,
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func toolOK(v any) map[string]any {
	text, err := marshalPretty(v)
	if err != nil {
		text = fmt.Sprint(v)
	}
	out := map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": text,
		}},
		"isError": false,
	}
	if obj, ok := asObject(v); ok {
		out["structuredContent"] = obj
	}
	return out
}

func toolError(err error) map[string]any {
	return map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": err.Error(),
		}},
		"isError": true,
	}
}

func asObject(v any) (map[string]any, bool) {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

func unmarshalParams(raw json.RawMessage, dest any) error {
	if len(bytesTrim(raw)) == 0 {
		return errors.New("params are required")
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}

func unmarshalArgs(raw json.RawMessage, dest any) error {
	if len(bytesTrim(raw)) == 0 || string(bytesTrim(raw)) == "null" {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func bytesTrim(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(trimSpace(raw))
}

func trimSpace(b []byte) []byte {
	i := 0
	j := len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\t' || b[i] == '\r') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\t' || b[j-1] == '\r') {
		j--
	}
	return b[i:j]
}

func nonNil(mems []store.Memory) []store.Memory {
	if mems == nil {
		return []store.Memory{}
	}
	return mems
}

type flexTags []string

func (t *flexTags) UnmarshalJSON(b []byte) error {
	b = trimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*t = splitComma(s)
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("tags must be an array of strings or a comma-separated string")
	}
	*t = list
	return nil
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := trimASCII(s[start:i])
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func trimASCII(s string) string {
	i := 0
	j := len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}

func obj(props map[string]any, required []string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func kindProp(desc string) map[string]any {
	kinds := memory.Kinds()
	enum := make([]any, len(kinds))
	for i, k := range kinds {
		enum[i] = k
	}
	return map[string]any{
		"type":        "string",
		"description": desc,
		"enum":        enum,
	}
}

func tagsProp() map[string]any {
	return map[string]any{
		"description": "Short labels without spaces. An array, or a comma-separated string.",
		"oneOf": []any{
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			map[string]any{"type": "string"},
		},
	}
}
