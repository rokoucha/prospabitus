package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/rokoucha/prospabitus/assets"
)

func TestSkillInstallDestinationsDryRunAndIdempotence(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			data := filepath.Join(home, "archive")
			dir := ".codex"
			if agent == "claude" {
				dir = ".claude"
			}
			path := filepath.Join(home, dir, "skills", "prospabitus", "SKILL.md")
			r := invoke(t, data, 0, "skill", "install", "--agent", agent, "--dry-run")
			if r["status"] != "would-install" || r["path"] != path || r["changed"] != false {
				t.Fatal(r)
			}
			if _, err := os.Stat(filepath.Join(home, dir)); !os.IsNotExist(err) {
				t.Fatal("dry-run created directories", err)
			}
			r = invoke(t, data, 0, "skill", "install", "--agent", agent)
			if r["status"] != "installed" || r["changed"] != true {
				t.Fatal(r)
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != assets.Skill {
				t.Fatal("not exact bundled content", err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, extra := range [][]string{nil, {"--force"}, {"--dry-run"}} {
				args := append([]string{"skill", "install", "--agent", agent}, extra...)
				r = invoke(t, data, 0, args...)
				if r["status"] != "current" || r["changed"] != false {
					t.Fatal(r)
				}
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
				t.Fatal("current Skill was rewritten", err)
			}
			if _, err := os.Stat(data); !os.IsNotExist(err) {
				t.Fatal("Skill command created archive DB", err)
			}
			r = invoke(t, data, 0, "doctor")
			skills := r["skills"].(map[string]any)
			if skills[agent].(map[string]any)["status"] != "current" {
				t.Fatal(skills)
			}
			if _, err := os.Stat(data); !os.IsNotExist(err) {
				t.Fatal("doctor created archive DB", err)
			}
		})
	}
}

func TestSkillModifiedRefusalDiffAndForcedUpdate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	data := filepath.Join(home, "archive")
	path, err := skillPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	custom := "# personal customization\n日本語で説明して\n"
	if err := os.WriteFile(path, []byte(custom), 0600); err != nil {
		t.Fatal(err)
	}
	r := invoke(t, data, 0, "doctor")
	if r["skills"].(map[string]any)["codex"].(map[string]any)["status"] != "outdated-or-modified" {
		t.Fatal(r)
	}
	r = invoke(t, data, 0, "skill", "install", "--agent", "codex", "--dry-run")
	diff := r["diff"].(string)
	if r["status"] != "would-update" || r["requires_force"] != true || !strings.Contains(diff, "- 日本語で説明して") || !strings.Contains(diff, "+ name: prospabitus") {
		t.Fatal(r)
	}
	r = invoke(t, data, 1, "skill", "install", "--agent", "codex")
	message := r["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "--dry-run") || !strings.Contains(message, "--force") {
		t.Fatal(r)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != custom {
		t.Fatal("custom Skill overwritten", err)
	}
	r = invoke(t, data, 0, "skill", "install", "--agent", "codex", "--force", "--dry-run")
	if r["changed"] != false || r["status"] != "would-update" {
		t.Fatal(r)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != custom {
		t.Fatal("force dry-run changed Skill")
	}
	r = invoke(t, data, 0, "skill", "install", "--agent", "codex", "--force")
	raw, err = os.ReadFile(path)
	if err != nil || string(raw) != assets.Skill || r["status"] != "updated" {
		t.Fatal(r, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("permissions not preserved", err)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("created archive DB", err)
	}
}

func TestSkillRefusesSymlinksAndSpecialFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path, _ := skillPath("claude")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(home, "external.md")
			if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "directory":
				err = os.Mkdir(path, 0755)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			data := filepath.Join(home, "data")
			for _, extra := range [][]string{nil, {"--force"}, {"--dry-run"}} {
				args := append([]string{"skill", "install", "--agent", "claude"}, extra...)
				invoke(t, data, 1, args...)
			}
			r := invoke(t, data, 0, "doctor")
			if r["skills"].(map[string]any)["claude"].(map[string]any)["status"] != "unreadable" {
				t.Fatal(r)
			}
			raw, err := os.ReadFile(target)
			if err != nil || string(raw) != "keep" {
				t.Fatal("external file changed", err)
			}
		})
	}
}

func TestSkillMissingAgentAndDoctorNotInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	data := filepath.Join(home, "data")
	for _, args := range [][]string{{"skill", "install"}, {"skill", "install", "--agent", "other"}} {
		invoke(t, data, 1, args...)
	}
	r := invoke(t, data, 0, "doctor")
	for _, v := range r["skills"].(map[string]any) {
		if v.(map[string]any)["status"] != "not-installed" {
			t.Fatal(r)
		}
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("created DB", err)
	}
}

func TestSkillAtomicPublicationAndCleanup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeSkill(path, []byte("replacement"), false, 0644); !errors.Is(err, os.ErrExist) {
		t.Fatal("expected concurrent-file collision", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "keep" {
		t.Fatal("existing file overwritten", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary file leaked", entries, err)
	}
	targetDir := filepath.Join(dir, "directory")
	if err := os.Mkdir(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeSkill(targetDir, []byte("replacement"), true, 0644); err == nil {
		t.Fatal("replaced directory")
	}
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatal("temporary file leaked after rename failure", entries, err)
	}
}

func TestSkillConcurrentNewInstallDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skills", "SKILL.md")
	results := make(chan error, 2)
	for _, content := range []string{"complete first Skill", "complete second Skill"} {
		go func(content string) { results <- writeSkill(path, []byte(content), false, 0644) }(content)
	}
	successes, collisions := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, os.ErrExist) {
			collisions++
		} else {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || successes != 1 || collisions != 1 || (string(raw) != "complete first Skill" && string(raw) != "complete second Skill") {
		t.Fatal(successes, collisions, string(raw), err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary file leaked", entries, err)
	}
}

func TestSkillDiffTrailingNewlineAndEmptyFiles(t *testing.T) {
	withNewline := skillDiff([]byte("same"), []byte("same\n"))
	withoutNewline := skillDiff([]byte("same\n"), []byte("same"))
	if withNewline != "--- installed\n+++ bundled\n- same\n\\ No newline at end of file\n+ same\n" {
		t.Fatal(withNewline)
	}
	if withoutNewline != "--- installed\n+++ bundled\n- same\n+ same\n\\ No newline at end of file\n" {
		t.Fatal(withoutNewline)
	}
	if d := skillDiff(nil, []byte("\n")); d != "--- installed\n+++ bundled\n+ \n" {
		t.Fatal(d)
	}
}
