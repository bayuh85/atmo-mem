// Package cli is the atmo-mem command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/bayuh85/atmo-mem/internal/config"
	"github.com/bayuh85/atmo-mem/internal/llm"
	"github.com/bayuh85/atmo-mem/internal/mcp"
	"github.com/bayuh85/atmo-mem/internal/memory"
	"github.com/bayuh85/atmo-mem/internal/store"
	"github.com/bayuh85/atmo-mem/internal/version"
)

const usage = `atmo-mem is persistent memory for coding agents.

Usage:
  atmo-mem                 Start the MCP server when stdin is not a terminal
  atmo-mem mcp             Start the MCP server on stdio
  atmo-mem save            Save a memory
  atmo-mem search <query>  Full-text search
  atmo-mem recall [query]  Rank memories for a task
  atmo-mem list            List recent memories
  atmo-mem get <id>        Show one memory
  atmo-mem delete <id>     Delete one memory
  atmo-mem summarize       Summarize notes with the configured LLM
  atmo-mem version         Print version and active config
  atmo-mem help            Show this help

Save, search, and recall work offline. Summarize calls an OpenAI-compatible
chat completions API (default https://atmorouter.dev/v1, model zai/glm-5.3).

Environment:
  ATMO_MEM_API_KEY     API key for summarization
  ATMO_MEM_BASE_URL    Base URL including /v1 (default https://atmorouter.dev/v1)
  ATMO_MEM_MODEL       Model id (default zai/glm-5.3)
  ATMO_MEM_DB          SQLite path (default ~/.atmo-mem/atmo-mem.db)
  ATMO_MEM_PROJECT     Default project name
  ATMO_MEM_TIMEOUT     Summarization timeout (default 60s)

A .env file in the working directory fills any of these that are still empty.
OPENAI_API_KEY, OPENAI_BASE_URL, and OPENAI_MODEL are fallbacks.
Flags come before positional arguments.

Run "atmo-mem <command> -h" for command flags.
`

// Run executes one invocation. args does not include the program name.
func Run(args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) == 0 {
		if isTerminal(in) {
			fmt.Fprint(out, usage)
			return 0
		}
		return cmdMCP(nil, in, out, errOut)
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return 0
	case "version", "-v", "--version":
		return cmdVersion(out, errOut)
	case "mcp", "serve":
		return cmdMCP(args[1:], in, out, errOut)
	case "save":
		return cmdSave(args[1:], out, errOut)
	case "search":
		return cmdSearch(args[1:], out, errOut)
	case "recall":
		return cmdRecall(args[1:], out, errOut)
	case "list":
		return cmdList(args[1:], out, errOut)
	case "get":
		return cmdGet(args[1:], out, errOut)
	case "delete":
		return cmdDelete(args[1:], out, errOut)
	case "summarize":
		return cmdSummarize(args[1:], in, out, errOut)
	default:
		fmt.Fprintf(errOut, "unknown command %q\n\n%s", args[0], usage)
		return 1
	}
}

func cmdVersion(out, errOut io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(errOut, "config: %v\n", err)
		return 1
	}
	key := "missing"
	if cfg.APIKey != "" {
		key = "set"
	}
	project := cfg.Project
	if project == "" {
		project = "(none)"
	}
	fmt.Fprintf(out, "atmo-mem %s\ndatabase: %s\nllm: %s @ %s\napi key: %s\nproject: %s\n",
		version.Version, cfg.DBPath, cfg.Model, cfg.BaseURL, key, project)
	return 0
}

func cmdMCP(args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbPath := fs.String("db", "", "SQLite database path")
	fs.Usage = func() {
		fmt.Fprint(out, "Usage: atmo-mem mcp [--db path]\n\nServe the MCP memory tools on stdin/stdout.\n")
	}
	if code, ok := parse(fs, args); !ok {
		return code
	}
	svc, st, err := openService(*dbPath)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	defer st.Close()
	srv := &mcp.Server{
		Mem:     svc,
		Version: version.Version,
		Log:     log.New(errOut, "atmo-mem: ", 0),
	}
	if err := srv.Serve(context.Background(), in, out); err != nil {
		fmt.Fprintf(errOut, "mcp: %v\n", err)
		return 1
	}
	return 0
}

func cmdSave(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbPath := fs.String("db", "", "SQLite database path")
	title := fs.String("title", "", "memory title")
	content := fs.String("content", "", "memory body")
	kind := fs.String("kind", "", "observation, decision, bugfix, preference, fact, summary, or task")
	tags := fs.String("tags", "", "comma-separated labels")
	project := fs.String("project", "", "project name")
	session := fs.String("session", "", "session id")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprint(out, "Usage: atmo-mem save --title T --content C [--kind K] [--tags a,b] [--project P] [--session S] [--db path] [--json]\n")
	}
	if code, ok := parse(fs, args); !ok {
		return code
	}
	svc, st, err := openService(*dbPath)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	defer st.Close()
	mem, err := svc.Save(context.Background(), memory.SaveInput{
		Title:     *title,
		Content:   *content,
		Kind:      *kind,
		Tags:      splitComma(*tags),
		Project:   *project,
		SessionID: *session,
	})
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	if *asJSON {
		return printJSON(out, errOut, mem)
	}
	fmt.Fprintf(out, "saved %s [%s] %s\n", mem.ID, mem.Kind, mem.Title)
	return 0
}

func cmdSearch(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbPath := fs.String("db", "", "SQLite database path")
	project := fs.String("project", "", "project name")
	kind := fs.String("kind", "", "kind filter")
	limit := fs.Int("limit", 0, "maximum hits")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprint(out, "Usage: atmo-mem search [--project P] [--kind K] [--limit N] [--db path] [--json] <query>\n")
	}
	if code, ok := parse(fs, args); !ok {
		return code
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		fmt.Fprintln(errOut, "query is required")
		return 1
	}
	svc, st, err := openService(*dbPath)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	defer st.Close()
	mems, err := svc.Search(context.Background(), memory.SearchInput{
		Query: query, Project: *project, Kind: *kind, Limit: *limit,
	})
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	if *asJSON {
		return printJSON(out, errOut, map[string]any{"memories": mems})
	}
	printMemoryList(out, mems)
	return 0
}

func cmdRecall(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbPath := fs.String("db", "", "SQLite database path")
	project := fs.String("project", "", "project name")
	limit := fs.Int("limit", 0, "maximum memories")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprint(out, "Usage: atmo-mem recall [--project P] [--limit N] [--db path] [--json] [query]\n")
	}
	if code, ok := parse(fs, args); !ok {
		return code
	}
	svc, st, err := openService(*dbPath)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	defer st.Close()
	res, err := svc.Recall(context.Background(), memory.RecallInput{
		Query:   strings.Join(fs.Args(), " "),
		Project: *project,
		Limit:   *limit,
	})
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	if *asJSON {
		return printJSON(out, errOut, res)
	}
	fmt.Fprint(out, res.Context)
	if !strings.HasSuffix(res.Context, "\n") {
		fmt.Fprintln(out)
	}
	return 0
}

func cmdList(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbPath := fs.String("db", "", "SQLite database path")
	project := fs.String("project", "", "project name")
	kind := fs.String("kind", "", "kind filter")
	limit := fs.Int("limit", 0, "maximum rows")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprint(out, "Usage: atmo-mem list [--project P] [--kind K] [--limit N] [--db path] [--json]\n")
	}
	if code, ok := parse(fs, args); !ok {
		return code
	}
	svc, st, err := openService(*dbPath)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	defer st.Close()
	mems, err := svc.List(context.Background(), memory.ListInput{
		Project: *project, Kind: *kind, Limit: *limit,
	})
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	if *asJSON {
		return printJSON(out, errOut, map[string]any{"memories": mems})
	}
	printMemoryList(out, mems)
	return 0
}

func cmdGet(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbPath := fs.String("db", "", "SQLite database path")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { fmt.Fprint(out, "Usage: atmo-mem get [--db path] [--json] <id>\n") }
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(errOut, "memory id is required")
		return 1
	}
	svc, st, err := openService(*dbPath)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	defer st.Close()
	mem, err := svc.Get(context.Background(), fs.Arg(0))
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	if *asJSON {
		return printJSON(out, errOut, mem)
	}
	fmt.Fprintf(out, "id: %s\nkind: %s\nproject: %s\nsession: %s\ncreated: %s\ntags: %s\ntitle: %s\n\n%s\n",
		mem.ID, mem.Kind, dash(mem.Project), dash(mem.SessionID),
		mem.CreatedAt.UTC().Format(time.RFC3339), dash(strings.Join(mem.Tags, ", ")),
		mem.Title, mem.Content)
	return 0
}

func cmdDelete(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbPath := fs.String("db", "", "SQLite database path")
	fs.Usage = func() { fmt.Fprint(out, "Usage: atmo-mem delete [--db path] <id>\n") }
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(errOut, "memory id is required")
		return 1
	}
	svc, st, err := openService(*dbPath)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	defer st.Close()
	if err := svc.Delete(context.Background(), fs.Arg(0)); err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	fmt.Fprintf(out, "deleted %s\n", fs.Arg(0))
	return 0
}

func cmdSummarize(args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbPath := fs.String("db", "", "SQLite database path")
	notes := fs.String("notes", "", "session notes")
	file := fs.String("file", "", "read session notes from a file")
	project := fs.String("project", "", "project name")
	session := fs.String("session", "", "session id")
	title := fs.String("title", "", "summary title")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprint(out, "Usage: atmo-mem summarize [--notes TEXT | --file PATH] [--session S] [--project P] [--title T] [--db path] [--json]\n")
	}
	if code, ok := parse(fs, args); !ok {
		return code
	}
	transcript, err := readNotes(in, *notes, *file)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	svc, st, err := openService(*dbPath)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	defer st.Close()
	res, err := svc.Summarize(context.Background(), memory.SummarizeInput{
		Transcript: transcript,
		SessionID:  *session,
		Project:    *project,
		Title:      *title,
	})
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	if *asJSON {
		return printJSON(out, errOut, res)
	}
	fmt.Fprintf(out, "saved %s [%s] %s\n\n%s\n", res.Memory.ID, res.Memory.Kind, res.Memory.Title, res.Summary)
	return 0
}

func openService(dbOverride string) (*memory.Service, *store.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(dbOverride) != "" {
		cfg.DBPath = dbOverride
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	client := llm.New(cfg.BaseURL, cfg.APIKey, cfg.Model, "atmo-mem/"+version.Version, cfg.Timeout)
	return memory.New(st, client, cfg.Project), st, nil
}

func parse(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 1, false
	}
	return 0, true
}

func printJSON(out, errOut io.Writer, v any) int {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(errOut, "encode json: %v\n", err)
		return 1
	}
	return 0
}

func printMemoryList(out io.Writer, mems []store.Memory) {
	if len(mems) == 0 {
		fmt.Fprintln(out, "no memories found")
		return
	}
	for i, m := range mems {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "%s  %s  %s  %s\n  %s\n",
			m.ID, m.Kind, m.CreatedAt.UTC().Format("2006-01-02"), m.Title, preview(m.Content, 160))
	}
}

func preview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

func readNotes(in io.Reader, notes, file string) (string, error) {
	if file != "" && strings.TrimSpace(notes) != "" {
		return "", errors.New("pass notes with --notes or --file, not both")
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if strings.TrimSpace(notes) != "" {
		return notes, nil
	}
	if f, ok := in.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeCharDevice != 0 {
			return "", errors.New("nothing to summarize; pass --notes, --file, or pipe a transcript")
		}
	}
	b, err := io.ReadAll(in)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", errors.New("nothing to summarize; pass --notes, --file, or pipe a transcript")
	}
	return string(b), nil
}

func splitComma(s string) []string {
	if strings.TrimSpace(s) == "" {
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

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func isTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
