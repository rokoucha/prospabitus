package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func decode(t *testing.T, s string) []Record {
	t.Helper()
	_, _, r, err := Decode([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestFixtures(t *testing.T) {
	paths, err := filepath.Glob("../../testdata/*.jsonl")
	if err != nil || len(paths) != 4 {
		t.Fatal(paths, err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			name := "codex"
			if strings.HasPrefix(filepath.Base(path), "claude-code") {
				name = "claude-code"
			}
			a, _ := Lookup(name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, _, records, err := Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			meta, events, err := a.Parse(records, path)
			if err != nil {
				t.Fatal(err)
			}
			expected := "cli"
			if strings.Contains(path, "desktop") {
				expected = "desktop"
			}
			if meta["client"] != expected || len(events) == 0 {
				t.Fatal(meta, events)
			}
		})
	}
}
func TestDecodeCompleteLargeAndMalformed(t *testing.T) {
	raw := []byte("{\"text\":\"" + strings.Repeat("語", 70000) + "\"}\n{\"partial\":")
	complete, pending, r, err := Decode(raw)
	if err != nil || len(r) != 1 || pending == 0 || !strings.HasSuffix(string(complete), "\n") {
		t.Fatal(len(r), pending, err)
	}
	for _, s := range []string{"null\n", "[]\n", "{\"x\":\n", "{\"x\":\"\xff\"}\n"} {
		if _, _, _, err := Decode([]byte(s)); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func TestCodexMirrorOccurrencesAndProvenance(t *testing.T) {
	records := decode(t, `{"type":"session_meta","payload":{"id":"one","source":{"subagent":{"other":"guardian"}},"originator":"Codex Desktop"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"repeat"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"repeat"}],"internal_chat_message_metadata_passthrough":{"turn_id":"A"}}}
{"type":"event_msg","payload":{"type":"item_completed","turn_id":"A","item":{"type":"UserMessage","content":[{"type":"text","text":"repeat"}]}}}
{"type":"event_msg","payload":{"type":"item_completed","turn_id":"B","item":{"type":"UserMessage","content":[{"type":"text","text":"repeat"}]}}}
{"type":"event_msg","payload":{"type":"item_completed","turn_id":"B","item":{"type":"AgentMessage","content":[{"type":"Text","text":"確認してよいですか？"}]}}}
`)
	meta, events, err := parseCodex(records, "one.jsonl")
	if err != nil || len(events) != 3 || meta["internal"] != true {
		t.Fatal(meta, events, err)
	}
	if events[2].Text != "確認してよいですか？" || events[0].Origin != "unknown" {
		t.Fatal(events)
	}
	// Similar substrings are different messages; image annotations are separate blocks.
	records = decode(t, `{"type":"session_meta","payload":{"id":"one"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"写真"},{"type":"input_text","text":"image metadata"}]}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"写真"}]}}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"真"}]}}}
`)
	_, events, err = parseCodex(records, "one.jsonl")
	if err != nil || len(events) != 2 {
		t.Fatal(events, err)
	}
}
func TestMachineContextAndTypes(t *testing.T) {
	for _, kind := range []string{"agents_md.instructions", "user.heartbeat"} {
		rs := decode(t, "{\"type\":\"session_meta\",\"payload\":{\"id\":\"one\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[],\"internal_chat_message_metadata_passthrough\":{\"content_item_kinds\":[\""+kind+"\"]}}}\n")
		_, es, err := parseCodex(rs, "")
		if err != nil || es[0].Kind == "message" {
			t.Fatal(es, err)
		}
	}
	for _, s := range []string{
		"{\"type\":\"session_meta\",\"payload\":{\"id\":123}}\n",
		"{\"type\":\"session_meta\",\"payload\":{\"id\":{}}}\n",
	} {
		if _, _, err := parseCodex(decode(t, s), ""); err == nil {
			t.Fatal("accepted bad ID")
		}
	}
	rs := decode(t, "{\"type\":\"user\",\"sessionId\":123,\"message\":{\"content\":\"bad\"}}\n")
	if _, _, err := parseClaude(rs, "11111111-2222-4333-8444-555555555555.jsonl"); err == nil {
		t.Fatal("accepted bad Claude ID")
	}
}
func TestClaudeResumedIdentityContextAndSubagent(t *testing.T) {
	rs := decode(t, `{"type":"user","sessionId":"original","entrypoint":"cli","message":{"content":"copied"}}
{"type":"assistant","sessionId":"original","message":{"content":[{"type":"text","text":"answer"}]}}
`)
	native := "11111111-2222-4333-8444-555555555555"
	path := native + ".jsonl"
	meta, events, err := parseClaude(rs, path)
	if err != nil || meta["native_id"] != native || events[0].Kind != "context" || meta["client"] != "unknown" {
		t.Fatal(meta, events, err)
	}
	rs = append(rs, Record{"type": "user", "sessionId": native, "entrypoint": "claude-desktop", "message": Record{"content": "current"}})
	meta, events, err = parseClaude(rs, path)
	if err != nil || meta["native_id"] != native || meta["client"] != "desktop" || events[2].Kind != "message" {
		t.Fatal(meta, events, err)
	}
	if !reflect.DeepEqual(meta["inherited_session_ids"], []string{"original"}) {
		t.Fatal(meta)
	}
	arbitrary, _, _ := parseClaude(rs, "export.jsonl")
	if arbitrary["native_id"] != "original" {
		t.Fatal(arbitrary)
	}
	sub, _, _ := parseClaude(rs, "/logs/subagents/agent-one.jsonl")
	if sub["native_id"] != "original/agent-one" || sub["internal"] != true {
		t.Fatal(sub)
	}
	for _, flag := range []string{"isCompactSummary", "isMeta"} {
		rs[2][flag] = true
		_, es, err := parseClaude(rs, path)
		if err != nil || es[2].Kind != "context" {
			t.Fatal(es, err)
		}
		delete(rs[2], flag)
	}
}
func TestToolProjectionPreservesJSON(t *testing.T) {
	rs := decode(t, `{"type":"user","sessionId":"one","message":{"content":[{"type":"text","text":"説明"},{"type":"tool_result","content":"結果"}]}}
`)
	_, events, err := parseClaude(rs, "export.jsonl")
	if err != nil || len(events) != 2 {
		t.Fatal(events, err)
	}
	var blocks any
	if err := json.Unmarshal([]byte(events[1].Text), &blocks); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(blocks, obj(rs[0]["message"])["content"]) {
		t.Fatal(events)
	}
}

func TestLargeToolIntegerRetainsPrecision(t *testing.T) {
	records := decode(t, `{"type":"session_meta","payload":{"id":"one"}}
{"type":"response_item","payload":{"type":"function_call","value":9007199254740993}}
`)
	_, events, err := parseCodex(records, "")
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Text, "9007199254740993") {
		t.Fatal(events, err)
	}
}
