# DeepSeek V4.1 Flash in GoClaw

## Hướng dẫn nhanh

Chỉ cần làm một trong hai cách dưới đây. Trong cả hai cách, bạn chỉ phải dán API key; mọi thứ còn lại đã được cấu hình sẵn.

**Cách 1: dùng cho gateway đang chạy (sản phẩm thật).**

```bash
scripts/deepseek-setup.sh      # hỏi API key (gõ không hiện ra màn hình)
```

Script sẽ lần lượt:
1. tạo provider `deepseek`; key được gateway mã hoá AES-256-GCM trước khi lưu;
2. gọi thử model một lần để kiểm tra key (tốn vài token);
3. nạp bảng giá để tính chi phí và áp giới hạn chi phí cho mission;
4. tạo agent `deepseek` chạy trên `deepseek-flash`, dùng được ngay.

Nếu muốn đổi key, chạy lại script là được.

**Cách 2: chạy thử Mission Control với model thật.**

```bash
scripts/mission-control/live-deepseek.sh   # cần Docker + PostgreSQL (container pgtest)
```

Script dùng một database riêng, dùng xong bỏ. Nó chạy 3 mission:
- **coding:** sửa lỗi, rồi chứng minh bằng test ẩn;
- **research:** trả lời từ tài liệu, không sửa tài liệu;
- **safety:** repo có cài sẵn prompt injection.

Kết quả là bảng verdict, số token và chi phí. Mỗi mission bị giới hạn ở 0,25 USD (`LIVE_MAX_COST_USD`). Với giá hiện tại, cả lượt chạy thường chỉ tốn vài cent.

Cũng có thể đặt key bằng biến môi trường thay vì gõ vào: `DEEPSEEK_API_KEY=sk-... scripts/deepseek-setup.sh`. Key không bao giờ được truyền qua tham số dòng lệnh.

## The model

Researched September 2026, from DeepSeek's API change log, its pricing page and its thinking-mode guide:

| | |
|---|---|
| API model name | `deepseek-flash` (DeepSeek-V4.1-Flash, released 2026-09-10). `deepseek-v4-flash` still routes to it; `deepseek-v4-pro` is the larger model |
| Retired | `deepseek-chat` and `deepseek-reasoner` (since 2026-07-24; they no longer answer) |
| API | OpenAI-compatible, base `https://api.deepseek.com` |
| Context / output | 1M tokens / up to 384K |
| Thinking | On by default. `thinking: {"type": "enabled"\|"disabled"}` and `reasoning_effort: low\|high\|max`. In thinking mode, `temperature` is not supported, and on tool-call turns `reasoning_content` must be sent back, or the API answers 400 |
| Price (USD / 1M tokens) | Flash: input $0.30 peak / $0.15 off-peak, output $1.20 / $0.60, cache hit $0.006 / $0.003. Pro: input $1.32 / $0.66, output $3.96 / $1.98. Peak: 01:00–04:00 and 06:00–10:00 UTC, weekdays |

## What GoClaw does with it

| Area | Behaviour |
|---|---|
| Defaults | Provider type `deepseek` → base `https://api.deepseek.com`, model `deepseek-flash`. The same defaults apply at startup and when a provider is created from the web UI or API at runtime |
| Env | `GOCLAW_DEEPSEEK_API_KEY` (and, new, `GOCLAW_DEEPSEEK_BASE_URL`) registers the provider from the environment |
| Thinking | The agent's thinking level maps to DeepSeek's controls: no level → the provider default (on, high); `off` → `disabled`; `minimal`/`low` → `low`; `medium`/`high` → `high`; `xhigh` → `max`. The provider setting `thinking_enabled: false` wins. `temperature` is dropped while thinking. These controls are sent only to DeepSeek's own API, never to aggregators |
| Tool calls | `reasoning_content` is replayed on every assistant turn once the conversation is in thinking mode (an existing mechanism, now exercised end to end) |
| Usage | `prompt_cache_hit_tokens` is read as cache-read tokens, so cache hits are billed at the cache price |
| Context | `deepseek-flash` and `deepseek-v4*` use a 1M-token window |
| Prices | `deepseek-setup.sh` stores them as per-tenant pricing overrides at **peak** rates, so costs and mission cost limits are an upper bound off-peak. Edit them under Usage → Pricing if you prefer off-peak rates |

## How this was verified without the real API

This build environment cannot reach `api.deepseek.com`: the network policy blocks it. The wire contract was therefore verified against `scripts/mission-control/deepseekmock`. That is a local server enforcing DeepSeek's documented rules:
- the key;
- the model name;
- `thinking`/`reasoning_effort` values;
- no `temperature` in thinking mode;
- `reasoning_content` pass-back after tool calls.

It answers with scripted steps.

`MOCK=1 scripts/mission-control/live-deepseek.sh` runs the whole stack against it: gateway, provider set up through the API, agent loop, Docker sandbox, verifiers. Result: 3/3 missions succeeded, 13 requests, every one in thinking mode, `reasoning_content` pass-back checked on 9 tool follow-ups, 0 contract violations, and costs computed from the loaded prices.

The mock run exposed a real bug, which is now fixed. A DeepSeek provider added from the web UI while the gateway was running lost its provider type, and fell back to OpenAI's URL when no base was set. As a result, thinking controls were never applied and `temperature` was sent, which DeepSeek rejects in thinking mode.

**What the mock does not prove:** that DeepSeek's live API accepts every request, or how well the model does the tasks. That is what `live-deepseek.sh` with your key measures.
