package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/rokoucha/prospabitus/assets"
	"github.com/rokoucha/prospabitus/internal/agent"
	"github.com/rokoucha/prospabitus/internal/archive"
)

var Version = "dev"
var Commit = "unknown"

// App keeps process exit and output separate, so the actual CLI can be tested.
type App struct{ Stdout, Stderr io.Writer }

func New() App { return App{os.Stdout, os.Stderr} }

type pageArgs struct {
	After int64 `default:"0" help:"Last ID from the previous page."`
	Limit int   `default:"20" help:"Page size, 1–100."`
}
type scopeArgs struct {
	Session  string `xor:"scope" help:"Session ID."`
	Revision *int64 `xor:"scope" help:"Immutable revision ID."`
}
type listCommand struct {
	Agent           string `enum:"codex,claude-code," default:"" help:"Filter agent."`
	IncludeInternal bool   `help:"Include approval reviews and subagents."`
	After           string `help:"Session ID cursor."`
	Limit           int    `default:"20"`
}
type searchCommand struct {
	Text            string `arg:"" help:"Case-sensitive literal substring."`
	Session         string
	Agent           string `enum:"codex,claude-code," default:""`
	Role            string `enum:"user,assistant,developer,tool," default:""`
	IncludeInternal bool
	All             bool `help:"Include tool, instruction, context and notification events."`
	pageArgs        `embed:""`
}
type commandLine struct {
	Profile string   `default:"default" help:"Independent local profile."`
	DataDir string   `help:"Archive root (default: XDG_DATA_HOME/prospabitus)."`
	JSON    bool     `name:"json" help:"JSON output (already the default)."`
	Doctor  struct{} `cmd:"" help:"Show agent roots and runtime versions without reading logs."`
	Sync    struct {
		Source []string `sep:"none" help:"AGENT=ROOT, repeatable; saved per profile."`
	} `cmd:"" help:"Preserve new and updated local sessions."`
	Sources  struct{} `cmd:"" help:"List saved source roots."`
	Status   struct{} `cmd:"" help:"Show archive statistics."`
	Sessions struct {
		List listCommand `cmd:"" help:"List ordinary sessions (internal sessions are opt-in)."`
		Show struct {
			ID string `arg:""`
		} `cmd:"" help:"Inspect a session's provenance."`
	} `cmd:""`
	Revisions struct {
		Session  string
		pageArgs `embed:""`
	} `cmd:"" help:"List snapshots, including conflicts."`
	Events struct {
		scopeArgs `embed:""`
		pageArgs  `embed:""`
		All       bool
	} `cmd:"" help:"Read a session or immutable snapshot."`
	Search searchCommand `cmd:"" help:"Search ordinary messages, including short Japanese terms."`
	Event  struct {
		ID     int64 `arg:""`
		Offset int64 `default:"0"`
	} `cmd:"" help:"Read text in 8000-character pages."`
	Context struct {
		ID     int64 `arg:""`
		Radius int   `default:"3"`
	} `cmd:"" help:"Read surrounding events in the same revision."`
	Changes pageArgs `cmd:"" help:"Read committed changes after a saved cursor."`
	Export  struct {
		scopeArgs `embed:""`
		Output    string `required:""`
	} `cmd:"" help:"Restore raw JSONL without overwriting an existing file."`
	Skill struct {
		Show struct{} `cmd:"" help:"Print the bundled agent Skill."`
	} `cmd:""`
	Version struct{} `cmd:"" help:"Print build version."`
}

func (a App) Run(ctx context.Context, args []string) int {
	if len(args) > 0 && args[0] == "--version" {
		args = []string{"version"}
	}
	if len(args) == 0 {
		args = []string{"--help"}
	}
	var c commandLine
	exit := -1
	parser, err := kong.New(&c, kong.Name("prospabitus"), kong.Description("Local Codex / Claude Code session archive. JSON on stdout; no network calls."), kong.Writers(a.Stdout, a.Stderr), kong.ConfigureHelp(kong.HelpOptions{Compact: true}), kong.Exit(func(code int) { exit = code }))
	if err != nil {
		return a.failure(err)
	}
	parsed, err := parser.Parse(args)
	if exit >= 0 {
		return exit
	}
	if err != nil {
		return a.failure(err)
	}
	if parsed.Command() == "version" {
		fmt.Fprintf(a.Stdout, "%s (%s)\n", Version, Commit)
		return 0
	}
	result, err := run(ctx, &c, parsed.Command())
	if err != nil {
		return a.failure(err)
	}
	result["schema_version"] = 1
	result["ok"] = true
	if errs, ok := result["errors"].([]map[string]any); ok && len(errs) > 0 {
		result["ok"] = false
	}
	enc := json.NewEncoder(a.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(result); err != nil {
		fmt.Fprintln(a.Stderr, err)
		return 1
	}
	if result["ok"] == false {
		return 1
	}
	return 0
}
func (a App) failure(err error) int {
	_ = json.NewEncoder(a.Stdout).Encode(map[string]any{"schema_version": 1, "ok": false, "error": map[string]any{"code": "operation_failed", "message": err.Error()}})
	return 1
}
func expand(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return filepath.Abs(path)
}
func roots(specs []string) ([]archive.Source, error) {
	result := []archive.Source{}
	if len(specs) > 0 {
		for _, spec := range specs {
			name, path, ok := strings.Cut(spec, "=")
			if _, exists := agent.Lookup(name); !ok || !exists || path == "" {
				return nil, fmt.Errorf("--source must be codex=ROOT or claude-code=ROOT")
			}
			abs, err := expand(path)
			if err != nil {
				return nil, err
			}
			stat, err := os.Lstat(abs)
			if err != nil {
				return nil, err
			}
			if stat.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("source root must not be a symlink: %s", abs)
			}
			if !stat.IsDir() {
				return nil, fmt.Errorf("source directory does not exist: %s", abs)
			}
			result = append(result, archive.Source{Agent: name, Path: abs})
		}
	} else {
		for _, a := range agent.Adapters {
			for _, p := range a.Roots() {
				abs, err := expand(p)
				if err != nil {
					return nil, err
				}
				result = append(result, archive.Source{Agent: a.Name, Path: abs})
			}
		}
	}
	return result, nil
}
func validatePage(p pageArgs) error {
	if p.After < 0 || p.Limit < 1 || p.Limit > 100 {
		return fmt.Errorf("cursor must be nonnegative; limit must be between 1 and 100")
	}
	return nil
}
func validScope(s scopeArgs) error {
	if (s.Session == "") == (s.Revision == nil) {
		return fmt.Errorf("exactly one of --session or --revision is required")
	}
	if s.Revision != nil && *s.Revision < 0 {
		return fmt.Errorf("revision must be nonnegative")
	}
	return nil
}
func resolve(ctx context.Context, s *archive.Store, scope scopeArgs) (int64, error) {
	if err := validScope(scope); err != nil {
		return 0, err
	}
	var r int64
	if scope.Revision != nil {
		r = *scope.Revision
	}
	return s.Revision(ctx, scope.Session, r)
}
func page(ctx context.Context, s *archive.Store, query string, params []any, limit int, key string) (map[string]any, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("limit must be between 1 and 100")
	}
	rows, err := s.Query(ctx, query, append(params, limit+1)...)
	if err != nil {
		return nil, err
	}
	var next any
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1][key]
	}
	return map[string]any{"items": rows, "next_cursor": next}, nil
}
func clip(result map[string]any) {
	for _, r := range result["items"].([]map[string]any) {
		clipText(r, 0, 4000)
	}
}
func clipText(row map[string]any, offset int64, size int) {
	text := []rune(row["text"].(string))
	length := int64(len(text))
	start := min(offset, length)
	end := min(start+int64(size), length)
	row["text"] = string(text[int(start):int(end)])
	row["text_length"] = length
	row["next_text_offset"] = nil
	if end < length {
		row["next_text_offset"] = end
	}
}
func run(ctx context.Context, c *commandLine, command string) (map[string]any, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(c.Profile) {
		return nil, fmt.Errorf("profile must contain 1-64 ASCII letters, digits, _ or -")
	}
	if command == "skill show" {
		return map[string]any{"text": assets.Skill}, nil
	}
	if command == "doctor" {
		rr, err := roots(nil)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, r := range rr {
			stat, err := os.Stat(r.Path)
			out = append(out, map[string]any{"agent": r.Agent, "root": r.Path, "exists": err == nil && stat.IsDir()})
		}
		return map[string]any{"version": Version, "commit": Commit, "go": runtime.Version(), "sources": out}, nil
	}
	var sourceRoots []archive.Source
	var err error
	if command == "sync" {
		sourceRoots, err = roots(c.Sync.Source)
		if err != nil {
			return nil, err
		}
	}
	dataDir := c.DataDir
	if dataDir == "" {
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			base = filepath.Join(home, ".local", "share")
		}
		dataDir = filepath.Join(base, "prospabitus")
	}
	dataDir, err = expand(dataDir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, c.Profile, "archive.sqlite3")
	readOnly := false
	if command != "sync" {
		if _, err := os.Stat(path); err == nil {
			readOnly = true
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	s, err := archive.Open(ctx, path, readOnly)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	switch command {
	case "sync":
		saved, err := s.Sources(ctx)
		if err != nil {
			return nil, err
		}
		if len(c.Sync.Source) == 0 && len(saved) > 0 {
			sourceRoots = saved
		}
		if len(c.Sync.Source) > 0 || len(saved) == 0 {
			if err := s.SetSources(ctx, sourceRoots); err != nil {
				return nil, err
			}
		}
		r, err := s.Sync(ctx, sourceRoots)
		if err != nil {
			return nil, err
		}
		return map[string]any{"counts": r.Counts, "errors": r.Errors, "missing_roots": r.MissingRoots, "pending_files": r.PendingFiles, "cursor": r.Cursor}, nil
	case "sources":
		r, err := s.Sources(ctx)
		return map[string]any{"items": r}, err
	case "status":
		out := map[string]any{"profile": c.Profile, "database": path}
		for key, q := range map[string]string{"sessions": "SELECT count(*) FROM streams", "revisions": "SELECT count(*) FROM revisions", "conflicts": "SELECT count(*) FROM revisions WHERE state='conflict'", "raw_bytes": "SELECT coalesce(sum(length(raw)),0) FROM revisions", "cursor": "SELECT coalesce(max(id),0) FROM changes"} {
			n, err := s.Scalar(ctx, q)
			if err != nil {
				return nil, err
			}
			out[key] = n
		}
		return out, nil
	case "sessions show <id>":
		rows, err := s.Query(ctx, "SELECT * FROM streams WHERE id=?", c.Sessions.Show.ID)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("session not found")
		}
		var meta any
		if err := json.Unmarshal([]byte(rows[0]["metadata"].(string)), &meta); err != nil {
			return nil, err
		}
		rows[0]["metadata"] = meta
		return rows[0], nil
	case "sessions list":
		v := c.Sessions.List
		where := "id>?"
		params := []any{v.After}
		if v.Agent != "" {
			where += " AND agent=?"
			params = append(params, v.Agent)
		}
		if !v.IncludeInternal {
			where += " AND coalesce(json_extract(metadata,'$.internal'),0)=0"
		}
		return page(ctx, s, "SELECT id,agent,native_id,active FROM streams WHERE "+where+" ORDER BY id LIMIT ?", params, v.Limit, "id")
	case "revisions":
		v := c.Revisions
		if err := validatePage(v.pageArgs); err != nil {
			return nil, err
		}
		where := "id>?"
		params := []any{v.After}
		if v.Session != "" {
			where += " AND stream=?"
			params = append(params, v.Session)
		}
		return page(ctx, s, "SELECT id,stream,digest,path,state,created,length(raw) AS bytes FROM revisions WHERE "+where+" ORDER BY id LIMIT ?", params, v.Limit, "id")
	case "changes":
		v := c.Changes
		if err := validatePage(v); err != nil {
			return nil, err
		}
		r, err := page(ctx, s, "SELECT * FROM changes WHERE id>? ORDER BY id LIMIT ?", []any{v.After}, v.Limit, "id")
		if err != nil {
			return nil, err
		}
		rows := r["items"].([]map[string]any)
		r["cursor"] = v.After
		if len(rows) > 0 {
			r["cursor"] = rows[len(rows)-1]["id"]
		}
		return r, nil
	case "event <id>":
		v := c.Event
		if v.ID < 0 || v.Offset < 0 {
			return nil, fmt.Errorf("ID and offset must be nonnegative")
		}
		rows, err := s.Query(ctx, "SELECT e.*,r.stream FROM events e JOIN revisions r ON r.id=e.revision WHERE e.id=?", v.ID)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("event not found")
		}
		clipText(rows[0], v.Offset, 8000)
		return rows[0], nil
	case "context <id>":
		v := c.Context
		if v.ID < 0 || v.Radius < 1 || v.Radius > 100 {
			return nil, fmt.Errorf("ID must be nonnegative; radius must be between 1 and 100")
		}
		rev, err := s.Scalar(ctx, "SELECT revision FROM events WHERE id=?", v.ID)
		if err != nil {
			return nil, fmt.Errorf("event not found: %w", err)
		}
		before, err := s.Query(ctx, "SELECT e.*,r.stream FROM events e JOIN revisions r ON r.id=e.revision WHERE revision=? AND e.id<=? ORDER BY e.id DESC LIMIT ?", rev, v.ID, v.Radius+1)
		if err != nil {
			return nil, err
		}
		after, err := s.Query(ctx, "SELECT e.*,r.stream FROM events e JOIN revisions r ON r.id=e.revision WHERE revision=? AND e.id>? ORDER BY e.id LIMIT ?", rev, v.ID, v.Radius)
		if err != nil {
			return nil, err
		}
		for i, j := 0, len(before)-1; i < j; i, j = i+1, j-1 {
			before[i], before[j] = before[j], before[i]
		}
		r := map[string]any{"items": append(before, after...)}
		clip(r)
		return r, nil
	case "events", "search <text>":
		var v pageArgs
		all := false
		where := "e.id>?"
		var params []any
		if command == "events" {
			v = c.Events.pageArgs
			all = c.Events.All
			rev, err := resolve(ctx, s, c.Events.scopeArgs)
			if err != nil {
				return nil, err
			}
			where += " AND e.revision=?"
			params = []any{v.After, rev}
		} else {
			q := c.Search
			v = q.pageArgs
			all = q.All
			if q.Text == "" {
				return nil, fmt.Errorf("search text must not be empty")
			}
			where += " AND e.revision IN (SELECT active FROM streams) AND instr(e.text,?)>0"
			params = []any{v.After, q.Text}
			if !q.IncludeInternal {
				where += " AND coalesce(json_extract(r.metadata,'$.internal'),0)=0"
			}
			if q.Agent != "" {
				where += " AND json_extract(r.metadata,'$.agent')=?"
				params = append(params, q.Agent)
			}
			if q.Role != "" {
				where += " AND e.role=?"
				params = append(params, q.Role)
			}
			if q.Session != "" {
				where += " AND r.stream=?"
				params = append(params, q.Session)
			}
		}
		if err := validatePage(v); err != nil {
			return nil, err
		}
		if !all {
			where += " AND e.kind='message' AND e.role IN ('user','assistant')"
		}
		r, err := page(ctx, s, "SELECT e.*,r.stream FROM events e JOIN revisions r ON r.id=e.revision WHERE "+where+" ORDER BY e.id LIMIT ?", params, v.Limit, "id")
		if err == nil {
			clip(r)
		}
		return r, err
	case "export":
		rev, err := resolve(ctx, s, c.Export.scopeArgs)
		if err != nil {
			return nil, err
		}
		raw, err := s.Raw(ctx, rev)
		if err != nil {
			return nil, err
		}
		output, err := expand(c.Export.Output)
		if err != nil {
			return nil, err
		}
		if err := exportRaw(output, raw); err != nil {
			return nil, err
		}
		return map[string]any{"revision": rev, "output": output, "bytes": len(raw)}, nil
	}
	return nil, fmt.Errorf("unknown command %q", command)
}

func exportRaw(output string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(output), ".prospabitus-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(name, output); err != nil {
		return err
	}
	return nil
}
