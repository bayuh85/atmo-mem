# atmo-mem

Persistent cross-session memory for coding agents, in one static binary.

atmo-mem speaks the [Model Context Protocol](https://modelcontextprotocol.io) over stdio. An agent saves decisions, bugfixes, and facts while it works. A later session searches and recalls them from a local SQLite database. At the end of a session, atmo-mem can compress the notes into a summary with any OpenAI-compatible model.

Save, search, and recall stay on disk. Only `session_summarize` calls the network.

Inspired by [claude-mem](https://github.com/thedotmack/claude-mem) and [engram](https://github.com/Gentleman-Programming/engram).

## Install

`atmo-mem` is a single binary. SQLite is compiled in, so you do not need CGO or a system SQLite library.

### From source

```bash
go install github.com/atmorouter/atmo-mem/cmd/atmo-mem@latest
```

Put `$(go env GOPATH)/bin` on your `PATH` if it is not already.

Or build it in a checkout:

```bash
git clone https://github.com/atmorouter/atmo-mem.git
cd atmo-mem
CGO_ENABLED=0 go build -o atmo-mem ./cmd/atmo-mem
```

### Release archives

Tagged releases publish Linux, macOS, and Windows archives (`amd64` and `arm64`) through [GoReleaser](https://goreleaser.com). Download the archive for your OS, unpack `atmo-mem`, and place it on your `PATH`.

Check the binary:

```bash
atmo-mem version
```

## Configure

Settings come from flags, the environment, then a `.env` file in the working directory. The first non-empty value wins. `ATMO_MEM_*` wins over the `OPENAI_*` fallbacks.

| Variable | Default | Purpose |
| --- | --- | --- |
| `ATMO_MEM_API_KEY` | empty | Bearer token for summarization. Also accepts `OPENAI_API_KEY`. |
| `ATMO_MEM_BASE_URL` | `https://atmorouter.dev/v1` | OpenAI-compatible base URL, including `/v1`. Also accepts `OPENAI_BASE_URL`. |
| `ATMO_MEM_MODEL` | `zai/glm-5.3` | Model id sent to `/chat/completions`. Also accepts `OPENAI_MODEL`. |
| `ATMO_MEM_DB` | `~/.atmo-mem/atmo-mem.db` | SQLite file. `~/` is expanded. |
| `ATMO_MEM_PROJECT` | empty | Project used when a call omits `project`. |
| `ATMO_MEM_TIMEOUT` | `60s` | Summarization timeout. |

```bash
cp .env.example .env
# edit ATMO_MEM_API_KEY
```

The database directory is created as `0700`. The file mode is set to `0600` when the OS allows it.

## MCP setup

Run `atmo-mem` with no arguments (or `atmo-mem mcp`). It reads JSON-RPC from stdin and writes JSON-RPC to stdout. Logs go to stderr. Do not wrap it in a script that prints a banner.

Set `ATMO_MEM_PROJECT` to the repo name so memories from different trees do not mix. Pass `ATMO_MEM_API_KEY` if you want end-of-session summaries.

### Claude Code

Project file `.mcp.json`:

```json
{
  "mcpServers": {
    "atmo-mem": {
      "command": "atmo-mem",
      "args": [],
      "env": {
        "ATMO_MEM_API_KEY": "your-key",
        "ATMO_MEM_PROJECT": "my-app"
      }
    }
  }
}
```

Or register it with the CLI (flags vary slightly by Claude Code version; the JSON above is the stable form):

```bash
claude mcp add --transport stdio --scope project \
  --env ATMO_MEM_API_KEY=your-key \
  --env ATMO_MEM_PROJECT=my-app \
  atmo-mem -- atmo-mem
```

### Cursor

Project file `.cursor/mcp.json` (global config is `~/.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "atmo-mem": {
      "command": "atmo-mem",
      "args": [],
      "env": {
        "ATMO_MEM_API_KEY": "your-key",
        "ATMO_MEM_PROJECT": "my-app"
      }
    }
  }
}
```

Reload MCP servers from Cursor settings after saving the file. `atmo-mem` must be on the `PATH` Cursor uses, or set `command` to the absolute path.

### Cline

Open the Cline MCP settings (`cline_mcp_settings.json` via **MCP Servers → Configure**) and add:

```json
{
  "mcpServers": {
    "atmo-mem": {
      "command": "atmo-mem",
      "args": [],
      "env": {
        "ATMO_MEM_API_KEY": "your-key",
        "ATMO_MEM_PROJECT": "my-app"
      },
      "disabled": false,
      "autoApprove": ["memory_recall", "memory_search", "memory_list", "memory_get"]
    }
  }
}
```

Leave `memory_delete` and `session_summarize` off auto-approve so a destructive or billed call still asks.

### What the agent should do

1. At the start of a task, call `memory_recall` with the task and the project.
2. When a decision, fix, or preference lands, call `memory_save`.
3. At the end of the session, call `session_summarize` with notes or a `session_id` that was used on the way.

## Tools

| Tool | What it does |
| --- | --- |
| `memory_save` | Store `title`, `content`, optional `kind`, `tags`, `project`, `session_id`. |
| `memory_search` | AND keyword search. Words of 3+ characters are prefix-matched (`auth` finds `authentication`). |
| `memory_recall` | Rank memories for a task. An empty query returns recent project memory. The newest summary is pinned first. |
| `memory_list` | Recent memories, newest first. |
| `memory_get` | One memory by id. |
| `memory_delete` | Permanently delete one memory by id. |
| `session_summarize` | Compress a transcript and that session's memories with the LLM, then store a `summary`. |

Kinds: `observation` (default), `decision`, `bugfix`, `preference`, `fact`, `summary`, `task`.

`session_id` is optional. Use letters, digits, and `. _ : -` (80 characters max) if you want `session_summarize` to pull memories saved during that session.

## Command line

The same database is available without an agent:

```bash
atmo-mem save --project my-app --kind decision \
  --title "Use FTS5" \
  --content "Memory search is SQLite FTS5." \
  --tags sqlite,search

atmo-mem search --project my-app fts
atmo-mem recall --project my-app memory search
atmo-mem summarize --project my-app \
  --notes "We stored memories in SQLite and exposed them over MCP."
```

Flags come before positional arguments. `--json` prints a JSON object for `save`, `search`, `recall`, `list`, `get`, and `summarize`. `atmo-mem help` lists every command.

Running `atmo-mem` in a terminal with no arguments prints help. When stdin is a pipe, the same invocation starts the MCP server, which is what editor configs expect.

## How recall works

Search keeps a hit only when every query word matches, ordered by FTS5 bm25 with the title weighted above the body and tags. Recall is wider: any word can match, a small recency bonus breaks near ties, and the latest `summary` for the project is moved to the front even if it shares no words with the query. A much stronger text match still outranks a memory that is merely newer.

Common English glue words (`the`, `and`, `for`, …) are dropped when the query still has another word. Matching is case-insensitive. Very short words (under two characters) are ignored; two-character words such as `go` match exactly.

## Storage

Memories live in one SQLite file. An FTS5 index (`unicode61`) is maintained by triggers on insert and delete. There is no update call: delete the old id and save again.

Sessions are bookkeeping rows. The summary the next session actually reads is a normal memory with kind `summary`.

## Powered by AtmoRouter

Session summarization defaults to [AtmoRouter](https://atmorouter.dev). Out of the box, atmo-mem posts to `https://atmorouter.dev/v1/chat/completions` with model `zai/glm-5.3`.

Set `ATMO_MEM_API_KEY` to a key from AtmoRouter. To use another host — OpenAI, a local server, or any compatible gateway — set `ATMO_MEM_BASE_URL` (include `/v1`) and `ATMO_MEM_MODEL`. The client sends `Authorization: Bearer <key>` and a standard chat-completions body (`model`, `messages`, `temperature`, `max_tokens`).

## Development

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 go build -o atmo-mem ./cmd/atmo-mem
```

CI runs `gofmt`, `go vet`, and `go test` with `CGO_ENABLED=0`. Release binaries are described in `.goreleaser.yaml`.

`modernc.org/sqlite` supplies SQLite, including FTS5, so release builds stay pure Go. Keep `modernc.org/libc` on the version required by that SQLite module.

## License

[MIT](LICENSE)

Inspired by [claude-mem](https://github.com/thedotmack/claude-mem) and [engram](https://github.com/Gentleman-Programming/engram).
