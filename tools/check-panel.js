// check-panel.js —— 校验 web/index.html 的内联脚本语法与关键引用
// 用法: node check-panel.js web/index.html
const fs = require('fs');
const vm = require('vm');

const path = process.argv[2] || 'web/index.html';
const html = fs.readFileSync(path, 'utf8');

let fail = 0;
const ok = (m) => console.log('  ok   ' + m);
const bad = (m) => { console.log('  FAIL ' + m); fail++; };

// ── 1. 提取内联 <script>（无 src）并做语法检查 ──
const scripts = [...html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/gi)];
if (!scripts.length) bad('没有找到内联 script');
scripts.forEach((m, i) => {
  const code = m[1];
  try {
    new vm.Script(code, { filename: `inline-${i}.js` });
    ok(`内联脚本 #${i} 语法通过（${code.split('\n').length} 行）`);
  } catch (e) {
    bad(`内联脚本 #${i} 语法错误: ${e.message}`);
  }
});

// ── 2. 括号 / 引号配平的粗检（语法检查已覆盖，这里只做兜底提示）──
const css = [...html.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/gi)].map((m) => m[1]).join('\n');
let depth = 0;
for (const ch of css) { if (ch === '{') depth++; else if (ch === '}') depth--; if (depth < 0) break; }
if (depth === 0) ok('CSS 花括号配平'); else bad(`CSS 花括号不配平（差 ${depth}）`);

// ── 3. 检查 JS 里用到的 DOM id 是否都在 HTML 中定义（或由 JS 动态创建）──
const htmlIds = new Set([...html.matchAll(/\bid="([A-Za-z0-9_-]+)"/g)].map((m) => m[1]));
const jsIds = new Set([...html.matchAll(/\$\('([A-Za-z0-9_-]+)'\)/g)].map((m) => m[1]));
// 这些 id 是渲染后才出现、或由 JS 动态创建的，属于预期
const dynamic = new Set([
  'actCheckin', 'actRefresh', 'actModels', 'btnLogin', 'btnRefresh', 'mq',
  'authUrl', 'openBtn', 'copyBtn', 'cancelBtn', 'mask', 'loginStatus',
  'bannerHost', // JS 启动时 createElement 后插入
  'keyInput', 'keySave', // 远程访问时由 renderBanner 在 #bannerHost 内动态生成
]);
const missing = [...jsIds].filter((id) => !htmlIds.has(id) && !dynamic.has(id));
if (missing.length) bad('JS 引用了未定义的 id: ' + missing.join(', '));
else ok('JS 引用的 DOM id 全部有来源');

// ── 4. 检查图标名是否都在 I 表里 ──
const iconTable = html.match(/var I = \{([\s\S]*?)\n  \};/);
if (!iconTable) { bad('找不到图标表 I'); }
else {
  const defined = new Set([...iconTable[1].matchAll(/^\s{4}([A-Za-z]+):/gm)].map((m) => m[1]));
  const used = new Set([...html.matchAll(/\bic\('([A-Za-z]+)'/g)].map((m) => m[1]));
  const miss = [...used].filter((n) => !defined.has(n));
  if (miss.length) bad('ic() 使用了未定义的图标: ' + miss.join(', '));
  else ok(`图标引用完整（定义 ${defined.size} 个，使用 ${used.size} 个）`);
}

// ── 5. 检查后端接口路径是否都存在 ──
const backend = fs.readFileSync('internal/app/app.go', 'utf8') + fs.readFileSync('internal/server/handler.go', 'utf8');
const called = [...new Set([...html.matchAll(/['"`](\/(?:api|v1|healthz)[A-Za-z0-9/_-]*)['"`?]/g)].map((m) => m[1]))];
const routes = [...backend.matchAll(/HandleFunc\("(?:GET|POST) ([^"]+)"/g)].map((m) => m[1]);
const unknown = called.filter((p) => !routes.includes(p) && p !== '/v1');
if (unknown.length) bad('面板调用了后端未注册的路径: ' + unknown.join(', '));
else ok(`接口路径全部存在（面板调用 ${called.length} 个）`);

// ── 6. 服务端注入占位符必须存在且唯一 ──
// 面板的 API Key 靠 web/web.go 替换这个占位符拿到。改名/多写一处都会让 Key
// 静默注入不进去 —— 面板只会显示「读不到数据」，排查成本很高。
const webGo = fs.readFileSync('web/web.go', 'utf8');
const phMatch = webGo.match(/keyPlaceholder\s*=\s*"([^"]+)"/);
if (!phMatch) bad('web/web.go 里找不到 keyPlaceholder 定义');
else {
  const ph = phMatch[1];
  const hits = html.split(ph).length - 1;
  if (hits === 1) ok(`服务端注入占位符唯一（${ph}）`);
  else bad(`占位符 ${ph} 在 index.html 中出现 ${hits} 次，期望 1 次`);
}

// ── 7. 手填 Key 只能是回退，服务端注入必须仍是主路径 ──
// 历史：面板曾经只能手填（侧栏 id="key"），后来改成服务端注入；2026-09-22 起
// 远程访问（局域网 / EasyTier）需要手填能力，于是加回 —— 但入口挪进了 #bannerHost，
// 且必须「注入优先」。这条断言守的就是那个顺序：反过来的话本机访问也要抄 Key，是倒退。
if (/\bid="key"/.test(html)) bad('侧栏又出现了 id="key" 手填框（远程入口应在 #bannerHost 内）');
else ok('侧栏无手填 API Key 输入框');
if (/\bid="keyInput"/.test(html)) ok('面板有远程手填 Key 入口（#keyInput）');
else bad('面板缺少远程手填 Key 入口（#keyInput）');
const gkFn = html.match(/function getKey\(\)\{[\s\S]*?\n  \}/);
if (!gkFn) bad('找不到 getKey() 定义');
else if (/if\(W2A_KEY\)\s*return\s+W2A_KEY;/.test(gkFn[0])) ok('getKey() 注入优先，手填仅作回退');
else bad('getKey() 未优先返回服务端注入的 Key —— 会退化成必须手填');

console.log(fail ? `\n== ${fail} 项失败 ==` : '\n== 全部通过 ==');
process.exit(fail ? 1 : 0);
