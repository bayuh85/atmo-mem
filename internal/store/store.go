// Package store persists memories in SQLite and searches them with FTS5.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bayuh85/atmo-mem/internal/id"

	_ "modernc.org/sqlite" // register the pure-Go SQLite driver
)

// ErrNotFound is returned when a memory or session id does not exist.
var ErrNotFound = errors.New("not found")

// Memory is one durable note.
type Memory struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id,omitempty"`
	Project   string    `json:"project,omitempty"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Session is the bookkeeping row for one agent session.
type Session struct {
	ID        string     `json:"id"`
	Project   string     `json:"project,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Summary   string     `json:"summary,omitempty"`
}

// Hit is a full-text match. Score is the FTS5 bm25 value (more negative is better).
type Hit struct {
	Memory Memory
	Score  float64
}

// Store is a single-writer SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates the parent directory, opens (or creates) the database, and
// migrates the schema. The connection pool is one connection so writers queue
// instead of hitting SQLITE_BUSY.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}
	if strings.Contains(path, "?") {
		return nil, errors.New("database path must not contain '?'")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("database path: %w", err)
	}
	abs = filepath.Clean(abs)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	// Best-effort. Memories can contain source snippets and credentials.
	_ = os.Chmod(abs, 0o600)

	s := &Store{db: db, path: abs}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func dsn(path string) string {
	// Without a file: prefix, modernc.org/sqlite applies _pragma itself and
	// does not forward those parameters to SQLite's URI parser.
	return path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate"
}

// Path returns the absolute database path.
func (s *Store) Path() string { return s.path }

// Close checkpoints the WAL and closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	err := s.db.Close()
	s.db = nil
	if err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	return nil
}

func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	for _, stmt := range schemaStatements() {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w\nSQL: %s", err, stmt)
		}
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_meta`).Scan(&n); err != nil {
		return fmt.Errorf("migrate schema_meta: %w", err)
	}
	if n == 0 {
		if _, err := tx.Exec(`INSERT INTO schema_meta (version) VALUES (1)`); err != nil {
			return fmt.Errorf("migrate version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

// Save inserts a memory. An empty ID is generated. Zero timestamps become the current time.
func (s *Store) Save(ctx context.Context, m Memory) (Memory, error) {
	if m.ID == "" {
		gen, err := id.New("mem_")
		if err != nil {
			return Memory{}, err
		}
		m.ID = gen
	}
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.CreatedAt
	}
	m.CreatedAt = m.CreatedAt.UTC()
	m.UpdatedAt = m.UpdatedAt.UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memories (
			id, session_id, project, kind, title, content, tags, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, m.ID, m.SessionID, m.Project, m.Kind, m.Title, m.Content, encodeTags(m.Tags), m.CreatedAt.UnixMilli(), m.UpdatedAt.UnixMilli())
	if err != nil {
		return Memory{}, fmt.Errorf("save memory: %w", err)
	}
	m.Tags = decodeTags(encodeTags(m.Tags))
	return m, nil
}

// Get loads one memory by id.
func (s *Store) Get(ctx context.Context, memoryID string) (Memory, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE id = ?`, memoryID)
	m, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Memory{}, fmt.Errorf("memory %s: %w", memoryID, ErrNotFound)
	}
	if err != nil {
		return Memory{}, fmt.Errorf("get memory: %w", err)
	}
	return m, nil
}

// Delete removes a memory and its full-text row.
func (s *Store) Delete(ctx context.Context, memoryID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, memoryID)
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("memory %s: %w", memoryID, ErrNotFound)
	}
	return nil
}

// List returns recent memories, optionally filtered by project and kind.
func (s *Store) List(ctx context.Context, project, kind string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 20
	}
	mems, err := s.queryMemories(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE (? = '' OR project = ?)
		  AND (? = '' OR kind = ?)
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`, project, project, kind, kind, limit)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	return mems, nil
}

// Search runs an FTS5 query. match must come from BuildMatch.
// Results are ordered by bm25 (best first). project and kind are exact filters;
// an empty filter matches everything.
func (s *Store) Search(ctx context.Context, match, project, kind string, limit int) ([]Hit, error) {
	if strings.TrimSpace(match) == "" {
		return nil, errors.New("search match is required")
	}
	if limit <= 0 {
		limit = 10
	}
	// Weights follow column order: memory_id, title, content, tags.
	// memory_id is unindexed; title is weighted above body and tags.
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.session_id, m.project, m.kind, m.title, m.content, m.tags,
		       m.created_at, m.updated_at,
		       bm25(memories_fts, 0.0, 8.0, 1.0, 3.0) AS score
		FROM memories_fts
		JOIN memories AS m ON m.id = memories_fts.memory_id
		WHERE memories_fts MATCH ?
		  AND (? = '' OR m.project = ?)
		  AND (? = '' OR m.kind = ?)
		ORDER BY score ASC, m.created_at DESC
		LIMIT ?
	`, match, project, project, kind, kind, limit)
	if err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		var tags string
		var created, updated int64
		if err := rows.Scan(
			&h.Memory.ID, &h.Memory.SessionID, &h.Memory.Project, &h.Memory.Kind,
			&h.Memory.Title, &h.Memory.Content, &tags, &created, &updated, &h.Score,
		); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		h.Memory.Tags = decodeTags(tags)
		h.Memory.CreatedAt = time.UnixMilli(created).UTC()
		h.Memory.UpdatedAt = time.UnixMilli(updated).UTC()
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	return out, nil
}

// LatestByKind returns the newest memory of kind in project.
// An empty project searches across projects.
func (s *Store) LatestByKind(ctx context.Context, project, kind string) (Memory, bool, error) {
	mems, err := s.queryMemories(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE kind = ? AND (? = '' OR project = ?)
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, kind, project, project)
	if err != nil {
		return Memory{}, false, fmt.Errorf("latest memory: %w", err)
	}
	if len(mems) == 0 {
		return Memory{}, false, nil
	}
	return mems[0], true, nil
}

// BySession returns memories saved with sessionID, oldest first.
func (s *Store) BySession(ctx context.Context, sessionID string) ([]Memory, error) {
	mems, err := s.queryMemories(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE session_id = ?
		ORDER BY created_at ASC, id ASC
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list session memories: %w", err)
	}
	return mems, nil
}

// TouchSession inserts a session row or refreshes its project.
func (s *Store) TouchSession(ctx context.Context, sessionID, project string, started time.Time) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	if started.IsZero() {
		started = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, project, started_at, summary)
		VALUES (?, ?, ?, '')
		ON CONFLICT(id) DO UPDATE SET
			project = CASE
				WHEN excluded.project != '' THEN excluded.project
				ELSE sessions.project
			END
	`, sessionID, project, started.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// SetSessionSummary records the compressed text for a session and marks it ended.
func (s *Store) SetSessionSummary(ctx context.Context, sessionID, project, summary string, ended time.Time) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	if ended.IsZero() {
		ended = time.Now().UTC()
	}
	ended = ended.UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, project, started_at, ended_at, summary)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project = CASE
				WHEN excluded.project != '' THEN excluded.project
				ELSE sessions.project
			END,
			ended_at = excluded.ended_at,
			summary = excluded.summary
	`, sessionID, project, ended.UnixMilli(), ended.UnixMilli(), summary)
	if err != nil {
		return fmt.Errorf("set session summary: %w", err)
	}
	return nil
}

// GetSession loads session bookkeeping.
func (s *Store) GetSession(ctx context.Context, sessionID string) (Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, project, started_at, ended_at, summary
		FROM sessions WHERE id = ?
	`, sessionID)
	var sess Session
	var started int64
	var ended sql.NullInt64
	err := row.Scan(&sess.ID, &sess.Project, &started, &ended, &sess.Summary)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("session %s: %w", sessionID, ErrNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	sess.StartedAt = time.UnixMilli(started).UTC()
	if ended.Valid {
		t := time.UnixMilli(ended.Int64).UTC()
		sess.EndedAt = &t
	}
	return sess, nil
}

// CountFTS returns the number of rows in the full-text index.
func (s *Store) CountFTS(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM memories_fts`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count fts: %w", err)
	}
	return n, nil
}

func (s *Store) queryMemories(ctx context.Context, query string, args ...any) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMemory(sc scanner) (Memory, error) {
	var m Memory
	var tags string
	var created, updated int64
	if err := sc.Scan(&m.ID, &m.SessionID, &m.Project, &m.Kind, &m.Title, &m.Content, &tags, &created, &updated); err != nil {
		return Memory{}, err
	}
	m.Tags = decodeTags(tags)
	m.CreatedAt = time.UnixMilli(created).UTC()
	m.UpdatedAt = time.UnixMilli(updated).UTC()
	return m, nil
}

func encodeTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return strings.Join(tags, ",")
}

func decodeTags(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
