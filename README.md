# period-close-compliance-go

按会计期间归集日常账务明细、原子结账并产出合规口径申报结果的本地可复现实例。
只依赖**进程内状态 + 本地文件持久化**（预写日志、快照、审计日志），不依赖任何外部数据库或服务。

## 能力总览

| # | 需求 | 实现方式 |
|---|------|----------|
| 1 | 按期归集/结账，迟到、重复、乱序明细不偏移已结账结果 | 全局幂等键去重；明细按 `(OccurredAt, IdempotencyKey)` 确定性排序；`closed` 期间拒绝摄入 |
| 2 | 结账原子性，读不到半成品/字段级混合 | 两阶段结账：`close_start`（意图）→ 计算 → `close_commit`；读取只暴露已提交的完整 `CloseVersion` |
| 3 | 口径规则版本化、按时刻生效 | `RuleBook` 中规则发布后不可变；结账时刻固定 `RuleVersion` 并写入版本；已结账期间永不漂移 |
| 4 | 重开与追加更正，重结与全量重算对齐 | 仅 `closed` 可重开；只能 `AppendAdjustment` 追加；重新结账基于"全部原始明细+全部更正"全量重算 |
| 5 | 并发结账/调整/读取，慢期间不阻塞他期 | 每期间一把锁 + 全局短临界区；读者拿到的永远是某个自洽版本（深拷贝快照） |
| 6 | 失败与重启恢复，不重不漏不留部分写 | 所有状态变更先写 fsync 的 WAL 再改内存；重启整段重放；半截尾行截断；孤儿 `closing` 可自动回滚或人工干预 |
| 7 | 全流程审计与权限 | 7 个权限点 + 预置角色；越权拒绝并写 `allowed=false`；审计日志只追加、互斥串行、逐次 fsync |
| 8 | 自动化测试与文档 | 见 `*_test.go` 与本文档 |

## 领域模型

- `Entry`：日常账务明细，携带全局唯一 `IdempotencyKey`、归属 `Period`、分类、金额（最小货币单位 `int64`）。
- `Rule`：某一版本的申报口径（税率基点、可抵扣分类、生效时刻），版本号唯一，**发布即不可变**。
- `Adjustment`：重开后追加的更正，金额可正可负，只能追加，不改写历史。
- `CloseVersion`：一次结账提交的**不可变完整版本**——余额、申报结果、计入明细键、更正 ID、规则版本、WAL 序号、结账人/时间。
- `PeriodView`：读取方视图（状态 + 当前版本 + 历史版本），深拷贝，调用方无法污染内部状态。

### 期间生命周期

```
            Ingest                 close_start + close_commit
  (none) ───────────▶ open ────────────────────────────────▶ closed
                        ▲                                      │
                        │ close_abort（崩溃恢复/人工放弃）       │ Reopen（需 reopen 权限）
                        │                                      ▼
                     closing ◀──── close_start ─────────── reopened
                        │                                      │
                        └──────── close_abort ────────── (AppendAdjustment 后
                                                               可再次 close)
```

## 结账语义（关键约定）

1. **幂等**：同一 `IdempotencyKey` 全局只计入一次；重复提交（即使金额不同）返回 `ErrDuplicateEntry`，并发抢注只有一个成功。
2. **确定性**：版本中的明细按 `(OccurredAt, IdempotencyKey)`、更正按 `ID` 排序，任何时候重算结果唯一。
3. **关门**：`closed` 期间摄入返回 `ErrPeriodClosed`；迟到明细不会进账。`closing` 期间摄入返回 `ErrPeriodClosing`。
4. **原子提交**：意图写入后期间进入 `closing`，但读者此时仍只看到旧的完整版本（或没有版本）；提交成功才切换为新 `CloseVersion`。
5. **规则选择**：结账时刻 `EffectiveAt(at)` 命中的最新规则被固定进版本；之后发布新规则不改变历史版本。
6. **重开**：前置条件为期间处于 `closed` 且调用方有 `reopen` 权限；历史版本保留在 `History` 中不动。
7. **调整**：仅 `reopened` 期间可追加更正；更正 ID 去重；重新结账对"原始明细 ∪ 全部更正"全量计算，因此结果与按全量口径重算严格一致（测试 `TestReopenAdjustmentAlignsWithFullRecompute` 校验）。

### 申报口径

- `Gross` = 非抵扣分类余额合计
- `Deductible` = 抵扣分类正余额合计
- `Taxable` = `max(Gross - Deductible, 0)`
- `TaxDue` = `Taxable * TaxRateBasisPoints / 10000`

## 权限模型

| 角色 | 权限 |
|------|------|
| `NewAdmin` | ingest, close, reopen, adjust, rule, read, audit |
| `NewOperator` | ingest, close, adjust, read |
| `NewAuditor` | read, audit |
| `NewReader` | read |
| `NewUser(name, perms...)` | 自定义 |

所有被拒绝的操作都会落一条 `Allowed=false` 的审计记录（actor/action/target/reason）。

## 配置项（`Config`）

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `DataDir` | 空（`DefaultConfig` 时由调用方指定；空目录=纯内存模式） | `wal.log`、`snapshot.json`、`audit.log` 的存放目录 |
| `Fsync` | `true` | 每条 WAL/审计追加是否 `fsync`；关闭可提速但崩溃可能丢最近一次提交 |
| `CloseTimeout` | `5s` | 等待期间锁的超时；`0` 表示一直等待。超时返回 `ErrPeriodClosing` |
| `DefaultRules` | 一条 0% 税率的自始生效兜底规则 | 规则簿为空时启动发布（同样写 WAL） |
| `RecoverOrphanClose` | `true` | 重启遇到"有 `close_start` 无 `close_commit/abort`"的孤儿期间时，自动回滚为 `open`/`reopened` 并写审计；`false` 则保留 `closing`，等待 `AbortClose` 人工干预 |

辅助构造：`DefaultConfig()`、`InMemoryConfig()`。

## 本地持久化与恢复

```
<DataDir>/
├── wal.log        # 只追加 JSON Lines：entry / adjustment / rule /
│                  # close_start / close_commit / close_abort / reopen
├── snapshot.json  # 最近已提交检查点，temp+rename 原子替换（仅加速/可观测，恢复以 WAL 为准）
└── audit.log      # 只追加 JSON Lines 审计记录，独立序号空间，逐次 fsync
```

- 重启时**整段重放 WAL**：序号必须连续，否则判定日志被外部破坏并拒绝启动（避免跳过/重复）。
- WAL 最后一行若是半截写（崩溃），打开时截断到最后一条完整记录。
- 完整重放后仍处于 `closing` 的期间即为"进行中的结账"，与 `closed` 明确可区分。
- 审计日志独立于业务 WAL，崩溃恢复/孤儿回滚本身也留痕。

## 本地验证

```bash
go test ./... -race      # 全部自动化测试（含重复/迟到/乱序、规则切换、重开调整、并发、重启恢复）
go run ./cmd/demo        # 端到端冒烟：摄入→去重→结账→规则升级→重开→调整→重结→重启比对
```

`cmd/demo` 会在 `./.demo-data` 下产出 WAL/快照/审计文件，可直接打开检查，随后用相同 `DataDir`
再启动即完成一次"重启后结果一致"的本地复现。

## 已知边界

- 单进程设计：多进程共享同一 `DataDir` 未做文件锁协调（题目要求仅进程内状态）。
- 审计与 WAL 各自 fsync，不在同一个磁盘事务内；极端"提交成功、审计 fsync 前掉电"的情况下，
  业务结果由 WAL 中的 commit 记录保证可追溯，可能少一条人类可读审计行。
