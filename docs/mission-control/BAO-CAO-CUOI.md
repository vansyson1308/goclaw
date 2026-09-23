# Báo cáo cuối: GoClaw Mission Control (Pha A → H)

Nhánh: `claude/blissful-pascal-jo32ao` · PR nháp: vansyson1308/goclaw#1 (vào `main` của fork, **chưa merge**).
Mọi con số dưới đây đều lấy từ lần chạy thật, ghi trong [EVIDENCE.md](EVIDENCE.md). Không có số liệu nào được dựng lên.

## 1. Kết quả theo 5 chiều đánh giá

| Chiều | Trạng thái | Căn cứ |
|---|---|---|
| **SOURCE READY** | **Sẵn sàng để review.** Đã làm xong A–H trên nhánh, có tài liệu và kiểm thử | RELEASE-MANIFEST.md |
| **OFFLINE/INTEGRATION VERIFIED** | **Đã kiểm chứng.** Ngoại lệ duy nhất là các lỗi do môi trường container này, đã ghi rõ | EVIDENCE §A–§H |
| **LIVE PROVIDER VERIFIED** | **BỊ CHẶN** (BLOCKED). Không có khóa API và chưa có ngân sách được duyệt. Mọi lần chạy agent đều dùng provider `scripted` (tất định, $0) | SCOPE.md |
| **COMMERCIAL LICENSE READY** | **BỊ CHẶN** (BLOCKED). Upstream dùng giấy phép CC BY-NC 4.0 (phi thương mại). Em **không** đổi giấy phép; muốn dùng thương mại thì phải giải quyết bản quyền riêng | LICENSE |
| **PRODUCTION RELEASE APPROVED** | **Không.** Không có yêu cầu này. Chưa tag, chưa publish, chưa deploy | — |

## 2. Đã làm được gì (theo pha)

- **A. Đồng bộ upstream.** Fork được fast-forward lên upstream `dev` (2.063 commit), giữ nguyên lịch sử, không force-push. Diễn tập nâng cấp schema từ v5 lên phiên bản hiện tại phát hiện **2 lỗi thật**, đã sửa: provider hoặc agent biến mất khi cột có giá trị NULL. Thêm vào đó là lỗi thứ tự tool call trong luồng streaming. CI được chỉnh để không publish khi chạy trên fork.
- **B. Evolution đúng đắn.**
  - Apply/rollback chạy trong transaction, đúng một lần (exactly-once), có audit.
  - Rollback khôi phục chính xác cả trường hợp một key "không tồn tại".
  - Áp dụng skill an toàn khi bị crash.
  - Đã sửa cron evolution, vốn không phân tích agent nào trên PG.
- **C. Mission (lát cắt dọc).** Contract được version hóa và có digest.
  - Agent làm việc trong workspace riêng. Verifier chạy **bên ngoài agent**; các cơ chế chính là `must_change`, test ẩn và `expect_tests`.
  - Có đủ API, CLI và giao diện web.
  - **Review đối kháng độc lập** tìm ra 3 lỗi mức cao, tiêu biểu: dùng `TestMain` để thắng mọi test, dùng `.git` để thực thi lệnh hoặc che file, đọc được secret. Cả 3 đã sửa, mỗi lỗi có test hồi quy và đã kiểm tra bằng đột biến (mutation).
- **D. Bền vững.**
  - Lease và fencing theo **đồng hồ của DB**. Một lần chạy lỗi được thử lại trong workspace mới.
  - Receipt cho từng tool call được ghi **trước khi** chạy, theo kiểu fail-closed.
  - Allowlist công cụ, ngân sách token, giới hạn chi phí tính trên tổng.
  - **Review đối kháng thứ hai** tìm ra 2 lỗi mức cao, đã sửa: CLI provider lọt qua chuỗi fallback; process chạy ngầm làm giả được bằng chứng.
  - E2E thật: `kill -9` gateway giữa chừng thì mission tự chạy lại và thành công ở lần thử 2.
- **E. Ranh giới cưỡng chế.** Docker là mặc định và **fail-closed**.
  - Mỗi lần kiểm tra chạy trong một container riêng: không mạng, root chỉ đọc, bỏ mọi capability.
  - `exec` của agent chạy trong container riêng của từng lần thử, không bao giờ rơi về host.
  - Tình huống prompt injection: agent làm theo lệnh bị cài sẵn trong repo nhưng đều bị chặn. Một đoạn dò đã lách được deny-list vẫn chỉ nhìn thấy nội dung bên trong container.
- **F. Bộ đánh giá.** 27 ca (dev 10 / regression 9 / held-out 8). Kết quả **27/27 trên host và 27/27 trên Docker, 0 lần báo thành công sai**. Khi cố tình tắt integrity check, bộ đánh giá bắt được **4 lần báo thành công sai**, tức là bộ đánh giá có tác dụng thật.
- **G. Vòng đời cải tiến.** Luồng chạy thật đúng như thiết kế:
  - bị từ chối: ứng viên kém hơn, ứng viên "gian lận" và ứng viên overfit dev;
  - được thăng hạng: ứng viên tốt hơn;
  - sự cố làm lộ ra một hồi quy nên hệ thống **tự rollback**, sau đó ứng viên đã sửa được thăng hạng.

  Ledger lưu digest làm bằng chứng cho mọi quyết định.
- **H. Hoàn thiện sản phẩm.** Ba hành trình người dùng chạy qua E2E thật:
  1. sửa code;
  2. nghiên cứu chỉ đọc;
  3. phục hồi và học: mission thất bại được chuyển thành "incident" bằng `export-task` và đưa vào benchmark.

  Ngoài ra có tài liệu nhập môn (GETTING-STARTED), manifest phát hành và gói bằng chứng.

## 3. Những điều em nói thẳng

- **Provider scripted không phải model thật.** Nó chỉ kiểm chứng *hợp đồng* giữa agent loop và mission, không phải năng lực của model. Việc model hoặc prompt thật có tiến bộ hay không là **CHƯA CHỨNG MINH** (NOT DEMONSTRATED).
- **Verifier vẫn chạy code do agent viết.** Một agent đủ tinh vi vẫn có thể giả output nếu dò được tên của test ẩn. Hiện tại hệ thống giảm thiểu bằng `expect_tests`, integrity scan và container, nhưng **không** tuyên bố đã loại bỏ hoàn toàn rủi ro này.
- **Executor `host` không phải là ranh giới bảo mật.** Chỉ nên dùng cho repo tin cậy.
- **Test phụ thuộc môi trường.** Một số test phụ thuộc môi trường vẫn đỏ trong container này: tải tiktoken bị chặn, PID 1 không reap zombie, và một test thời gian kế thừa từ upstream. Các test này không bị tắt; em phân loại chúng kèm bằng chứng.
- **CI độc lập (GitHub Actions trên fork).** CI chạy thật trên PR #1:
  - job `go`: build, build `sqliteonly`, vet, `go test -race ./...`, invariants và integration với PostgreSQL 18;
  - job `web`: lint và build;
  - job `release-versioning`.

  Cả 3 job đều **xanh** trên commit `e0ebcb7b`. Job `claude-review` bị skip vì không có secret. Các test phụ thuộc môi trường ở trên **không làm đỏ** job trên runner của GitHub, nên lỗi của chúng là do container này.
- **Tóm tắt nền của agent.** Sau mỗi mission, bộ nhớ nền (tóm tắt episodic, trích xuất knowledge graph) vẫn gọi model của agent và lưu tóm tắt phiên mission vào bộ nhớ. Lượng gọi này không nằm trong `max_tokens`. Hạn chế này đã ghi trong MISSIONS.md.

## 4. Anh cần quyết định

1. **Có merge PR #1 vào `main` của fork không.** Em không tự merge.
2. **Có cấp ngân sách và khóa provider để kiểm chứng LIVE không.** Nếu có, em sẽ chạy bộ đánh giá với model thật.
3. **Giấy phép.** Nếu định dùng thương mại, cần làm rõ với upstream (CC BY-NC 4.0).

## 5. Cách kiểm tra lại

```bash
go build ./... && go vet ./...
go test ./internal/mission/... ./internal/store/... ./internal/tools/
go test -race -tags integration ./tests/integration/ -run 'Mission|Evolution'   # cần PG
UI_CHECK=1 scripts/mission-control/e2e-mission.sh                               # cần Docker + PG
goclaw mission eval && goclaw mission eval --executor docker
```
