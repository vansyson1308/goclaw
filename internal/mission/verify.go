package mission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// Criterion outcomes. "error" means the check could not be evaluated
// (timeout, missing binary, unreadable file) and never counts as a pass.
const (
	ResultPass  = "pass"
	ResultFail  = "fail"
	ResultError = "error"
)

const (
	outputTailBytes = 4 << 10
	// maxCaptureBytes bounds the full output kept for parsing (go test -json).
	maxCaptureBytes = 16 << 20
)

// CriterionResult is the evidence for one acceptance criterion.
type CriterionResult struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`
	Description    string   `json:"description,omitempty"`
	Status         string   `json:"status"`
	Detail         string   `json:"detail,omitempty"`
	Command        []string `json:"command,omitempty"`
	ExitCode       *int     `json:"exit_code,omitempty"`
	DurationMs     int64    `json:"duration_ms"`
	OutputTail     string   `json:"output_tail,omitempty"`
	MatchedFiles   []string `json:"matched_files,omitempty"`
	BaselineStatus string   `json:"baseline_status,omitempty"` // must_change: result on the unmodified baseline
	// Tests maps each expect_tests name to what go test -json reported
	// (pass, fail, skip, or missing).
	Tests          map[string]string `json:"tests,omitempty"`
	ProvesChange   bool              `json:"proves_change"` // counts as progress evidence
	Executor       string            `json:"executor"`
	ContractDigest string            `json:"contract_digest"`
}

// ExecRequest is one verifier command.
type ExecRequest struct {
	Dir     string
	Argv    []string
	Timeout time.Duration
	Scratch string // writable dir for HOME/TMP/caches, outside the workspace
	// Capture keeps the full combined output (up to maxCaptureBytes) in
	// ExecResult.Full, for criteria that parse it.
	Capture bool
}

// ExecResult is what an executor observed.
type ExecResult struct {
	ExitCode int
	Output   []byte // combined stdout+stderr (tail kept)
	Full     []byte // full output when requested (nil if it exceeded the cap)
	TimedOut bool
	Err      error // spawn failure etc.; nil when the process ran to an exit code
}

// Executor runs verifier commands. Phase E adds a container executor; the
// host executor scrubs the environment and kills the process group on timeout
// but does not confine the filesystem or network.
type Executor interface {
	Name() string
	Run(ctx context.Context, req ExecRequest) ExecResult
}

// HostExecutor runs commands directly on the gateway host.
type HostExecutor struct{}

func (HostExecutor) Name() string { return "host" }

func (HostExecutor) Run(ctx context.Context, req ExecRequest) ExecResult {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	for _, d := range []string{"home", "tmp", "gocache", "gopath"} {
		_ = os.MkdirAll(filepath.Join(req.Scratch, d), 0o755)
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(req.Scratch, "home"),
		"TMPDIR=" + filepath.Join(req.Scratch, "tmp"),
		"GOCACHE=" + filepath.Join(req.Scratch, "gocache"),
		"GOPATH=" + filepath.Join(req.Scratch, "gopath"),
		"GOPROXY=off",
		"GOFLAGS=-mod=mod",
		"GOTOOLCHAIN=local",
		"LANG=C.UTF-8",
	}
	out := tailBuffer{capture: req.Capture}
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return ExecResult{ExitCode: -1, Err: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		timedOut = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		waitErr = <-done
	}
	res := ExecResult{Output: out.Bytes(), Full: out.Full(), TimedOut: timedOut}
	var exitErr *exec.ExitError
	switch {
	case timedOut:
		res.ExitCode = -1
	case waitErr == nil:
		res.ExitCode = 0
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		res.ExitCode, res.Err = -1, waitErr
	}
	return res
}

// VerifyEnv is where and how criteria are evaluated.
type VerifyEnv struct {
	Workspace string
	Scratch   string            // writable, outside the workspace
	Changed   []string          // added or modified files vs the base snapshot (not deletions)
	Overlays  map[string]string // criterion id → absolute hidden overlay dir
	// OverlayPins maps criterion id → TreeDigest taken at mission creation.
	// An overlay whose content no longer matches is an error, not a pass.
	OverlayPins map[string]string
	Exec        Executor
}

// Baseline runs every must_change criterion against the unmodified
// workspace (on a throwaway copy) before the agent starts.
func Baseline(ctx context.Context, c *Contract, env VerifyEnv) map[string]CriterionResult {
	out := map[string]CriterionResult{}
	digest := c.Digest()
	for _, cr := range c.Acceptance {
		switch {
		case cr.Kind == KindCommand && cr.MustChange:
			out[cr.ID] = runCommand(ctx, cr, env, digest, true)
		case cr.Kind == KindFileContains && cr.MustChange:
			out[cr.ID] = fileContains(cr, env, digest)
		}
	}
	return out
}

// Verify evaluates every criterion against the workspace. It never returns
// a pass for a check it could not run, nor for a must_change check that
// already passed on the baseline.
func Verify(ctx context.Context, c *Contract, env VerifyEnv, baseline map[string]CriterionResult) []CriterionResult {
	digest := c.Digest()
	out := make([]CriterionResult, 0, len(c.Acceptance))
	for _, cr := range c.Acceptance {
		start := time.Now()
		var r CriterionResult
		switch cr.Kind {
		case KindCommand:
			r = runCommand(ctx, cr, env, digest, false)
			if cr.MustChange {
				applyBaseline(&r, baseline)
			}
		case KindFileChanged:
			r = CriterionResult{ID: cr.ID, Kind: cr.Kind, Description: cr.Description, Executor: env.Exec.Name(), ContractDigest: digest}
			for _, f := range env.Changed {
				if ok, _ := path.Match(cr.Glob, path.Base(f)); ok {
					r.MatchedFiles = append(r.MatchedFiles, f)
				} else if ok, _ := path.Match(cr.Glob, f); ok {
					r.MatchedFiles = append(r.MatchedFiles, f)
				}
			}
			r.Status = ResultFail
			if len(r.MatchedFiles) > 0 {
				r.Status = ResultPass
			} else {
				r.Detail = "no changed file matches " + cr.Glob
			}
		case KindFileContains:
			r = fileContains(cr, env, digest)
			if cr.MustChange {
				applyBaseline(&r, baseline)
			}
		default:
			r = CriterionResult{ID: cr.ID, Kind: cr.Kind, Status: ResultError, Detail: "unknown criterion kind", Executor: env.Exec.Name(), ContractDigest: digest}
		}
		r.DurationMs = time.Since(start).Milliseconds()
		r.ProvesChange = cr.ProvesChange()
		out = append(out, r)
	}
	return out
}

func fileContains(cr Criterion, env VerifyEnv, digest string) CriterionResult {
	r := CriterionResult{ID: cr.ID, Kind: cr.Kind, Description: cr.Description, Executor: env.Exec.Name(), ContractDigest: digest}
	data, err := readInside(env.Workspace, cr.Path)
	switch {
	case err != nil && os.IsNotExist(err):
		r.Status, r.Detail = ResultFail, "file does not exist"
	case err != nil:
		r.Status, r.Detail = ResultError, err.Error()
	case bytes.Contains(data, []byte(cr.Text)):
		r.Status = ResultPass
	default:
		r.Status, r.Detail = ResultFail, "text not found"
	}
	return r
}

func applyBaseline(r *CriterionResult, baseline map[string]CriterionResult) {
	b, ok := baseline[r.ID]
	switch {
	case !ok:
		r.BaselineStatus = ResultError
		r.Status, r.Detail = ResultError, "baseline was not evaluated"
	case b.Status == ResultPass:
		r.BaselineStatus = ResultPass
		r.Status, r.Detail = ResultError, "check already passes on the unmodified baseline, so it cannot demonstrate the change (fix the contract)"
	case b.Status == ResultError:
		r.BaselineStatus = ResultError
		r.Status, r.Detail = ResultError, "baseline check could not run: "+b.Detail
	default:
		r.BaselineStatus = ResultFail // failed before the change, as required
	}
}

// runCommand executes a command criterion. With an overlay, or on the
// baseline, it runs in a throwaway copy of the workspace so hidden files
// never reach the agent's workspace and checks cannot change the diff.
func runCommand(ctx context.Context, cr Criterion, env VerifyEnv, digest string, baseline bool) CriterionResult {
	r := CriterionResult{ID: cr.ID, Kind: cr.Kind, Description: cr.Description, Command: cr.Command, Executor: env.Exec.Name(), ContractDigest: digest}
	dir := env.Workspace
	overlay := env.Overlays[cr.ID]
	if cr.OverlayDir != "" && overlay == "" {
		r.Status, r.Detail = ResultError, "overlay directory not resolved"
		return r
	}
	if overlay != "" || baseline {
		tag := "final"
		if baseline {
			tag = "baseline"
		}
		tmp := filepath.Join(env.Scratch, "checks", tag+"-"+cr.ID)
		_ = os.RemoveAll(tmp)
		defer os.RemoveAll(tmp)
		if _, err := copyTree(env.Workspace, tmp); err != nil {
			r.Status, r.Detail = ResultError, "copy workspace for check: "+err.Error()
			return r
		}
		if overlay != "" {
			digest, _, err := copyTreeDigest(overlay, tmp)
			if err != nil {
				r.Status, r.Detail = ResultError, "apply hidden acceptance files: "+err.Error()
				return r
			}
			if pin := env.OverlayPins[cr.ID]; pin == "" || pin != digest {
				r.Status, r.Detail = ResultError, "hidden acceptance files changed since the mission was created (or were never pinned)"
				return r
			}
		}
		dir = tmp
	}
	argv := cr.Command
	if len(cr.ExpectTests) > 0 {
		// go test -json: per-test results instead of trusting the exit code.
		argv = append([]string{argv[0], argv[1], "-json"}, argv[2:]...)
	}
	res := env.Exec.Run(ctx, ExecRequest{Dir: dir, Argv: argv, Timeout: time.Duration(cr.TimeoutSeconds) * time.Second, Scratch: env.Scratch, Capture: len(cr.ExpectTests) > 0})
	r.OutputTail = cleanText(tools.ScrubCredentials(string(res.Output)))
	switch {
	case res.Err != nil:
		r.Status, r.Detail = ResultError, "could not run: "+res.Err.Error()
	case res.TimedOut:
		r.Status, r.Detail = ResultError, fmt.Sprintf("timed out after %ds", cr.TimeoutSeconds)
	default:
		code := res.ExitCode
		r.ExitCode = &code
		r.Status = ResultFail
		if code == 0 {
			r.Status = ResultPass
		}
	}
	if len(cr.ExpectTests) > 0 && res.Err == nil && !res.TimedOut {
		applyExpectedTests(&r, cr.ExpectTests, res.Full)
	}
	return r
}

// applyExpectedTests requires every expected test to report an explicit
// pass. A zero exit code with a missing or failed expected test is a fail.
func applyExpectedTests(r *CriterionResult, expected []string, full []byte) {
	if full == nil {
		r.Status, r.Detail = ResultError, "test output exceeded the capture limit"
		return
	}
	seen := parseGoTestJSON(full)
	r.Tests = map[string]string{}
	var tail tailBuffer
	for _, line := range bytes.Split(full, []byte("\n")) {
		var ev goTestEvent
		if json.Unmarshal(line, &ev) == nil && ev.Action == "output" {
			_, _ = tail.Write([]byte(ev.Output))
		}
	}
	r.OutputTail = cleanText(tools.ScrubCredentials(string(tail.Bytes())))
	var missing []string
	for _, name := range expected {
		st, ok := seen[name]
		if !ok {
			st = "missing"
		}
		r.Tests[name] = st
		if st != "pass" {
			missing = append(missing, name+"="+st)
		}
	}
	if len(missing) > 0 && r.Status == ResultPass {
		r.Status = ResultFail
	}
	if len(missing) > 0 {
		r.Detail = "expected tests did not pass: " + strings.Join(missing, ", ")
	}
}

type goTestEvent struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
	Output string `json:"Output"`
}

// parseGoTestJSON returns the final status per test name. A test that
// reported fail anywhere (any package) stays failed.
func parseGoTestJSON(out []byte) map[string]string {
	seen := map[string]string{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev goTestEvent
		if json.Unmarshal(line, &ev) != nil || ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			if seen[ev.Test] != "fail" {
				seen[ev.Test] = ev.Action
			}
		}
	}
	return seen
}

// readInside reads rel under root, refusing paths that resolve outside it
// (the agent may have created symlinks in the workspace).
func readInside(root, rel string) ([]byte, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, err
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if resolved != rootResolved && !strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		return nil, fmt.Errorf("path %q resolves outside the workspace", rel)
	}
	return os.ReadFile(resolved)
}

// Outcome maps criterion results to a mission status. Only criteria that
// prove a change count as progress: guard checks that already passed on the
// untouched code cannot turn "nothing was done" into "partial".
func Outcome(results []CriterionResult) string {
	var fail, errs, evidencePass, evidence int
	for _, r := range results {
		if r.ProvesChange {
			evidence++
		}
		switch r.Status {
		case ResultPass:
			if r.ProvesChange {
				evidencePass++
			}
		case ResultFail:
			fail++
		default:
			errs++
		}
	}
	switch {
	case len(results) == 0 || evidence == 0 || errs > 0:
		return StatusBlocked
	case fail == 0:
		return StatusSucceeded
	case evidencePass == 0:
		return StatusFailed
	default:
		return StatusPartial
	}
}

// tailBuffer keeps the last outputTailBytes written and, with capture, the
// full output up to maxCaptureBytes. Safe for concurrent stdout/stderr use.
type tailBuffer struct {
	mu       sync.Mutex
	buf      []byte
	capture  bool
	full     []byte
	overflow bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > outputTailBytes {
		t.buf = t.buf[len(t.buf)-outputTailBytes:]
	}
	if t.capture && !t.overflow {
		if len(t.full)+len(p) > maxCaptureBytes {
			t.overflow, t.full = true, nil
		} else {
			t.full = append(t.full, p...)
		}
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte { return t.buf }

// Full returns the captured output, or nil if capture was off or overflowed.
func (t *tailBuffer) Full() []byte {
	if !t.capture || t.overflow {
		return nil
	}
	if t.full == nil {
		return []byte{}
	}
	return t.full
}
