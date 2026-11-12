// Package archive stores immutable raw snapshots and their query projection.
package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/rokoucha/prospabitus/internal/agent"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sources (agent TEXT NOT NULL, path TEXT NOT NULL, PRIMARY KEY(agent,path));
CREATE TABLE IF NOT EXISTS streams (id TEXT PRIMARY KEY,agent TEXT NOT NULL,native_id TEXT NOT NULL,active INTEGER,metadata TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS revisions (id INTEGER PRIMARY KEY AUTOINCREMENT,stream TEXT NOT NULL,digest TEXT NOT NULL,raw BLOB NOT NULL,path TEXT NOT NULL,state TEXT NOT NULL,metadata TEXT NOT NULL,created TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),UNIQUE(stream,digest));
CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY,revision INTEGER NOT NULL,line INTEGER NOT NULL,timestamp TEXT,role TEXT NOT NULL,text TEXT NOT NULL,kind TEXT NOT NULL,origin TEXT NOT NULL,parent TEXT);
CREATE INDEX IF NOT EXISTS events_revision ON events(revision,id);
CREATE TABLE IF NOT EXISTS changes (id INTEGER PRIMARY KEY AUTOINCREMENT,stream TEXT NOT NULL,revision INTEGER NOT NULL,state TEXT NOT NULL);
PRAGMA user_version=1;`

type Store struct{ db *sql.DB }

func Open(ctx context.Context, path string, readOnly bool) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !readOnly {
		if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			err = f.Close()
		} else if errors.Is(err, fs.ErrExist) {
			err = nil
		}
		if err != nil {
			return nil, err
		}
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{"_pragma": {"busy_timeout(30000)"}, "_txlock": {"immediate"}}
	if readOnly {
		q.Set("mode", "ro")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*Store, error) { _ = db.Close(); return nil, err }
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version != 0 && version != 1 {
		return fail(fmt.Errorf("unsupported database schema %d", version))
	}
	if readOnly && version != 1 {
		return fail(fmt.Errorf("archive has not been initialized"))
	}
	if !readOnly {
		if _, err := db.ExecContext(ctx, schema); err != nil {
			return fail(err)
		}
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Query(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		item := map[string]any{}
		for i, c := range cols {
			if b, ok := values[i].([]byte); ok {
				item[c] = string(b)
			} else {
				item[c] = values[i]
			}
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func (s *Store) Scalar(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}
func (s *Store) Revision(ctx context.Context, session string, revision int64) (int64, error) {
	var n int64
	var err error
	if session != "" {
		err = s.db.QueryRowContext(ctx, "SELECT active FROM streams WHERE id=?", session).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx, "SELECT id FROM revisions WHERE id=?", revision).Scan(&n)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("session or revision not found")
	}
	return n, err
}
func (s *Store) Raw(ctx context.Context, revision int64) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT raw FROM revisions WHERE id=?", revision).Scan(&raw)
	return raw, err
}

type Source struct {
	Agent string `json:"agent"`
	Path  string `json:"path"`
}

func (s *Store) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT agent,path FROM sources ORDER BY agent,path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Source{}
	for rows.Next() {
		var r Source
		if err := rows.Scan(&r.Agent, &r.Path); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) SetSources(ctx context.Context, sources []Source) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM sources"); err != nil {
		return err
	}
	for _, r := range sources {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sources VALUES (?,?)", r.Agent, r.Path); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type Import struct {
	Path           string `json:"path"`
	Session        string `json:"session,omitempty"`
	Revision       int64  `json:"revision,omitempty"`
	State          string `json:"state"`
	ActiveRevision *int64 `json:"active_revision,omitempty"`
	PendingBytes   int    `json:"pending_bytes"`
}

func (s *Store) Ingest(ctx context.Context, a agent.Adapter, path string) (Import, error) {
	result := Import{Path: path}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// Nonblocking/no-follow open also covers replacement between discovery and
	// reading. FIFOs and symlinks must never turn a scan into an unkillable wait.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return result, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return result, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return result, fmt.Errorf("source must be a regular file")
	}
	data, err := io.ReadAll(file)
	closeErr := file.Close()
	if err != nil {
		return result, err
	}
	if closeErr != nil {
		return result, closeErr
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	raw, pending, records, err := agent.Decode(data)
	if err != nil {
		return result, err
	}
	result.PendingBytes = pending
	if len(raw) == 0 {
		result.State = "pending"
		return result, nil
	}
	meta, events, err := a.Parse(records, path)
	if err != nil {
		return result, fmt.Errorf("invalid %s log: %w", a.Name, err)
	}
	native, ok := meta["native_id"].(string)
	if !ok || native == "" {
		return result, fmt.Errorf("session ID must be a nonempty string")
	}
	result.Session = a.Name + ":" + native
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])
	metadata, err := json.Marshal(meta)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var active int64
	var prior []byte
	err = tx.QueryRowContext(ctx, "SELECT r.id,r.raw FROM streams s JOIN revisions r ON r.id=s.active WHERE s.id=?", result.Session).Scan(&active, &prior)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if active != 0 {
		result.ActiveRevision = &active
	}
	var existing int64
	var state string
	err = tx.QueryRowContext(ctx, "SELECT id,state FROM revisions WHERE stream=? AND digest=?", result.Session, hash).Scan(&existing, &state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if err == nil {
		result.Revision = existing
		result.State = "unchanged"
		if state == "conflict" {
			result.State = "conflict"
		}
	} else if active != 0 && bytes.HasPrefix(prior, raw) {
		result.State = "older"
		result.Revision = active
	} else {
		result.State = "imported"
		if active != 0 && !bytes.HasPrefix(raw, prior) {
			result.State = "conflict"
		}
		row, err := tx.ExecContext(ctx, "INSERT INTO revisions(stream,digest,raw,path,state,metadata) VALUES (?,?,?,?,?,?)", result.Session, hash, raw, path, result.State, string(metadata))
		if err != nil {
			return result, err
		}
		result.Revision, err = row.LastInsertId()
		if err != nil {
			return result, err
		}
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO events(revision,line,timestamp,role,text,kind,origin,parent) VALUES (?,?,?,?,?,?,?,?)")
		if err != nil {
			return result, err
		}
		for _, e := range events {
			if _, err = stmt.ExecContext(ctx, result.Revision, e.Line, e.Timestamp, e.Role, e.Text, e.Kind, e.Origin, e.Parent); err != nil {
				_ = stmt.Close()
				return result, err
			}
		}
		if err := stmt.Close(); err != nil {
			return result, err
		}
		if result.State == "imported" {
			if _, err = tx.ExecContext(ctx, "INSERT INTO streams VALUES (?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET active=excluded.active,metadata=excluded.metadata", result.Session, a.Name, native, result.Revision, string(metadata)); err != nil {
				return result, err
			}
			result.ActiveRevision = &result.Revision
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO changes(stream,revision,state) VALUES (?,?,?)", result.Session, result.Revision, result.State); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

type SyncResult struct {
	Counts       map[string]int   `json:"counts"`
	Errors       []map[string]any `json:"errors"`
	MissingRoots []Source         `json:"missing_roots"`
	PendingFiles int              `json:"pending_files"`
	Cursor       int64            `json:"cursor"`
}

func (s *Store) Sync(ctx context.Context, roots []Source) (SyncResult, error) {
	result := SyncResult{Counts: map[string]int{"imported": 0, "unchanged": 0, "older": 0, "pending": 0, "conflict": 0}, Errors: []map[string]any{}, MissingRoots: []Source{}}
	seen := map[string]bool{}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		a, ok := agent.Lookup(root.Agent)
		if !ok {
			return result, fmt.Errorf("unsupported agent %q", root.Agent)
		}
		stat, err := os.Lstat(root.Path)
		if errors.Is(err, fs.ErrNotExist) {
			result.MissingRoots = append(result.MissingRoots, root)
			continue
		}
		if err != nil {
			result.Errors = append(result.Errors, map[string]any{"code": "source_unreadable", "path": root.Path, "message": err.Error()})
			continue
		}
		if stat.Mode()&os.ModeSymlink != 0 {
			result.Errors = append(result.Errors, map[string]any{"code": "symlink_source", "path": root.Path, "message": "source root must not be a symlink"})
			continue
		}
		if !stat.IsDir() {
			result.MissingRoots = append(result.MissingRoots, root)
			continue
		}
		var paths []string
		err = filepath.WalkDir(root.Path, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Type()&os.ModeSymlink == 0 && !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				info, err := e.Info()
				if err != nil {
					return err
				}
				if info.Mode().IsRegular() {
					paths = append(paths, path)
				}
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			result.Errors = append(result.Errors, map[string]any{"code": "source_unreadable", "path": root.Path, "message": err.Error()})
		}
		// Walk order differs from a globally sorted file list when names share prefixes.
		sort.Strings(paths)
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			canonical, err := filepath.EvalSymlinks(path)
			if err != nil {
				result.Errors = append(result.Errors, map[string]any{"code": "invalid_source", "path": path, "message": err.Error()})
				continue
			}
			key := root.Agent + "\x00" + canonical
			if seen[key] {
				continue
			}
			seen[key] = true
			r, err := s.Ingest(ctx, a, path)
			if err != nil {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				result.Errors = append(result.Errors, map[string]any{"code": "invalid_source", "path": path, "message": err.Error()})
				continue
			}
			result.Counts[r.State]++
			if r.PendingBytes > 0 {
				result.PendingFiles++
			}
			if r.State == "conflict" {
				result.Errors = append(result.Errors, map[string]any{"code": "conflict", "path": r.Path, "session": r.Session, "revision": r.Revision, "active_revision": r.ActiveRevision, "state": r.State, "pending_bytes": r.PendingBytes})
			}
		}
	}
	var err error
	result.Cursor, err = s.Scalar(ctx, "SELECT coalesce(max(id),0) FROM changes")
	return result, err
}
