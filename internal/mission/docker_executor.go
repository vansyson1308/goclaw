package mission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// DockerExecutor runs each verifier command in a throwaway container: no
// network, read-only root filesystem, all capabilities dropped, no new
// privileges, CPU/memory/process limits. Only the check's own copy of the
// workspace (/workspace) and its scratch directory (/scratch) are mounted,
// so a check can neither reach the host nor leave anything behind for the
// next check. The image must already be present (--pull never).
type DockerExecutor struct {
	Image     string
	User      string // "uid:gid"; default: the gateway's own uid:gid
	MemoryMB  int    // default 2048
	CPUs      float64
	PidsLimit int // default 512
	// HostPath maps local paths to Docker host paths (the gateway may itself
	// run in a container). Default: identity.
	HostPath func(ctx context.Context, p string) string
}

func (DockerExecutor) Name() string { return "docker" }

// dockerRunFailed are `docker run` exit codes that mean the command never
// ran (daemon error, not executable, not found): an error, never a fail.
var dockerRunFailed = map[int]string{125: "docker could not start the container", 126: "command cannot be invoked", 127: "command not found in the verifier image"}

func (e DockerExecutor) Run(ctx context.Context, req ExecRequest) ExecResult {
	if len(req.Argv) == 0 {
		return ExecResult{ExitCode: -1, Err: errors.New("empty command")}
	}
	for _, d := range []string{"home", "gocache", "gopath"} {
		_ = os.MkdirAll(filepath.Join(req.Scratch, d), 0o777)
	}
	hostPath := e.HostPath
	if hostPath == nil {
		hostPath = func(_ context.Context, p string) string { return p }
	}
	user := e.User
	if user == "" {
		user = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	mem, cpus, pids := e.MemoryMB, e.CPUs, e.PidsLimit
	if mem <= 0 {
		mem = 2048
	}
	if cpus <= 0 {
		cpus = 2
	}
	if pids <= 0 {
		pids = 512
	}
	name := "goclaw-verify-" + uuid.NewString()[:12]
	args := []string{
		"run", "--rm", "--name", name, "--pull", "never",
		"--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(pids), "--memory", fmt.Sprintf("%dm", mem), "--cpus", strconv.FormatFloat(cpus, 'f', 1, 64),
		"--user", user,
		"--tmpfs", "/tmp:size=1024m,exec,nosuid,nodev",
		"-v", hostPath(ctx, req.Dir) + ":/workspace:rw",
		"-v", hostPath(ctx, req.Scratch) + ":/scratch:rw",
		"-w", "/workspace",
		"-e", "HOME=/scratch/home", "-e", "TMPDIR=/tmp",
		"-e", "GOCACHE=/scratch/gocache", "-e", "GOPATH=/scratch/gopath",
		"-e", "GOFLAGS=-mod=mod", "-e", "GOPROXY=off", "-e", "GOTOOLCHAIN=local",
		"-e", "LANG=C.UTF-8",
		e.Image,
	}
	args = append(args, req.Argv...)

	runCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	cmd := exec.Command("docker", args...)
	out := tailBuffer{capture: req.Capture}
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return ExecResult{ExitCode: -1, Err: fmt.Errorf("docker: %w", err)}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		timedOut = true
		// Killing the docker CLI does not stop the container: remove it.
		rmCtx, rmCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = exec.CommandContext(rmCtx, "docker", "rm", "-f", name).Run()
		rmCancel()
		_ = cmd.Process.Kill()
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
		if why, ok := dockerRunFailed[res.ExitCode]; ok {
			res.Err = fmt.Errorf("%s (exit %d)", why, res.ExitCode)
		}
	default:
		res.ExitCode, res.Err = -1, waitErr
	}
	return res
}

// CheckDockerImage reports whether Docker is reachable and image is present
// locally (the executor never pulls).
func CheckDockerImage(ctx context.Context, image string) error {
	if image == "" {
		return errors.New("no verifier image configured")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(cctx, "docker", "image", "inspect", "--format", "{{.Id}}", image).CombinedOutput(); err != nil {
		return fmt.Errorf("verifier image %q is not available locally (pull it first): %v: %s", image, err, out)
	}
	return nil
}

// sandboxImageProbe checks what the mission sandbox's file tools need in
// the image: a POSIX shell, sleep (keep-alive), tee, and GNU realpath -e.
const sandboxImageProbe = `realpath -e -- / >/dev/null && command -v sleep >/dev/null && command -v tee >/dev/null && command -v cat >/dev/null`

// CheckSandboxImage verifies the image can host a mission attempt (agent
// file tools and exec), e.g. Debian-based images; busybox/Alpine lacks
// GNU realpath and is rejected here rather than failing at the first edit.
func CheckSandboxImage(ctx context.Context, image string) error {
	if err := CheckDockerImage(ctx, image); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "run", "--rm", "--pull", "never", "--network", "none", "--read-only",
		image, "sh", "-c", sandboxImageProbe).CombinedOutput()
	if err != nil {
		return fmt.Errorf("image %q cannot host the mission sandbox (needs sh, sleep, tee, cat and GNU realpath; use a Debian-based image): %v: %s", image, err, out)
	}
	return nil
}
