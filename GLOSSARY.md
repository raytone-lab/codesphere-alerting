# 企业预付费余额预警

Alerting 侧对计费企业账户做余额耗尽前预警；客群圈选与发送路由分离。

## Language

**预付费企业**：
活跃企业账户中非「授信活跃」者；进入余额预警池。余额耗尽即停服。
_Avoid_：按 `billing_account_credit_configs` 判定的预付费、资金处理方式

**授信活跃企业**：
满足任一即为授信活跃、不进预警池：存在未清垫付债务；或近 N 天内产生过新垫付（N = `credit_advance_window_days`，默认 90）。
_Avoid_：授信企业（v6 口径：有可用信用配置）、信用配置 ACTIVE

**信用垫付**：
消费穿 0 后由信用额度支付的部分，登记于计费库 `balance_consumption_allocations`（`approved_credit_usd > 0`）。

**未清垫付债务**：
垫付记录上 `approved_credit_usd - coalesce(settled_usd, 0) > 0.0001` 的未核销部分。
阈值取 0.0001 而不是 `> 0`：金额列是 `Decimal(20,12)`，核销到分位后仍可能留灰尘尾数。
_Avoid_：`> 0`（浮点噪声会把已结清账户判成欠款）

**授信排除名单**：
物化表 `balance_alert_credit_excludes`（**alerting 自己的库**，随 AutoMigrate 建表），每日全量刷新；预警圈选在内存中按名单账户集合过滤，不扫消费大表、不跨库 join。有上一份成功名单时刷新失败则沿用；从未成功过则当轮不圈人、不发送。
_Avoid_：人工排除名单、身份写死字段、刷新过程中的空表窗口、把该表建到计费库（会让 billing 为一张派生缓存承担建表与授权）

**信用核销**：
`enterprise_credit_settlements` 中 `debt_source = 'ENTERPRISE_CREDIT'` 的核销。冗余保护只认此类。
_Avoid_：`NEGATIVE_BALANCE` 核销（真预付费穿 0 后再充也会有）

**排除原因**：
物化行 `reason` 取值：`OPEN_DEBT`（未清 approved 垫付）、`RECENT_ADVANCE`（窗口内产生过 `approved_credit_usd > 0` 的垫付）、`RECENT_SETTLEMENT`（窗口内有信用核销，allocations 极端修复时的冗余保护）。多条件同时命中时优先级：`OPEN_DEBT` > `RECENT_ADVANCE` > `RECENT_SETTLEMENT`。
_Avoid_：把未批准欠款 `unapproved_debt_usd` 或非信用核销算进授信活跃

**识别窗口**：
参数 `credit_advance_window_days`，落在 alerting 的 `balance_alert_settings`，默认 90 天；参数设置页可调，只接受 1–365 的整数（0/负数/超限直接拒绝保存，不静默纠正）。改参后下一次排除名单成功刷新生效（产品可表述为「一般次日」）。

**排除名单刷新**：
每日 07:30（Asia/Shanghai）全量刷新物化表；与每 15 分钟预警判定分离——判定轮次只读名单。
跨库两段式：先在计费库跑一条**只读**聚合 SELECT 得到「账户 -> 主因」，再整份替换进 alerting 库；计费库不接受任何写入。
整体替换在**单个事务内** `DELETE + INSERT` 一次提交：MVCC 下判定轮次只会看到完整旧名单或完整新名单，不存在空表窗口。不加跨实例锁——整份替换幂等，多副本并发刷最终收敛到某一份完整名单。
刷新状态记在 alerting 配置 `balance_alert_exclude_refresh`（上次成功时间、连续失败数、名单规模与 reason 分布），跨重启保留；「从未成功」是 fail closed 的依据，不是「名单为空」。
参数设置页的「立即刷新名单」是唯一能让改参即时生效的通道；成功后返回人数与三个 reason 计数。
连续 ≥2 次刷新失败经试点钉钉告警，成功则清零计数；失败期间沿用上一份成功名单。
_Avoid_：把刷新状态放在进程内存（重启即丢失）、先提交 DELETE 再 INSERT、在计费库写名单
