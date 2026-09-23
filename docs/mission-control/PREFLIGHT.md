# GoClaw Mission Control — Preflight chỉ-đọc (23/09/2026)

Không thay đổi file dự án, branch, remote, DB hay dependency nào. Upstream được clone blobless vào scratchpad để kiểm tra.
Mọi nhận định dưới đây đã đối chiếu với code tại SHA cố định. Agent phụ đọc code độc lập, sau đó tôi tự đọc lại code từng điểm trọng yếu.

## 1. Ref thực tế và công việc local

| Ref | SHA | Ghi chú |
|---|---|---|
| fork main (origin) | f3ba434e | Khớp báo cáo. Local clean, không có stash, không có worktree khác |
| branch làm việc | claude/blissful-pascal-jo32ao | Trùng f3ba434, chưa có commit riêng |
| upstream v3.14.0 | 2f3d68e8 | Lightweight tag, commit ngày 15/06/2026 |
| upstream main | 549c81fd | |
| upstream dev | 4f808241 | Commit #1570 ngày 21/09/2026 |

- Fork đi sau: v3.14.0 là 0/1.752, main là 0/1.768, dev là 0/2.063. Fork là **ancestor** của cả main lẫn dev, nên nhập được bằng **fast-forward**, không có commit riêng của fork cần giữ.
- dev và main: dev có 296 commit riêng, main có 1. Commit riêng của main là 549c81fd (vault dedup #1565).
  - `git cherry` xác nhận fix này **chưa** có trên dev. `internal/vault/links.go` trên dev không có đoạn dedup.
  - Ngược lại, dev có fix UTF-8 ở `ExtractWikilinks` mà main không có. Hai fix nằm ở hai hunk khác nhau, nên cherry-pick dự kiến sạch.
- Số file: fork 555, main 4.009, dev 4.598. Fork→dev: 4.124 A / 81 D / 418 M. **Khớp báo cáo.**
- Không có cài đặt GoClaw cũ trên máy này: không có `~/.goclaw`, không có `.env.local`, Docker daemon không chạy, cluster PG16 local ở trạng thái down.
  - Repo fork **không phải** bằng chứng của một DB đang chạy. Nếu anh có server GoClaw cũ ở nơi khác, cần báo để lập kế hoạch backup.

## 2. CI (tự tái hiện, không chỉ đọc log)

- **dev@4f808241**:
  - `go build ./...` OK.
  - `go build -tags sqliteonly ./...` OK.
  - `go vet ./...` **exit 0**.
- **main@549c81fd**: `go vet` **FAIL**, tái hiện đúng lỗi báo cáo nêu.
  - Ở `internal/http/builtin_tools_tenant_settings_test.go:339` và `tenant_scope_hotfix_test.go:319`, `recordingBuiltinToolStore` thiếu `Upsert`.
  - Trên dev, interface `BuiltinToolStore` không có `Upsert` (`internal/store/builtin_tool_store.go:27-34`), nên dev không bị lỗi này.
- Chưa chạy unit/integration test: cần PostgreSQL + pgvector. Đây là việc của Phase A sau khi được duyệt.
- Log Docker arm64 trên GitHub Actions upstream: không đọc được qua MCP vì quyền chỉ phạm vi fork. Mục này không ảnh hưởng quyết định, vì v1 không build image đa kiến trúc.

## 3. Kiểm chứng các nhận định của báo cáo (≥15, có file:line tại dev@4f808241)

| # | Nhận định | Kết luận | Bằng chứng |
|---|---|---|---|
| 1 | use_skill "thành công" = kích hoạt, không phải nhiệm vụ thành công | ĐÚNG | `internal/tools/use_skill.go:48-67`; `agent/loop_pipeline_tool_callbacks.go:312-371` ghi "succeeded" khi `!IsError` |
| 2 | REST `rolled_back` chỉ đổi status, không khôi phục config | ĐÚNG | `internal/http/evolution_handlers.go:209-215,285`; `RollbackSuggestion` chỉ được gọi từ `EvaluateApplied` |
| 3 | Apply ghi config trước baseline/status, không có transaction | ĐÚNG | `internal/agent/evolution_guardrails.go:109-130` (3 lệnh ghi autocommit riêng) |
| 4 | Rollback không xóa key vốn không tồn tại | ĐÚNG | `evolution_guardrails.go:82-85, 162` (`maps.Copy`) |
| 5 | Dùng `DefaultGuardrails()`, bỏ qua cấu hình theo agent | ĐÚNG | `evolution_handlers.go:270`, `cmd/gateway_evolution_cron.go:152`; không có code Go nào đọc `evolution_guardrails` |
| 6 | EvaluateApplied đo từ `created_at` | ĐÚNG, có thêm lỗi | `evolution_guardrails.go:205-225`: không có cột applied_at, so sánh với mọi source |
| 7 | Kích hoạt skill patch trước khi ghi version/status | ĐÚNG MỘT PHẦN | `internal/http/skills_evolution.go:434-461`: skill đã live trước `CreateSkillVersion`/`MarkSuggestionApplied` |
| 8 | Approve tool_order tắt tool ở cấp tenant | ĐÚNG | `evolution_handlers.go:234-255`; lỗi `Set` chỉ được log mà status vẫn thành applied; rollback không bật lại |
| 9 | Lịch 03/09/15/21, evaluate hằng tuần; docs mô tả khác | ĐÚNG | `cmd/gateway_evolution_cron.go:18,39`; docs/08:198 và docs/21:664 mô tả khác |
| 10 | checkpoint_stage "lưu" pending messages định kỳ | **SAI MỘT PHẦN** | `pipeline/checkpoint_stage.go` → `sessions.AddMessage` chỉ append vào **cache RAM** (`store/pg/sessions.go:177-190`). Chỉ ghi DB khi finalize. Crash giữa run là mất |
| 11 | eventbus drop khi hàng đợi đầy, dedup/retry trong process | ĐÚNG | `internal/eventbus/bus_impl.go:68-73`, `dedup.go:11-12` |
| 12 | Team task có claim/lease/dependency/review/recovery | ĐÚNG | `store/team_store.go:41-48,225-282`; lease 60 phút (PG) / 30 phút (SQLite); `internal/tasks/task_ticker.go` chạy mỗi 5 phút; khi khởi động thì `ForceRecoverAllTasks` |
| 13 | Có delegation completion ledger | ĐÚNG | `tools/delegate_tool.go:530-590`, `store/subagent_store.go:62-102` |
| 14 | License hiện tại CC BY-NC 4.0; README fork cũ có badge MIT, không có file LICENSE | ĐÚNG | `LICENSE` trên dev; README fork dòng 7; LICENSE được thêm ở upstream commit b9a1808a |
| 15 | main go vet fail vì thiếu Upsert | ĐÚNG (tái hiện được) | Xem mục 2 |
| 16 | Hook có 4 loại command/http/script/prompt, fail-closed | ĐÚNG, CÓ NGOẠI LỆ | `hooks/types.go:24-39`, `dispatcher.go:223-285`; `on_timeout=allow` cho phép fail-open |
| 17 | Budget được enforce | ĐÚNG MỘT PHẦN | Usage caps theo tenant/agent/provider/model được enforce (`usage/caps/service.go:110-160`); `budget_monthly_cents` chỉ được lưu, **không ai đọc**; không có budget theo run |
| 18 | Upstream đã có khái niệm "mission/acceptance/verifier" | KHÔNG CÓ | Chỉ xuất hiện trong prompt text và `plans/*.md` |

**Phát hiện mới, báo cáo gốc chưa nêu:**

- **A. Tự tiến hóa agent chưa có tác dụng thật.** Loại auto-apply duy nhất ghi `retrieval_threshold`, nhưng **không có code runtime nào đọc key này**; chỉ `evolution_guardrails.go` tham chiếu nó. Vì vậy "cải tiến" hiện chỉ đổi dữ liệu, không đổi hành vi.
- **B. Actor trong audit có thể bị giả mạo.** Người gọi được tự khai `reviewed_by` trong body request (`evolution_handlers.go:218-221`).
- **C. ACP terminal không cô lập env.** Nó chạy `exec.CommandContext(context.Background())` và không set `cmd.Env`, nên kế thừa toàn bộ env của host, bao gồm DSN và secret (`providers/acp/terminal.go:101-113`). Claude CLI chỉ lọc vài biến `CLAUDE*`.
- **D. Mặc định exec và sandbox đều mở.** Exec approval mặc định `security=full, ask=off`, và Docker sandbox mặc định `ModeOff`. Cả hai không thể dùng làm biên an toàn cho mission nếu không cấu hình riêng.

## 4. Rủi ro khi nhập dev

| ID | Rủi ro | Mức | Xử lý đề xuất |
|---|---|---|---|
| R1 | Upstream **đã bỏ standalone (file-based) mode**; bắt buộc PG hoặc bản build sqlite (`config_load.go:298`, `cmd/gateway_stores_pg.go:24`) | Cao nếu có cài đặt standalone cũ | Không thấy cài đặt nào trên máy. Ghi vào runbook; bản cài standalone cũ không có đường migrate tự động |
| R2 | Migration 5→97 tuyến tính (10 file cũ giống hệt byte), nhưng có bước **phá hủy**: 000023 hard-delete agent đã soft-delete; 000024/026/027 DROP bảng; **000039 TRUNCATE agent_links**; 000082/087/090 xóa dữ liệu | Cao với DB thật | Diễn tập trên DB dùng một lần: seed schema v5 → `migrate up` → kiểm tra. Phải backup `pg_dump` trước. Down migration không phải chiến lược rollback |
| R3 | Context pruning: CHANGELOG nói opt-in, nhưng code **bật mặc định** (`agent/pruning.go:19,176-180`); fork cũ tắt | Trung bình (thay đổi hành vi) | Giữ đúng hành vi upstream, ghi vào release notes và runbook; không tự sửa |
| R4 | Đổi tên env: `GOCLAW_FEISHU_*` → `GOCLAW_LARK_*`; bỏ `GOCLAW_SESSIONS_STORAGE`; `GOCLAW_MODE` chỉ còn cảnh báo. `GOCLAW_ENCRYPTION_KEY` **giữ nguyên tên** | Thấp–trung bình | Ghi vào runbook upgrade |
| R5 | Fork hiện **không có `.github/`**. Nhập dev sẽ kéo theo 8 workflow: `dev-beta-release.yaml` (push dev → tạo tag/release và fetch tag upstream), `fork-image.yaml` (push dev → GHCR của fork), `release*.yaml` (Docker Hub `digitop/goclaw` + Discord nếu có secret), `claude*.yml` | Trung bình | Thêm guard `github.repository` để workflow **phát hành** không chạy trên fork, trừ khi anh bật. Giữ `ci.yaml`. Push file workflow có thể bị GitHub từ chối nếu token thiếu quyền `workflows` (xem Q4) |
| R6 | Updater: desktop tự thay binary từ `nextlevelbuilder/goclaw` (`internal/updater/updater.go:25`); bản Standard chỉ thông báo (`gateway/update_check.go:18`) | Trung bình cho Lite | v1 tập trung Standard; ghi nhận và đề xuất cấu hình repo cập nhật. Không đổi module path |
| R7 | Cần Go 1.26 (máy có 1.25.5; `GOTOOLCHAIN=auto` tải được, đã thử) | Thấp | Ghi vào runbook |
| R8 | Hạ tầng test: Docker daemon không chạy; PG16 local chưa có pgvector (apt có `postgresql-16-pgvector 0.6.0`); upstream khuyến nghị pg18 | Trung bình | Thử bật dockerd và dùng `pgvector/pgvector:pg18`; nếu không được thì dùng PG16 + apt pgvector, và ghi rõ khác biệt |
| R9 | **Không có API key provider nào** trong môi trường | Chặn live eval | Mọi thứ chạy bằng provider giả lập xác định (deterministic). LIVE PROVIDER = BLOCKED cho tới khi anh cấp key và trần chi phí |
| R10 | License CC BY-NC 4.0 | Chặn thương mại | Giữ nguyên LICENSE và attribution; COMMERCIAL LICENSE READY = BLOCKED; không liên hệ maintainer |
| R11 | Diff 2.063 commit khiến PR khó review | Trung bình | Tách commit nhập upstream khỏi commit harness; PR nêu rõ SHA đầu/cuối của phần import |

## 5. Kiến trúc: giữ lại và bổ sung

**Giữ nguyên, tái sử dụng:**
- pipeline 8 stage;
- team task store (claim/lease/blocked_by/review/recovery ticker);
- subagent ledger;
- hooks + exec approval;
- usage caps;
- sandbox Docker;
- evolution stores + skill versions;
- tracing;
- UI Teams/Approvals/Skills/Traces.

**Bổ sung (đề xuất, sẽ kiểm chứng lại khi vào thiết kế Phase C):**
- Bảng `missions`:
  - nội dung: contract JSONB có version, tenant/owner, base revision, budget, trạng thái, verifier result;
  - liên kết task qua `team_tasks` sẵn có (thêm cột `mission_id` hoặc bảng nối), **không dựng task engine thứ hai**.
  - Trạng thái mission ánh xạ tường minh sang status team task.
- Bảng `mission_receipts`: tác vụ có side effect, gồm action digest, idempotency key, kết quả. Có thể thêm `mission_evidence` cho kết quả verifier và artifact.
- **Verifier chạy thật** trong workspace cô lập (lệnh test, kiểm tra file/diff). Lỗi của verifier không bao giờ được tính là pass.
- **Provider giả lập xác định**, bật qua config dev hoặc test, dùng cho fixture và eval offline. Không bao giờ bật ngầm ở production.
- Migration song song cho PG (`migrations/000098+`, bump `RequiredSchemaVersion`) và SQLite (`schema.sql` + patch, bump `SchemaVersion`), theo đúng quy định AGENTS.md.
- Surface: API HTTP + WS method, lệnh CLI `goclaw mission ...`, trang web `missions`. Có i18n đủ en/vi/zh.

## 6. Kế hoạch theo phase và gate

Mỗi phase kết thúc bằng: commit, cập nhật file trạng thái, và chạy lại build/vet/test liên quan.

| Phase | Nội dung | Gate nghiệm thu |
|---|---|---|
| A | Bundle backup fork. Fast-forward branch làm việc lên dev@4f808241, cherry-pick 549c81fd. Thêm guard cho workflow phát hành. Tạo manifest pin SHA. Dựng PG test; chạy build PG + sqliteonly, vet, unit, `test-invariants`, integration. Diễn tập migrate v5→97 trên DB dùng một lần, có backup/restore | Build/vet xanh; phân loại rõ lỗi test kế thừa và lỗi mới; diễn tập migrate + restore thành công; không bỏ fix nào của main |
| B | Viết test đặc tả (characterization) cho 9 lỗi evolution + phát hiện B, rồi sửa. Apply/rollback trong transaction với CAS/version; rollback xóa key vốn không có; REST `rolled_back` gọi khôi phục thật; dùng guardrail theo agent; thêm `applied_at` và lọc đúng source; skill patch có reconcile; tool_order chỉ phạm vi agent hoặc có cảnh báo rõ khi áp dụng tenant; actor lấy từ auth. Báo cáo bản ghi cũ bị lệch, **không tự sửa dữ liệu** | Test đồng thời, retry và lỗi được tiêm vào đều xanh trên PG thật và SQLite |
| C | Mission Contract + một luồng xuyên suốt: repo seed có lỗi → tái hiện → test hồi quy → sửa → verifier → diff/evidence. Đủ API/CLI/web | Chạy được từ UI và CLI trên DB test; kiểm tra bằng Playwright; verifier từ chối "done" giả |
| D | Receipts, fencing lease, huỷ lan truyền xuống tiến trình con, budget cộng dồn giữa cha và con, phát hiện vòng lặp không tiến triển. Ghi checkpoint mission bền vững vào DB | Bộ test lỗi xác định (các điểm crash liệt kê trong prompt) không làm lặp side effect |
| E | Ma trận executor v1: native fs/exec trong sandbox + verifier. ACP/CLI **tắt cho mission** cho tới khi env được cô lập (phát hiện C). Test cô lập tenant | Fixture tấn công (prompt injection, traversal, cross-tenant, approval cũ) đều fail an toàn |
| F | Bộ eval ≥24 case (dev / regression / held-out gắn nhãn hạn chế), chạy offline bằng provider giả lập | Có báo cáo baseline và candidate; phần live = BLOCKED |
| G | Mở rộng evolution sẵn có: taxonomy lỗi → candidate có version → eval → promote → rollback. Mặc định suggest-only | 1 candidate đi hết đường promote, 1 candidate tệ bị loại, 1 regression kích hoạt rollback. "Cải tiến thật" = BLOCKED/NOT DEMONSTRATED nếu không có live trial |
| H | 3 hành trình demo, runbook, backup/upgrade, manifest release, evidence pack | Báo cáo 5 chiều (SOURCE / OFFLINE / LIVE / LICENSE / PRODUCTION) |

**Nói thẳng về phạm vi:** A→H là khối lượng lớn. Tôi sẽ làm tuần tự, commit và ghi file handoff sau mỗi phase để phiên sau làm tiếp. Không đánh dấu phase nào là xong khi gate chưa đạt.

**Ma trận test:**
- `go build` (PG) và `go build -tags sqliteonly`;
- `go vet`;
- `go test -race` cho package thay đổi;
- `make test-invariants`, `tests/integration` (tag `integration`, PG thật);
- `test-contracts`/`test-scenarios` với gateway chạy local;
- `pnpm build` + vitest (nếu repo có) cho `ui/web`;
- Playwright/Chromium cho luồng mission.

## 7. Hành động và ngân sách

**Đã được phép sau khi anh duyệt:**
- sửa và commit trên `claude/blissful-pascal-jo32ao`;
- push branch đó;
- mở **draft PR** vào `main` của fork;
- worktree/DB/sandbox trong container;
- cài công cụ test **trong container** (dockerd/pgvector, `pnpm install` theo lockfile).

**Không làm:**
- ghi vào `main` của fork, force-push, tag/release, deploy;
- ghi bất cứ thứ gì lên upstream;
- gửi tin nhắn hay email;
- sửa LICENSE;
- tiêu tiền provider.

**Ngân sách:** $0 chi phí provider (offline). Live eval chỉ chạy khi anh cấp key và trần chi phí.

## 8. Rollback

- `main` của fork không bị động tới (vẫn ở f3ba434) và là điểm khôi phục chính.
- Thêm một git bundle của f3ba434 trong scratchpad.
- Mọi thay đổi nằm trên branch và PR draft: bỏ bằng cách đóng PR hoặc reset branch về f3ba434. Việc reset chỉ làm khi anh yêu cầu.
- DB: chỉ dùng DB tạm; runbook sẽ mô tả `pg_dump`/restore cho DB thật.
