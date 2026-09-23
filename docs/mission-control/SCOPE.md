# Mission Control — approved scope and upstream manifest

Approved by the repository owner on 2026-09-23 ("kế hoạch này thì anh duyệt rồi … làm từ A đến H").
The approval persists across sessions. Preflight: see `PREFLIGHT.md` in this folder.

## Pinned sources

| Ref | SHA | Role |
|---|---|---|
| fork main (vansyson1308/goclaw) | f3ba434e8ecbe275e5ad14df1929fe665eb273d6 | Untouched recovery point; also bundled offline |
| upstream v3.14.0 | 2f3d68e806c11a20b59b0591bca75410ed1237f7 | Stable comparison reference |
| upstream main | 549c81fd2406875be34cec3d7274125238e7d4a0 | Only main-only commit is the vault fix (#1565); `go vet` fails here (test stub missing `Upsert`) |
| upstream dev | 4f8082417782a2e29a295fa2639b716326971fee | **Integration base** (fast-forward from fork main) |
| cherry-pick of 549c81fd | ee4adc0e (on work branch) | Vault wikilink dedup, not present on dev |

The upstream remote is fetch-only (`git remote set-url --push upstream DISABLED_NO_PUSH_TO_UPSTREAM`).

## Authorized

- Commits on `claude/blissful-pascal-jo32ao`; push that branch; a draft PR into fork `main`.
- Local containers, throwaway databases, sandboxes, and test tooling installed inside the dev container.
- Fork-safety edits to CI workflows (publishing becomes opt-in).

## Not authorized without a new, explicit request

- Writing to fork `main`, force-pushing, tags or releases, deploying.
- Any write to upstream (nextlevelbuilder/goclaw).
- Outbound messages, changing LICENSE, and provider spend.

## Budget

- Provider spend is **$0**. All agent runs use the deterministic scripted provider.
- Live-provider gates stay **BLOCKED** until the owner supplies a key and a spend ceiling.

## Defaults

- Product: self-hosted Standard edition (Go + PostgreSQL + web UI + CLI).
- Desktop/Lite must keep compiling (`-tags sqliteonly`), stay schema-compatible (SQLite migrations) and be feature-gated.
- Commercial release is gated on licensing (CC BY-NC 4.0 upstream). There is no relicensing.
