# Missions (v1) — design

A **mission** is a durable, verifiable unit of work. It has an objective, an isolated workspace, one agent run (or a bounded sequence of runs), and machine-checkable acceptance criteria that are judged **outside** the agent. The agent's narrative ("done!") never counts as evidence.

## Mapping onto existing GoClaw pieces

| Need | Reused as-is | New |
|---|---|---|
| Run an agent turn without a channel | `agent.Router.Get` → `Loop.Run`, via the scheduler (like cron) | `RunRequest.MissionWorkspace` (fail-closed workspace pin) |
| Provider / model | Agent's provider, or `ProviderOverride` | `scripted` provider for deterministic offline runs (env-gated) |
| File and exec tools confined to a directory | Tool workspace + `restrict_to_workspace` | — |
| Usage and cost | `RunResult.Usage`, `RunResult.Calls[].CostUSD`, traces | Aggregated onto the mission record |
| Audit trail | — | `mission_events` (append-only) |
| Multi-agent decomposition | Team tasks (`metadata.mission_id`), a later step | Not in v1 |

**Why missions are not team tasks in v1:**
- Team tasks require a team, members, and claim-by-member semantics.
- A v1 mission is one agent working on one contract, judged by verifiers.
- The mission record wraps the run; it does not replace the task board.
- Delegated sub-work can later be linked through `team_tasks.metadata.mission_id` (see DECISIONS D13).

## Contract (version 1)

```json
{
  "version": 1,
  "title": "Fix Sum for negative numbers",
  "objective": "…what must be true when done…",
  "constraints": ["Do not change the public API"],
  "non_goals": ["Refactoring unrelated code"],
  "agent": "coder",
  "workspace": {"source_dir": "sumrepo"},
  "acceptance": [
    {"id": "tests", "description": "all tests pass", "kind": "command", "command": ["go", "test", "./..."], "timeout_seconds": 120},
    {"id": "behavior", "description": "hidden acceptance tests pass", "kind": "command", "command": ["go", "test", "./..."],
     "must_change": true, "overlay_dir": "sumrepo-acceptance", "expect_tests": ["TestAcceptanceSumIncludesNegatives"]},
    {"id": "regression-test", "description": "a regression test was added", "kind": "file_changed", "glob": "*_test.go"}
  ],
  "limits": {"max_iterations": 30, "timeout_seconds": 900, "max_cost_usd": 1.0,
             "max_tokens": 200000, "max_attempts": 2, "tools": ["read_file", "list_files", "write_file", "edit", "exec"]}
}
```

- **Validation** rejects:
  - absolute paths, the source root itself (`"."`), and source/overlay paths that resolve outside the configured mission source root (including through symlinks);
  - more than 20 criteria;
  - unknown kinds;
  - empty commands;
  - duplicate criterion ids.
- **Criterion kinds:**
  - `command`: runs in the mission workspace. Exit code 0 is a pass, non-zero is a fail. A timeout, spawn failure or missing binary is **error**, never pass.
    - `must_change: true`: the command must **fail on the untouched baseline**. It is run on a throwaway copy before the agent starts. If it already passes, the criterion is an error ("cannot demonstrate the change").
    - `overlay_dir`: hidden acceptance files, stored under the source root outside the agent's copy. They are copied over a throwaway copy of the workspace just before the command runs. The agent never sees them and they never enter the diff.
    - `expect_tests` (`go test` only): the verifier adds `-json` and requires each named test to report an explicit `pass`. Exit code 0 alone is not trusted, because the command runs code the agent wrote. The names are not shown to the agent.
  - `file_changed`: at least one added or modified file (vs the base snapshot) matches the glob. Deleted files do not count.
  - `file_contains`: the file at `path` contains the literal `text`. `must_change` is supported (the text must be absent at baseline). Symlinks resolving outside the workspace are errors.
- **Evidence rule.** A contract must contain at least one criterion that *proves change*: `file_changed`, or any criterion with `must_change`. The other checks are guards (for example "existing tests still pass"). Guards passing never counts as progress. This was found by the false-completion test, where an agent that did nothing was first graded `partial` because the pre-existing tests passed.
- The contract is stored immutably with its SHA-256 digest. Evidence references that digest.

## Lifecycle

```
planned → preparing → running → verifying → succeeded | partial | failed | blocked
                 ↘ (any) → cancelled
```

| Status | Meaning |
|---|---|
| `succeeded` | Every criterion passed, and the agent run itself succeeded within its cost limit |
| `partial` | At least one change-proving criterion passed and at least one criterion failed, with no errors |
| `failed` | No change-proving criterion passed, the agent run failed, or the cost limit was exceeded |
| `blocked` | Any verifier **error** (environment problem, timeout, missing tool, a `must_change` check passing on the baseline, changed inputs, an integrity finding), a cost limit that cannot be verified because cost is unknown, or the workspace/overlay could not be prepared |
| `cancelled` | Stopped by the user |

Precedence: `blocked` wins over `failed` when a criterion could not be evaluated, even if the agent run also failed, because the evidence is incomplete. A failed or over-budget run can never be `succeeded` or `partial`.

## Durability (attempts, leases, recovery)

- **Claim.** A worker claims a planned mission by moving it to `preparing`. This increments `attempt` and sets a lease `(lease_owner, lease_expires_at)`. `max_attempts` (default 2, at most 5) bounds how many attempts may be claimed.
- **Heartbeat.** The worker renews the lease every TTL/3 (`GOCLAW_MISSIONS_LEASE_SECONDS`, default 60, minimum 3). Each renewal has a TTL/3 timeout. If the lease is lost (cancelled from any gateway, or taken over after a partition), or has not been renewed for 2/3 of a TTL, the worker stops its run immediately, before anyone else can consider the lease expired.
- **Clock.** Lease expiry is set and judged by the **database clock** (PostgreSQL `NOW()`), under the row lock. A gateway whose clock is wrong can neither extend nor steal a lease. The Lite edition is a single process and uses its own clock.
- **Fencing.** Every transition and every tool receipt is conditional on `(lease_owner, attempt)`. A stale worker cannot overwrite a newer attempt and cannot run tools.
- **Recovery** runs at startup and every TTL/2, on every gateway:
  - planned and unleased: started;
  - active with an expired lease: requeued for a fresh attempt, or `failed` ("no attempts left");
  - a live lease held by another gateway: left alone.
- **End of run.**
  - When the agent run returns (or is stopped; a cancelled run gets up to 30s to actually stop), every process whose working directory is inside the attempt directory is killed. That includes detached `nohup`/`setsid` children of `exec`.
  - The workspace is then **frozen** into `attempt-<n>/evidence-<random>`. The diff, the integrity scan and every check are computed from that one copy, so a late write cannot make the recorded evidence and the verified tree disagree. `workspace_path` points to the evidence copy.
  - Tool receipts are only accepted while the attempt is `running`, and never after its context is done.
- **Fresh attempts.** Each attempt works in `attempt-<n>/workspace`, copied from the pinned source. A retried attempt never sees what a crashed attempt did; the old attempt directory is kept for inspection. Retries start a new agent conversation, not a mid-run resume. Only lost attempts are retried; a verification failure is final.
- **Usage.** Totals are summed across attempts and include cached input tokens. `usage_incomplete` is set when an attempt ended without reporting usage, and the totals are then a lower bound. Cost is unknown (never summed as zero) unless every model call of the run was priced. `max_cost_usd` applies to the mission total across attempts; `max_tokens` applies to each attempt.
- A graceful shutdown behaves like a crash: the mission continues after the lease expires (crash-only design).
- **Known limits (stated, not hidden):**
  - With the host executor, a process that leaves the workspace (`cd /`) before the run ends is not found by the sweep. It can still write to the host and, if it learns the random evidence path, to the evidence copy. The docker executor closes this: the attempt's container is destroyed when the run ends.
  - A claim whose commit succeeded but whose acknowledgement was lost burns that attempt: it is retried after the lease expires, or failed if it was the last one.
  - Do not run pre-Phase-D gateways against the same database. Their startup recovery fails every active mission.
  - Model calls outside the agent loop are not counted against `max_tokens`: history compaction/summarization, memory flush, LLM-type hooks, `read_document`'s internal call, and post-run consolidation (which also summarizes the mission session into the agent's episodic/knowledge memory, using the agent's background provider). One call can also overshoot the remaining budget.

## Tools and receipts

- **Allowlist.** A mission's agent may only call tools in `limits.tools` (default: `read_file`, `list_files`, `write_file`, `edit`, `exec`, `datetime`; `web_fetch`/`web_search`/`read_document` can be enabled). Messaging, scheduling, delegation, memory, skills, MCP and every other tool are refused. Their effects leave the workspace and could not be safely repeated on retry.
- **Receipts.** Every call, allowed or denied, gets a receipt `(attempt, seq, tool, class, status, args digest, duration)`. The receipt is written **before** the call runs and is fenced by the lease. If it cannot be written, the call is refused, so no side effect happens without a durable record. `started` without a later `ok`/`error` means the outcome was never acknowledged (for example, a crash mid-call).
- **Providers.** Providers that execute their own tools (Claude CLI, ACP) are refused for missions, because the guard cannot see those calls.
- **Budget.** `limits.max_tokens` stops the run before the model call that would start over budget (prompt, cached input and completion tokens).
- **Providers behind a fallback chain** count as running their own tools if any candidate does.

## Workspace and evidence

- The workspace lives at `<data>/missions/<tenant>/<mission-id>/workspace`. The base snapshot repository is `base.git` next to it, **outside** the workspace, so the agent cannot rewrite history, config or excludes.
- **Pins.** At creation the source tree and every hidden overlay are hashed (`sha256` over paths and contents, same skip rules as the copy). The copy made at start and each overlay applied at verification are checked against those digests; a mismatch blocks the mission.
- The source directory is copied in (symlinks, special files and `.git` skipped), then the base snapshot is committed. The base revision is the SHA of that snapshot commit.
- After the run, `git diff` against the base produces the **diff artifact**, stored on the mission, cleaned to valid UTF-8, cut on a rune boundary, size-capped and flagged if truncated. Git runs with `--no-ext-diff --no-textconv`, `core.fsmonitor=false`, no excludes file, and `add -A --force`, so nothing in the workspace can run commands or hide files. Changed files are listed separately.
- **Integrity check.** The full diff is scanned for changes that can subvert verifier commands: an added `TestMain`, an added `init()`, `//go:linkname`, `os.Exit`/`syscall.Exit`/`runtime.Goexit` in test code, a deleted test file, or any nested `.git` entry. A finding adds an `_integrity` error, so the mission is `blocked` for human review. This is a heuristic, not a proof (see the threat model).
- Each criterion result records:
  - status, exit code and duration;
  - the output tail (last 4 KiB, secrets redacted);
  - the command;
  - the contract digest.

## Boundaries (Phase E)

`GOCLAW_MISSIONS_EXECUTOR` selects where code runs:

| | `docker` (default) | `host` (explicit opt-in) |
|---|---|---|
| Verifier commands | One throwaway container per check: `--network none`, read-only root, `--cap-drop ALL`, `no-new-privileges`, memory/CPU/pids limits, the gateway's uid (or `GOCLAW_MISSIONS_SANDBOX_USER`), only the check's own copy (`/workspace`) and scratch (`/scratch`) mounted, `--pull never`; removed on timeout | Host process group with a scrubbed environment |
| Agent `exec` and file tools | One container per attempt (same restrictions, attempt workspace mounted read-write, `/tmp` allows running built binaries). `exec` never falls back to the host. Stored CLI credentials are never injected. The container is destroyed when the run ends, which ends every process it started; a dead worker's containers are removed by recovery (labels `goclaw.mission`, `goclaw.attempt`) | Host (confined to the workspace for file tools only). `exec` gets an allowlisted environment (toolchain and locale variables only, so no DSN, encryption key or provider keys) and no stored CLI credentials |
| Startup | Fails closed: missions stay disabled if Docker is unreachable or `GOCLAW_MISSIONS_IMAGE` (default `golang:1.26-bookworm`, must be pulled) cannot host the sandbox (needs `sh`, `sleep`, `tee`, `cat`, GNU `realpath`; Alpine/busybox is rejected) | Logged as `security.missions_host_executor` |

In both modes:
- Every command check runs in its own copy of the frozen evidence, with its own `HOME` and build cache. Checks cannot modify the evidence or leave a poisoned cache for the next check.
- Agent file tools are confined to the mission workspace; tool-level, tenant and team paths are ignored, and a mission run cannot be a team or delegation run.
- The agent's virtual files (`AGENTS.md`, `SOUL.md`, `USER.md`, `MEMORY.md`, `memory/*`, stored in the database and injected into every later run of that agent) are neither read nor written from a mission. The repository's own files with those names are used instead, so a prompt-injected mission cannot persist instructions into the agent.
- Criterion ids are plain names (`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`): they name per-check directories on the host, and the ids `_integrity`/`_diff` are reserved.
- Tenant-scoped viewers see host paths redacted in status reasons, check details and events. Internal errors are logged, not returned. The agent's summary and the stored diff are credential-scrubbed like command output; the CLI strips terminal control characters from agent-controlled text.
- Creating a mission requires the **admin role and the master scope** (system owner or master tenant admin). Cancelling is open to operators.
- The gateway sets `PR_SET_DUMPABLE=0`, so processes running as the same non-root user cannot read its `/proc/<pid>/environ`. With the docker executor, the containers cannot see host processes at all (separate PID namespace).
- If the gateway runs in a container with the Docker socket mounted, paths are mapped to host paths (`sandbox.HostPath`).

## Threat model and residual risk

Verifier commands **execute code the agent wrote** (a `go test` compiles the agent's package). A malicious or reward-hacking agent can therefore try to make checks pass without doing the work. Controls, strongest first:

1. `expect_tests`: named hidden tests must report an explicit pass. A `TestMain`/`init()` exit or a skipped run yields `missing`, so the check fails.
2. The integrity check blocks the mission when the diff contains constructs that can take over a test binary.
3. Hidden overlays are pinned and invisible to the agent. `.git`, ignore files and git drivers cannot hide or forge the diff.

**Still possible**, and therefore not claimed as prevented:
- code that discovers hidden test names at runtime (the overlay is present in the check's copy) and prints forged `--- PASS` lines;
- with the **host** executor, tampering with the host through `exec`. The host executor is not a security boundary; the docker executor is the default for that reason.
- Container isolation relies on Docker and the kernel; a container escape is out of scope. A `succeeded` mission means "the pinned checks passed and nothing suspicious was found in the diff", not a proof against a determined adversary. The mitigations are the Phase E sandbox and human review of the diff, which the evidence view is built for.

## Surfaces

**HTTP**
- `POST /v1/missions`: create and start
- `GET /v1/missions`: list
- `GET /v1/missions/{id}`
- `GET /v1/missions/{id}/events`
- `GET /v1/missions/{id}/receipts`
- `POST /v1/missions/{id}/cancel`

**CLI**
- `goclaw mission create -f contract.json [--wait]`
- `goclaw mission list`
- `goclaw mission show <id>` (criteria, evidence, tool calls)
- `goclaw mission cancel <id>`
- `goclaw mission export-task <id>`: the mission's contract as a benchmark task (incident) for `goclaw improve`
- `goclaw mission eval`: offline evaluation suite (EVALS.md)

**Web**
- Missions page: list; create from a contract; detail with criteria, evidence, diff, usage (with attempt and lower-bound marking), tool calls and events.
