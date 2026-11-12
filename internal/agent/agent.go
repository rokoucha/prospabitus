// Package agent isolates product-specific discovery roots and transcript parsing.
package agent

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

type Record = map[string]any

type Event struct {
	Line      int     `json:"line"`
	Timestamp *string `json:"timestamp"`
	Role      string  `json:"role"`
	Text      string  `json:"text"`
	Kind      string  `json:"kind"`
	Origin    string  `json:"origin"`
	Parent    *string `json:"parent"`
}

type Adapter struct {
	Name  string
	Roots func() []string
	Parse func([]Record, string) (Record, []Event, error)
}

var Adapters = []Adapter{
	{"codex", codexRoots, parseCodex},
	{"claude-code", claudeRoots, parseClaude},
}

func Lookup(name string) (Adapter, bool) {
	for _, a := range Adapters {
		if a.Name == name {
			return a, true
		}
	}
	return Adapter{}, false
}

func home() string { h, _ := os.UserHomeDir(); return h }
func envPath(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func codexRoots() []string {
	base := envPath("CODEX_HOME", filepath.Join(home(), ".codex"))
	return []string{filepath.Join(base, "sessions"), filepath.Join(base, "archived_sessions")}
}
func claudeRoots() []string {
	return []string{filepath.Join(envPath("CLAUDE_CONFIG_DIR", filepath.Join(home(), ".claude")), "projects")}
}

// Decode consumes only complete newline-delimited records, with no Scanner line limit.
func Decode(data []byte) ([]byte, int, []Record, error) {
	end := bytes.LastIndexByte(data, '\n') + 1
	raw, pending := data[:end], len(data)-end
	if len(raw) == 0 {
		return raw, pending, nil, nil
	}
	if !utf8.Valid(raw) {
		return nil, pending, nil, fmt.Errorf("log is not valid UTF-8")
	}
	lines := bytes.Split(raw, []byte{'\n'})
	records := make([]Record, 0, len(lines)-1)
	for i, line := range lines[:len(lines)-1] {
		var r Record
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		if !json.Valid(line) {
			return nil, pending, nil, fmt.Errorf("line %d: invalid JSON object", i+1)
		}
		if err := decoder.Decode(&r); err != nil {
			return nil, pending, nil, fmt.Errorf("line %d: invalid JSON object: %w", i+1, err)
		}
		if r == nil {
			return nil, pending, nil, fmt.Errorf("line %d: JSONL records must be objects", i+1)
		}
		records = append(records, r)
	}
	return raw, pending, records, nil
}
func obj(v any) Record { m, _ := v.(map[string]any); return m }
func str(v any) string { s, _ := v.(string); return s }
func value(m Record, key, fallback string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return fallback
}
func nullable(v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("expected string or null")
	}
	return &s, nil
}
func textBlocks(c any) string {
	if s, ok := c.(string); ok {
		return s
	}
	var parts []string
	for _, b := range blocks(c) {
		switch str(b["type"]) {
		case "text", "Text", "input_text", "output_text":
			if s, ok := b["text"].(string); ok {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}
func blocks(c any) []Record {
	var out []Record
	if list, ok := c.([]any); ok {
		for _, v := range list {
			if b := obj(v); b != nil {
				out = append(out, b)
			}
		}
	}
	return out
}
func makeEvent(line int, r Record, role, text, kind, origin string, parent any) (Event, error) {
	ts, err := nullable(r["timestamp"])
	if err != nil {
		return Event{}, fmt.Errorf("timestamp: %w", err)
	}
	p, err := nullable(parent)
	if err != nil {
		return Event{}, fmt.Errorf("parent: %w", err)
	}
	return Event{line, ts, role, text, kind, origin, p}, nil
}
func Compact(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

type mirror struct {
	role, text, turn string
	parts            []string
	used             bool
}

func parseCodex(records []Record, _ string) (Record, []Event, error) {
	var meta Record
	for _, r := range records {
		if r["type"] == "session_meta" {
			meta = obj(r["payload"])
			break
		}
	}
	if meta == nil {
		return nil, nil, fmt.Errorf("Codex session_meta with session ID is required")
	}
	native := str(meta["id"])
	if native == "" {
		native = str(meta["session_id"])
	}
	if native == "" {
		return nil, nil, fmt.Errorf("session ID must be a nonempty string")
	}
	source := meta["source"]
	if source == nil {
		source = "unknown"
	}
	originator, ok := meta["originator"].(string)
	if meta["originator"] != nil && !ok {
		return nil, nil, fmt.Errorf("originator must be a string")
	}
	client := "unknown"
	if strings.Contains(strings.ToLower(originator), "desktop") {
		client = "desktop"
	} else if source == "cli" || source == "exec" {
		client = "cli"
	}
	internal := false
	if m := obj(source); m != nil {
		_, internal = m["subagent"]
	}
	info := Record{"native_id": native, "cwd": meta["cwd"], "client": client, "source": source, "version": meta["cli_version"], "parent": meta["parent_thread_id"], "agent": "codex", "internal": internal}
	result := []Event{}
	mirrors := []mirror{}
	add := func(n int, r Record, role, text, kind, origin string) error {
		e, err := makeEvent(n, r, role, text, kind, origin, nil)
		if err == nil {
			result = append(result, e)
		}
		return err
	}
	for n, r := range records {
		p := obj(r["payload"])
		if r["type"] != "response_item" || p == nil {
			continue
		}
		switch str(p["type"]) {
		case "message":
			role := value(p, "role", "unknown")
			if p["role"] != nil {
				if _, ok := p["role"].(string); !ok {
					return nil, nil, fmt.Errorf("role must be a string")
				}
			}
			text := textBlocks(p["content"])
			kind, origin := "message", "unknown"
			if role == "assistant" {
				origin = "agent"
			}
			provenance := obj(p["internal_chat_message_metadata_passthrough"])
			kinds, ok := provenance["content_item_kinds"].([]any)
			if provenance["content_item_kinds"] != nil && !ok {
				return nil, nil, fmt.Errorf("content_item_kinds must be an array")
			}
			if role == "user" && len(kinds) > 0 {
				heartbeat, user, unknown := false, false, false
				for _, k := range kinds {
					s, ok := k.(string)
					if !ok {
						return nil, nil, fmt.Errorf("content_item_kinds must contain strings")
					}
					heartbeat = heartbeat || s == "user.heartbeat"
					user = user || strings.HasPrefix(s, "user.")
					unknown = unknown || s == "unknown"
				}
				if heartbeat {
					kind, origin = "notification", "automation"
				} else if !user && !unknown {
					kind, origin = "context", "system"
				}
			}
			if err := add(n, r, role, text, kind, origin); err != nil {
				return nil, nil, err
			}
			m := mirror{role: role, text: text, turn: str(provenance["turn_id"])}
			for _, b := range blocks(p["content"]) {
				if t, ok := b["text"].(string); ok {
					m.parts = append(m.parts, t)
				}
			}
			mirrors = append(mirrors, m)
		case "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
			if err := add(n, r, "tool", Compact(p), "tool", "tool"); err != nil {
				return nil, nil, err
			}
		}
	}
	for n, r := range records {
		p := obj(r["payload"])
		if r["type"] != "event_msg" || p["type"] != "item_completed" {
			continue
		}
		item := obj(p["item"])
		t := str(item["type"])
		if t != "UserMessage" && t != "AgentMessage" {
			continue
		}
		role, origin := "user", "unknown"
		if t == "AgentMessage" {
			role, origin = "assistant", "agent"
		}
		text := textBlocks(item["content"])
		if text == "" {
			continue
		}
		turn := str(p["turn_id"])
		match := -1
		for i, m := range mirrors {
			if m.used || m.role != role || (m.turn != "" && m.turn != turn) {
				continue
			}
			same := m.text == text
			for _, part := range m.parts {
				same = same || part == text
			}
			if !same {
				continue
			}
			if match < 0 {
				match = i
			}
			if m.turn != "" && m.turn == turn {
				match = i
				break
			}
		}
		if match >= 0 {
			mirrors[match].used = true
		} else if err := add(n, r, role, text, "message", origin); err != nil {
			return nil, nil, err
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Line < result[j].Line })
	return info, result, nil
}

func isUUID(s string) bool {
	// Claude's native filenames are canonical UUIDs (not arbitrary export names).
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	return err == nil
}
func parseClaude(records []Record, path string) (Record, []Event, error) {
	candidates := []Record{}
	for _, r := range records {
		t := str(r["type"])
		if (t == "user" || t == "assistant" || t == "system") && r["sessionId"] != nil && r["sessionId"] != "" {
			if _, ok := r["sessionId"].(string); !ok {
				return nil, nil, fmt.Errorf("Claude sessionId must be a string")
			}
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("Claude session record with sessionId is required")
	}
	native := str(candidates[0]["sessionId"])
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	subagent := false
	for _, p := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		subagent = subagent || p == "subagents"
	}
	if !subagent && isUUID(stem) {
		native = stem
	}
	var meta Record
	inherited := []string{}
	seen := map[string]bool{}
	for _, r := range candidates {
		id := str(r["sessionId"])
		if id == native && meta == nil {
			meta = r
		}
		if id != native && !seen[id] {
			inherited = append(inherited, id)
			seen[id] = true
		}
	}
	entry := value(meta, "entrypoint", "unknown")
	client := "unknown"
	if entry == "claude-desktop" {
		client = "desktop"
	} else if entry == "cli" {
		client = "cli"
	}
	var parent any
	sid := native
	if subagent {
		sid = native + "/" + stem
		parent = native
	}
	info := Record{"native_id": sid, "cwd": meta["cwd"], "client": client, "source": entry, "version": meta["version"], "parent": parent, "inherited_session_ids": inherited, "agent": "claude-code", "internal": subagent || meta["isSidechain"] == true}
	result := []Event{}
	for n, r := range records {
		role := str(r["type"])
		if role != "user" && role != "assistant" {
			continue
		}
		message := obj(r["message"])
		if message == nil {
			return nil, nil, fmt.Errorf("Claude message must be an object")
		}
		content := message["content"]
		text := textBlocks(content)
		if text != "" {
			current := r["sessionId"] == nil || r["sessionId"] == native
			context := !current || r["isMeta"] == true || r["isCompactSummary"] == true
			kind, origin := "message", "unknown"
			if role == "assistant" {
				origin = "agent"
			}
			if context {
				kind = "context"
				if current {
					origin = "system"
				}
			}
			e, err := makeEvent(n, r, role, text, kind, origin, r["parentUuid"])
			if err != nil {
				return nil, nil, err
			}
			result = append(result, e)
		}
		for _, b := range blocks(content) {
			if b["type"] == "tool_use" || b["type"] == "tool_result" {
				e, err := makeEvent(n, r, "tool", Compact(content), "tool", "tool", r["parentUuid"])
				if err != nil {
					return nil, nil, err
				}
				result = append(result, e)
				break
			}
		}
	}
	return info, result, nil
}
