package archive

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/rokoucha/prospabitus/internal/agent"
)

// Explicit opt-in: CI has no access to private logs. The database is read-only.
func TestExistingArchiveProjectionParity(t *testing.T) {
	path := os.Getenv("PROSPABITUS_PARITY_ARCHIVE")
	if path == "" {
		t.Skip("set PROSPABITUS_PARITY_ARCHIVE to check an existing Python archive read-only")
	}
	ctx := context.Background()
	s, err := Open(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	revisions, err := s.Query(ctx, "SELECT id,agent,path,metadata FROM revisions JOIN (SELECT id AS sid,agent FROM streams) s ON s.sid=revisions.stream ORDER BY revisions.id")
	if err != nil {
		t.Fatal(err)
	}
	eventsChecked := 0
	toolFormattingChanges := 0
	for _, r := range revisions {
		raw, err := s.Raw(ctx, r["id"].(int64))
		if err != nil {
			t.Fatal(err)
		}
		_, pending, records, err := agent.Decode(raw)
		if err != nil || pending != 0 {
			t.Fatalf("revision %v decode: %v", r["id"], err)
		}
		a, ok := agent.Lookup(r["agent"].(string))
		if !ok {
			t.Fatal("unsupported agent")
		}
		meta, events, err := a.Parse(records, r["path"].(string))
		if err != nil {
			t.Fatalf("revision %v parse: %v", r["id"], err)
		}
		var expectedMeta map[string]any
		if err := json.Unmarshal([]byte(r["metadata"].(string)), &expectedMeta); err != nil {
			t.Fatal(err)
		}
		// Normalize Go integer types in metadata through JSON, not text fields.
		encoded, _ := json.Marshal(meta)
		var normalized map[string]any
		_ = json.Unmarshal(encoded, &normalized)
		if !reflect.DeepEqual(normalized, expectedMeta) {
			t.Fatalf("revision %v metadata differs", r["id"])
		}
		stored, err := s.Query(ctx, "SELECT line,timestamp,role,text,kind,origin,parent FROM events WHERE revision=? ORDER BY id", r["id"])
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != len(stored) {
			t.Fatalf("revision %v: %d events, expected %d", r["id"], len(events), len(stored))
		}
		for i, e := range events {
			b, _ := json.Marshal(e)
			var actual map[string]any
			_ = json.Unmarshal(b, &actual)
			want := stored[i]
			want["line"] = float64(want["line"].(int64))
			if e.Kind == "tool" && actual["text"] != want["text"] {
				var x, y any
				errX := json.Unmarshal([]byte(e.Text), &x)
				errY := json.Unmarshal([]byte(want["text"].(string)), &y)
				if errX != nil || errY != nil || !reflect.DeepEqual(x, y) {
					t.Fatalf("revision %v event %d tool content differs", r["id"], i)
				}
				// Only JSON formatting can change for newly projected tool bodies. Existing
				// SQLite event text, offsets and IDs are left untouched by the Go CLI.
				actual["text"] = want["text"]
				toolFormattingChanges++
			}
			if !reflect.DeepEqual(actual, want) {
				var keys []string
				for k := range actual {
					if !reflect.DeepEqual(actual[k], want[k]) {
						keys = append(keys, k)
					}
				}
				t.Fatalf("revision %v event %d differs: %s", r["id"], i, strings.Join(keys, ","))
			}
			eventsChecked++
		}
	}
	t.Logf("verified %d revisions and %d events; %d tool bodies differ only in JSON formatting", len(revisions), eventsChecked, toolFormattingChanges)
}
