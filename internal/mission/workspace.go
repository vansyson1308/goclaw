package mission

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxWorkspaceFiles = 20000
	maxWorkspaceBytes = 200 << 20 // 200 MiB
	maxDiffBytes      = 256 << 10 // 256 KiB stored on the mission
)

// PreparedWorkspace is the isolated copy an agent works in.
type PreparedWorkspace struct {
	Path         string
	BaseRevision string
	Skipped      []string // entries not copied (symlinks, special files)
}

// PrepareWorkspace copies src into dst (which must not exist) and records the
// copy as the base snapshot in a private git repository. .git directories and
// symlinks are never copied, so the agent cannot reach outside the copy
// through them and no repository hooks come along.
func PrepareWorkspace(ctx context.Context, src, dst string) (*PreparedWorkspace, error) {
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("mission source %q is not a directory", src)
	}
	if _, err := os.Stat(dst); err == nil {
		return nil, fmt.Errorf("workspace %q already exists", dst)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}
	pw := &PreparedWorkspace{Path: dst}
	skipped, err := copyTree(src, dst)
	if err != nil {
		_ = os.RemoveAll(dst)
		return nil, fmt.Errorf("copy source: %w", err)
	}
	pw.Skipped = skipped
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"commit", "-q", "--allow-empty", "-m", "mission base"},
	} {
		if _, err := runGit(ctx, dst, args...); err != nil {
			_ = os.RemoveAll(dst)
			return nil, err
		}
	}
	rev, err := runGit(ctx, dst, "rev-parse", "HEAD")
	if err != nil {
		_ = os.RemoveAll(dst)
		return nil, err
	}
	pw.BaseRevision = strings.TrimSpace(rev)
	return pw, nil
}

// copyTree copies regular files and directories from src into dst
// (merging into existing directories, overwriting files). .git directories,
// symlinks and special files are skipped and reported. Size-capped.
func copyTree(src, dst string) ([]string, error) {
	var skipped []string
	var files int
	var total int64
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0, !d.Type().IsRegular() && !d.IsDir():
			skipped = append(skipped, filepath.ToSlash(rel))
			return nil
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		}
		files++
		if files > maxWorkspaceFiles {
			return fmt.Errorf("more than %d files", maxWorkspaceFiles)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Size()
		if total > maxWorkspaceBytes {
			return fmt.Errorf("exceeds %d bytes", maxWorkspaceBytes)
		}
		return copyFile(p, target, fi.Mode().Perm())
	})
	return skipped, err
}

// WorkspaceDiff is the change set relative to the base snapshot.
type WorkspaceDiff struct {
	Patch        string
	Truncated    bool
	ChangedFiles []string
}

// Diff stages every change (including new files) and diffs it against base.
func Diff(ctx context.Context, ws, base string) (*WorkspaceDiff, error) {
	if _, err := runGit(ctx, ws, "add", "-A"); err != nil {
		return nil, err
	}
	names, err := runGit(ctx, ws, "diff", "--cached", "--name-only", base)
	if err != nil {
		return nil, err
	}
	patch, err := runGit(ctx, ws, "diff", "--cached", "--no-color", base)
	if err != nil {
		return nil, err
	}
	d := &WorkspaceDiff{}
	for _, n := range strings.Split(strings.TrimSpace(names), "\n") {
		if n != "" {
			d.ChangedFiles = append(d.ChangedFiles, n)
		}
	}
	if len(patch) > maxDiffBytes {
		patch = patch[:maxDiffBytes]
		d.Truncated = true
	}
	d.Patch = patch
	return d, nil
}

// runGit runs git with an isolated configuration: no system/global config,
// no hooks, fixed identity. Output is combined stdout.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	full := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=goclaw-mission", "GIT_AUTHOR_EMAIL=mission@goclaw.invalid",
		"GIT_COMMITTER_NAME=goclaw-mission", "GIT_COMMITTER_EMAIL=mission@goclaw.invalid",
		"GIT_TERMINAL_PROMPT=0",
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
