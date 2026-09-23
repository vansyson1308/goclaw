# Operator runbook (in progress; completed in Phase H)

## Upgrading an existing install to this branch

1. **Back up first.** A down migration is not a rollback strategy.
   ```bash
   pg_dump -Fc -f goclaw-$(date +%F).dump "$GOCLAW_POSTGRES_DSN"
   ```
   Keep `GOCLAW_ENCRYPTION_KEY` unchanged. Stored provider keys are AES-GCM encrypted with it.
2. **Rehearse on a copy.** Restore the dump into a throwaway database, then run `./scripts/mission-control/upgrade-rehearsal.sh`. That script shows the pattern; point your copy at `goclaw migrate up`.
3. **Know the data-changing migrations** before running them on real data:

   | Migration | Effect |
   |---|---|
   | 000023 | Hard-deletes soft-deleted agents (cascades). Also moves `group_file_writers` into `agent_config_permissions` |
   | 000024 / 000026 / 000027 | Drop legacy team workspace, handoff and custom tool tables |
   | 000039 | `TRUNCATE agent_links`. Delegation links must be recreated |
   | 000082 / 000087 / 000090 | Remove orphaned or duplicate rows |

4. **Configuration changes:**
   - Standalone (file) mode is gone. PostgreSQL is required, or use the `sqliteonly` Lite build.
   - `GOCLAW_FEISHU_*` is renamed to `GOCLAW_LARK_*`.
   - `GOCLAW_MODE` is ignored (it only logs a warning).
   - Context pruning now defaults to on (`cache-ttl`). Set `contextPruning.mode: "off"` to keep the old behavior.
   - Go 1.26 is required to build.
5. **Roll back** by restoring the dump taken in step 1 into a fresh database and running the previous binary. The rehearsal script verifies this restore path.

## CI and publishing on the fork

Upstream workflows that tag, publish images or notify Discord are gated to `nextlevelbuilder/goclaw`. To enable them on this fork, set repository variables:

| Variable | Enables |
|---|---|
| `GOCLAW_RELEASES_ENABLED=true` | `release.yaml`, `release-beta.yaml`, `release-desktop.yaml`, `dev-beta-release.yaml` (beta tagging), `fork-image.yaml` |
| `GOCLAW_CLAUDE_REVIEW_ENABLED=true` | `claude-code-review.yml` (needs `CLAUDE_CODE_OAUTH_TOKEN`) |

Before enabling, check `DOCKERHUB_IMAGE` in those workflows (`digitop/goclaw` upstream) and change it to your own namespace.

## Update checks

- The Standard edition only notifies about new upstream releases (`internal/gateway/update_check.go`).
- The Lite desktop self-updater downloads from `nextlevelbuilder/goclaw` `lite-v*` releases (`internal/updater/updater.go`). Do not ship Lite builds from this fork until that is repointed.
