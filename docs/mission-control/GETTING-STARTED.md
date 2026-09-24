# Mission Control: getting started

A **mission** gives an agent an objective and has **verifiers outside the agent** decide whether the objective was reached. The agent's own "done!" never counts. This guide takes you from zero to a verified mission, then to evaluating agent versions.

## 1. Prerequisites

- A Standard (PostgreSQL) gateway, migrated to the current schema: `goclaw migrate up`.
- Docker on the gateway host, and the mission image pulled (the gateway never pulls):
  ```bash
  docker pull golang:1.26-bookworm
  ```
  Any Debian-based image with your project's toolchain works. Set it with `GOCLAW_MISSIONS_IMAGE`.
- An admin login in the master scope (system owner or master tenant admin). Creating missions runs code, so it is restricted like shell access; operators can view and cancel missions.

## 2. Enable missions

```bash
export GOCLAW_MISSIONS=1
export GOCLAW_MISSIONS_SOURCE_ROOT=/srv/mission-sources   # repositories + hidden acceptance tests
# optional: GOCLAW_MISSIONS_MAX_CONCURRENT=2  GOCLAW_MISSIONS_LEASE_SECONDS=60
./goclaw
```

The startup log confirms `missions enabled (verifiers and agent exec run in containers)`. If Docker or the image is unavailable, missions stay disabled and the log says why. There is never a silent fallback to the host.

## 3. Prepare a source and its hidden checks

Each mission works on a **copy** of a directory under the source root. Hidden acceptance files live in a **sibling** directory that the agent never sees:

```
/srv/mission-sources/
  sumrepo/              # what the agent works on
  sumrepo-acceptance/   # hidden tests, applied only when verifying
```

The repository ships a ready-made example: copy `evals/missions/testdata/*` into your source root.

## 4. Create your first mission

**Web:** open **Missions → New mission**. The editor is pre-filled with the `fix-sum` contract; set `agent` to one of your agents and submit.

**CLI:**
```bash
goclaw mission create -f examples/missions/fix-sum/contract.json --wait
```

The key parts of the contract (full reference in [MISSIONS.md](MISSIONS.md)):

```json
{"acceptance": [
  {"id": "existing-tests", "kind": "command", "command": ["go", "test", "./..."]},
  {"id": "behavior", "kind": "command", "command": ["go", "test", "./..."],
   "must_change": true, "overlay_dir": "sumrepo-acceptance", "expect_tests": ["TestAcceptanceSumIncludesNegatives"]},
  {"id": "regression-test", "kind": "file_changed", "glob": "*_test.go"}
]}
```

- `must_change`: the check must **fail before** the agent's work, so passing proves the change.
- `overlay_dir`: hidden tests the agent cannot read or edit.
- `expect_tests`: named tests must report an explicit pass. The exit code alone is not trusted.

A read-only research mission (answer a question from documents without touching them) is in `examples/missions/research-zephyr/contract.json`.

## 5. Read the evidence

`goclaw mission show <id>` (or the mission page) shows:
- **Status:**
  - `succeeded`: every check passed;
  - `partial`: some change was proven, but some checks failed;
  - `failed`: no change was proven;
  - `blocked`: something could not be judged (environment problem, integrity finding, changed inputs, unknown cost under a cost limit);
  - `cancelled`.
- **Criteria:** each check's result, with the baseline result, the per-test results and an output tail.
- **Evidence:** the diff and changed files, taken from a frozen copy of the workspace.
- **Tool calls:** every call the agent made, recorded **before** it ran. `started` without a result means the outcome is unknown, for example after a crash.
- **Attempts and usage:** if a gateway died mid-run, the mission is retried in a fresh workspace, and the usage totals are marked as a lower bound.

## 6. Evaluate and improve agents

```bash
goclaw mission eval                            # 27 offline cases: does the system judge agents correctly?
goclaw improve propose v2 --ledger ledger.json --initial-champion v1
goclaw mission export-task <failed-mission-id> > evals/improve/incidents/my-incident.json
goclaw improve monitor --ledger ledger.json --incidents evals/improve/incidents
```

- A new agent version is promoted only when the benchmark shows no gaming, no regression, held-out results not lower, and a strict improvement.
- A failed mission can become an incident task that every future version is judged on.

See [EVALS.md](EVALS.md).

## 7. What this does not do yet

- **Live models:** evaluation with live models and improvement of live agents need a provider budget. The offline suite uses scripted agents.
- **Desktop (Lite) edition:** no Missions UI. The storage layer exists.
- **Mixed versions:** do not run pre-Phase-D gateways on the same database (see [RUNBOOK.md](RUNBOOK.md)).
- **Container escapes** are out of scope. With `GOCLAW_MISSIONS_EXECUTOR=host`, there is no boundary at all; use it only for trusted repositories.
