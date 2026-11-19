// B17 浏览器点击验收（Playwright + chromium headless shell）
// 断言项：默认顺序回退 / 切入竞速并持久化 / 刷新恢复 / 参选为空退化顺序 /
// 未星标不可参选 / 撤星后旧条目失效 / B 模式不影响 A 的 Host 开关与星标。
const { chromium } = require('/opt/data/cache/scratch/pw/node_modules/playwright');
const fs = require('fs');

const BASE = process.argv[2];
const EXE = process.argv[3];
const OUT = process.argv[4];
const USER = 'r5admin';
const PASS = 'R5-drill-pass-1';

const result = { steps: [], network: [], console: [], assertions: {} };
const A = (name, ok, detail) => {
  result.assertions[name] = { ok: !!ok, detail: detail === undefined ? null : detail };
  console.log(`[assert] ${name} => ${ok ? 'OK' : 'FAIL'} ${detail ? JSON.stringify(detail).slice(0, 300) : ''}`);
};

async function main() {
  const browser = await chromium.launch({ executablePath: EXE, headless: true, args: ['--no-sandbox'] });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  page.on('console', m => result.console.push({ type: m.type(), text: m.text().slice(0, 400) }));
  page.on('request', r => { if (r.url().includes('/api/')) result.network.push({ m: r.method(), u: r.url().replace(BASE, ''), body: (r.postData() || '').slice(0, 300) }); });

  // 登录（共享 cookie）
  await ctx.request.post(BASE + '/api/setup', { data: { user: USER, pass: PASS } });
  const lg = await ctx.request.post(BASE + '/api/login', { data: { user: USER, pass: PASS } });
  A('api_login', lg.status() === 200, lg.status());

  // 初始状态快照（A 层：Host 开关 + 星标）
  const snapA = async () => {
    const me = await (await ctx.request.get(BASE + '/api/me')).json();
    const srcs = await (await ctx.request.get(BASE + '/api/sources')).json();
    const settings = await (await ctx.request.get(BASE + '/api/settings')).json();
    return {
      moHosts: settings.multi_origin_hosts,
      star: (srcs.items || srcs.sources || []).map(s => [s.id, !!s.preferred]),
      csrf: me.csrf,
    };
  };
  const a0 = await snapA();
  fs.writeFileSync(OUT + '/a-layer-before.json', JSON.stringify(a0, null, 2));

  await page.goto(BASE + '/', { waitUntil: 'networkidle' });
  await page.waitForTimeout(800);

  // 1) 默认顺序回退
  const seq = page.locator('#crossSec button[data-rmode="sequential"]');
  const race = page.locator('#crossSec button[data-rmode="race"]');
  const seqPressed = await seq.getAttribute('aria-pressed');
  const desc0 = (await page.locator('#crossDesc').textContent()) || '';
  A('1_default_sequential', seqPressed === 'true' && desc0.includes('省请求'), { seqPressed, desc0 });
  await page.screenshot({ path: OUT + '/01-default-sequential.png', fullPage: false });

  // 2) 切到跨站竞速并持久化
  const putRace = page.waitForResponse(r => r.url().includes('/api/cross-registry') && r.request().method() === 'PUT');
  await race.click();
  const pr = await putRace;
  await page.waitForTimeout(600);
  const racePressed = await race.getAttribute('aria-pressed');
  const desc1 = (await page.locator('#crossDesc').textContent()) || '';
  const cr1 = await (await ctx.request.get(BASE + '/api/cross-registry')).json();
  A('2_switch_race_persisted', pr.status() === 200 && racePressed === 'true' && cr1.configured === 'race',
    { status: pr.status(), racePressed, configured: cr1.configured, desc: desc1 });
  await page.screenshot({ path: OUT + '/02-race-mode.png', fullPage: false });

  // 3) 刷新后状态恢复
  await page.reload({ waitUntil: 'networkidle' });
  await page.waitForTimeout(800);
  const racePressed2 = await page.locator('#crossSec button[data-rmode="race"]').getAttribute('aria-pressed');
  const desc2 = (await page.locator('#crossDesc').textContent()) || '';
  A('3_reload_restore', racePressed2 === 'true' && desc2.includes('并行'), { racePressed2, desc2 });
  await page.screenshot({ path: OUT + '/03-after-reload.png', fullPage: false });

  // 5) 勾选未星标 Source 被拒（未星标 id=3 由 Python 预建；这里只看 UI 不提供该勾选项 + API 拒绝）
  const me = await (await ctx.request.get(BASE + '/api/me')).json();
  const rej = await ctx.request.put(BASE + '/api/cross-registry', {
    headers: { 'X-CSRF-Token': me.csrf },
    data: { selected: [3] },
  });
  const rejBody = await rej.json().catch(() => ({}));
  const selBoxes = await page.locator('#crossSel input[data-cross-sel]').count();
  const ids3 = await page.locator('#crossSel input[data-cross-sel]').evaluateAll(
    els => els.map(e => parseInt(e.getAttribute('data-cross-sel'), 10)));
  // 断言：API 明确拒绝未星标 id（400 + 说明），且 UI 不提供该 id 的勾选项
  A('5_unstarred_rejected', rej.status() >= 400 && !ids3.includes(3),
    { status: rej.status(), body: rejBody, checkboxCount: selBoxes, checkboxIds: ids3 });

  // 4) 参选为空（退化顺序的真实命中数由 Python 侧取证）
  const selEmpty = await ctx.request.put(BASE + '/api/cross-registry', {
    headers: { 'X-CSRF-Token': me.csrf }, data: { selected: [] },
  });
  A('4a_selected_cleared', selEmpty.status() === 200, selEmpty.status());

  // 6) 撤销星标后旧条目不再参与
  const srcsNow = await (await ctx.request.get(BASE + '/api/sources')).json();
  const items = srcsNow.items || srcsNow.sources || [];
  const starredNow = items.filter(s => s.preferred).map(s => s.id);
  const targetId = starredNow[starredNow.length - 1];
  // 先勾上它（若已在）
  await ctx.request.put(BASE + '/api/cross-registry', {
    headers: { 'X-CSRF-Token': me.csrf }, data: { selected: starredNow },
  });
  const beforeUnstar = await (await ctx.request.get(BASE + '/api/cross-registry')).json();
  const boxesBefore = await page.locator('#crossSel input[data-cross-sel]').count();
  await ctx.request.patch(BASE + '/api/sources/' + targetId, {
    headers: { 'X-CSRF-Token': me.csrf }, data: { preferred: false },
  });
  await page.reload({ waitUntil: 'networkidle' });
  await page.waitForTimeout(700);
  const afterUnstar = await (await ctx.request.get(BASE + '/api/cross-registry')).json();
  // 注：设置表里的 selected 不做剪枝（不 star 的 id 仍留在存储里），
  // 资格由「星标 + 勾选 + 能力」三者共同裁决；这里断言 UI 不再提供该勾选项，
  // 真实「不再参与」由 Python 用上游命中数取证。
  const boxesAfter = await page.locator('#crossSel input[data-cross-sel]').count();
  const idsAfter = await page.locator('#crossSel input[data-cross-sel]').evaluateAll(
    els => els.map(e => parseInt(e.getAttribute('data-cross-sel'), 10)));
  // 断言：撤星后该 id 既不在 starred，也不在 UI 勾选清单；勾选项数量相应减一
  A('6_unstar_hides_from_ui',
    !(afterUnstar.starred || []).includes(targetId) && !idsAfter.includes(targetId) && boxesAfter === boxesBefore - 1,
    { targetId, before: beforeUnstar.selected, after: afterUnstar.selected, starred: afterUnstar.starred,
      boxesBefore, boxesAfter, idsAfter });
  await page.screenshot({ path: OUT + '/06-after-unstar.png', fullPage: false });

  // 7) B 模式变化不影响 A 的 Host 开关与 Source 星标
  const a1 = await snapA();
  fs.writeFileSync(OUT + '/a-layer-after.json', JSON.stringify(a1, null, 2));
  const hostsSame = JSON.stringify(a0.moHosts) === JSON.stringify(a1.moHosts);
  // 星标只允许因第 6 步的显式撤星而变化（targetId 之外必须一致）
  const starOthersSame = JSON.stringify(
    a0.star.filter(x => x[0] !== targetId)) === JSON.stringify(a1.star.filter(x => x[0] !== targetId));
  A('7_b_mode_isolated_from_a', hostsSame && starOthersSame, { hostsSame, starOthersSame, before: a0.star, after: a1.star });

  // 回到顺序回退，便于后续行为断言
  await page.locator('#crossSec button[data-rmode="sequential"]').click();
  await page.waitForTimeout(500);

  fs.writeFileSync(OUT + '/b17-result.json', JSON.stringify(result, null, 2));
  await browser.close();
  const failed = Object.entries(result.assertions).filter(([, v]) => !v.ok).map(([k]) => k);
  console.log('FAILED=' + JSON.stringify(failed));
  process.exit(failed.length ? 1 : 0);
}

main().catch(e => { console.error('DRILL ERROR', e && e.stack || e); process.exit(2); });
