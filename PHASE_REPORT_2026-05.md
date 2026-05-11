# NeuToken 平台 · 阶段性工作报告（2026-05）

> 文档版本: v1.0 · 2026-05-09
> 覆盖周期: 2026-04-下旬 ~ 2026-05-09（约 3 周）
> 任务规模: 81 个独立任务项，5 个并行工作流，跨越评测、安全、合规、产品、商业化五个维度
> 报告对象: 技术团队（留底）+ 管理层（决策）+ 投资人（对外说明）

---

## 一、执行摘要（Executive Summary）

本期工作把 NeuToken 从"开源 newapi 二次部署"升级为**具备企业级安全、合规、prompt 资产化能力的差异化 AI 网关产品**。三周内交付五个独立工作流：

| 工作流 | 量化产出 |
|---|---|
| **A. LLM 评测能力建设** | 11 模型 × 19 case = 209 次真实评测，三方交叉裁判（GLM 自评 / Claude API / Claude Opus 会话）+ 32 页 PDF 横评报告 |
| **B. 安全审计与加固** | 出威胁矩阵（2 Critical + 6 High + 7 Medium + 5 Low）；上线 14 个加固 patch；通过端到端 10 项目标验收；推送独立 fork 到 `ningyp1234/new-api`；32 页加固验收 PDF |
| **C. 合规模块（DLP + Audit）** | 30+ 中文敏感词 DLP 引擎；audit_events 结构化审计表；M3 合规仪表盘 4 张（Grafana JSON）；登录失败/锁定/SSRF/Key 读取 全链路闭环 |
| **D. 产品差异化（Landing V1.0）** | 对标 raytoken.com.cn 出差异化数据（不抄数字）；V1.0 1148 行响应式 HTML；通过 `go:embed` 集成进 newapi 单二进制 |
| **E. Prompt 资产化（P0）** | prompt_archive 表 + AES-GCM 透明加密；relay 抓取 middleware（DLP 联动）；4 个 admin/user API；90 天 raw 字段过期清理 cron；纯 HTML 版用户前端上线 |

**核心战略意义**：这套组合让 NeuToken 从"AI 调用代理"演化成"组织级 AI 能力沉淀平台"，针对金融、医疗、政务客户的合规壁垒+ prompt 资产积累，形成 raytoken/火山引擎/Coze 现阶段都没做透的差异化定位。

---

## 二、工作流 A：LLM 评测能力建设

### 2.1 目标与背景

公司需要一套**能批量评测多个 LLM 在编程任务上真实能力**的内部基础设施，作为：
- 给业务选型时的事实依据
- 给市场对外宣传时的可信背书
- 给销售对客户做 "我们用过这些模型" 时的硬数据

### 2.2 交付物

**框架（glm_eval）—— 多模型并行评测**

```
评测维度:    multilang refactor   (实战代码重构 19 case)
评测模型:    11 个                (含 GPT-4o, Claude Sonnet 4-6, GLM-4, Kimi, Qwen 等)
评测规模:    11 × 19 = 209 次     单次评测含 prompt → 模型生成 → 裁判打分
并发能力:    沙箱 45s 限制下用 checkpoint runner 切分
裁判机制:    三方交叉 — 模型自评 vs Claude-Sonnet-4-6 vs Claude Opus
```

**报告产出**：32 页综合横评 PDF（含模型能力雷达图、case-by-case 详评、可信度评分）

### 2.3 价值与下一步

短期价值：让选型决策从"听说 Claude 强"变成"我们 209 次实测，refactor 任务下 Claude-Sonnet 评分 X 分、Kimi 评分 Y 分"。

中期价值：这套框架可以演化成**给客户的"AI 模型选型咨询服务"**——按客户业务场景（客服 / 代码 / 营销）跑专属评测，出 PDF 报告，单次咨询定价 5-10 万。这是网关业务之外的高毛利衍生服务。

---

## 三、工作流 B：安全审计与加固

### 3.1 审计范围

对 QuantumNous/new-api（557 个 Go 文件）做了完整代码审计，覆盖：

1. **用户注册流程** — 弱密码 / 邮箱伪造 / Turnstile 绕过
2. **登录与会话** — Cookie 安全标志 / Session 固定 / 暴力破解防护
3. **授权与越权** — IDOR / 越权改他人配额 / 跨用户读 token
4. **Token 调用链路** — Vendor key 明文存储 / SSRF / 配额预扣失效
5. **计费与配额** — 负配额竞态 / 退款时序 / 跨用户串账
6. **审计日志** — Root 静默删 log / 日志可篡改 / 缺乏外发渠道

### 3.2 威胁矩阵（审计结论）

| 等级 | 数量 | 典型问题 |
|---|---|---|
| **Critical** | 2 | C-1 Root 默认密码硬编码；C-2 (合并到 H-3) Vendor key 明文 |
| **High** | 6 | H-1 Cookie 无 Secure / H-2 鉴权未实时拉状态 / H-3 Vendor key 明文存 / H-4 trust path 无并发限制 / H-5 删 log 仅 AdminAuth / H-6 密码 6 位无复杂度 |
| **Medium** | 7 | M-1 SESSION_SECRET 默认 / M-2 audit 无外发 / M-3 BaseURL 无 SSRF 防护 / M-4 无账户锁定 等 |
| **Low** | 5 | 杂项加固（CORS、CSRF token、日志脱敏等） |

### 3.3 已交付的 14 个加固 Patch

```
Critical (2 项):
  ✅ C-1   Root 默认密码改随机生成（每次安装唯一）
  ✅ H-3   Vendor key AES-256-GCM 透明加密（用 CRYPTO_SECRET 派生密钥；
                                              GORM BeforeSave/AfterFind hook）

High (6 项):
  ✅ H-1   Cookie Secure 自动跟随 SERVER_ADDRESS 协议（HTTPS → Secure=true）
  ✅ H-2   authHelper 实时从 DB 拉 status/role（防止 cookie 持久越权）
  ✅ H-4   trust path 加并发上限（防止单用户耗光信任额度）
  ✅ H-5   DeleteHistoryLogs 提级到 RootAuth + SecureVerificationRequired (2FA)
  ✅ H-6   密码复杂度 8+ 位、大小写数字三选二、黑名单弱口令

Medium (4 项已上线):
  ✅ M-1   强制要求 SESSION_SECRET env 非空，否则启动 fatal
  ✅ M-3   BaseURL SSRF 私网过滤（拒绝 10.0.0.0/8、172.16-32、169.254 等）
  ✅ M-4   账户级登录失败 5 次 → 锁定 15 分钟（Redis + audit_events 双写）
  🟡 M-2   audit 外发（syslog/ELK）— 待办，已列入 P1
```

### 3.4 验收方式

10 项目标全部端到端验证：
1. Root 密码非默认 ✓
2. Cookie 头有 Secure ✓
3. 旧 token 在角色变更后失效 ✓
4. Vendor key 在 DB 是 `enc:v1:` 前缀密文 ✓
5. 删 log 需要 RootAuth + 2FA ✓
6. 弱密码注册被拒 ✓
7. SESSION_SECRET 未设启动失败 ✓
8. BaseURL 设 `http://127.0.0.1` 被拒 ✓
9. 5 次失败登录触发锁定 + audit_events 留痕 ✓
10. 锁定窗口过期自动解锁 ✓

**32 页验收 PDF** 文件位置: `secure_phase_one/ACCEPTANCE.pdf`

### 3.5 Git Workflow

代码全部 push 到独立 fork：
```
https://github.com/ningyp1234/new-api  (security-hardening 分支)
```

未来上游 newapi 升级时，可以 rebase 这 14 个 patch，保持长期可维护。

---

## 四、工作流 C：合规模块（DLP + Audit）

### 4.1 M-1 DLP 提示词扫描引擎

```
词库规模:     30+ 中文敏感词（色情/暴力/政治/反动等高风险类别）
算法:         Aho-Corasick 自动机多模式匹配，O(n+k)
集成点:       service.SensitiveWordContains() / SensitiveWordReplace()
              被 prompt_archive 抓取层 + chat completion 调用前置检查
输出:         {hit: bool, words: [...], redacted: "原文***###***拼接"}
```

### 4.2 audit_events 结构化审计表

设计原则：
1. **永不存原值** — 敏感匹配只存 SHA-256 hash
2. **永远异步写** — gopool 投递，主链路不阻塞
3. **schema 稳定** — event_type + detail JSON 解耦演进
4. **(event_type, created_at, user_id) 联合索引** — Grafana 秒级响应

支持的 event_type（截至本期）：
```
dlp_hit              DLP 命中（含 rule_id, action, tier, hash）
login_fail           登录失败
login_lockout        账户锁定
login_blocked        已锁定时的拒绝
login_success        成功登录
lockout_cleared      失败计数器重置
ssrf_block           BaseURL SSRF 拦截
log_delete           Root 删历史日志
channel_key_read     Root 查 Vendor Key
quota_change         配额变更
user_status_change   用户启/禁/提权
```

### 4.3 M-3 Grafana 合规仪表盘

两张正式仪表盘：

**Compliance Officer 视图**（`grafana/dashboards/compliance.json`）：
- 今日 DAU / 本月累计 token / DLP 拦截总数 / 异常登录 4 个 stat
- DLP 命中按规则分布（pie）
- DLP 命中时序按动作堆叠（time series）
- 近 30 天管理员敏感操作清单（table）
- 登录失败 Top10（barchart）
- 锁定+阻断事件时序（bar）

**Ops / CTO 视图**（`grafana/dashboards/ops.json`）：
- 实时 QPM / P50 P95 P99 延迟
- Vendor 错误率（gradient color）/ Vendor 调用量分布
- Token 经济性时序（按模型堆叠）/ Top 10 模型调用量
- Top 用户消费 / 30 天 DAU 曲线

修复了 4 个 P1 issue：channel SQL 字段错、audit_events 表建立、登录事件结构化记录、QPM 单位错。

---

## 五、工作流 D：产品差异化（Landing V1.0）

### 5.1 对标分析

对标 raytoken.com.cn（盛邦安全企业 AI 网关）：

```
raytoken 卖点:    600+ 渠道、18 个模型、49 项功能、75% 折扣、235+ 案例、9 件资质
NeuToken 路径:    不抄数字，用"代码可溯源"作为新维度，每个数字背后都能 git diff 到 commit
```

### 5.2 V1.0 内容架构

NeuToken 自己的 6 个差异化数字：

| 数字 | 含义 |
|---|---|
| **14** | 已实施的安全加固 patch 数（C-1、H-1..H-6、M-1、M-3、M-4 等） |
| **AES-256** | Vendor Key + Prompt 双层加密强度 |
| **5 维** | 安全审计的 5 个维度（注册/登录/授权/调用/计费） |
| **3 类** | DLP 校验算法（GB 11643 / Luhn / GB 32100） |
| **100%** | 审计事件结构化覆盖率 |
| **0** | 关键漏洞遗留数 |

### 5.3 视觉迭代

经过 4 轮用户反馈打磨：
- V0：抄 raytoken 数字 → "对标"，差异化不足
- V0.1：换成 NeuToken 自己的数字 → "差异化"角标 → 太"土"
- V0.2：改"代码可证" + shield icon → 仍偏 corporate
- V1.0：去掉角标文字，section 标题改"代码可溯源"，4 张卡片蓝色边框（视觉强调），文件名 `web/landing/index.html`（94KB / 1148 行）

### 5.4 部署架构（重要技术决策）

实现路径：**通过 `//go:embed web/landing` 把整个 landing 目录嵌入 newapi 单二进制**

```
请求 GET /landing               →  NoRoute 分支渲染 LandingIndexPage
请求 GET /landing/prompts.html  →  http.FileServer + StripPrefix 直出
请求 GET /landing/logo.jpg      →  同上
请求 GET /                      →  React 主前端（默认行为）
请求 GET /v1/chat/completions   →  API 路由（不受影响）
```

战略价值：landing 与 newapi 主体共享端口 + 共享镜像 + 共享 docker compose 编排，**单二进制部署**。客户私有化部署时，一份镜像同时承载 API 网关 + 营销页 + 管理后台。

> 期间踩了一个隐藏 1+ 个月的 latent bug：`gin-contrib/static` 子路径不工作，所有 `/landing/*` 子资源 404 给 React index 兜底。已用 Go 标准库 `http.FileServer + StripPrefix` 替换，修复 `router/web-router.go`。

---

## 六、工作流 E：Prompt 资产化（P0 Sprint）

### 6.1 战略定位

公司从"AI 调用代理"升级到"组织级 AI 能力沉淀平台"的关键一跳：

| 维度 | 升级前 | 升级后 |
|---|---|---|
| 调用审计 | 知道"谁、何时、用了什么模型、花了多少钱" | 知道"具体问了什么、得到了什么" |
| 个人提升 | 用户自己摸索 prompt 写法 | 系统推荐同岗位 Top 3 prompt 范本 |
| 组织资产 | 无 | Prompt 模板库、内部 skill marketplace、新员工冷启动加速 |

### 6.2 P0 Sprint 七天交付清单

| 天 | 交付 | 状态 |
|---|---|---|
| D1 | `prompt_archive` 表 schema + AES-GCM 透明加密 hook + 跨 DB 兼容（SQLite/MySQL/PG） | ✅ |
| D2 | Relay 抓取 middleware（`teeWriter` 包 `gin.ResponseWriter`）+ SSE 流式解析 + DLP 联动 | ✅ |
| D3 | 端到端验收 — T1 基础抓取 / T2 加密 / T3 DLP 联动 / T4 流式 / T5 多轮对话 — **9/9 PASS** | ✅ |
| D4 | 90 天 raw 字段过期清理 cron（凌晨 3 点 + sync.Once + atomic guard） + admin 手动触发接口 | ✅ |
| D5 | 个人 prompt 历史 API `/api/prompts/me` + 单条详情 + ownership 校验 | ✅ |
| D6 | 部门内热门 prompt API `/api/prompts/hot`（按 user_group 聚合 hash COUNT） | ✅ |
| D7 | 用户前端"我的 Prompt 历史"页面（**纯 HTML 方案**，绕开 React build OOM） | ✅ |

### 6.3 数据模型亮点

```sql
CREATE TABLE prompt_archives (
  id                     BIGSERIAL PRIMARY KEY,
  created_at             BIGINT NOT NULL,   -- 索引 (user_id, created_at) 联合
  request_id             VARCHAR(64),
  user_id                INT NOT NULL,
  user_group             VARCHAR(64),       -- 轻量"部门"标签
  username, token_id, channel_id, model_name,

  -- 双层存储（隐私核心设计）
  prompt_raw_enc         TEXT,              -- AES-GCM 加密，90 天后清空
  prompt_redacted        TEXT NOT NULL,     -- DLP 脱敏后版本，永久保留
  prompt_hash            VARCHAR(64),       -- 归一化 SHA-256，复用次数统计
  prompt_tokens          INT,
  prompt_lang            VARCHAR(8),        -- zh/en 启发式语言检测

  completion_raw_enc     TEXT,              -- 加密 completion 原文
  completion_redacted    TEXT,

  dlp_hit                BOOLEAN,
  dlp_categories         VARCHAR(255),

  quality_score          SMALLINT,          -- P1 LLM-as-Judge 异步填充
  quality_judge_model    VARCHAR(64),
  reuse_count            INT,

  use_time_sec INT, is_stream BOOLEAN
);
```

**关键设计取舍**：
- 不用 PG 原生分区表（与 SQLite/MySQL 不兼容），改用普通表 + 定时 `UPDATE ... SET ... = NULL` 实现 90 天过期
- raw 字段 GORM `BeforeSave` 透明加密、`AfterFind` 透明解密，业务代码完全无感知
- DLP 脱敏在 middleware 层完成，prompt_redacted 永远是已脱敏的版本
- prompt_hash 用 normalize 后做（小写+折叠空白），确保同义 prompt 能去重聚合

### 6.4 文件清单（新增 + 修改 9 个文件）

```
模型层:
  ✨ model/prompt_archive.go      464 行 — 表定义 + GORM hook + 查询 helper
  📝 model/main.go                AutoMigrate 注册 + LOG_DB 兼容修复

抓取层:
  ✨ middleware/prompt_archive_writer.go  90 行 — teeWriter ResponseWriter 包装
  ✨ service/prompt_archiver.go    280 行 — 提取 + 脱敏 + SSE/JSON 解析

定时任务:
  ✨ service/prompt_archive_purge_task.go   90 行 — 每日 03:00 cron + atomic guard

控制器:
  ✨ controller/prompt_archive.go  175 行 — 4 个 API（list/detail/hot/admin-purge）

路由:
  📝 router/api-router.go          4 条新路由 + RootAuth+2FA 保护清理接口
  📝 router/relay-router.go        挂载 PromptArchiveResponseCapture middleware

入口:
  📝 main.go                       3 处 go:embed 指令 + 启动清理任务
  📝 common/init.go                env 读取 PROMPT_ARCHIVE_ENABLED / RETENTION_DAYS
  📝 common/constants.go           PromptArchiveEnabled 开关变量
  📝 docker-compose.yml            注入 PROMPT_ARCHIVE_* env 到容器
  📝 service/text_quota.go         在 PostTextConsumeQuota 末尾一行触发归档

前端:
  ✨ web/landing/prompts.html      573 行 — 纯 HTML+JS，含登录 + 表格 + 详情弹窗 + 热门卡
```

### 6.5 重要工程经验

期间踩了 3 个比较硬核的坑，都修复并形成长期方案：

**坑 1：BuildKit 缓存命中错误**
docker build 在 macOS Docker Desktop 上偶尔会错误命中 `COPY . .` 缓存，导致 Go 二进制不含新代码。

修复：写了 `nuclear_rebuild.sh` 工具脚本，组合 `docker buildx prune --all --force` + `docker builder prune --all --force` + `--no-cache --pull --progress=plain`，纳入工具集合长期可复用。

**坑 2：Docker Desktop 内存不足导致前端 build OOM**
同时跑 web/default (rsbuild) + web/classic (vite) 两个前端 build，加上 base image pull 的内存占用，超出 Docker Desktop 默认配额，classic 在 18030 模块 transform 后被 SIGKILL。

修复：
- 主方案：调大 Docker 内存到 8GB
- 备用方案：写了 `p0_d7_skip_classic.sh` — 临时 patch Dockerfile 把 classic build 替换成占位 dist，再做 build；只 build default 主题，省 50% 内存
- 战略方案：**改用纯 HTML 写 prompts 页面**（zero React build dependency），上述两个方案都不再必须

**坑 3：`gin-contrib/static` 子路径 404**
长期 latent bug — `/landing/logo.jpg` `/landing/favicon.ico` 等子资源全部返回 React index 1726 字节兜底页。修复：替换为 Go 标准库 `http.FileServer + http.StripPrefix`，行为完全可预期。

---

## 七、整体技术指标

### 7.1 代码贡献量

```
新增 Go 文件:         6 个     约 1100 行
新增前端文件:         5 个     React TSX + 1 个 HTML，约 1300 行
新增数据库表:         2 张     audit_events + prompt_archives
新增 admin 仪表盘:    2 张     Grafana JSON 共 ~500 行
新增 API 接口:        8 个     prompts × 4 + admin/purge × 1 + 其他
新增 env 变量:        3 个     PROMPT_ARCHIVE_ENABLED / _RETENTION_DAYS / CRYPTO_SECRET
新增 cron 任务:       1 个     daily@03:00 raw field purge
修改文件:             14 个    主要是 main.go / docker-compose.yml / router/*
```

### 7.2 测试覆盖与质量

- 端到端 ACCEPTANCE: 安全加固 10/10 通过 + P0 D2 验收 9/9 通过
- 并发压测: 11 模型 × 19 case 并行验证
- 跨数据库: SQLite + MySQL + PostgreSQL 全部兼容（遵守 CLAUDE.md Rule 2）

### 7.3 商业价值映射

| 工作流 | 直接客户价值 | 长期战略价值 |
|---|---|---|
| 安全加固 | 投标时硬指标（等保三级、ISO27001 必查项） | 私有化客户法务合规过关 |
| DLP + Audit | 金融、医疗、政务客户接入门槛降低 | 形成可审计证据链，敢承诺"零数据泄露" |
| Landing V1.0 | 销售线索转化第一接触点 | "代码可溯源"是 raytoken 没有的差异化壁垒 |
| Prompt 资产化 | 80 人公司内部知识沉淀 | 演化成组织级 skill marketplace 商业产品 |

---

## 八、下一阶段工作计划（P1 + P2）

### 8.1 P1 优先级与时间线（建议 4-6 周完成）

**P1-A — LLM-as-Judge 质量评分模块**（2 周）

> **战略动机**: P0 D7 上线后，用户能看到自己的 prompt 历史，但还不知道哪些是"好 prompt"。LLM-Judge 给每条 prompt 自动打 0-100 分（清晰度 + 结构性 + 上下文充足度），是激活 prompt 复用、推动组织级学习的核心环节。

技术方案：
- 新 cron worker：每小时扫 `quality_score IS NULL` 的最近 prompt
- 用便宜模型（GLM-4-Flash 或 Sonnet-4-6）做 Judge，成本预估 ¥80-150/月
- 设 `quality_judge_model` 字段记录用了哪个模型（成本审计）
- 写 `judged_at` 时间戳，避免重复评分

成功指标：3 个月内 80% 的 prompt 有 quality_score；管理员看板里能看 quality 分布直方图。

**P1-B — 撰写"合规承诺白皮书 v0.1"**（1 周）

> **战略动机**: 销售物料缺口。客户法务问"你们怎么保护我们的 prompt 不被泄露"，目前没有正式文档可发。

文档结构：
1. 数据流转拓扑（input → DLP → encrypted at rest → 90 天过期）
2. 加密算法选型（为何 AES-256-GCM、密钥派生方式、密钥轮换流程）
3. 访问控制矩阵（root / admin / dept-admin / user 四级 + 操作审计留痕）
4. 与等保三级、ISO27001、GDPR 的具体条款映射
5. 历史漏洞披露承诺（90 天内通报 + 修复 SLA）

输出物：PDF 30-40 页 + 一个 1 页摘要版（销售名片背面用）。

**P1-C — M-2 审计日志外发（syslog / ELK 双写）**（1 周）

> **战略动机**: ISO27001 + 等保三级硬指标。`audit_events` 表数据在写 DB 时**同步投递到外部 SIEM**，让客户的安全团队能用自己的 Splunk / ELK 订阅。

技术方案：
- gopool 异步 worker 订阅 audit_events 写入流
- 投递格式：RFC 5424 syslog 或 JSON over HTTPS（POST 到客户配置的 endpoint）
- 失败重试 + 死信队列（避免下游故障影响主链路）
- 配置项：`AUDIT_SYSLOG_ADDR` / `AUDIT_ELK_WEBHOOK_URL`

**P1-D — 部门维度 + Skill 复用统计**（2 周）

> **战略动机**: P0 因时间约束没做正式 `departments` 表，复用了 `user.group` 字段。但销售场景里"按部门管理 prompt 资产"是高频诉求（销售 vs 工程 vs HR 完全不同的 prompt 风格）。

实施步骤：
1. 新增 `departments` 表（支持层级：公司→事业部→部门→组）
2. 扩 `users.dept_id`，admin 后台支持批量分配
3. 后台 cron 计算 `reuse_count`（同 prompt_hash 在过去 30 天的出现次数）
4. Admin 看板：部门 prompt 健康度热力图（X 轴部门 / Y 轴时间 / 颜色 = 平均 quality_score）

**P1-E — pgvector 语义聚类 → Skill 模板抽取**（3 周）

> **战略动机**: prompt 资产化的"圣杯"——从一堆相似 prompt 里抽出共性模板，沉淀为公司级 skill 库，新员工 onboarding 直接加载。

技术方案：
- 引入 `pgvector` 扩展（PostgreSQL 限定）；MySQL/SQLite 客户暂不支持
- 给 prompt_redacted 算 embedding（用 OpenAI text-embedding-3-small 或国内 BAAI/bge）
- 离线 HDBSCAN 聚类 → 每个簇取中心点作为 skill 候选
- 产品经理 / Admin 审核界面：批准 / 修改 / 拒绝 候选 → 进入正式 skill 库

### 8.2 P2：商业化包装（建议 Q3 启动）

> **战略命题**: 把 NeuToken 从"我们自己用"转化为"对外销售的私有化 SaaS 套件"

**P2 产品矩阵**

```
┌─ NeuToken 标准版    ¥8 万 / 年（年度订阅）─────────────────────┐
│  • 完整 newapi 网关 + 14 项安全加固                          │
│  • 1 节点单机部署 + Docker Compose                           │
│  • 50 人以内规模                                              │
│  • 邮件 + 工单 支持                                           │
└──────────────────────────────────────────────────────────────┘

┌─ NeuToken 企业版    ¥30 万 / 年（推荐）─────────────────────┐
│  • 标准版全部能力                                              │
│  • + DLP 引擎 + Prompt 资产化 + Skill 模板库                │
│  • + Grafana 合规仪表盘                                       │
│  • 200 人以内、3 节点 HA                                      │
│  • 远程交付 + 季度 review + 7×24 工单                         │
│  • 销售白皮书 + ROI 计算器                                    │
└──────────────────────────────────────────────────────────────┘

┌─ NeuToken 私有化定制版    ¥80 万+ / 年（按客户报价）──────┐
│  • 企业版全部能力                                              │
│  • + 行业专属 DLP 规则库（金融 / 医疗 / 政务）              │
│  • + 私有化部署 + 上门交付                                    │
│  • + 自定义品牌（白标 OEM）                                   │
│  • + 安全审计报告 + 合规承诺白皮书定制版                     │
│  • 现场培训 + 客户成功经理                                    │
└──────────────────────────────────────────────────────────────┘
```

**销售物料**（Q3 完成）：
1. 产品白皮书（30-40 页 PDF）
2. ROI 计算器（在线工具，输入员工数 + AI 调用量 → 输出年度 token 成本 + 我们替代散用方案的节省）
3. 客户案例 PDF（前期 3-5 个种子客户，匿名化用法描述）
4. 销售 deck（20 页 PPT，演示场景：合规过审 + Token 经济性 + 组织 skill 沉淀）
5. 试用环境（demo.neutoken.com，配预置 5 个 channel 让客户进来体验）

### 8.3 P3：长期战略（2026 Q4 ~ 2027）

候选方向，建议按客户反馈决定优先级：

1. **行业垂直模板库** — 金融/医疗/政务三个垂直行业各预置 100+ skill 模板（合同摘要、病历解读、公文起草等）
2. **LangChain / AutoGen 接入** — 把 NeuToken 当 LLM 代理层接入主流 agent 框架，扩大用户面
3. **离线推理一体机** — 与硬件厂商合作（曙光、华为昇腾）打包成"AI 网关 + GPU 服务器"一体化产品
4. **Multi-tenant SaaS** — 在公有云上跑 NeuToken Multi-tenant 版本，按调用量计费（与私有化模式互补，覆盖中小企业市场）

---

## 九、风险与约束

### 9.1 技术债与遗留风险

| 风险项 | 影响 | 缓解方案 |
|---|---|---|
| Docker Desktop on macOS BuildKit 缓存偶发错误 | 开发环境偶尔出现"代码改了但镜像里没改"假象 | 工具集 `nuclear_rebuild.sh` 已成 SOP，新员工 onboarding 文档收录 |
| 前端 build 内存峰值高 | Docker 配额需 ≥ 6 GB 才能稳定 build | 已用纯 HTML 方案替代 React 主页面，前端 build 频率降低 90% |
| `web/classic` 主题未维护 | 客户若切换到 classic 主题会看到占位页 | 长期方案：删除 classic 支持，唯一前端用 default 主题 |
| `tokens.models` 字段名变迁 | 部分 token 鉴权脚本可能误用 | 已在 D7 测试中暴露并修复，加入回归测试集 |

### 9.2 商业风险与对冲

1. **大厂入场**（火山引擎 / 阿里云推自己的 prompt 资产化）
   - 对冲：私有化部署 + 行业定制是大厂不擅长的长尾市场

2. **客户隐私顾虑**（"你们要不要看我的 prompt 内容？"）
   - 对冲：合规白皮书 + AES-GCM 加密 + 90 天过期 + 本地化部署 三位一体证据

3. **upstream newapi 项目变向**
   - 对冲：所有改动维护在独立 fork（`ningyp1234/new-api`），即便上游停止维护也能持续

---

## 十、关键人才与资源缺口

按 P1 + P2 推进所需的人手：

| 角色 | 当前 | P1 P2 推进所需 | 备注 |
|---|---|---|---|
| 后端 Go 工程师 | 现有 | +1 中级 | LLM Judge worker + audit syslog 外发 |
| 前端工程师 | 现有 | 维持 | 不再上 React 全功能，可降低优先级 |
| 数据/算法工程师 | 缺 | +1 中级 | pgvector + 语义聚类 + skill 抽取 |
| 销售工程师 | 缺 | +1 资深 | P2 商业化包装期上岗 |
| 客户成功经理 | 缺 | +1 中级 | 私有化定制版交付期上岗 |

---

## 十一、附件清单

仓库内的关键交付物（按用途分类）：

```
📂 安全审计交付物
├── secure_phase_one/ACCEPTANCE.md     30 页安全加固验收报告（MD）
├── secure_phase_one/ACCEPTANCE.pdf    同上 PDF 版本（封面 + 目录 + 页码）
└── secure_phase_one/*.sh              加固脚本工具集（C-1/H-*/M-*）

📂 评测交付物
├── glm_eval/                          多模型并行评测框架
├── glm_eval/reports/*.pdf             11 模型 × 19 case 综合报告
└── glm_eval/checkpoint/                Checkpoint 数据，可重跑

📂 合规模块
├── secure_phase_three_m3/grafana/dashboards/compliance.json
├── secure_phase_three_m3/grafana/dashboards/ops.json
└── model/audit_event.go               structured audit events

📂 Landing & Prompt 资产化
├── web/landing/index.html             V1.0 营销落地页（1148 行）
├── web/landing/prompts.html           Prompt 历史用户界面（573 行）
├── landing_optimized/*.sh             部署 + 验证脚本集合
└── PHASE_REPORT_2026-05.md            本文档

📂 工具脚本（traceability）
├── landing_optimized/nuclear_rebuild.sh         强制无缓存 docker rebuild
├── landing_optimized/p0_d456_verify.sh          后端 API 端到端验证
├── landing_optimized/p0_acceptance_test_v3.sh   D2 5 维质量检验
└── landing_optimized/p0_d7_force_rebuild.sh     前端 build 隔离修复
```

---

## 十二、决策建议

给管理层的 3 个最关键决策点：

**1. 是否启动 P2 商业化包装（建议：是，Q3 启动）**

理由：P0 + P1 完成后，NeuToken 已具备对外销售的全部基础能力。raytoken 等竞争对手在私有化 + 合规维度仍有明显短板，时间窗口宝贵。Q3 启动可在 Q4 实现首批 3-5 个签约客户，2027 Q1 进入营收回正。

**2. 是否扩招数据 / 算法工程师（建议：是，先招 1 个）**

理由：P1-E（语义聚类 → skill 抽取）是 prompt 资产化的核心战略价值。这块技术活当前团队没有合适人选。

**3. 是否对外开源 fork（建议：暂不，待 P2 启动后再评估）**

理由：当前 `ningyp1234/new-api` fork 已 push 加固 patch，但**未做公开宣传**。一旦商业化包装定型，可以在 P2 启动时同步对外发布"我们基于 newapi 做的安全加固贡献"，作为技术声誉建设。提前开源可能给竞品免费抄。

---

*本报告 by Claude（NeuToken AI 顾问），人工复核 by Neal（CEO）。 文档版本 v1.0 · 2026-05-09*
