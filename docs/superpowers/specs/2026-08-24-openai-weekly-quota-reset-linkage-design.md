# GPT 官方周额度重置联动规则设计

## 背景

OpenAI Pro 账号的 GPT 周额度通常按七天窗口重置，但官方也可能提前开启新窗口。系统需要在观测到指定 Pro 账号已经进入新的官方周额度窗口后，自动清零指定分组成员的 OpenAI 周额度。

触发条件是官方窗口真实变化，不是本地时间到达旧窗口的预计结束时间。提前重置和按时重置必须使用同一条检测与执行链路。

## 目标

- 管理员可在现有运维规则区域创建“GPT 周额度重置联动”规则。
- 每条规则绑定一个来源 OpenAI OAuth 账号和一个目标分组。
- 后台主动读取来源账号的官方额度，不依赖用户流量、账号列表页面或手动刷新。
- 首次成功观测只建立基线；后续观测到新的官方七天窗口时执行一次联动重置。
- 清零目标分组有效成员的 OpenAI 周用量，并把用户周窗口起点对齐到官方新窗口起点。
- 多实例、服务重启、重复轮询和任务重试不能对同一个来源规则及官方窗口重复执行。
- 执行结果可审计，失败可重试，不静默丢失。

## 非目标

- 不处理 OpenAI 五小时额度窗口。
- 不清零用户余额、订阅用量、API Key 独立额度、OpenAI 日额度或月额度。
- 不根据 `used_percent` 单独判断重置。
- 不把普通告警阈值规则改造成带副作用的自动化规则。
- 不自动重置没有 OpenAI 平台额度记录的用户；这类用户不受该额度限制，执行结果中记为跳过。

## 管理端交互

运维规则区域增加“额度联动”视图，与现有“告警规则”并列展示。两类规则使用独立 API 和数据表，避免现有告警规则中的指标、运算符、阈值、持续时间等字段出现无意义配置。

创建或编辑联动规则时展示以下字段：

- 名称：必填。
- 描述：可选。
- 来源 Pro 账号：必填，只允许选择非影子的 OpenAI OAuth 账号。
- 目标分组：必填，只允许选择有效分组。
- 启用状态：默认启用。

规则列表展示来源账号、目标分组、最近观测时间、当前官方周窗口结束时间、最近执行状态和最近错误。一个来源账号可通过多条规则绑定多个分组；相同来源账号与目标分组不允许存在重复有效规则。

## 数据模型

### 联动规则

新增 `openai_weekly_quota_reset_rules`：

- `id`、`name`、`description`、`enabled`
- `source_account_id`、`target_group_id`
- `last_observed_reset_at`：最近成功观测到的官方周窗口结束时间
- `last_observed_window_seconds`：官方窗口长度，用于计算窗口起点和校验七天窗口
- `last_observed_fetched_at`、`last_run_at`
- `last_error`
- `created_at`、`updated_at`、`deleted_at`

来源账号和目标分组使用外键。有效记录上建立 `(source_account_id, target_group_id)` 唯一约束。

### 执行记录

新增 `openai_weekly_quota_reset_executions`：

- `id`、`rule_id`、`source_account_id`、`target_group_id`
- `official_reset_at`、`official_window_start`、`official_window_seconds`
- `status`：`pending`、`running`、`succeeded`、`retryable_failed`、`permanent_failed`
- `matched_users`、`reset_users`、`skipped_users`
- `error_message`、`detected_at`、`completed_at`、`created_at`、`updated_at`

建立 `(rule_id, official_reset_at)` 唯一约束，作为数据库级幂等键。即使多个实例同时检测到同一新窗口，也只能创建一条执行记录。

## 官方窗口检测

新增独立后台服务，每 60 秒扫描启用规则，并按来源账号去重后读取官方 `/wham/usage`。现有 OpenAI quota 客户端抽取一个“不查询 reset credits”的轻量用量读取路径，避免每次轮询产生无关请求。

检测步骤：

1. 从官方响应的窗口长度识别七天窗口，不依赖 primary/secondary 的固定位置。
2. 要求窗口包含有效的 `ResetAt`，且 `LimitWindowSeconds` 位于 6 天至 8 天之间；超出范围的窗口不得作为周窗口处理。
3. 规则没有 `last_observed_reset_at` 时，仅保存当前窗口作为基线，不执行用户重置。
4. 当前官方 `ResetAt` 等于已观测值时，不执行。
5. 当前官方 `ResetAt` 早于已观测值时，视为过期或乱序响应，只记录诊断信息，不回退基线。
6. 当前官方 `ResetAt` 晚于已观测值时，判定进入新周窗口。无论旧窗口是否已到预计结束时间，都创建幂等执行记录。

官方新窗口起点按 `ResetAt - LimitWindowSeconds` 计算。若服务离线期间跨过多个窗口，恢复后只对当前窗口执行一次重置，因为用户只需要与当前官方窗口重新对齐。

## 联动执行

目标成员以 `user_allowed_groups` 为准，并只包含未软删除且 `status = active` 的用户。执行时批量查找这些用户的有效 `platform = openai` 平台额度记录：

1. 在数据库事务中把 `weekly_usage_usd` 设为零，并把 `weekly_window_start` 设为官方新窗口起点。
2. 同一事务更新执行记录的数据库阶段结果。
3. 事务提交后，原子更新对应 Redis quota cache 的周用量和周窗口起点，并重新加入 dirty 集，确保额度校验立即看到零用量且后续会落库。
4. Redis 更新失败时把执行标记为 `retryable_failed`；后台重试使用相同官方窗口起点，不创建新执行记录。
5. 全部缓存更新完成后标记 `succeeded`，记录匹配、重置和跳过数量。

为防止 quota flusher 已读取的旧快照覆盖刚完成的重置，`BatchSnapshotUsage` 对每个窗口采用单调窗口起点保护：只有快照的窗口起点不早于数据库当前窗口起点时，才允许覆盖该窗口的用量和起点。该保护同时修复现有管理员强制重置的同类竞态。

多个规则命中同一用户时，清零操作本身保持幂等；每条规则仍保留独立审计记录。

## 并发与容错

- 轮询服务使用现有后台任务生命周期和 Redis leader lock，避免多个实例重复发起同一轮上游请求。
- 数据库唯一幂等键是最终一致性保障，不能只依赖 leader lock。
- 官方读取失败、账号认证失败或缺少七天窗口时不推进基线，也不执行重置；保存最近错误并在下一轮重试。
- 来源账号、目标分组被删除或失效时，规则不执行并显示明确错误。
- 执行数据库事务失败时不修改缓存；缓存阶段失败时保留可重试执行状态。
- 删除或禁用规则不删除历史执行记录。

## API

新增管理员 API：

- `GET /admin/ops/openai-weekly-quota-reset-rules`
- `POST /admin/ops/openai-weekly-quota-reset-rules`
- `PUT /admin/ops/openai-weekly-quota-reset-rules/:id`
- `DELETE /admin/ops/openai-weekly-quota-reset-rules/:id`
- `GET /admin/ops/openai-weekly-quota-reset-executions`

创建和更新时由服务端再次校验账号平台、OAuth 类型、影子状态、目标分组和重复绑定，不能只依赖前端过滤。

## 可观测性

每次窗口观测变化和联动执行输出结构化日志，至少包含规则 ID、来源账号 ID、目标分组 ID、旧/新官方窗口、执行状态和用户计数。执行历史在运维页面可查询，最近错误直接显示在规则行。

## 测试

- 检测单元测试：首次建基线、相同窗口不触发、正常到期窗口变化、提前窗口变化、乱序旧响应、缺失七天窗口。
- 幂等测试：重复快照、多实例并发、服务重启和失败重试只生成一条执行记录。
- 仓储集成测试：只重置目标分组有效成员、跳过无 OpenAI quota 的用户、官方窗口起点正确写入。
- flusher 回归测试：旧窗口快照不能覆盖更新后的周窗口及零用量，新窗口快照仍可正常落库。
- 缓存测试：周用量立即归零、dirty 标记恢复、缓存失败进入可重试状态。
- Handler 测试：账号/分组校验、重复规则、增删改查和执行历史分页。
- 前端测试：视图切换、账号与分组选择、表单校验、错误展示和执行状态展示。

## 验收标准

- 管理员可创建“一个 Pro 账号到一个分组”的联动规则。
- 首次启用不误清额度。
- 模拟官方按时或提前开启新七天窗口后，目标分组用户的 OpenAI 周额度都只重置一次。
- 普通轮询、用量下降、旧响应和服务重启不会误触发。
- 用户余额及其他额度窗口不受影响。
- 旧 Redis/DB 快照不能把已重置的周额度恢复为旧值。
