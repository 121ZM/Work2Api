# Work2Api

把 **WorkBuddy（腾讯 CodeBuddy）** 与 **TRAE SOLO** 的上游 API 反代成 **OpenAI 兼容接口**。

纯 Go 标准库实现，零第三方依赖。

```bash
go build -o dist/work2api.exe ./cmd/work2api
./dist/work2api.exe import      # 扫描本机已登录的客户端账号
./dist/work2api.exe serve       # 启动服务，打开 http://127.0.0.1:7865
```

---

## 两个核心能力

### 1. 国内版 / 国际版区分

**region 是账号级属性，不是全局配置。** 这是与常见参考实现最根本的差异 ——
它们把 region 写成 `config.region` 单值，一个进程只能服务一个版本。

本项目的模型：

```
Channel = Kind + Region          分组键（workbuddy/cn、workbuddy/global、trae/cn、trae/global）
  └─ 每渠道独立：Pool / Upstream / Scheduler / 状态文件
```

于是同一个进程里：

- 国内版与国际版账号**共存**，按各自账号的 region 选上游域名
- 国内版账号挂了不影响国际版的路由与冷却
- 模型名前缀显式区分版本，也可用聚合别名跨版本自动选号

`auth.LoadDir` **不按 region 过滤**账号，region 由每个凭证文件自身决定
（文件名段 + `domain` 后缀交叉校验）。这条不变量锁在 `TestLoadDirKeepsBothRegions`。

### 2. 国内版签到

签到是**能力位**，不是开关：

```go
region.CheckinSupported(kind, region)   // 仅 cn 返回 true
```

- 国际版渠道：`Upstream.DailyCheckin` 直接返回 `provider.ErrCheckinUnsupported`，
  调度器**整池跳过**，不计失败、不进入冷却、不查积分
- 国内版：按 `schedule.checkin_times` 定时执行，也可在面板手动触发

---

## 模型命名（前缀路由）

| 前缀 | 路由到 |
|---|---|
| `workbuddy-cn/<model>` | 仅国内版 WorkBuddy 账号 |
| `workbuddy-global/<model>` | 仅国际版 WorkBuddy 账号 |
| `trae-cn/<model>` | 仅国内版 TRAE 账号 |
| `trae-global/<model>` | 仅国际版 TRAE 账号 |
| `workbuddy/<model>` | 两区自动选号（国内优先） |
| `trae/<model>` | 两区自动选号（国内优先） |
| `codebuddy` / `traework` | `workbuddy` / `trae` 的同义别名 |

`GET /v1/models` 返回的 ID 可直接回传给 `/v1/chat/completions` ——
这条闭环锁在 `TestModelIDSplitRoundTrip`（历史上这里出过 bug：
暴露的 ID 用斜杠拼接，与别名表的连字符不一致，导致客户端拿 ID 直接调用会走错渠道）。

---

## 多账号如何选号

**积分只在「同渠道同版本」内竞争。** `workbuddy/cn` 与 `workbuddy/global` 分属两个
独立账号池，冷却、熔断、积分排序互不影响。模型名带前缀就锁定池；用聚合别名
（`workbuddy/<model>`）才在 cn / global 之间挑，**国内版优先**。

### 账号快关：开启才进入号池

面板「已接入账号」列表里，每个账号行最左边是一个开关（`.sw`）。**开启才进入号池**，
关掉的账号不会被任何请求选中 —— 适合临时雪藏某个号（例如想留着积分给别的用途），
而不必把它从凭证目录里删掉。

| 开关 | 底层 | 效果 |
|---|---|---|
| 开 | `disabled = false` | 正常参与 `Pick()` 选号，计入 `Healthy()` |
| 关 | `disabled = true`，原因 `manual` | `Pick()` / `UsableAuth()` 一律跳过，不计入 `Healthy()`，行整体压暗并显示「已停用」 |

对应的接口是 `POST /api/account/toggle`，`enabled` 字段**必填**（缺失返回 400，
不会拿零值 `false` 静默把账号停掉）。

几点需要知道的：

- **复用的是同一个 `disabled` 标志**，没有为「退出号池」另造字段。因为
  `Pick()` / `UsableAuth()` / `Healthy()` 已经全部认这个标志，另起一个意味着
  每个可用性判断都得同时看两处，迟早漂移 —— 粘性路由就踩过这类坑（见上一节）。
  所以「会话失效自动停用」（`session_dead`）与「手动移出」（`manual`）是同一个字段的
  两种原因，面板据此显示不同文案。
- **开启会一并清掉冷却与错误计数**（`SetDisabled(false)` 的行为），也就是「手动开回来」
  等价于一次强制复位。**这是有意为之** —— 否则积分不足进了 12h 长冷却的账号，
  用户没有任何手动覆盖手段。但要清楚：手动开启后它会**立刻重新接流量**，
  哪怕积分问题并没解决。面板的 toast 会提示「同时清除了冷却」。
- **持久化在 `data/state-<kind>-<region>.json`**，重启后保持。这条不变量锁在
  `TestDisabledSurvivesStateRoundTrip`（变异验证过：拿掉持久化该测试立刻变红）。

### 选号顺序（`pool.better`，三段）

跳过已停用与冷却中的账号后：

1. **已知余额的优先于未知余额的** —— 未知不等于 0，不能当 0 处理
2. 都有余额 → **余额大的优先**（先烧积分多的，把积分少的留到最后）
3. 余额相同或都未知 → **最久未使用的优先**（LRU，避免总压一个账号）

### 粘性：同一账号连续用最多 50 次

选中一个账号后，后续请求优先复用它（`stickyMaxReqs = 50`）—— 提升上游缓存命中，
又不至于长期压一个号。轮换时机：

- 连续成功 50 次 → 主动重新选号
- 该次请求失败 → 清掉粘性，下次按积分重新选
- 该账号被**停用 / 进入冷却** → 立刻改选池内其他健康账号

> 最后一条曾经是 bug：早期判断写的是「账号在池里 && 池里有健康账号」，
> 而后者是**池级**计数、与这个账号无关，导致已停用/已冷却的账号被一直复用
> （手动停用那条路径尤其明显，它不会清粘性）。已改为检查账号**自身**状态
> （`pool.UsableAuth`），并锁在 `internal/server/handler_sticky_test.go` 的两个测试里。

### 积分不足怎么办

| 上游信号 | 分类 | 动作 |
|---|---|---|
| HTTP 402，或 body 命中 `积分不足` / `额度不足` / `quota exceeded` / `insufficient credit` 等 | `ErrHardCredit` | **长冷却 12 小时** |
| HTTP 429 | `ErrSoftRate` | 短冷却 60 秒 |
| 5xx / 其他 4xx | `ErrServer` / `ErrClient` | 累计 3 次后冷却 10 分钟 |
| 登录态失效 | `ErrSessionDead` | **直接停用**（不是冷却） |
| 请求形态类错误（如 `11128`） | `ErrClient` | 不冷却、不重试，直接透传上游 `code` + `msg` |

冷却后换下一个账号重试，**单次请求最多尝试 4 个账号**；全部不可用则返回 429 / 502。

### 积分什么时候刷新

| 时机 | 触发方式 |
|---|---|
| 定时签到 | `schedule.checkin_times`（默认 09:00 / 21:00），签到后顺带拉一次 |
| token 保活 | `schedule.keepalive_hours`（默认 22:00） |
| 手动刷新 | 面板「刷新」或 `POST /api/account/refresh` |
| 查积分明细 | 面板点开某账号的「积分明细」 |
| 登录成功 | 导入新账号后顺手拉一次 |
| 进程启动 | 从 `data/state.json` 恢复上次记录的值 |

**注意：对话请求本身不刷新余额。** 面板上的积分是「最近一次刷新时」的值，
不会随每次对话实时变化 —— 要看最新的点一下刷新。

---

## 接口

### OpenAI 兼容

```
POST /v1/chat/completions     流式（SSE）与非流式
GET  /v1/models
GET  /status                  账号池快照
GET  /healthz                 健康检查（不鉴权）
```

鉴权：`Authorization: Bearer <api_key>` 或 `x-api-key: <api_key>`。
`api_key` 为空则不鉴权（仅建议在监听 `127.0.0.1` 时）。

#### API Key 从哪来

**首次运行自动生成**，不需要手填：

1. 启动时若 `config.json` 不存在，会按默认值创建它；
2. 若其中的 `api_key` 为空，则用 `crypto/rand` 生成一个
   `w2a_` + 32 位十六进制（128 位随机）的 Key，并**写回该文件**；
3. 之后每次启动都复用同一个 Key —— 不会每次重启就换掉，否则已配置的客户端会全部 401。

优先级：`W2A_API_KEY` 环境变量 > `config.json` 的 `api_key` > 自动生成。
（环境变量只覆盖运行时值，**不会**写回文件。）

在哪里看这个 Key：

- 面板 → **接入** 页，可直接复制；
- 或 `config.json` 的 `api_key` 字段；
- 启动日志只打印打码后的形式（`w2a_****…`），不落明文。

### 管理 API（均需 API Key）

```
GET  /api/state                     全部渠道状态 + 账号明细
POST /api/account/checkin           立即签到（{ "channel": "workbuddy/cn" } 可限定渠道）
POST /api/account/refresh           刷新 token + 积分（{ "channel": ..., "uid": ... } 可限定）
POST /api/account/resource          查单账号积分明细 → { total, items[] }，每条含 expire_at
POST /api/account/toggle            账号快关：{ "channel", "uid", "enabled" }，enabled 必填
POST /api/import/local              扫描本机客户端凭证并导入（?dry_run=1 只扫描）
POST /api/login/start               发起交互式登录 → { kind, region }
GET  /api/login/poll                轮询登录状态 → { kind, region }
POST /api/login/cancel              放弃在途登录
POST /api/models/refresh            立即重拉各渠道上游模型表
GET  /                              内嵌控制台面板（不鉴权）
```

面板页面本身不鉴权，但**只有来自回环地址的请求**才会拿到注入的 API Key
（见 `web/web.go`）。若把 `listen.host` 改成 `0.0.0.0`，外部访问者打开的
面板里 Key 为空，只会看到「请从本机访问」的提示 —— 避免把 Key 送给公网扫描器。

### 面板的刷新策略（不轮询）

面板**不做定时轮询**。刷新时机只有三个：

| 时机 | 触发 |
|---|---|
| 打开页面 | 启动时 `GET /api/state` 一次 |
| 操作之后 | 签到 / 刷新 / 账号快关 / 登录成功，各自在完成后刷一次 |
| 手动 | 渠道页的「刷新状态」按钮、总览页的「刷新全部账号」 |
| 切回页面 | `visibilitychange` 变可见时补一次，免得看到明显过期的数据 |

**为什么不能轮询**：不只是多余（服务端状态只在我们自己的操作之后才变），
而是**有害** —— 早期是每 20 秒 `setInterval` 调一次 `refresh()`，而 `refresh()`
会整体重渲染 `#view`，把已展开的积分明细冲回「展开时自动查询…」，
且**不会自动重查**。实测：展开明细后等 20 秒，24 条变 0 条。

修法两处，缺一不可：

1. 去掉定时器（改为上表的按需刷新）；
2. 积分明细的渲染结果改存 **JS 侧缓存** `resCache["渠道|uid"]`，
   而不是 DOM 上的 `data-loaded` —— 每次重渲染 `#view` 都是全新的 DOM，
   `dataset` 会丢，于是「展开过但没数据」和「从没查过」无法区分，
   要么白打一次上游，要么（实际发生的）内容被冲掉。

`render-check.js` 用两个数真实请求的断言把这两条钉住：
「重渲染后积分明细仍保留（24 → 24 条）」和「空闲 25 秒内没有任何 `/api/state` 轮询」。

### CLI

```
work2api serve  [-config config.json]
work2api import [-config config.json] [-auth-dir DIR] [-dry-run] [-json]
work2api login  -kind <workbuddy|trae> -region <cn|global> [-timeout 5m]
```

---

## 凭证来源

### 本机导入（`import` / 面板「扫描本机凭证并导入」）

| 渠道 | 路径 | 形态 |
|---|---|---|
| WorkBuddy cn | `%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\workbuddy-desktop.info` | 明文 JSON |
| WorkBuddy global | 同目录 `workbuddy-desktop-ai.info` | 明文 JSON |
| TRAE cn/global | `%APPDATA%\TRAE SOLO[ CN]\User\globalStorage\storage.json` | **TLV 二进制，不可解析** |

导入是**单向**的：只读客户端文件，绝不回写。写入本项目的 `auths/` 目录，文件名
`<kind>-<region>-<uid>.json`。

TRAE 只能走 OAuth —— 客户端 `storage.json` 里的 `iCubeAuthInfo://*` base64 解码后
是 TLV 二进制（以 `dGMFEAAA` 开头），不是明文 JWT。

### 交互式登录（`login` / 面板「登录新账号」）

- **WorkBuddy**：服务端签发 state → 浏览器授权 → 轮询取 token（无 PKCE）
- **TRAE**：PKCE S256 + 本地 `127.0.0.1:<随机端口>` 一次性回调，5 分钟超时

---

## 配置

见 `config.example.json`。环境变量 `W2A_*` 可覆盖文件值。

`config.json` 首次运行会自动创建；`api_key` 留空即自动生成随机值并写回
（详见上文「API Key 从哪来」）。该文件含密钥，已在 `.gitignore` 中排除。

`regions.workbuddy` / `regions.trae` 可覆盖内置域名表（键 `cn` / `global`），
留空即用内置表（见 `internal/region`）。

---

## 已实测的上游契约

以下均为**本机真实请求验证过**的事实，不是推断：

| 事实 | 值 |
|---|---|
| WorkBuddy cn 主端点 | `https://copilot.tencent.com` |
| WorkBuddy global 主端点 | `https://www.workbuddy.ai` |
| WorkBuddy cn 计费/签到域 | `https://www.codebuddy.cn` |
| 客户端 `.info` 的 `expiresAt` | **13 位毫秒**，必须 `/1000` 归一 |
| `auth/state?platform=` | 两版**都是 `CLI`**；版本区分靠 base URL |
| cn `auth.domain` | `www.workbuddy.cn`（**不是** Origin，Origin 要发 `www.codebuddy.cn`） |
| 客户端账号库 ID | `workbuddy-desktop`（cn）/ `workbuddy-desktop-ai`（global） |
| 国际版 chat 首条消息 | **必须是 `system`**，否则 `400 code=11128` |
| 国内版 chat 首条消息 | 无此要求 |
| 签到「今天已签到」 | 表达为 **HTTP 400 + `code=10001`**，非 `checked_in: true` |
| 签到接口 | `POST https://copilot.tencent.com/v2/billing/meter/daily-checkin` |
| TRAE cn Agent / UG / Console | `trae-api-cn.mchost.guru` / `api.trae.cn` / `www.trae.cn` |
| TRAE global Agent / UG / Console | `coresg-normal.trae.ai` / `growsg-normal.trae.ai` / `www.trae.ai` |
| WorkBuddy 模型接口 | `GET {ChatBase}/console/enterprises/personal/models`，取 `data.agents[]` 中 `name=="cli"` 的 `models` 列表 |
| TRAE 模型接口 | `POST {Agent}/api/ide/v1/get_detail_param`，取 `config_info_list[]` |
| TRAE 模型判别字段 | `usage` = `chat_completion` / `custom_model` / `summary`；`is_invisible_to_user` |
| TRAE 内部子 agent 配置 | `model_extra_config.v3_sub_agent_model_config_names` 明确列出 `explore_sub_agent_v2`、`browser_use_subagent` |
| WorkBuddy global 模型接口 | 稳定返回 **HTTP 500 + HTML**，带 `X-Apisix-Upstream-Status: 500` —— 上游业务服务故障，不是鉴权拒绝 |

### global 模型接口 500 是怎么定性的

结论：**国际版后端的这个接口存在（APISIX 已给它配了 upstream）但会崩**，
属于服务端故障；既不是账号权限问题，也不是我们请求构造的问题。客户端侧无法绕过。

判据来自一组对照探测：

| 探测 | 结果 | 含义 |
|---|---|---|
| global `/console/enterprises/personal/models` | **500** + `X-Apisix-Upstream-Status: 500` | 请求穿过了 EdgeOne WAF 和 APISIX，**上游业务服务自己返回 500** |
| global `/v1/models` | **404** `{"error_msg":"404 Route Not Found"}` | 纯路由不存在（APISIX 风格） |
| global `/console/enterprises/personal` | **403** `access_denied / not_authorized` | 路由存在，被业务层鉴权拒绝 |
| global 其余 `/console/*`（`agents` / `config` / `models` 等 6 条） | **403**，与上一条完全一致 | 只有 `.../personal/models` 这一条返回 500，说明它**确实有 upstream** |
| 同路径换 cn 账号 | **200** + `Server: APISIX/3.9.1` + `X-User-Id` | 路径、鉴权头、token 形态全都正确 |

三条决定性理由：

1. `X-Apisix-Upstream-Status: 500` 表示 500 来自 APISIX **后面**的服务，
   不是网关自己造的 —— 权限不足时它会像上面那样返回 403 `access_denied`。
2. 同一路径、同一套 header，在 cn 账号上返回 200 —— 排除「我们请求构造有问题」。
3. 去掉 `Origin`/`Referer`、去掉 `Accept`、连发 5 次，**全部 500 且 body 长度恒为 249** ——
   稳定复现，与请求头无关。

### 模型列表从哪来

`/v1/models` 以**上游实时拉取**为准，静态表只作兜底。这不是可有可无的优化 ——
实测静态兜底表和账号真实权限差得很远：

| 渠道 | 静态兜底表 | 上游实时 | 说明 |
|---|---|---|---|
| workbuddy/cn | 37 | **16** | 兜底表列了账号没有权限的模型 |
| workbuddy/global | 20 → **12** | 拉取失败 → 12 | 上游 500 **稳定**失败；静态表经逐模型实测筛选 |
| traework/cn | 2 | **19** | 兜底表严重缺漏 |
| traework/global | 1 | **2** | 兜底表写的 `glm-5.2` 该账号**根本没有** |

拉取时机：进程启动时拉一次，之后每 30 分钟刷新；面板「模型 → 重新拉取」
或 `POST /api/models/refresh` 可手动触发。**某次拉取失败不会清空已有结果**，
仍用上一次的上游结果（见 `Runtime.SetModels`）。

#### global 静态表为什么只剩 12 个

因为 global 的模型接口**永远拉不到**，这张静态表就是该渠道对外的真实清单 ——
列一个调不通的模型，比列表短更糟。所以对 region=global 的真实账号
**逐个模型发了最小 chat 请求**（不是推断），20 个原始条目实测结果：

| 结果 | 数量 | 处理 |
|---|---|---|
| `200` 可用 | 11 | 全部保留 |
| `11102` service info not found | 8 | **全部剔除** |
| `429` too many requests | 1（`glm-5.0`） | 保留 |

被剔除的 8 个在客户端 `product.json` 里存在，但服务端没注册对应 service info：
`default-model-lite`（别名指向 `codewise-default-cw-api-3`）、`gpt-5.1-codex`、
`gpt-5.1-codex-mini`、`gemini-3.1-flash-lite`、`gemini-3.0-flash`、
`gemini-2.5-pro`（→ `gemini-2.5-pro-us-central1`）、
`gemini-2.5-flash`（→ `gemini-2.5-flash-us-central1`）、`deepseek-v3-2-volc`。

`glm-5.0` 保留的依据是一次**对照实验**：同一时刻同一账号，`kimi-k2.5` 返回 200
而 `glm-5.0` 返回 429 —— 所以这是「模型存在但该模型单独限流/无配额」，
既不是账号级限流，也不是不存在。`11102` 的判定发生在限流检查之前，
这也从侧面印证了 429 ≠ 不存在。

防回归锁在 `TestWorkBuddyGlobalStaticModelsAreCallable`：这张表是从
`product.json` 生成的，重新生成时很容易把这 8 个又带回来。

TRAE 的过滤规则（`traework.pickUserModels`）只保留：

1. `usage == "chat_completion"` —— 排除第三方代理模型与内部摘要配置
2. 有真实显示名（显示名为空或 `-` 的 `sagitta` / `aquila` 不是给用户选的）
3. 名字不是内部 agent 配置（`*_subagent` / `*_sub_agent` / `*_agent`）

**刻意不按 `is_invisible_to_user` 过滤** —— 实测 traework/global 账号的全部
对话模型（`gpt-5.4` / `gpt-5.2`，Beta）都被标为 invisible，按它过滤该渠道会
一个模型都不剩，反而退回那张写错的兜底表。`invisible` 只表示「不在客户端
选择器里默认展示」，不代表不可调用。

### 请求体改写（不变量，勿删）

1. 强制 `stream: true` —— 上游拒绝非流式请求，非流式由本地聚合事件流实现
2. `tool_choice` 归一为 string —— 对象形式会 `400 code=11101`
3. `developer` → `system` —— 上游对 `developer` 角色一律命中内容过滤
4. 国际版补首条 `system` —— 见上表

### 积分明细的字段（含到期时间）

`get-user-resource` 返回的每个权益包里有三个和「到期」沾边的字段。实测
（workbuddy/cn 真实账号，24 条权益包）后**只用其中一个**：

| 字段 | 形态 | 实测表现 |
|---|---|---|
| `DeductionEndTime` | UTC **毫秒**戳 | 24/24 条都有值 ← **采用** |
| `CycleEndTime` | `"2006-01-02 15:04:05"` **北京时间** | 24/24 条都有值 ← 仅作兜底 |
| `ExpiredTime` | 同格式字符串 | **时有时无**；有值的那些恰好都是已过期（剩余 0）的包 |

同一包的两个可用字段**换算后完全相等**，例如
`DeductionEndTime = 1792195250000` → `1792195250` s → `2026-10-17T00:00:50Z`
= `CycleEndTime = "2026-10-17 08:00:50"`（CST）。

所以接口只暴露一个 `expire_at`（Unix 秒）：优先 `DeductionEndTime`，
缺失时按 **CST** 解析 `CycleEndTime`，都没有则返回 0（面板整段不渲染 ——
不显示「未知」，以免和「已过期」混淆）。

面板上按 **`到期时间：2026-10-08 10:41:39`** 原样显示完整时刻，
**不做「多少天后」的相对换算** —— 相对值每次刷新都在变，反而没法跟上游给的
时刻核对。已过期的条目染红。

`ExpiredTime` **刻意不用**：它表达的是「已过期」这个状态而不是到期时刻，
且时有时无，拿来当到期时间会把「已过期」和「未提供」混为一谈。
这几条口径锁在 `internal/upstream/resource_test.go`，其中
`TestExpireAtFallsBackToCycleEndTime` 专门盯时区（按 UTC 误解析会差 28800 秒）。

### TRAE 明细的字段（与 WorkBuddy 完全不同）

TRAE 走另一个接口（`/trae/api/v2/pay/web_user_ent_usage`），字段名与形态都不一样。
实测（traework/cn 真实账号，13 条权益包）：

| 字段 | 用途 | 实测 |
|---|---|---|
| `expire_time` | 到期时间（Unix **秒**） | 13/13 有值 ← **采用** |
| `entitlement_base_info.end_time` | 同上 | 与 `expire_time` **恒等** ← 兜底 |
| `display_desc` | 显示名（`老用户福利` / `签到奖励` / `每月登录赠送`） | 有值 ← **用作名称** |
| `group_name` | 分组名（`用户福利` / `每日签到`） | `display_desc` 缺失时兜底 |
| `entitlement_base_info.quota.credits_limit` | 额度 | 实测有一条为 **0** |

两条口径：

1. **额度为 0 的权益包过滤掉**。实测有一条 `display_desc = "免费"`、`credits_limit = 0`
   —— 它对合计没有任何贡献（remain 必为 0），留在列表里只是噪音。
   过滤前后合计剩余都是 **6247**，证明被滤掉的确实是无贡献条目。
   同一口径也用在 WorkBuddy 侧（那边当前没有这种条目）。
2. **名称改用 `display_desc`**，不再拼「权益包 N」—— 面板上 13 行全是
   「权益包 1…13」，完全看不出是什么包。

> **关于「有 2 条一模一样」**：实测 `老用户福利` 那两条**不是重复**，是两个独立权益包 ——
> `entitlement_id` 分别是 `352177393410` / `352177393666`，`product_id` 分别是
> **208 / 209**，只是额度（各 2000）与到期时刻恰好相同。合计剩余 6247 里两条都算进去了，
> **去重会凭空少算 2000 分**，所以刻意不做去重。

### 错误处理

请求形态类错误（`ErrClient`，如 `11128` / `11101`）**不重试、不冷却**，
直接透传上游 `code` + `msg` 给客户端 —— 换账号也是同一个结果，
重试只会白白把好账号拖进冷却。

只有可能账号相关的错误才换号重试：`ErrHardCredit`（积分不足）、
`ErrSoftRate`（限流）、`ErrSessionDead`（会话失效）、`ErrServer`（上游 5xx）。

---

## 尚未实测的部分

诚实标注 —— 以下路径**代码已按 region 参数化写完并通过编译/单元测试，
但未用真实账号跑通**（缺对应账号）：

| 项 | 状态 |
|---|---|
| WorkBuddy **global** OAuth 登录流程 | 未实测（cn 流程来自参考实现；global base URL 取自 `product.json`） |
| global refresh 的 `X-Auth-Refresh-Source` 取值 | 未实测（客户端用 `plugin`，参考实现用 `workbuddy`） |

> WorkBuddy global 模型接口的 500 **已定性为国际版服务端故障**（不是权限问题，
> 也不是我们请求构造的问题），详见「已实测的上游契约」表下的小节。

### 已实测跑通

- WorkBuddy cn/global **对话**（流式 + 非流式）、cn **签到**、global **签到跳过**
- **TRAE cn/global 登录**（面板授权流程实际产出凭证文件）、**对话**（`trae-cn/glm-5.2`、
  `trae-global/gpt-5.2` 均返回正常内容）、**cn 签到**（两个账号均签到成功并取回真实积分）
- 四个渠道的**模型列表**均来自上游实时拉取（workbuddy/global 除外，见上表）
- WorkBuddy global **静态表 20 个模型逐个真实调用**：筛出 11 个可用、剔除 8 个 `11102`、
  1 个模型特有 `429`（用同账号同刻的对照实验定性），该渠道最终对外 **12 个**
- 本机凭证**导入**、账号池冷却与路由、面板全部交互
  （86 项渲染断言 + 18 项冒烟断言 + 7 项静态校验）

---

## 目录结构

```
cmd/work2api/          CLI 入口（serve / import / login）
internal/region/       域名表 + 签到能力位（叶子包，无内部依赖）
internal/config/       配置加载 + 环境变量覆盖
internal/auth/         凭证解析、region 判定、原子写回
internal/importauth/   本机客户端凭证单向导入
internal/login/        WorkBuddy OAuth（region 参数化）
internal/login_trae/   TRAE PKCE 登录 + 本地回调
internal/loginsvc/     登录会话管理（发起 → 轮询 → 落盘）
internal/provider/     渠道枚举、错误分类、上游接口、静态模型表
internal/upstream/     WorkBuddy 上游客户端
internal/traework/     TRAE SOLO 上游客户端 + SOLO→OpenAI 事件流转换
internal/pool/         账号池（积分降序 + 最久未使用 + 熔断 + 粘性路由）
internal/scheduler/    签到 / token 保活调度
internal/server/       HTTP 路由与 /v1 接口
internal/app/          装配
web/                   内嵌控制台面板（单文件 HTML，go:embed）+ Key 注入
tools/                 校验脚本（见下）
```

---

## 校验脚本

面板是纯内联 HTML，没有构建步骤，所以改完必须靠脚本兜住。三个脚本都对着
**运行中的服务**或**真实磁盘文件**断言，不是看一眼截图就算过。

```bash
# 1. 静态校验：内联 JS 语法、CSS 配平、DOM id 引用、图标表、
#    接口路径是否真的在后端注册、注入占位符是否唯一、面板是否已无手填 Key
node tools/check-panel.js

# 2. 真实 Chromium 渲染断言：读渲染后的 DOM 计算值与文本，含暗色对比度、
#    窄屏布局、控制台无错误；数真实请求断言「不轮询」「重渲染不重复查上游」；
#    截图落在 dist/shots/
#    注意：末尾有一段 25 秒空闲观测，用来证明没有 /api/state 轮询，整脚本因此偏慢
node tools/render-check.js

# 3. 端到端冒烟：鉴权边界、模型/渠道清单、面板注入、
#    「请求形态错误不冷却账号」
node tools/smoke.js
```

三个脚本都需要 `playwright-core`（仅 `render-check.js`）。它装在托管目录，
不污染仓库：

```bash
NODE_PATH="C:/Users/dev/.workbuddy-ai/binaries/node/workspace/node_modules" node tools/render-check.js
```

`render-check.js` 用本机已有的 Chromium，不额外下载浏览器；路径可用
`CHROME_PATH` 覆盖。若本机设了 `http_proxy`，脚本已加 `--no-proxy-server`，
否则 Chromium 会把 `127.0.0.1` 也发给代理导致加载失败。

---

## 单元测试

```bash
go test ./... -cover
```

共 **135** 个测试。选测什么的标准是「**有没有值得锁的不变量**」，
不是「覆盖率低的都补」—— 反代项目里最该锁的，是从实测得来、
又容易被后人「顺手改整齐」破坏的事实。

| 包 | 覆盖 | 锁住的不变量 |
|---|---|---|
| `web` | 93.8% | Key 只对回环注入、占位符替换、恶意 Key 转义 |
| `region` | 82.2% | CN 的 `ChatBase` ≠ `BillingBase`；两版 `platform` 都是 `CLI`；签到能力位只对 cn 为真 |
| `importauth` | 80.7% | region 交叉校验、精确文件名（不读轮转备份）、**单向导入**（源文件字节不变） |
| `auth` | 76.6% | 毫秒归一、region 判定、两区账号共存 |
| `provider` | 75.0% | 模型 ID 往返闭环、静态表实测清单、黑名单按 region 独立 |
| `config` | 58.5% | Key 自动生成与写回、环境变量不落盘 |
| `scheduler` | 44.3% | 「今天已签到」记成功且不冷却、国际版整池跳过、签到失败不冷却 |
| `pool` | 68.5% | 三段选号排序、`Auth` 与 `UsableAuth` 的语义差异、**快关跨重启持久化** |
| `login` | 37.4% | region 由 `domain` 判定、相对 `Location` 解析、重定向跳数上限 |
| `login_trae` | 32.1% | PKCE S256、回调参数解析 |
| `server` | 16.9% | 模型缓存降级语义、粘性路由不复用已停用/冷却账号 |
| `upstream` | 13.6% | 请求体改写不变量、「今天已签到」识别不吞真实错误、**到期时间的字段取舍与时区** |
| `traework` | 4.1% | 模型过滤三条规则、**明细的到期字段与显示名取舍**（覆盖率低是因为该包大量代码是 HTTP 调用） |

`app` 与 `loginsvc` **未测**（0%）：前者是装配层、后者是直接打网络的会话状态机 ——
硬凑测试只会得到脆弱的 mock，收益低于维护成本。这是刻意的取舍，不是遗漏。
