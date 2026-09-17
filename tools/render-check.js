// render-check.js —— 用真实 Chromium 渲染控制台面板并断言 DOM
//
// 这不是「看一眼截图」式的检查：每条断言都读真实渲染后的 DOM 文本，
// 任何一条不成立就以非零码退出。
//
// 用法: node render-check.js [baseURL]
require('module').Module._initPaths();
const { chromium } = require('playwright-core');
const fs = require('fs');
const path = require('path');

const BASE = process.argv[2] || 'http://127.0.0.1:7865';
const OUT = path.resolve('dist/shots');

// 本机 ms-playwright 缓存里已有 chromium，但版本号与 playwright-core 期望的不一致，
// 所以直接指定可执行文件，避免为了跑一次断言去下载浏览器。
const CHROME = process.env.CHROME_PATH ||
  'C:/Users/dev/AppData/Local/ms-playwright/chromium-1223/chrome-win64/chrome.exe';

// API Key 从 config.json 读（首次运行由服务端随机生成并落盘）。
// 不硬编码、也不经 shell 传参，避免转义问题。
function readAPIKey() {
  const cfg = JSON.parse(fs.readFileSync('config.json', 'utf8'));
  if (!cfg.api_key) throw new Error('config.json 里没有 api_key');
  return cfg.api_key;
}

let fail = 0;
const ok = (m) => console.log('  ok   ' + m);
const bad = (m) => { console.log('  FAIL ' + m); fail++; };
function assert(cond, msg, extra) {
  if (cond) ok(msg);
  else bad(msg + (extra ? '  → ' + extra : ''));
}

let browser = null;

(async () => {
  const apiKey = readAPIKey();
  fs.mkdirSync(OUT, { recursive: true });

  // ── 期望值从服务端派生，不写死 ──
  // 账号数是会变的：用户随时可能在面板里登录新账号。把「57 个模型」「2 个账号」
  // 这类数字写死，只会在数据正常变化时误报，反而掩盖真问题。
  // 这里断言的是「面板有没有把服务端的数据算对」，而不是「账号恰好几个」。
  const api = async (p) => {
    const r = await fetch(BASE + p, { headers: { Authorization: 'Bearer ' + apiKey } });
    if (!r.ok) throw new Error(`${p} → HTTP ${r.status}`);
    return r.json();
  };
  const state = await api('/api/state');
  const modelIds = (await api('/v1/models')).data.map((m) => m.id);

  const num = (v) => Number(v).toLocaleString('en-US'); // 与面板 num() 同口径
  const channels = state.channels || [];
  const accts = channels.flatMap((c) => c.accounts || []);
  const expTotal = channels.reduce((n, c) => n + c.total, 0);
  const expHealthy = channels.reduce((n, c) => n + c.healthy, 0);
  const creditAccts = accts.filter((a) => a.has_remain);
  const expCredits = creditAccts.reduce((n, a) => n + a.remain, 0);
  const ckAccts = accts.filter((a) => a.checkin_supported);
  const expCheckin = `${ckAccts.filter((a) => a.last_checkin_at).length}/${ckAccts.length}`;
  const wbCn = channels.find((c) => c.channel === 'workbuddy/cn') || { total: 0, accounts: [] };
  const wbGl = channels.find((c) => c.channel === 'workbuddy/global') || { total: 0, accounts: [] };

  browser = await chromium.launch({
    executablePath: CHROME,
    // 本机设了 http_proxy，必须显式禁用，否则浏览器会把 127.0.0.1 也发给代理
    args: ['--no-proxy-server'],
  });
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 960 } });

  // 刻意不做任何 Key 注入：面板必须靠服务端注入自己拿到 Key。
  // 这里要是还得手动喂 Key，就说明注入链路断了。
  const page = await ctx.newPage();
  const consoleErrors = [];
  const failedReqs = [];
  page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text()); });
  page.on('pageerror', (e) => consoleErrors.push('pageerror: ' + e.message));
  page.on('requestfailed', (r) => failedReqs.push(r.url() + ' ' + (r.failure() || {}).errorText));

  await page.goto(BASE, { waitUntil: 'networkidle' });
  await page.waitForSelector('.nav-item', { timeout: 10000 });
  // 侧栏是同步渲染的，等它通过说明不了数据到位 —— 必须等 /api/state 回来后的真实内容。
  await page.waitForSelector('#view .stat', { timeout: 10000 });
  await page.waitForTimeout(400);

  // 曾经踩过的坑：页面函数漏写 return，innerHTML 被赋成字面量 "undefined"，
  // 页面看着空白但控制台一声不吭。这里显式拦一道。
  const viewRaw = await page.$eval('#view', (e) => e.innerHTML);
  if (viewRaw.trim() === 'undefined') {
    throw new Error('#view 的 innerHTML 是字面量 "undefined" —— 某个页面渲染函数没有 return');
  }

  // ── 侧栏 ──
  const navLabels = await page.$$eval('.nav-item .lbl', (els) => els.map((e) => e.textContent.trim()));
  assert(navLabels.join('|') === '总览|WorkBuddy|TRAE SOLO|模型|接入',
    '侧栏 5 个导航项且顺序正确', navLabels.join('|'));

  const sbTitle = await page.textContent('.sb-name');
  assert(sbTitle.trim() === 'Work2Api', '侧栏标题为 Work2Api', sbTitle);
  const sbSub = await page.textContent('.sb-sub');
  assert(/WorkBuddy/.test(sbSub) && /TRAE/.test(sbSub), '侧栏副标题含两个渠道', sbSub);

  // 主色应当是 WorkNiuMa 的紫罗兰 hsl(262 83% 58%) → rgb(124, 59, 237)
  const logoBg = await page.$eval('.sb-logo', (e) => getComputedStyle(e).backgroundColor);
  assert(logoBg === 'rgb(124, 59, 237)', '主色为紫罗兰 #7c3bed（与 WorkNiuMa 一致）', logoBg);

  // ── API Key：服务端注入链路 ──
  assert(await page.$('#key') === null, '侧栏没有手填 API Key 的输入框', '仍存在 #key');
  assert(await page.$('.sb-field') === null, '侧栏没有残留的 .sb-field 容器');

  // W2A_KEY 在 IIFE 内、不是全局变量，所以从服务端返回的 HTML 原文里核对注入结果。
  const servedHTML = await page.content();
  const inj = servedHTML.match(/var W2A_KEY = ("(?:[^"\\]|\\.)*");/);
  assert(!!inj && JSON.parse(inj[1]) === apiKey,
    '服务端把 config.json 里的 Key 注入了面板', inj ? inj[1] : '未找到 W2A_KEY 赋值');
  assert(!/__W2A_KEY_EXPR__/.test(servedHTML),
    '占位符已被替换（页面里不残留 __W2A_KEY_EXPR__）');

  // ── 总览 ──
  const statCards = await page.$$eval('.stat', (els) => els.map((e) => ({
    k: e.querySelector('.k').textContent.trim(),
    v: e.querySelector('.v').textContent.trim(),
  })));
  if (!statCards.length) {
    const viewText = (await page.$eval('#view', (e) => e.innerHTML)).replace(/\s+/g, ' ').slice(0, 400);
    console.log('     [debug] #view.innerHTML = ' + viewText);
    console.log('     [debug] 失败请求 = ' + (failedReqs.join(' | ') || '无'));
    console.log('     [debug] 控制台 = ' + (consoleErrors.join(' | ') || '无'));
  }
  assert(statCards.length === 4, '总览有 4 个指标卡', String(statCards.length));
  const byKey = Object.fromEntries(statCards.map((s) => [s.k, s.v]));
  assert(byKey['账号总数'] === expTotal + '个', `账号总数 = ${expTotal}（与 /api/state 一致）`, JSON.stringify(byKey));
  assert(byKey['健康'] === `${expHealthy}/${expTotal}`, `健康 = ${expHealthy}/${expTotal}`, JSON.stringify(byKey));
  assert(byKey['可用积分'] === num(expCredits), `可用积分 = ${num(expCredits)}（含千位分隔）`, JSON.stringify(byKey));
  assert(byKey['今日签到'] === expCheckin, `今日签到 = ${expCheckin}`, JSON.stringify(byKey));

  // 渠道列表 4 行
  const chRows = await page.$$eval('[data-goto]', (els) => els.map((e) => e.textContent.replace(/\s+/g, ' ').trim()));
  assert(chRows.length === 4, '总览列出 4 个渠道', String(chRows.length));
  assert(chRows.some((t) => /WorkBuddy · 国内版/.test(t)), '含 WorkBuddy 国内版');
  assert(chRows.some((t) => /WorkBuddy · 国际版/.test(t)), '含 WorkBuddy 国际版');
  assert(chRows.some((t) => /TRAE SOLO · 国内版/.test(t)), '含 TRAE 国内版');
  assert(chRows.some((t) => /TRAE SOLO · 国际版/.test(t)), '含 TRAE 国际版');
  assert(chRows.some((t) => /无签到体系/.test(t)), '国际版标注「无签到体系」');

  // 点渠道行必须真的跳过去。曾经因为渠道键写成 'trae' 而真实值是 'traework'，
  // familyOf() 查不到 → 渲染函数返回 undefined → 点击毫无反应且不报错。
  await page.click('[data-goto="traework"]');
  await page.waitForTimeout(400);
  const traeH1 = await page.textContent('.h1');
  assert(traeH1.trim() === 'TRAE SOLO', '点 TRAE 渠道行可跳转到渠道页', traeH1);
  await page.click('.nav-item[data-nav="overview"]');
  await page.waitForTimeout(400);

  await page.screenshot({ path: path.join(OUT, '01-overview.png'), fullPage: true });

  // ── WorkBuddy 页 ──
  await page.click('.nav-item[data-nav="workbuddy"]');
  await page.waitForSelector('.seg', { timeout: 5000 });
  await page.waitForTimeout(300);

  const segLabels = await page.$$eval('.seg button', (els) => els.map((e) => e.textContent.replace(/\s+/g, ' ').trim()));
  assert(segLabels.length === 2, 'WorkBuddy 有 2 个版本分段', JSON.stringify(segLabels));
  assert(/国内版/.test(segLabels[0]) && /国际版/.test(segLabels[1]), '分段为 国内版 / 国际版', JSON.stringify(segLabels));

  const h1 = await page.textContent('.h1');
  assert(h1.trim() === 'WorkBuddy', '标题为 WorkBuddy', h1);

  let rows = await page.$$eval('.arow[data-key]', (els) => els.map((e) => e.textContent.replace(/\s+/g, ' ').trim()));
  assert(rows.length === wbCn.total, `国内版账号行数 = ${wbCn.total}`, String(rows.length));
  if (wbCn.total > 0) {
    const a0 = wbCn.accounts[0];
    const blob = rows.join(' | ');
    assert(blob.includes(a0.uid), '账号行显示了账号 ID', blob.slice(0, 120));
    if (a0.has_remain) {
      assert(blob.includes(num(a0.remain)), `账号行显示积分 ${num(a0.remain)}`, blob.slice(0, 120));
    }
    assert(/正常|冷却|已停用/.test(blob), '账号行显示了状态徽标', blob.slice(0, 120));
    assert(/签到/.test(blob), '国内版账号行含签到信息', blob.slice(0, 120));
  }

  // 展开详情 → 积分明细（这一步依赖真实上游返回权益包明细，没账号就跳过）
  if (wbCn.total > 0) {
  await page.click('.arow[data-key] [data-det]');
  await page.waitForSelector('.det', { timeout: 5000 });
  await page.waitForTimeout(1200);
  const detText = await page.textContent('.det');
  assert(/积分明细/.test(detText), '详情面板含「积分明细」');
  const hasBar = await page.$('.det .bar') !== null;
  assert(hasBar, '详情面板渲染出用量条', '未找到 .bar');

  // 条宽必须与同一行的「剩余 / 总量」文案同口径（±2% 容差）。
  // 这条断言是为了钉死「条按最大包算、文案按本包算」这类混用。
  const barCheck = await page.$$eval('.det .bar', (els) => els.map((b) => {
    const pkg = b.previousElementSibling;
    const pv = pkg ? pkg.querySelector('.pv') : null;
    const m = pv ? pv.textContent.match(/([\d,]+)\s*\/\s*([\d,]+)/) : null;
    if (!m) return null;
    const remain = Number(m[1].replace(/,/g, ''));
    const total = Number(m[2].replace(/,/g, ''));
    const barW = b.getBoundingClientRect().width;
    const iW = b.querySelector('i').getBoundingClientRect().width;
    return {
      got: barW > 0 ? (iW / barW) * 100 : 0,
      want: total > 0 ? (remain / total) * 100 : 0,
      label: pv.textContent.trim(),
    };
  }).filter(Boolean));
  const mismatch = barCheck.filter((r) => Math.abs(r.got - r.want) > 2);
  assert(barCheck.length > 0 && mismatch.length === 0,
    '每条用量条宽度与「剩余/总量」同口径（' + barCheck.length + ' 条）',
    JSON.stringify(mismatch.slice(0, 3)));
  }

  await page.screenshot({ path: path.join(OUT, '02-workbuddy-detail.png'), fullPage: true });

  // ── 账号快关：开启才进入号池 ──
  // 「按钮渲染出来了」挡不住漏绑 onclick，所以这里真点一次，并且回查 /api/state
  // 确认服务端状态与号池健康数同步变化。
  // 注意：这会临时改动真实账号状态，因此用 try/finally 保证无论如何都复原。
  const swCount = await page.$$eval('.arow[data-key] [data-sw]', (els) => els.length);
  assert(swCount === wbCn.total, `每个账号行都有快关（${swCount} 个）`, String(swCount));

  if (wbCn.total > 0) {
    const target = wbCn.accounts[0];
    const sel = `.arow[data-key="${wbCn.channel}|${target.uid}"]`;
    const wasDisabled = !!target.disabled;
    const baseHealthy = wbCn.healthy;
    const readSw = () => page.$eval(`${sel} [data-sw]`, (e) => e.getAttribute('aria-checked'));

    const sw0 = await readSw();
    assert(sw0 === (wasDisabled ? 'false' : 'true'),
      '快关初始状态与 /api/state 的 disabled 一致', sw0 + ' vs disabled=' + wasDisabled);

    // 缺 enabled 必须 400，不能拿零值 false 静默把账号停掉。
    const missing = await fetch(BASE + '/api/account/toggle', {
      method: 'POST',
      headers: { Authorization: 'Bearer ' + apiKey, 'Content-Type': 'application/json' },
      body: JSON.stringify({ channel: wbCn.channel, uid: target.uid }),
    });
    assert(missing.status === 400, '缺 enabled 字段时返回 400（不会静默停用账号）', String(missing.status));

    // 点一下 = 状态翻转：在池 → 移出，已移出 → 开进池
    const wantEnabled = wasDisabled;
    try {
      await page.click(`${sel} [data-sw]`);
      await page.waitForFunction(
        ([s, want]) => {
          const e = document.querySelector(s + ' [data-sw]');
          return !!e && e.getAttribute('aria-checked') === want;
        },
        [sel, wantEnabled ? 'true' : 'false'],
        { timeout: 10000 },
      );

      const st = await api('/api/state');
      const c = st.channels.find((x) => x.channel === wbCn.channel);
      const acct = c.accounts.find((a) => a.uid === target.uid);
      assert(acct.disabled === !wantEnabled,
        `点快关后服务端 disabled 变为 ${!wantEnabled}`, String(acct.disabled));
      assert(c.healthy === baseHealthy + (wantEnabled ? 1 : -1),
        `号池健康数随之${wantEnabled ? ' +1' : ' -1'}（${baseHealthy} → ${c.healthy}）`,
        `${baseHealthy} → ${c.healthy}`);

      if (!wantEnabled) {
        const cls = await page.$eval(sel, (e) => e.className);
        assert(/\boff\b/.test(cls), '移出号池的行带 .off 压暗类', cls);
        const rowText = await page.$eval(sel, (e) => e.textContent.replace(/\s+/g, ' '));
        assert(/已停用/.test(rowText), '移出号池的行显示「已停用」徽标', rowText.slice(0, 120));
        assert(/已手动移出号池/.test(rowText),
          '停用原因渲染成中文文案（不是原始枚举 manual）', rowText.slice(0, 160));
      }
    } finally {
      // 无论上面成败，都不许把用户的账号留在被改动后的状态
      const cur = await api('/api/state');
      const cc = cur.channels.find((x) => x.channel === wbCn.channel);
      const ca = cc.accounts.find((a) => a.uid === target.uid);
      if (!!ca.disabled !== wasDisabled) {
        const r = await fetch(BASE + '/api/account/toggle', {
          method: 'POST',
          headers: { Authorization: 'Bearer ' + apiKey, 'Content-Type': 'application/json' },
          body: JSON.stringify({ channel: wbCn.channel, uid: target.uid, enabled: !wasDisabled }),
        });
        ok('快关测试后已复原账号状态（enabled=' + !wasDisabled + '，HTTP ' + r.status + '）');
      }
      await page.click('.nav-item[data-nav="workbuddy"]');
      await page.waitForTimeout(300);
      await page.click('.seg button:nth-child(1)');
      await page.waitForTimeout(300);
    }
  }

  // 切到国际版
  await page.click('.seg button:nth-child(2)');
  await page.waitForTimeout(400);
  const glRows = await page.$$eval('.arow[data-key]', (els) => els.map((e) => e.textContent.replace(/\s+/g, ' ').trim()));
  assert(glRows.length === wbGl.total, `国际版账号行数 = ${wbGl.total}`, String(glRows.length));
  if (wbGl.total > 0) {
    const glBlob = glRows.join(' | ');
    assert(glBlob.includes(wbGl.accounts[0].uid), '国际版账号行显示了账号 ID', glBlob.slice(0, 120));
    // 该版本没有签到体系：行内不应出现签到按钮或签到信息
    assert(!/签到/.test(glBlob), '国际版账号行不含签到入口（该版本无签到体系）', glBlob.slice(0, 120));
  }
  const warnCard = await page.$('.card.warncard');
  assert(warnCard !== null, '国际版页展示「无签到体系」提示卡');

  await page.screenshot({ path: path.join(OUT, '03-workbuddy-global.png'), fullPage: true });

  // ── 模型页 ──
  await page.click('.nav-item[data-nav="models"]');
  await page.waitForSelector('.mcell', { timeout: 10000 });
  await page.waitForTimeout(400);
  const mCount = await page.$$eval('.mcell', (els) => els.length);
  assert(mCount === modelIds.length, `模型页渲染 ${modelIds.length} 个模型（与 /v1/models 一致）`, String(mCount));
  const mGroups = await page.$$eval('.sec-h', (els) => els.map((e) => e.textContent.replace(/\s+/g, ' ').trim()));
  const expGroups = [...new Set(modelIds.map((i) => i.split('/')[0]))].sort();
  const missGroup = expGroups.filter((g) => !mGroups.some((t) => t.includes(g)));
  assert(missGroup.length === 0, `按前缀分组齐全（${expGroups.join(', ')}）`, JSON.stringify(mGroups.slice(0, 8)));

  // 模型表来源必须如实展示（兜底表可能多也可能少，用户有权知道）
  const srcText = await page.textContent('#view');
  assert(/上游实时|静态兜底/.test(srcText), '模型页展示了模型表来源');
  assert(/上游实时/.test(srcText), '模型页至少有一个渠道是上游实时');
  const modelBtn = await page.$('#actModels');
  assert(modelBtn !== null, '模型页有「重新拉取」按钮');

  // 按钮「渲染出来」不等于「接了线」—— 这类面板最常见的坑就是漏绑 onclick。
  // 真点一次，等成功 toast，再确认列表被重新拉回来了。
  await page.click('#actModels');
  await page.waitForSelector('.toast.ok', { timeout: 90000 });
  const toastText = await page.textContent('.toast.ok .tt');
  assert(/模型列表已更新/.test(toastText), '「重新拉取」按钮可用且提示成功', toastText);
  await page.waitForSelector('.mcell', { timeout: 30000 });
  const mCount2 = await page.$$eval('.mcell', (els) => els.length);
  assert(mCount2 === modelIds.length, `重拉后模型数仍为 ${modelIds.length}`, String(mCount2));

  // 搜索过滤：期望值同样从 /v1/models 派生
  const q = 'gpt-5.5';
  const expHits = modelIds.filter((i) => i.toLowerCase().includes(q));
  assert(expHits.length > 0, `测试关键词 ${q} 在模型表里有命中（否则下面的断言恒真）`);
  await page.fill('#mq', q);
  await page.waitForTimeout(400);
  const filtered = await page.$$eval('.mcell .a', (els) => els.map((e) => e.textContent.trim()));
  assert(filtered.length === expHits.length && filtered.every((id) => id.includes(q)),
    `搜索 ${q} 命中 ${expHits.length} 条且都含关键词`, JSON.stringify(filtered));
  await page.fill('#mq', '');
  await page.waitForTimeout(400);

  await page.screenshot({ path: path.join(OUT, '04-models.png'), fullPage: true });

  // ── 接入页 ──
  await page.click('.nav-item[data-nav="usage"]');
  await page.waitForSelector('.code', { timeout: 5000 });
  await page.waitForTimeout(300);
  const usageText = await page.textContent('#view');
  assert(/Base URL/.test(usageText), '接入页含 Base URL');
  assert(/127\.0\.0\.1:7865\/v1/.test(usageText), 'Base URL 指向本服务');
  assert(/from openai import OpenAI/.test(usageText), '含 Python 片段');
  assert(/workbuddy-cn/.test(usageText) && /workbuddy-global/.test(usageText), '含模型前缀说明');
  const codeBlocks = await page.$$eval('.code', (els) => els.length);
  assert(codeBlocks >= 3, '至少 3 个代码块', String(codeBlocks));
  // 面板不再有输入框，用户只能从这里拿到 Key 去配置客户端 —— 必须能看到、能复制。
  assert(usageText.includes(apiKey), '接入页展示了完整 API Key（供复制到客户端）');
  assert(/w2a_[0-9a-f]{32}/.test(apiKey), 'API Key 形如 w2a_ + 32 位十六进制', apiKey);

  await page.screenshot({ path: path.join(OUT, '05-usage.png'), fullPage: true });

  // ── 深色主题 ──
  await page.click('.nav-item[data-nav="overview"]');
  await page.waitForTimeout(300);
  await page.click('#themeBtn');
  await page.waitForTimeout(400);
  const isDark = await page.evaluate(() => document.body.classList.contains('dark'));
  assert(isDark, '深色主题切换生效');
  const darkBg = await page.$eval('.card', (e) => getComputedStyle(e).backgroundColor);
  assert(darkBg !== 'rgb(255, 255, 255)', '深色下卡片背景不再是白色', darkBg);

  // 暗色对比度：代替「肉眼看能不能看清」。曾经 .btn 没设 color，
  // <button> 走 UA 的 buttontext（纯黑），outline 按钮在暗色下是黑字黑底。
  const contrast = await page.evaluate(() => {
    const rgb = (s) => {
      const m = s.match(/rgba?\(([^)]+)\)/);
      const p = m[1].split(',').map(parseFloat);
      return [p[0], p[1], p[2], p.length > 3 ? p[3] : 1];
    };
    const lum = (c) => {
      const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
      return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]);
    };
    const over = (fg, bg) => {
      const a = fg[3];
      return [fg[0] * a + bg[0] * (1 - a), fg[1] * a + bg[1] * (1 - a), fg[2] * a + bg[2] * (1 - a), 1];
    };
    // 逐层向上合成到不透明为止（badge 这类背景是半透明的）
    const effBg = (el) => {
      const layers = [];
      let n = el;
      while (n && n.nodeType === 1) {
        const c = rgb(getComputedStyle(n).backgroundColor);
        if (c && c[3] > 0) { layers.push(c); if (c[3] >= 1) break; }
        n = n.parentElement;
      }
      let b = [255, 255, 255, 1];
      for (let i = layers.length - 1; i >= 0; i--) b = over(layers[i], b);
      return b;
    };
    const ratioOf = (sel) => {
      const e = document.querySelector(sel);
      if (!e) return null;
      const bg = effBg(e);
      const fg = over(rgb(getComputedStyle(e).color), bg);
      const l1 = lum(fg), l2 = lum(bg);
      return +(((Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05))).toFixed(2);
    };
    return {
      outlineBtn: ratioOf('.btn.outline'),
      sub: ratioOf('.sub'),
      badgeSuccess: ratioOf('.badge.success'),
    };
  });
  assert(contrast.outlineBtn >= 4.5, '暗色下 outline 按钮文字对比度 ≥4.5', JSON.stringify(contrast));
  assert(contrast.sub >= 4.5, '暗色下次要文字对比度 ≥4.5', JSON.stringify(contrast));
  assert(contrast.badgeSuccess >= 4.5, '暗色下 success 徽标对比度 ≥4.5', JSON.stringify(contrast));
  await page.screenshot({ path: path.join(OUT, '06-dark.png'), fullPage: true });
  await page.click('#themeBtn');
  await page.waitForTimeout(300);

  // ── 无错误 ──
  assert(consoleErrors.length === 0, '浏览器控制台无错误', consoleErrors.slice(0, 3).join(' | '));
  assert(failedReqs.length === 0, '无失败请求', failedReqs.slice(0, 3).join(' | '));

  // ── 窄屏 ──
  await page.setViewportSize({ width: 420, height: 900 });
  await page.waitForTimeout(500);
  const navW = await page.$eval('.sidebar', (e) => e.getBoundingClientRect().width);
  assert(navW <= 70, '窄屏下侧栏收成图标条', String(Math.round(navW)));
  await page.screenshot({ path: path.join(OUT, '07-narrow.png'), fullPage: true });

  await browser.close();

  console.log('\n截图输出: ' + OUT);
  fs.readdirSync(OUT).sort().forEach((f) => console.log('  ' + f));
  console.log(fail ? `\n== ${fail} 项失败 ==` : '\n== 全部通过 ==');
  process.exit(fail ? 1 : 0);
})().catch(async (e) => {
  console.error('渲染检查异常:', e);
  // 断言中途抛错时也要收掉浏览器，否则会留下游离的 chrome 进程
  try { if (browser) await browser.close(); } catch (_) {}
  process.exit(1);
});
