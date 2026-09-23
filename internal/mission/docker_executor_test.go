package mission

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Container-boundary tests for the verifier executor. They need Docker and
// the verifier image locally; set GOCLAW_TEST_VERIFIER_IMAGE to override.
func verifierImage(t *testing.T) string {
	t.Helper()
	img := os.Getenv("GOCLAW_TEST_VERIFIER_IMAGE")
	if img == "" {
		img = "mirror.gcr.io/library/golang:1.26-bookworm"
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if err := CheckDockerImage(context.Background(), img); err != nil {
		t.Skipf("verifier image unavailable: %v", err)
	}
	return img
}

func dockerRun(t *testing.T, e DockerExecutor, timeout time.Duration, argv ...string) ExecResult {
	t.Helper()
	dir, scratch := t.TempDir(), t.TempDir()
	return e.Run(context.Background(), ExecRequest{Dir: dir, Scratch: scratch, Argv: argv, Timeout: timeout})
}

func TestDockerExecutorBoundaries(t *testing.T) {
	e := DockerExecutor{Image: verifierImage(t)}
	t.Setenv("GOCLAW_TEST_SECRET", "hunter2-must-not-leak")

	t.Run("no network", func(t *testing.T) {
		// Only the loopback interface exists (a bridged container would also
		// have eth0 and a default route, even where egress is filtered).
		r := dockerRun(t, e, time.Minute, "sh", "-c", "ls /sys/class/net; ip route; wget -q -T 3 -O- http://1.1.1.1/ && echo REACHED")
		if strings.TrimSpace(strings.SplitN(string(r.Output), "\n", 2)[0]) != "lo" || strings.Contains(string(r.Output), "default") ||
			strings.Contains(string(r.Output), "REACHED") {
			t.Fatalf("container has network access:\n%s", r.Output)
		}
	})
	t.Run("host environment and processes are invisible", func(t *testing.T) {
		r := dockerRun(t, e, time.Minute, "sh", "-c", "env; cat /proc/*/environ 2>/dev/null | tr '\\0' '\\n'; ps -o pid,args")
		if strings.Contains(string(r.Output), "hunter2") || strings.Contains(string(r.Output), "go test") {
			t.Fatalf("host secret or processes visible:\n%s", r.Output)
		}
	})
	t.Run("read-only root, no capabilities", func(t *testing.T) {
		r := dockerRun(t, e, time.Minute, "sh", "-c", "touch /etc/pwned || exit 3")
		if r.ExitCode != 3 {
			t.Fatalf("root filesystem writable: exit %d %s", r.ExitCode, r.Output)
		}
	})
	t.Run("timeout removes the container", func(t *testing.T) {
		start := time.Now()
		r := dockerRun(t, e, 2*time.Second, "sleep", "60")
		if !r.TimedOut || time.Since(start) > 40*time.Second {
			t.Fatalf("timeout not enforced: %+v after %s", r, time.Since(start))
		}
		out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name=goclaw-verify-").Output()
		if strings.TrimSpace(string(out)) != "" {
			t.Fatalf("verifier containers left behind: %s", out)
		}
	})
	t.Run("missing command is an error, not a fail", func(t *testing.T) {
		r := dockerRun(t, e, time.Minute, "no-such-binary")
		if r.Err == nil {
			t.Fatalf("missing binary reported as exit %d", r.ExitCode)
		}
	})
	t.Run("workspace is the only writable mount", func(t *testing.T) {
		dir, scratch := t.TempDir(), t.TempDir()
		r := e.Run(context.Background(), ExecRequest{Dir: dir, Scratch: scratch, Timeout: time.Minute,
			Argv: []string{"sh", "-c", "echo ok > /workspace/out.txt && echo x > /scratch/home/y && (echo z > /home/z 2>/dev/null && exit 4 || exit 0)"}})
		if r.ExitCode != 0 {
			t.Fatalf("exit %d: %s", r.ExitCode, r.Output)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "out.txt")); strings.TrimSpace(string(b)) != "ok" {
			t.Fatal("workspace mount not writable")
		}
	})
}

// Images that cannot host the sandbox's file tools are rejected up front.
func TestCheckSandboxImage(t *testing.T) {
	img := verifierImage(t)
	if err := CheckSandboxImage(context.Background(), img); err != nil {
		t.Fatalf("debian-based image rejected: %v", err)
	}
	const alpine = "mirror.gcr.io/library/golang:1.26-alpine"
	if CheckDockerImage(context.Background(), alpine) != nil {
		t.Skip("alpine image not present")
	}
	if err := CheckSandboxImage(context.Background(), alpine); err == nil || !strings.Contains(err.Error(), "realpath") {
		t.Fatalf("busybox image accepted: %v", err)
	}
	if err := CheckSandboxImage(context.Background(), "goclaw-no-such-image:0"); err == nil {
		t.Fatal("missing image accepted")
	}
}

// A whole mission verified in containers reaches the same verdicts.
func TestMissionWithDockerExecutor(t *testing.T) {
	img := verifierImage(t)
	svc, st, ctx := newTestService(t, &fakeRunner{reply: "fixed", edit: honestFix})
	svc.exec = DockerExecutor{Image: img}
	raw := contractWith(func(c *Contract) { c.Acceptance[1].ExpectTests = []string{"TestAcceptanceSumIncludesNegatives"} })
	m := runToEnd(t, svc, st, ctx, raw)
	res := results(t, m)
	if m.Status != StatusSucceeded || m.Executor != "docker" || res["behavior"].Executor != "docker" ||
		res["behavior"].Tests["TestAcceptanceSumIncludesNegatives"] != "pass" || res["behavior"].BaselineStatus != ResultFail {
		t.Fatalf("docker-verified mission: %s (%s) %+v", m.Status, m.StatusReason, res["behavior"])
	}

	liar, lst, lctx := newTestService(t, &fakeRunner{reply: "All done!"})
	liar.exec = DockerExecutor{Image: img}
	if lm := runToEnd(t, liar, lst, lctx, raw); lm.Status == StatusSucceeded || lm.Status == StatusPartial {
		t.Fatalf("false claim accepted under docker: %s", lm.Status)
	}
}
