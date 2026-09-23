// Browser check of the Missions UI against a running gateway + Vite dev
// server. Used by e2e-mission.sh when UI_CHECK=1.
//   node ui-missions.mjs <ui-base-url> <token> <out-dir>
// Requires the `playwright` package (PLAYWRIGHT_MODULE=/path/to/playwright).
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || "playwright");

const [base, token, out] = process.argv.slice(2);
let page;
const fail = async (msg) => {
  console.error("UI FAIL: " + msg);
  if (page) {
    await page.screenshot({ path: `${out}/ui-failure.png`, fullPage: true }).catch(() => {});
    console.error("URL: " + page.url());
    console.error("PAGE TEXT: " + (await page.locator("body").innerText().catch(() => "")).slice(0, 1500));
  }
  process.exit(1);
};

try {
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || undefined });
  page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const consoleErrors = [];
  page.on("dialog", (d) => d.accept()); // e.g. "skip setup?" confirmation
  page.on("console", (m) => { if (m.type() === "error") consoleErrors.push(m.text()); });
  const httpErrors = [];
  page.on("response", (r) => { if (r.status() >= 400) httpErrors.push(`${r.status()} ${r.url()}`); });
  page.on("pageerror", (e) => consoleErrors.push("JS: " + e.message));

  await page.goto(base + "/login");
  await page.locator('input[type="text"]').first().fill("operator");
  await page.locator('input[type="password"]').first().fill(token);
  await page.locator('button[type="submit"]').first().click();
  await page.waitForURL((u) => !u.pathname.startsWith("/login"), { timeout: 20000 });
  await page.waitForLoadState("networkidle").catch(() => {});

  // Client-side redirects right after login can abort a navigation; retry once.
  async function go(path) {
    try {
      await page.goto(base + path);
    } catch (e) {
      if (!String(e).includes("ERR_ABORTED")) throw e;
      await page.waitForTimeout(500);
      await page.goto(base + path);
    }
    await page.waitForLoadState("networkidle").catch(() => {});
    // A fresh gateway sends users to onboarding first; skip it for this check.
    if (new URL(page.url()).pathname.startsWith("/setup")) {
      await page.getByText(/Skip setup/i).click();
      await page.waitForURL((u) => !u.pathname.startsWith("/setup"));
      await page.goto(base + path);
    }
  }

  // 1. List shows existing missions with truthful statuses.
  await go("/missions");
  await page.getByTestId("mission-row").first().waitFor({ timeout: 20000 });
  const statuses = await page.getByTestId("mission-status").allInnerTexts();
  if (!statuses.some((s) => /succeeded/i.test(s))) await fail("no succeeded mission in list: " + statuses);
  if (!statuses.some((s) => /failed/i.test(s))) await fail("false-claim mission not shown as failed: " + statuses);
  await page.screenshot({ path: `${out}/ui-missions-list.png`, fullPage: true });

  // 2. Detail of a succeeded mission shows evidence.
  // The coding journey's mission (other succeeded missions have other evidence).
  await page.getByTestId("mission-row").filter({ hasText: /Succeeded/i }).filter({ hasText: /Fix Sum/ }).first().click();
  await page.getByTestId("mission-criteria").waitFor();
  const passCount = await page.locator('[data-testid^="criterion-"][data-status="pass"]').count();
  if (passCount !== 3) await fail(`expected 3 passing criteria, got ${passCount}`);
  const diff = await page.getByTestId("mission-diff").innerText();
  if (!diff.includes("if x > 0")) await fail("diff does not show the removed condition");
  await page.screenshot({ path: `${out}/ui-mission-detail.png`, fullPage: true });

  // 3. Create a mission from the UI and watch it finish.
  await go("/missions");
  await page.getByTestId("mission-new").click();
  await page.getByTestId("mission-contract").waitFor();
  // The submit button must be reachable even with a long contract.
  const submit = page.getByTestId("mission-submit");
  await submit.scrollIntoViewIfNeeded();
  const box = await submit.boundingBox();
  const vh = page.viewportSize().height;
  if (!box || box.y < 0 || box.y + box.height > vh) await fail(`submit button outside the viewport: ${JSON.stringify(box)} (viewport ${vh})`);
  await submit.click();
  await page.waitForURL(/\/missions\/[0-9a-f-]{36}$/, { timeout: 20000 });
  await page.getByTestId("mission-detail-status")
    .filter({ hasText: /Succeeded|Failed|Blocked|Partial|Cancelled/ }).waitFor({ timeout: 180000 });
  const finalStatus = await page.getByTestId("mission-detail-status").innerText();
  if (!/succeeded/i.test(finalStatus)) await fail("UI-created mission ended as " + finalStatus);
  await page.screenshot({ path: `${out}/ui-mission-created.png`, fullPage: true });

  // 4. Narrow viewport: no horizontal page overflow.
  await page.setViewportSize({ width: 390, height: 844 });
  await go("/missions");
  await page.getByTestId("mission-row").first().waitFor();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  if (overflow > 1) await fail(`horizontal overflow on mobile: ${overflow}px`);
  await page.screenshot({ path: `${out}/ui-missions-mobile.png`, fullPage: true });

  // Resource-load failures are reported separately below; external fetches
  // (fonts/CDNs) are blocked in sandboxed CI and are not app errors.
  const relevant = consoleErrors.filter((e) => !/favicon|WebSocket|ws:\/\/|Failed to load resource/i.test(e));
  if (relevant.length) await fail("console errors: " + relevant.join(" | "));
  const own = httpErrors.filter((e) => /\/v1\/missions/.test(e));
  if (own.length) await fail("mission API errors: " + own.join(" | "));
  if (httpErrors.length) console.log("non-mission HTTP errors (informational): " + [...new Set(httpErrors)].join(" | "));
  await browser.close();
  console.log("UI PASS");
} catch (e) {
  await fail(String(e));
}

