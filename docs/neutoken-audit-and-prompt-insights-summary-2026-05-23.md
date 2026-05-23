# NeuToken / NewAPI 安全审计与 Prompt 洞察增强总结

日期：2026-05-23  
测试服务地址：`http://192.168.50.150:3000`

## 1. 背景与目标

本次开发是在 NewAPI 服务基础上，为内网测试版增强安全、审计和 prompt 能力提升功能。目标不只是记录调用日志，而是把团队对 LLM 的使用过程沉淀为可复盘、可评价、可复用、最终可抽象为 skill 的工作体系。

核心目标：

- 记录用户的 prompt 与模型输出，形成可追溯历史。
- 对 prompt 输入质量做可解释评价。
- 对模型输出反馈做质量考核。
- 汇总高频输入，发现团队共性场景。
- 从高频且效果稳定的场景中提炼 skill 候选。
- 支持内网测试发布和外部浏览器访问。

## 2. 已完成能力

### 2.1 Prompt 全文归档

新增 `prompt_archives` 归档表，记录每次 LLM 调用的 prompt 与 completion。

主要字段：

- 用户信息：`user_id`、`username`、`user_group`、`token_id`
- 调用信息：`request_id`、`channel_id`、`model_name`、`use_time_sec`、`is_stream`
- Prompt：原文加密字段、脱敏字段、hash、tokens、语言
- Completion：原文加密字段、脱敏字段、tokens
- DLP：是否命中敏感信息、敏感类别
- 质量评分预留：`quality_score`、`quality_judge_model`、`judged_at`

隐私策略：

- raw 原文加密保存。
- 页面和统计默认使用脱敏文本。
- raw 字段按保留周期清理，默认保留脱敏数据用于长期统计。

### 2.2 Prompt 提取兼容

已支持从以下请求格式中提取 prompt：

- OpenAI Chat / Completions 风格消息
- Claude Messages 风格消息
- 多模态内容使用占位符，例如 `[image]`、`[audio]`、`[file]`、`[document]`
- Claude 工具调用、tool result、thinking 等内容做可读展开或占位

修复过的问题：

- 用户使用 `/v1/messages` 时，使用日志有数据但 prompt 历史看不到。原因是旧归档逻辑只识别 OpenAI 请求体，现已支持 Claude request。

### 2.3 Prompt 历史页面

页面地址：

`/landing/prompts.html`

功能：

- 登录后查看个人 prompt 历史。
- 搜索 prompt 或 completion。
- 按模型、时间范围筛选。
- 查看单条详情。
- 复制 prompt 作为模板。
- 查看 completion 原文或脱敏内容。
- 展示调用 token、模型、耗时、DLP、语言、stream 状态。

### 2.4 多维统计分析

接口：

`GET /api/prompts/analytics`

统计内容：

- 总调用数
- 唯一 prompt 数
- prompt 复用率
- DLP 命中数和命中率
- Prompt / completion token 总量
- 平均 prompt tokens
- 平均 completion tokens
- 平均耗时
- 模型分布
- 语言分布
- token 桶分布
- prompt 长度分布
- 每日调用趋势
- DLP 类别分布

### 2.5 Prompt 质量洞察

接口：

`GET /api/prompts/insights`

这是本次增强的重点。它不调用额外 LLM，使用本地可解释 rubric，读取脱敏 prompt / completion 和调用元数据完成评价。

输出内容：

- 输入质量均分
- 输出反馈均分
- 高质量 prompt 占比
- 弱 prompt 占比
- 弱输出占比
- 可复用 prompt 族群数量
- skill 候选数量
- 输入问题排行
- 输出问题排行
- 高频输入汇总
- skill 候选
- 团队提升建议

## 3. 输入质量评价算法

### 3.1 算法定位

当前版本是“本地规则 + 工作 rubric”算法，不依赖外部模型，优先保证：

- 可解释
- 成本低
- 可快速落地
- 可做团队级趋势统计

它不是最终的语义裁判，后续可叠加 LLM Judge 做更深的业务语义评分。

### 3.2 评价维度

输入质量按加权 rubric 评分，满分 100。

主要加分项：

- 任务目标明确：是否说明要分析、生成、总结、提取、翻译、评审、设计、诊断等。
- 背景或角色充分：是否说明业务背景、上下文、场景、角色、项目、客户或团队背景。
- 输入材料明确：是否给出文本、代码、日志、数据、需求、文档、报错等待处理材料。
- 输出格式清晰：是否指定 Markdown、JSON、表格、列表、步骤、字段、模板等。
- 约束条件明确：是否给出必须、不要、限制、风格、字数、边界、语言等约束。
- 验收标准明确：是否说明准确性、完整性、风险、依据、检查清单、成功标准等。
- 复杂任务有拆解：是否要求分步骤、分阶段、先后流程、自检或复核。
- 提供示例或参考。
- 说明输出受众。

主要扣分项：

- 输入过短，模型需要猜测。
- 信息量偏少。
- 输入很长但未拆分任务。
- 命中 DLP 敏感信息。

### 3.3 问题标签

系统会输出具体问题，而不是只给分数。

常见标签：

- 任务目标不够明确
- 缺少业务背景或角色设定
- 缺少明确输入材料
- 缺少输出格式要求
- 缺少约束条件
- 缺少验收标准
- 复杂任务缺少拆解步骤
- 输入很长但未拆分任务
- 包含敏感信息

## 4. 输出反馈考核算法

### 4.1 评价维度

输出反馈同样按 100 分制评价。

主要依据：

- 是否有有效输出。
- 是否满足 prompt 的简短意图。
- 输出与输入主题的相关性。
- 输出是否结构化。
- 是否包含建议、依据、风险、下一步、方案、示例或清单。
- 面对复杂 prompt 时，输出深度是否足够。
- 是否表达不确定性、假设或前提。
- 是否疑似拒答、错误、权限不足或失败。
- 响应耗时是否过长。

### 4.2 短答案校准

已加入“简短回答意图”识别。

如果 prompt 中包含：

- 一句话
- 简短
- 简洁
- 只回答
- 直接回答
- 不要解释
- brief / concise / short

则不会因为 completion 很短就简单判低分。例如：

`一句话回答 1+1`

输出：

`2`

这种场景会被视为符合用户意图。

### 4.3 相关性粗评估

系统会抽取 prompt 和 completion 中的信号词，计算主题重合度：

- 英文按较长词抽取。
- 中文按 2 字和 4 字片段抽取。
- 常见停用词会过滤。

这不是完整语义理解，但比只看输出长度更接近实际工作判断。

### 4.4 输出问题标签

常见标签：

- 无有效输出
- 输出过短
- 输出与输入主题关联偏弱
- 输出深度可能不足
- 疑似拒答或错误输出
- 输出结构化不足
- 缺少可执行建议或依据
- 响应耗时较长

## 5. 高频输入与 Skill 候选

### 5.1 高频输入汇总

系统会对脱敏 prompt 做归一化 hash：

- 折叠空白
- 转小写
- SHA-256 hash

相同或高度一致的 prompt 会聚合为一个 prompt family。

展示内容：

- 复用次数
- 平均输入质量分
- 平均输出反馈分
- prompt tokens
- completion tokens
- 是否适合沉淀
- 优化建议

### 5.2 Skill 候选规则

当前规则：

- 复用次数达到 2 次：进入高频观察。
- 复用次数达到 3 次，且输入均分不低、输出反馈稳定：标记为适合沉淀。
- 高频但质量不稳定：建议先模板化，再沉淀成 skill。

Skill 候选会自动给出：

- 推荐名称
- 目标
- 输入项
- 工作流
- 边界规则
- 样例 prompt
- 证据说明

## 6. 安全与账号体验修复

### 6.1 控制台登录态修复

修复 prompt 页面跳转主控制台后重复登录的问题：

- prompt 登录后写入控制台需要的 localStorage 用户信息。
- 控制台请求每次动态补充 `New-API-User` 请求头。
- 避免 header 缺失导致管理员接口 Unauthorized。

### 6.2 管理员创建用户修复

修复管理员创建用户失败的问题：

- 前端 API 客户端每次请求动态读取当前用户 id。
- 后端仍保留身份校验。
- 已验证管理员创建用户接口成功。

### 6.3 密码规则提示优化

后端密码规则：

- 至少 12 位
- 至少包含大写字母
- 至少包含小写字母
- 至少包含数字
- 至少包含符号

优化前，错误提示类似：

`User.Password min tag`

优化后，提示会指出具体原因：

`密码不符合要求：长度不足：当前 8 位，至少需要 12 位；缺少大写字母；缺少符号，例如 ! @ # $ %`

### 6.4 内网发布配置

当前服务绑定：

`0.0.0.0:3000`

当前内网访问地址：

`http://192.168.50.150:3000`

已更新：

- `.env` 中的 `SERVER_ADDRESS`
- 数据库 `options.ServerAddress`
- Docker 容器已重启并健康

## 7. 主要接口

Prompt 历史：

`GET /api/prompts/me`

Prompt 详情：

`GET /api/prompts/me/:id`

Prompt 多维统计：

`GET /api/prompts/analytics`

Prompt 能力洞察：

`GET /api/prompts/insights`

组内热门 prompt：

`GET /api/prompts/hot`

管理员清理 raw 字段：

`POST /api/prompts/admin/purge-raw`

## 8. 主要文件变更

后端：

- `model/prompt_archive.go`
- `controller/prompt_archive.go`
- `service/prompt_archiver.go`
- `service/prompt_archive_purge_task.go`
- `middleware/prompt_archive_writer.go`
- `router/api-router.go`
- `model/main.go`
- `service/text_quota.go`
- `controller/user.go`

前端与页面：

- `web/landing/prompts.html`
- `web/classic/src/App.jsx`
- `web/classic/src/helpers/api.js`
- `web/classic/src/components/table/users/modals/AddUserModal.jsx`
- `web/classic/src/components/table/users/modals/EditUserModal.jsx`
- `web/classic/src/components/layout/SiderBar.jsx`
- `web/classic/src/hooks/common/useSidebar.js`

配置与部署：

- `docker-compose.yml`
- `.env`

## 9. 验证情况

已完成验证：

- `go test ./model ./controller`
- `npm run build` for classic frontend
- Docker 镜像重建
- `new-api` 容器重启
- 健康检查通过
- `/api/prompts/analytics` 正常
- `/api/prompts/insights` 正常
- `/landing/prompts.html` 可通过当前内网 IP 访问
- 管理员创建用户接口验证通过
- 弱密码错误提示验证通过
- Claude `/v1/messages` prompt 归档验证通过

## 10. 当前局限

当前评分仍然是本地规则系统，不能完全理解业务语义。

局限包括：

- 不能真正判断事实是否正确。
- 不能判断复杂代码、法律、财务等专业结论是否可靠。
- 相关性判断是轻量词项重合，不是深度语义匹配。
- skill 候选仍是启发式生成，需要团队负责人复核。

## 11. 下一步建议

建议分三阶段继续增强。

第一阶段：校准规则

- 收集团队真实 prompt 样本。
- 人工标注 50-100 条高质量、中质量、低质量样本。
- 根据样本校准权重和阈值。

第二阶段：引入 LLM Judge

- 对高频 prompt 或抽样记录做异步模型评审。
- 输出更细维度评分：目标完整性、上下文充分性、输出可交付性、事实风险、复用价值。
- 将模型评审结果写入 `quality_score` / `quality_judge_model` / `judged_at`。

第三阶段：Skill 资产化

- 将高频场景沉淀为团队 skill。
- 为每个 skill 固化输入字段、工作流、输出模板和验收标准。
- 建立 skill 使用率、成功率、节省时间估算。
- 将优秀 prompt 自动推荐为团队模板。

## 12. 团队 Prompt 推荐结构

建议推广统一结构：

```text
角色：
你是……

背景：
……

目标：
请完成……

输入材料：
……

输出格式：
请按以下结构输出……

约束条件：
……

验收标准：
好的输出应该满足……
```

这套结构也可以作为后续 skill 生成的默认骨架。
