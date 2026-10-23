# 预付费余额预警 — 授信排除名单 · 设计上下文

| 项 | 值 |
| --- | --- |
| 状态 | **已实施（2026-10-23）**：§5 施工清单九步全部落地，§4 的 D1–D7 已消除；真库用例 TC-01 ~ TC-14 通过 |
| 日期 | 2026-10-23 |
| 评审方式 | 两轮 grilling + domain modeling，逐一对照代码与本地库事实 |
| 关联文档 | `docs/企业预付费余额预警-补充预付费企业识别规则-PRD-2026-10-18.md`、`GLOSSARY.md`、`docs/adr/0001`~`0002` |

本文记录**为什么这么定**，以及**当前实现与定稿口径的差距**。PRD 记需求，本文记决策与施工清单，ADR 记难以回退的取舍。

---

## 1. 一句话领域模型

> 预警池 = 活跃企业账户 − 授信活跃企业。
> 授信活跃的判定依据是**信用垫付行为 + 时间窗口**，不是任何身份字段、不是钱包余额、不是充值流水。

这条替换掉了 PRD v6 的旧口径（`billing_account_credit_configs.status = ACTIVE`）。旧口径的致命点是配置一经创建即恒为 ACTIVE，状态退化成"是否配置过信用"的历史记录。

---

## 2. 已确认的技术事实

以下均为 2026-10-22/23 在本地 PostgreSQL（`ruidong_billing` / `ruidong_alerting`）实测，不是推断。

| 事实 | 结论 | 影响 |
| --- | --- | --- |
| `balance_consumption_allocations` 规模 | 240 万行，其中 `source=BALANCE_GRANT` 34 万行且 `approved_credit_usd` 全为 0 | 证明 `RECENT_ADVANCE` 必须限定 `approved_credit_usd > 0`，否则真预付费被整体误排 |
| 部分索引 `balance_allocations_account_funding_open_idx` | 谓词 `(approved_credit_usd > 0 OR unapproved_debt_usd > 0)` | 刷新 SQL 谓词必须与之逐字一致才能走索引；实测命中 7 buffer、0.11ms |
| `settled_usd` 语义 | 欠款核销**累计**（Prisma 注释明确） | `approved - settled` 作为未清债务口径正确 |
| `enterprise_credit_settlements.occurred_at` | NOT NULL，但**无任何索引** | 时间过滤只能当 Filter，生产放大后核销表会成为刷新瓶颈 |
| `debt_source` | 本地全为 `ENTERPRISE_CREDIT`，无 NULL | 限定 `debt_source` 不会漏判 |
| `billing_accounts.status` | 取值 `active / suspended / deleted` | 候选集只取 active 正确；suspended 企业仍可能出现在名单里（名单不按状态过滤） |
| `provisioning_quarantined_at` | timestamptz，可空 | 候选集已包含该条件；但自助页查询 SQL 未包含，两侧口径不一致 |
| 单测运行环境 | `glebarez/sqlite`（纯 Go），`CGO_ENABLED=0` | `excludeScanSQL` 含 `make_interval`，**无法在现有单测里执行**；真库测试必须另设开关 |
| CI | 仓库无 `.github/workflows` | 不能指望 CI 兜住，集成测试必须本地可跑 |
| 启动结构 | `center.go:205` 是 `go balancealert.StartCron(ctx)` | 补刷同步化不会阻塞 HTTP 服务启动，只推迟 cron 注册 |
| 建表方式 | 模型已在 `models/migrate/migrate.go:245` 的 AutoMigrate 列表 | 新环境自动建表，无人工迁移步骤 |

---

## 3. 已定决策

### 3.1 客群判定口径

| 编号 | 决策 | 理由 |
| --- | --- | --- |
| Q1 | **信用核销保留为独立的第三类排除条件**；排除期起算点 = 「最后一次垫付或最后一次核销中较晚者」 | 「刚对完账代充」的企业事实上还没转成自主充值，砍掉会放过真实授信尾巴。错的是"冗余保护"这个措辞，不是代码 |
| — | `unapproved_debt_usd`（未批准穿零欠款）本期不参与判定 | 试点期先观察规模；混进来会把"额度外欠款"与"信用垫付"两个概念搅在一起 |
| — | 不要求 `funding_classification_complete = true` | 只认 `approved_credit_usd` 与未清差额，与"垫付事实"口径一致；历史行该字段不完整 |
| — | 窗口为滚动 N×24 小时 | 按自然日截断会让同一份数据在不同时刻得出不同客群 |

### 3.2 刷新与失败处置

| 编号 | 决策 | 理由 |
| --- | --- | --- |
| Q2 | 启动补刷改为**同步**（在 `refreshCron.Start()` / `c.Start()` 之前完成） | 兑现"补刷必然早于首轮判定"；否则 xx:59:5x 启动时首轮会静默 fail closed |
| Q3 | 失败告警按两种情形拆分：**从未成功**（整条链路不可用）与**沿用旧名单**（功能降级） | 前者只报 2 次后永久静默，是最不该省消息的场景 |
| Q9 | 启动补刷失败**不退出进程**，记录并告警后继续启动 | `n9e-center` 同时承载全部告警能力；为一张派生缓存让整平台停摆，代价远超收益。fail closed 已保证不会误发 |
| Q10 | 刷新状态新增 `last_notified_failures`（已通知到第几次失败）与 `last_notified_at`（该次通知送达时间） | 只有显式计数能区分"已通知"与"通知没发出去"；但要判断"群里那条坏了是否该收尾"，还需要通告时间与 `last_success_at` 比大小。只记次数会让"关闭期间恢复、随后又失败"两份消息一起丢 |
| Q15 | 「从未成功」的告警**只由定时任务发出**；手动刷新只在页面回显错误 | 点按钮的人就在页面上，再推钉钉是重复告知；钉钉留给没人盯着的场景 |
| Q8 / Q17 | `list_total` **只进 `exclude-state`**，不进判定 `Stats` | 判定轮次的职责是圈人，不该为日志多读一次配置；页面本就同时拉两个接口，拼接成本为零 |

### 3.3 参数与可观测

| 编号 | 决策 | 理由 |
| --- | --- | --- |
| Q7 | `credit_advance_window_days` 改为**指针**：缺失（nil）沿用已存值，仅显式越界拒绝 | 新增一个可选调参字段不该让仍按旧 schema 调用的脚本整体失败 |
| Q6 | 模型补 `default:now()`，与 PRD 的 DDL 对齐 | 代码始终显式赋值，加默认值只让新库结构一致，零行为影响 |
| Q13 | 客户侧只显示一句「当前按信用账期结算，不发送余额预警」；`reason` 细分类别**只留运营侧** | `OPEN_DEBT` / `RECENT_SETTLEMENT` 对客户是纯困惑源 |
| Q20 | `reason` 中文译名只保留**一处**（运营页 `EXCLUDE_REASON` 常量）；PRD 只定义语义 | 两处各写一套中文，迟早漂 |
| Q5 | 自助页必须感知排除名单 | 现在能配置成功却永远收不到告警且毫无提示，这是误导 |
| Q16 | 被排除企业的自助页表单**只读禁用** + 说明原因 | 可编辑却永不生效是最差组合；转回预付费、名单刷新后自动恢复可编辑 |

### 3.4 测试与文档

| 编号 | 决策 | 理由 |
| --- | --- | --- |
| Q4 | 补**真库集成测试**，删除无调用方的 `DecideExcludeReason` | 该函数给人的"优先级已被测试锁定"是假象；`excludeScanSQL` 目前一次都没被执行过 |
| Q11 | 集成测试用环境变量 `BALANCEALERT_PG_DSN` 守护，跳过时打印自解释提示 | 构建标签会让测试从日常视野里消失 |
| Q18 | 集成测试在同一 DSN 内建**临时 schema**，跑完 `DROP SCHEMA ... CASCADE` | 不依赖运维预建库，也不会因忘记 truncate 污染开发库 |
| Q12 | 新增 **TC-14**（最后垫付超窗 + 窗口内核销 → 仍排除）；修正 TC-04 起算点 | 现有 13 条用例没有任何一条覆盖 Q1 的结论，不锁就会被"顺手精简冗余分支"改掉 |
| Q14 | 补两篇 ADR，并修正 GLOSSARY 中"冗余保护"的表述 | 现有词汇表与定稿口径直接相悖，会继续误导实现 |
| Q19 | ADR 中记录被否方案（实时反连接 / 每轮扫大表） | 不写，三个月后就会有人提议"为什么不实时 join，反正有索引" |

---

## 4. 现有实现与定稿口径的偏差

| 编号 | 偏差 | 严重度 | 处置 |
| --- | --- | --- | --- |
| D1 | PRD 与代码注释把 settlements 分支称为"冗余保护""正常数据下两表判定一致"——**不成立** | 高（会导致下次被删） | 改注释与 PRD；ADR 0002 记录；TC-14 锁定 |
| D2 | PRD §5 称"全部用例以自动化测试覆盖"——TC-01~TC-09/11/13 一条都没有；`DecideExcludeReason` 是死代码 | 高 | 补集成测试；删死代码；PRD 改为真实覆盖范围 |
| D3 | FR-05.3 称"启动补刷必然早于下一轮判定"——实际是 goroutine，xx:59:5x 启动时首轮会 fail closed 且无告警 | 中 | Q2 同步化 |
| D4 | 告警全局停用期间失败计数照常累加，重新启用后计数已越过阈值，**永不报警**；从未成功时第 3 次起也静默 | 中 | Q3 + Q10 |
| D5 | 自助页 `my-config` / `my-status` 不感知排除名单，且其 SQL 缺 `provisioning_quarantined_at IS NULL`，两侧口径不一致 | 中 | Q5 + Q16 |
| D6 | 模型 tag 缺 `default:now()`，实测建出的 `refreshed_at` 无默认值，与 PRD DDL 不一致 | 低 | Q6 |
| D7 | 新参数变必填：不含该字段的旧格式 PUT 会整体失败（`识别窗口必须为 1–365 的整数天`） | 低 | Q7 |

补充：把 `1.5` 传给窗口参数时，接口原样透出 Go 类型名（`cannot unmarshal number 1.5 into Go struct field ... of type int`）。拒绝行为正确，措辞待收敛。

---

## 5. 施工清单

> 当前工作区状态：`balancealert/{billing,cron,exclude}.go`、`exclude_test.go`、`models/balance_alert_exclude.go` 与补充 PRD 有**上一轮未提交**的改动（60s 超时、失败告警去重 + 恢复通知、cron 日志补 `excluded`、PRD v1.3）。本文档与 ADR 未改动任何代码。

顺序按依赖排列：

1. **口径纠错**（无行为变化）：删掉注释里的"冗余保护/两表一致"，改为"第三类独立排除条件"；同步 PRD 的 D1 三处措辞与 TC-04 起算点。
2. **Q2 + Q9**：补刷同步化；失败仅记录 + 告警，进程继续。
3. **Q3 + Q10 + Q15**：拆分两种失败告警；状态加 `last_notified_failures`；手动刷新不发钉钉。
4. **Q7**：窗口参数改指针 + 缺失沿用；同时收敛非法值的错误文案。
5. **Q6**：模型补 `default:now()`。
6. **Q8 + Q17**：`exclude-state` 返回 `list_total`；运营页把"名单规模 / 本轮命中"并列展示。
7. **Q5 + Q13 + Q16**：自助页返回 `excluded` 标记与客户可读文案；被排除时表单只读；`reason` 不出客户侧。
8. **Q4 + Q11 + Q12 + Q18**：集成测试（临时 schema、`BALANCEALERT_PG_DSN` 守护、TC-01~TC-14）；删 `DecideExcludeReason`。
9. **Q20**：核对译名只有一处。

**billing 仓库保持零改动**是硬约束：本轮所有需要计费库配合的事项只有一件——给 `enterprise_credit_settlements` 补 `(debt_source, occurred_at)` 索引，交由 DBA 以 `CREATE INDEX CONCURRENTLY` 执行，不进 alerting 的迁移，也不需要修改 billing 代码。

---

## 6. 验证方式

```bash
# 单测（不需要真库）
go test ./balancealert/ ./models/

# 真库集成测试（TC-01 ~ TC-14）
# -run TestExclude 是刻意的：所有真库用例都以 TestExclude 开头，
# 任何新用例只要命名不合这个前缀就会在这个命令下"没跑"，而不是"通过"。
$env:BALANCEALERT_PG_DSN = "host=127.0.0.1 port=5432 user=... dbname=ruidong_billing sslmode=disable"
go test ./balancealert/ -count=1 -run TestExclude -v

# 刷新与状态
curl -X POST -H "Authorization: Bearer $TOKEN" localhost:17000/api/n9e/balance-alert/exclude-refresh
curl -H "Authorization: Bearer $TOKEN" localhost:17000/api/n9e/balance-alert/exclude-state

# 圈选与排除数
curl -X POST -H "Authorization: Bearer $TOKEN" localhost:17000/api/n9e/balance-alert/run
```

2026-10-22 的实测基线：名单 4 家（`OPEN_DEBT` 1 / `RECENT_ADVANCE` 3），判定轮次 `excluded=4, prepaid=6, skipped_income=1`；cron 在 11:00、11:15 均按时触发且结果一致。

---

## 7. 明确不做

- 不改 billing 仓库的代码、不加 billing 迁移。
- 不在判定轮次实时 join 计费库，也不每轮扫 `balance_consumption_allocations`（G4）。
- 不引入任何"授信身份"字段或人工维护的排除名单。
- 不把 `balance_alert_credit_excludes` 放回计费库。
- 不做客户侧"申请退出排除"入口——那属于授信管理流程。
- 不在告警停用期间静默丢弃"从未成功刷新"这一事实。
