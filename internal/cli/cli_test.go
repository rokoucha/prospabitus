package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func invoke(t *testing.T, data string, want int, args ...string) map[string]any {
	t.Helper()
	var out, err bytes.Buffer
	a := App{&out, &err}
	argv := append([]string{"--data-dir", data}, args...)
	if code := a.Run(context.Background(), argv); code != want {
		t.Fatalf("args %v: exit %d want %d, stdout=%s stderr=%s", args, code, want, out.String(), err.String())
	}
	var result map[string]any
	if e := json.Unmarshal(out.Bytes(), &result); e != nil {
		t.Fatalf("invalid JSON: %s (%v)", out.String(), e)
	}
	return result
}
func items(r map[string]any) []any { return r["items"].([]any) }
func TestCLIArchiveQueriesAndExport(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	if err := os.Mkdir(logs, 0700); err != nil {
		t.Fatal(err)
	}
	content := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"one\",\"source\":\"cli\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"承認・日本語🦀\"}]}}\n"
	if err := os.WriteFile(filepath.Join(logs, "one.jsonl"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	internal := strings.Replace(content, `"id":"one","source":"cli"`, `"id":"internal","source":{"subagent":"review"}`, 1)
	if err := os.WriteFile(filepath.Join(logs, "internal.jsonl"), []byte(internal), 0600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	r := invoke(t, data, 0, "sync", "--source", "codex="+logs)
	if r["ok"] != true {
		t.Fatal(r)
	}
	r = invoke(t, data, 0, "sync")
	if r["counts"].(map[string]any)["unchanged"] != float64(2) {
		t.Fatal(r)
	}
	if len(items(invoke(t, data, 0, "sources"))) != 1 {
		t.Fatal("sources not persisted")
	}
	if len(items(invoke(t, data, 0, "sessions", "list"))) != 1 || len(items(invoke(t, data, 0, "sessions", "list", "--include-internal"))) != 2 {
		t.Fatal("internal filtering failed")
	}
	r = invoke(t, data, 0, "search", "承認", "--agent", "codex", "--role", "user")
	if len(items(r)) != 1 {
		t.Fatal(r)
	}
	id := items(r)[0].(map[string]any)["id"].(float64)
	r = invoke(t, data, 0, "context", number(id))
	if len(items(r)) != 1 {
		t.Fatal(r)
	}
	r = invoke(t, data, 0, "event", number(id), "--offset", "6")
	if r["text"] != "🦀" || r["text_length"] != float64(7) {
		t.Fatal(r)
	}
	if len(items(invoke(t, data, 0, "search", "承認", "--include-internal"))) != 2 {
		t.Fatal("include-internal failed")
	}
	r = invoke(t, data, 0, "changes", "--limit", "1")
	if r["next_cursor"] == nil {
		t.Fatal(r)
	}
	r = invoke(t, data, 0, "changes", "--after", number(r["next_cursor"].(float64)))
	if len(items(r)) != 1 {
		t.Fatal(r)
	}
	r = invoke(t, data, 0, "sessions", "show", "codex:one")
	rev := number(r["active"].(float64))
	if len(items(invoke(t, data, 0, "events", "--revision", rev))) != 1 {
		t.Fatal("event retrieval")
	}
	out := filepath.Join(root, "export.jsonl")
	invoke(t, data, 0, "export", "--revision", rev, "--output", out)
	raw, err := os.ReadFile(out)
	if err != nil || string(raw) != content {
		t.Fatal(err)
	}
	invoke(t, data, 1, "export", "--revision", rev, "--output", out)
	r = invoke(t, data, 0, "--profile", "work", "status")
	if r["sessions"] != float64(0) {
		t.Fatal(r)
	}
	for _, args := range [][]string{{"--profile", "../escape", "status"}, {"events", "--revision", "999"}, {"events"}, {"events", "--revision", "1", "--session", "one"}, {"changes", "--after", "-1"}, {"search", "承認", "--limit", "0"}, {"search", "承認", "--limit", "101"}, {"context", "1", "--radius", "0"}, {"event", "1", "--offset", "-1"}} {
		invoke(t, data, 1, args...)
	}
}
func number(n float64) string { b, _ := json.Marshal(n); return string(b) }
func TestTextPaginationUnicodeAndOverflow(t *testing.T) {
	text := strings.Repeat("語🦀", 5000)
	r := map[string]any{"text": text}
	clipText(r, 0, 4000)
	if len([]rune(r["text"].(string))) != 4000 || r["next_text_offset"] != int64(4000) {
		t.Fatal(r["next_text_offset"])
	}
	r = map[string]any{"text": text}
	clipText(r, 1<<63-1, 8000)
	if r["text"] != "" || r["next_text_offset"] != nil {
		t.Fatal(r)
	}
}
func TestExportNoOverwriteAndCleanup(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "original")
	if err := os.WriteFile(output, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := exportRaw(output, []byte("replacement")); err == nil {
		t.Fatal("overwrote file")
	}
	raw, _ := os.ReadFile(output)
	if string(raw) != "keep" {
		t.Fatal(string(raw))
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	if err := exportRaw(filepath.Join(root, "missing", "file"), []byte("data")); err == nil {
		t.Fatal("expected failure")
	}
}
func TestHelpDoctorAndEmbeddedSkill(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	data := t.TempDir()
	r := invoke(t, data, 0, "doctor")
	if len(r["sources"].([]any)) != 3 {
		t.Fatal(r)
	}
	r = invoke(t, data, 0, "skill", "show")
	if !strings.Contains(r["text"].(string), "prospabitus") {
		t.Fatal(r)
	}
	for _, args := range [][]string{{"--help"}, {"sync", "--help"}, {"version"}, {"--version"}} {
		var out, err bytes.Buffer
		code := (App{&out, &err}).Run(context.Background(), args)
		if code != 0 || out.Len() == 0 {
			t.Fatal(args, code, out.String(), err.String())
		}
	}
}

func TestBareTildeAndHomeSubdirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{"~": home, "~/sessions": filepath.Join(home, "sessions")} {
		actual, err := expand(input)
		if err != nil || actual != want {
			t.Fatalf("%q => %q, want %q (%v)", input, actual, want, err)
		}
	}
}
