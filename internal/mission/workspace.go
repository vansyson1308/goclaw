package mission

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

const (
	maxWorkspaceFiles = 20000
	maxWorkspaceBytes = 200 << 20 // 200 MiB
	maxDiffBytes      = 256 << 10 // 256 KiB stored on the mission
)

// PreparedWorkspace is the isolated copy an agent works in.
type PreparedWorkspace struct {
	Path         string
	GitDir       string // base snapshot repository, outside Path
	BaseRevision string
	SourceDigest string   // TreeDigest of what was copied in
	Skipped      []string // entries not copied (symlinks, special files)
}

// PrepareWorkspace copies src into dst (which must not exist) and records the
// copy as the base snapshot in a private git repository at gitDir, which
// must be outside dst so the agent cannot rewrite the evidence (history,
// config, excludes). .git directories and symlinks are never copied.
func PrepareWorkspace(ctx context.Context, src, dst, gitDir string) (*PreparedWorkspace, error) {
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("mission source %q is not a directory", src)
	}
	if rel, err := filepath.Rel(dst, gitDir); err != nil || rel == "." || !strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("mission git dir must be outside the workspace")
	}
	for _, p := range []string{dst, gitDir} {
		if _, err := os.Stat(p); err == nil {
			return nil, fmt.Errorf("%q already exists", p)
		}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dst); _ = os.RemoveAll(gitDir) }
	pw := &PreparedWorkspace{Path: dst, GitDir: gitDir}
	digest, skipped, err := copyTreeDigest(src, dst)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("copy source: %w", err)
	}
	pw.Skipped, pw.SourceDigest = skipped, digest
	if _, err := runGitCmd(ctx, "", "", "init", "-q", "--bare", gitDir); err != nil {
		cleanup()
		return nil, err
	}
	for _, args := range [][]string{
		{"add", "-A", "--force"},
		{"commit", "-q", "--allow-empty", "-m", "mission base"},
	} {
		if _, err := runGit(ctx, pw, args...); err != nil {
			cleanup()
			return nil, err
		}
	}
	rev, err := runGit(ctx, pw, "rev-parse", "HEAD")
	if err != nil {
		cleanup()
		return nil, err
	}
	pw.BaseRevision = strings.TrimSpace(rev)
	return pw, nil
}

// copyTree copies regular files and directories from src into dst
// (merging into existing directories, overwriting files). .git directories,
// symlinks and special files are skipped and reported. Size-capped.
func copyTree(src, dst string) ([]string, error) {
	t, err := walkTree(src, dst)
	if err != nil {
		return nil, err
	}
	return t.skipped, nil
}

// TreeDigest hashes a directory the way copyTree copies it (same skip
// rules), without copying. Used to pin mission inputs at creation.
func TreeDigest(src string) (string, error) {
	t, err := walkTree(src, "")
	if err != nil {
		return "", err
	}
	return t.digest, nil
}

// copyTreeDigest copies like copyTree and returns the digest of exactly the
// bytes copied, so a check against a pin has no time-of-check gap.
func copyTreeDigest(src, dst string) (string, []string, error) {
	t, err := walkTree(src, dst)
	if err != nil {
		return "", nil, err
	}
	return t.digest, t.skipped, nil
}

type treeWalk struct {
	skipped []string
	digest  string
}

// walkTree visits src in lexical order, hashing every copied file's path
// and content, and copies into dst unless dst is empty.
func walkTree(src, dst string) (*treeWalk, error) {
	var skipped []string
	var files int
	var total int64
	h := sha256.New()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			if dst == "" {
				return nil
			}
			return os.MkdirAll(dst, 0o755)
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		target := ""
		if dst != "" {
			target = filepath.Join(dst, rel)
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0, !d.Type().IsRegular() && !d.IsDir():
			skipped = append(skipped, filepath.ToSlash(rel))
			return nil
		case d.IsDir():
			fmt.Fprintf(h, "d %s\x00", filepath.ToSlash(rel))
			if dst == "" {
				return nil
			}
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
		fmt.Fprintf(h, "f %s %d\x00", filepath.ToSlash(rel), fi.Size())
		return copyFile(p, target, fi.Mode().Perm(), h)
	})
	if err != nil {
		return nil, err
	}
	return &treeWalk{skipped: skipped, digest: "sha256:" + hex.EncodeToString(h.Sum(nil))}, nil
}

// WorkspaceDiff is the change set relative to the base snapshot.
type WorkspaceDiff struct {
	Patch        string
	Truncated    bool
	ChangedFiles []string // every touched path, deletions included
	Present      []string // added or modified paths that still exist
	// Nested lists .git entries inside the workspace. Git never tracks them
	// and nested repositories hide their content, so the diff cannot be
	// trusted as complete while any exist.
	Nested []string
	// Full is the untruncated, cleaned patch for integrity scanning. It is
	// not persisted.
	Full string
}

// Diff stages every change (including new and ignored files) and diffs it
// against base. The agent-writable workspace cannot influence how git runs:
// config, excludes and history live in the external git dir, and external
// diff/textconv drivers and fsmonitor are disabled.
func Diff(ctx context.Context, pw *PreparedWorkspace) (*WorkspaceDiff, error) {
	d := &WorkspaceDiff{}
	nested, err := findNestedGit(pw.Path)
	if err != nil {
		return nil, err
	}
	d.Nested = nested
	if _, err := runGit(ctx, pw, "add", "-A", "--force"); err != nil {
		return nil, err
	}
	status, err := runGit(ctx, pw, "diff", "--cached", "--name-status", "--no-renames", "--no-ext-diff", "--no-textconv", pw.BaseRevision)
	if err != nil {
		return nil, err
	}
	patch, err := runGit(ctx, pw, "diff", "--cached", "--no-color", "--no-ext-diff", "--no-textconv", pw.BaseRevision)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		code, name, ok := strings.Cut(line, "\t")
		if !ok || name == "" {
			continue
		}
		d.ChangedFiles = append(d.ChangedFiles, name)
		if !strings.HasPrefix(code, "D") {
			d.Present = append(d.Present, name)
		}
	}
	d.Full = cleanText(patch)
	d.Patch, d.Truncated = truncateText(d.Full, maxDiffBytes)
	return d, nil
}

// findNestedGit reports every entry named .git inside the workspace.
func findNestedGit(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && p != root {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	return out, err
}

// cleanText makes agent-produced text storable: invalid UTF-8 is replaced
// and NUL bytes (rejected by PostgreSQL text/jsonb) are removed.
func cleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
}

// truncateText cleans s and cuts it to at most n bytes on a rune boundary.
func truncateText(s string, n int) (string, bool) {
	s = cleanText(s)
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// runGit runs git against the mission's external git dir and workspace.
func runGit(ctx context.Context, pw *PreparedWorkspace, args ...string) (string, error) {
	return runGitCmd(ctx, pw.GitDir, pw.Path, args...)
}

// runGitCmd runs git with an isolated configuration: no system/global
// config, no hooks, no fsmonitor, no external diff or textconv, no excludes
// file, fixed identity. Output is stdout.
func runGitCmd(ctx context.Context, gitDir, workTree string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	full := []string{
		"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false",
		"-c", "core.fsmonitor=false", "-c", "core.excludesFile=/dev/null", "-c", "core.attributesFile=/dev/null",
		"-c", "diff.external=", "-c", "core.untrackedCache=false", "-c", "core.quotePath=true",
	}
	home := os.TempDir()
	dir := home
	if gitDir != "" {
		full = append(full, "--git-dir="+gitDir, "--work-tree="+workTree)
		dir = workTree
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_ATTR_NOSYSTEM=1",
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

// copyFile copies src to dst (or only hashes it when dst is empty), feeding
// the bytes read into h.
func copyFile(src, dst string, perm fs.FileMode, h io.Writer) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if dst == "" {
		_, err := io.Copy(h, in)
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// maxSummaryBytes bounds the agent's final message kept as the summary.
const maxSummaryBytes = 16 << 10

// scrubPatch removes credentials from the stored diff (shown to viewers).
// The frozen evidence copy on disk keeps the exact content.
func scrubPatch(p string) string { return tools.ScrubCredentials(p) }
