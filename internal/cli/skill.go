package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rokoucha/prospabitus/assets"
)

type skillInstallCommand struct {
	Agent  string `required:"" enum:"codex,claude" help:"Agent to install the Skill for."`
	DryRun bool   `help:"Show the destination and diff without writing files."`
	Force  bool   `help:"Replace a different existing Skill explicitly."`
}

type skillCheck struct {
	Status string `json:"status"`
	Path   string `json:"path,omitempty"`
}

func skillPath(agent string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var dir string
	switch agent {
	case "codex":
		dir = ".codex"
	case "claude":
		dir = ".claude"
	default:
		return "", fmt.Errorf("unsupported Skill agent %q", agent)
	}
	return filepath.Join(home, dir, "skills", "prospabitus", "SKILL.md"), nil
}

// Read only regular files, including if a path changes after the initial check.
// Parent symlinks remain usable for managed dotfile directories.
func readSkill(path string) ([]byte, os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("Skill must be a regular file; refusing symlink or special file: %s", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("Skill must be a regular file: %s", path)
	}
	content, err := io.ReadAll(f)
	return content, info.Mode().Perm(), err
}

func inspectSkill(agent string) skillCheck {
	path, err := skillPath(agent)
	if err != nil {
		return skillCheck{Status: "unavailable"}
	}
	result := skillCheck{Path: path}
	existing, _, err := readSkill(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		result.Status = "not-installed"
	case err != nil:
		result.Status = "unreadable"
	case bytes.Equal(existing, []byte(assets.Skill)):
		result.Status = "current"
	default:
		result.Status = "outdated-or-modified"
	}
	return result
}

func installSkill(args skillInstallCommand) (map[string]any, error) {
	path, err := skillPath(args.Agent)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"agent": args.Agent, "path": path, "dry_run": args.DryRun, "changed": false}
	content := []byte(assets.Skill)
	existing, mode, err := readSkill(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if exists && bytes.Equal(existing, content) {
		result["status"] = "current"
		return result, nil
	}
	if args.DryRun {
		result["status"] = "would-install"
		if exists {
			result["status"] = "would-update"
			result["diff"] = skillDiff(existing, content)
			result["requires_force"] = true
		}
		return result, nil
	}
	if exists && !args.Force {
		return nil, fmt.Errorf("existing Skill differs; inspect with --dry-run and replace explicitly with --force: %s", path)
	}
	if !exists {
		mode = 0644
	}
	if err := writeSkill(path, content, exists, mode); err != nil {
		return nil, err
	}
	result["status"] = "installed"
	if exists {
		result["status"] = "updated"
	}
	result["changed"] = true
	return result, nil
}

// Publish only fully written content. A concurrent new file is never overwritten.
func writeSkill(path string, content []byte, replace bool, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".prospabitus-skill-*")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if replace {
		return os.Rename(f.Name(), path)
	}
	return os.Link(f.Name(), path)
}

func skillDiff(old, next []byte) string {
	var b strings.Builder
	b.WriteString("--- installed\n+++ bundled\n")
	for _, part := range []struct {
		prefix  string
		content []byte
	}{{"-", old}, {"+", next}} {
		if len(part.content) == 0 {
			continue
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(part.content), "\n"), "\n") {
			fmt.Fprintf(&b, "%s %s\n", part.prefix, line)
		}
		if !bytes.HasSuffix(part.content, []byte("\n")) {
			b.WriteString("\\ No newline at end of file\n")
		}
	}
	return b.String()
}
