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
    {"id": "regression-test", "description": "a regression test was added", "kind": "file_changed", "glob": "*_test.go"}
  ],
  "limits": {"max_iterations": 30, "timeout_seconds": 900, "max_cost_usd": 1.0}
}
```

- **Validation** rejects:
  - absolute paths, and source/overlay paths that resolve outside the configured mission source root (including through symlinks);
  - more than 20 criteria;
  - unknown kinds;
  - empty commands;
  - duplicate criterion ids.
- **Criterion kinds:**
  - `command`: runs in the mission workspace. Exit code 0 is a pass, non-zero is a fail. A timeout, spawn failure or missing binary is **error**, never pass.
    - `must_change: true`: the command must **fail on the untouched baseline**. It is run on a throwaway copy before the agent starts. If it already passes, the criterion is an error ("cannot demonstrate the change").
    - `overlay_dir`: hidden acceptance files, stored under the source root outside the agent's copy. They are copied over a throwaway copy of the workspace just before the command runs. The agent never sees them and they never enter the diff.
  - `file_changed`: at least one changed file (vs the base snapshot) matches the glob.
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
| `blocked` | Any verifier **error** (environment problem, timeout, missing tool, a `must_change` check passing on the baseline), or the workspace/overlay could not be prepared |
| `cancelled` | Stopped by the user |

A mission that was running when the process died is marked `failed` with reason `interrupted` at startup. Durable resume arrives in Phase D.

## Workspace and evidence

- The workspace lives at `<data>/missions/<tenant>/<mission-id>/workspace`.
- The source directory is copied in, then a private git repository records the base snapshot. The base revision is the SHA of that snapshot commit.
- After the run, `git diff` against the base produces the **diff artifact**, stored on the mission, size-capped and flagged if truncated. Changed files are listed separately.
- Each criterion result records:
  - status, exit code and duration;
  - the output tail (last 4 KiB, secrets redacted);
  - the command;
  - the contract digest.

## Boundaries in v1

- The v1 executor runs verifiers on the host with a scrubbed environment: only `PATH` plus `HOME`, `TMPDIR`, `GOCACHE` and `GOPATH` pointing into a per-mission scratch directory outside the workspace; `GOPROXY=off`, `GOTOOLCHAIN=local`.
- Commands run in their own process group, killed on timeout.
- Phase E adds the Docker executor (network off, read-only root) and makes it mandatory for untrusted repositories. Until then the API labels the executor `host`.

## Surfaces

**HTTP**
- `POST /v1/missions`: create and start
- `GET /v1/missions`: list
- `GET /v1/missions/{id}`
- `GET /v1/missions/{id}/events`
- `POST /v1/missions/{id}/cancel`

**CLI**
- `goclaw mission create -f contract.json [--wait]`
- `goclaw mission list`
- `goclaw mission show <id>`
- `goclaw mission cancel <id>`

**Web**
- Missions page: list; create from a contract; detail with criteria, evidence, diff, usage and events.
