// smoke.js —— 对运行中的服务做端到端冒烟断言
//
// 覆盖鉴权边界、模型清单、渠道清单、面板注入、以及「请求形态错误不该冷却账号」
// 这条曾经踩过的坑。所有断言都打真实 HTTP 请求，不做任何 mock。
//
// 用法: node smoke.js [baseURL]
require('module').Module._initPaths();
const fs = require('fs');

const BASE = process.argv[2] || 'http://127.0.0.1:7865';
const key = JSON.parse(fs.readFileSync('config.json', 'utf8')).api_key;
if (!key) throw new Error('config.json 里没有 api_key');

let fail = 0;
const ok = (m) => console.log('  ok   ' + m);
const bad = (m) => { console.log('  FAIL ' + m); fail++; };
const assert = (c, m, extra) => (c ? ok(m) : bad(m + (extra ? '  → ' + extra : '')));

async function req(path, { method = 'GET', auth = true, body } = {}) {
  const headers = {};
  if (auth) headers.Authorization = 'Bearer ' + key;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetch(BASE + path, {
    method, headers, body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let json = null;
  try { json = JSON.parse(text); } catch (e) { /* 非 JSON 响应（HTML） */ }
  return { status: res.status, text, json };
}

(async () => {
  // ── 鉴权边界 ──
  const health = await req('/healthz', { auth: false });
  assert(health.status === 200, '/healthz 无需鉴权且返回 200', String(health.status));

  const noAuth = await req('/v1/models', { auth: false });
  assert(noAuth.status === 401, '不带 Key 访问 /v1/models 返回 401', String(noAuth.status));

  const badKey = await fetch(BASE + '/v1/models', { headers: { Authorization: 'Bearer wrong' } });
  assert(badKey.status === 401, '错误 Key 返回 401', String(badKey.status));

  // ── 模型清单 ──
  const models = await req('/v1/models');
  assert(models.status === 200, '带 Key 访问 /v1/models 返回 200', String(models.status));
  const ids = (models.json.data || []).map((m) => m.id);
  // 不写死总数：账号增减会改变模型数（TRAE 只暴露账号自己可用的模型）。
  // 真正该钉死的是**前缀形态** —— 曾经用斜杠拼（workbuddy/cn/glm-5.2）导致路由走错渠道。
  assert(ids.length > 0, `模型列表非空（${ids.length} 个）`, String(ids.length));
  const LEGAL = ['workbuddy-cn', 'workbuddy-global', 'trae-cn', 'trae-global'];
  const prefixes = [...new Set(ids.map((i) => i.split('/')[0]))].sort();
  const illegal = prefixes.filter((p) => !LEGAL.includes(p));
  assert(illegal.length === 0, `模型前缀全部合法（${prefixes.join(', ')}）`, illegal.join(', '));
  const notTwo = ids.filter((i) => i.split('/').length !== 2);
  assert(notTwo.length === 0, '模型 ID 均为「前缀/模型名」两段式', notTwo.slice(0, 3).join(', '));

  // ── 渠道清单 ──
  const state = await req('/api/state');
  assert(state.status === 200, '/api/state 返回 200', String(state.status));
  const chans = state.json.channels || [];
  assert(chans.length === 4, '渠道数 4', String(chans.length));
  const chanKeys = chans.map((c) => c.channel).sort();
  assert(chanKeys.join(',') === 'traework/cn,traework/global,workbuddy/cn,workbuddy/global',
    '渠道键与后端 Kind 一致（TRAE 是 traework）', chanKeys.join(','));
  const ckOk = chans.filter((c) => c.checkin_supported).map((c) => c.channel).sort();
  assert(ckOk.join(',') === 'traework/cn,workbuddy/cn',
    '仅国内版渠道 checkin_supported=true', ckOk.join(','));

  // 有账号的渠道必须至少暴露一个模型 —— 渠道键到模型前缀的映射断了这里就会露。
  // 注意这里刻意写出了那个不对称：渠道键是 traework，模型前缀是 trae。
  const chanPrefix = (c) => (c.kind === 'traework' ? 'trae' : c.kind) + '-' + c.region;
  const missingPrefix = chans.filter((c) => c.total > 0)
    .map(chanPrefix).filter((p) => !prefixes.includes(p));
  assert(missingPrefix.length === 0,
    '每个有账号的渠道都暴露了模型（' + chans.filter((c) => c.total > 0).map(chanPrefix).join(', ') + '）',
    missingPrefix.join(', '));

  // ── 面板注入 ──
  const page = await fetch(BASE + '/');
  const html = await page.text();
  assert(page.status === 200, 'GET / 返回 200', String(page.status));
  assert(html.includes('"' + key + '"'), '回环访问面板时注入了真实 Key');
  assert(!html.includes('__W2A_KEY_EXPR__'), '面板里不残留注入占位符');

  // ── 请求形态错误不该牵连账号 ──
  const before = (await req('/api/state')).json.channels
    .reduce((n, c) => n + (c.accounts || []).reduce((m, a) => m + (a.err_count || 0), 0), 0);

  const badModel = await req('/v1/chat/completions', {
    method: 'POST',
    body: { model: 'workbuddy-cn/definitely-not-a-real-model', messages: [{ role: 'user', content: 'hi' }] },
  });
  assert(badModel.status === 400, '不存在的模型返回 400', String(badModel.status));

  const after = (await req('/api/state')).json.channels
    .reduce((n, c) => n + (c.accounts || []).reduce((m, a) => m + (a.err_count || 0), 0), 0);
  assert(after === before, '请求形态错误未增加账号 err_count（不冷却）',
    `${before} → ${after}`);

  const cooled = (await req('/api/state')).json.channels
    .some((c) => (c.accounts || []).some((a) => a.cooldown_until));
  assert(!cooled, '没有账号因此进入冷却');

  console.log(fail ? `\n== ${fail} 项失败 ==` : '\n== 全部通过 ==');
  process.exit(fail ? 1 : 0);
})().catch((e) => { console.error('冒烟异常:', e); process.exit(1); });
