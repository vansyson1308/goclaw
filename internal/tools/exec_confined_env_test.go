//go:build !windows

package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// A mission (workspace-confined) run on the host executor must not hand the
// gateway's secrets to the agent's commands. The denylist scrub covers
// well-known provider keys only; a compiled program the agent writes can read
// its whole environment, including the database DSN and the key that
// decrypts every tenant's provider credentials.
func TestConfinedHostExecGetsAllowlistedEnvOnly(t *testing.T) {
	t.Setenv("GOCLAW_ENCRYPTION_KEY", "enc-key-must-not-leak")
	t.Setenv("GOCLAW_POSTGRES_DSN", "postgres://u:pw-must-not-leak@db/x")
	t.Setenv("SOME_VENDOR_SECRET", "vendor-must-not-leak")
	ws := t.TempDir()
	tool := NewExecTool(ws, true)
	ctx := WithWorkspaceConfined(WithToolWorkspace(context.Background(), ws))

	r := tool.executeOnHost(ctx, "env; echo exit-ok", ws)
	if r.IsError || !strings.Contains(r.ForLLM, "exit-ok") {
		t.Fatalf("command did not run: %#v", r)
	}
	for _, leak := range []string{"enc-key-must-not-leak", "pw-must-not-leak", "vendor-must-not-leak"} {
		if strings.Contains(r.ForLLM, leak) {
			t.Fatalf("confined exec saw gateway secret %q", leak)
		}
	}
	if !strings.Contains(r.ForLLM, "PATH=") {
		t.Fatalf("confined exec lost PATH: %s", r.ForLLM)
	}

	// Outside missions the existing behavior is unchanged.
	r = tool.executeOnHost(WithToolWorkspace(context.Background(), ws), "env", ws)
	if !strings.Contains(r.ForLLM, "vendor-must-not-leak") {
		t.Fatalf("non-mission exec env changed unexpectedly")
	}
}

// Mission runs never receive the tenant's stored CLI credentials, on the host
// executor too (not only when a sandbox is required): the lookup does not
// even happen.
func TestConfinedExecNeverLooksUpStoredCredentials(t *testing.T) {
	ws := t.TempDir()
	cli := newStubSecureCLIStore()
	cli.byName["cred-cli"] = &store.SecureCLIBinary{BinaryName: "cred-cli", TimeoutSeconds: 30, Enabled: true, IsGlobal: true}
	tool := NewExecTool(ws, true)
	tool.SetSecureCLIStore(cli)
	ctx := WithWorkspaceConfined(WithToolWorkspace(context.Background(), ws))

	_ = tool.Execute(ctx, map[string]any{"command": "cred-cli status"})
	cli.mu.Lock()
	calls := cli.lookupCalls
	cli.mu.Unlock()
	if calls != 0 {
		t.Fatalf("stored CLI credentials looked up %d time(s) in a mission run", calls)
	}
}
