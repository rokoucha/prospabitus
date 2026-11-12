package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rokoucha/prospabitus/internal/agent"
)

const initial = `{"type":"session_meta","payload":{"id":"one","source":"cli"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"承認して。日本語で"}]}}
`
const extra = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"承認して。日本語で"}]}}
`

func setup(t *testing.T) (*Store, string, string) {
	t.Helper()
	root := t.TempDir()
	db := filepath.Join(root, "state", "archive.sqlite3")
	s, err := Open(context.Background(), db, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	logs := filepath.Join(root, "logs")
	if err := os.Mkdir(logs, 0700); err != nil {
		t.Fatal(err)
	}
	return s, filepath.Join(logs, "session.jsonl"), db
}
func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
}
func count(t *testing.T, s *Store, table string) int64 {
	t.Helper()
	n, err := s.Scalar(context.Background(), "SELECT count(*) FROM "+table)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestSnapshotsPartialConflictsAndDeletion(t *testing.T) {
	ctx := context.Background()
	s, path, db := setup(t)
	a, _ := agent.Lookup("codex")
	write(t, path, initial)
	first, err := s.Ingest(ctx, a, path)
	if err != nil || first.State != "imported" {
		t.Fatal(first, err)
	}
	same, err := s.Ingest(ctx, a, path)
	if err != nil || same.State != "unchanged" {
		t.Fatal(same, err)
	}
	write(t, path, initial+extra+`{"partial":`)
	next, err := s.Ingest(ctx, a, path)
	if err != nil || next.PendingBytes == 0 || next.State != "imported" {
		t.Fatal(next, err)
	}
	if n, _ := s.Scalar(ctx, "SELECT count(*) FROM events WHERE revision=?", next.Revision); n != 2 {
		t.Fatal(n)
	}
	write(t, path, initial+extra+"broken\n")
	if _, err := s.Ingest(ctx, a, path); err == nil {
		t.Fatal("accepted malformed row")
	}
	if count(t, s, "changes") != 2 {
		t.Fatal("advanced cursor on parse failure")
	}
	write(t, path, initial)
	old, err := s.Ingest(ctx, a, path)
	if err != nil || old.State != "unchanged" {
		t.Fatal(old, err)
	}
	write(t, path, strings.ReplaceAll(initial, "承認", "変更"))
	conflict, err := s.Ingest(ctx, a, path)
	if err != nil || conflict.State != "conflict" || *conflict.ActiveRevision != next.Revision {
		t.Fatal(conflict, err)
	}
	again, err := s.Ingest(ctx, a, path)
	if err != nil || again.Revision != conflict.Revision || count(t, s, "changes") != 3 {
		t.Fatal(again, err)
	}
	raw, err := s.Raw(ctx, first.Revision)
	if err != nil || string(raw) != initial {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if count(t, s, "streams") != 1 {
		t.Fatal("source deletion propagated")
	}
	mode, err := os.Stat(db)
	if err != nil || mode.Mode().Perm() != 0600 {
		t.Fatal(mode, err)
	}
}
func TestSyncContinuesAfterBadFilesAndSkipsLinks(t *testing.T) {
	ctx := context.Background()
	s, path, _ := setup(t)
	write(t, path, initial)
	root := filepath.Dir(path)
	write(t, filepath.Join(root, "a-bad.jsonl"), "{\"type\":\"session_meta\",\"payload\":{\"id\":123}}\n")
	if err := os.Symlink(path, filepath.Join(root, "linked.jsonl")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Sync(ctx, []Source{{"codex", root}, {"codex", root + "-missing"}})
	if err != nil || r.Counts["imported"] != 1 || len(r.Errors) != 1 || len(r.MissingRoots) != 1 {
		t.Fatal(r, err)
	}
	link := root + "-link"
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	r, err = s.Sync(ctx, []Source{{"codex", link}})
	if err != nil || len(r.Errors) != 1 || r.Errors[0]["code"] != "symlink_source" {
		t.Fatal(r, err)
	}
}
func TestConcurrentWritersAndCancellation(t *testing.T) {
	ctx := context.Background()
	s, path, db := setup(t)
	write(t, path, initial)
	other, err := Open(ctx, db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	a, _ := agent.Lookup("codex")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func(s *Store) { defer wg.Done(); _, err := s.Ingest(ctx, a, path); errs <- err }(store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count(t, s, "changes") != 1 {
		t.Fatal("concurrent duplicate")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, initial+extra)
	timeout, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err = other.Ingest(timeout, a, path)
	if err == nil {
		tx.Rollback()
		t.Fatal("ignored lock/cancel")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if count(t, s, "changes") != 1 {
		t.Fatal("canceled transaction committed")
	}
	if _, err := other.Ingest(ctx, a, path); err != nil {
		t.Fatal(err)
	}
	if count(t, s, "changes") != 2 {
		t.Fatal("retry did not recover")
	}
	stopped, stop := context.WithCancel(ctx)
	stop()
	if _, err := s.Sync(stopped, []Source{{"codex", filepath.Dir(path)}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestClaudeIdentityStableAcrossAppends(t *testing.T) {
	ctx := context.Background()
	s, path, _ := setup(t)
	path = filepath.Join(filepath.Dir(path), "11111111-2222-4333-8444-555555555555.jsonl")
	a, _ := agent.Lookup("claude-code")
	inherited := "{\"type\":\"user\",\"sessionId\":\"original\",\"message\":{\"content\":\"copied\"}}\n"
	write(t, path, inherited)
	first, err := s.Ingest(ctx, a, path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, inherited+"{\"type\":\"user\",\"sessionId\":\"11111111-2222-4333-8444-555555555555\",\"message\":{\"content\":\"current\"}}\n")
	next, err := s.Ingest(ctx, a, path)
	if err != nil || first.Session != next.Session || count(t, s, "streams") != 1 {
		t.Fatal(first, next, err)
	}
	n, err := s.Scalar(ctx, "SELECT count(*) FROM events WHERE revision=? AND kind='message'", next.Revision)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestSpecialFilesNeverBlockIngestion(t *testing.T) {
	ctx := context.Background()
	s, path, _ := setup(t)
	write(t, path, initial)
	fifo := filepath.Join(filepath.Dir(path), "a-fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	r, err := s.Sync(deadline, []Source{{"codex", filepath.Dir(path)}})
	if err != nil || r.Counts["imported"] != 1 || len(r.Errors) != 0 {
		t.Fatal(r, err)
	}
	a, _ := agent.Lookup("codex")
	done := make(chan error, 1)
	go func() { _, err := s.Ingest(deadline, a, fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO")
		}
	case <-time.After(250 * time.Millisecond):
		// Unblock a regressed blocking reader so the failing test can still finish.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
		t.Fatal("FIFO blocked Ingest")
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingest(ctx, a, link); err == nil {
		t.Fatal("followed source symlink")
	}
}
