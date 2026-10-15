package store

// memoryColumns is the persisted memory projection, in scan order.
const memoryColumns = "id, session_id, project, kind, title, content, tags, created_at, updated_at"

func schemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS schema_meta (
			version INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS memories (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL DEFAULT '',
			project TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			title TEXT NOT NULL,
			content TEXT NOT NULL,
			tags TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			project TEXT NOT NULL DEFAULT '',
			started_at INTEGER NOT NULL,
			ended_at INTEGER,
			summary TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_project_created
			ON memories (project, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_session
			ON memories (session_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_kind_created
			ON memories (kind, created_at DESC)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
			memory_id UNINDEXED,
			title,
			content,
			tags,
			tokenize = 'unicode61'
		)`,
		// Inserts and deletes keep the FTS index aligned. There is no update
		// path: callers delete and save again. rowid is copied so deletes can
		// target the FTS row directly.
		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts (rowid, memory_id, title, content, tags)
			VALUES (new.rowid, new.id, new.title, new.content, new.tags);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
			DELETE FROM memories_fts WHERE rowid = old.rowid;
		END`,
	}
}
