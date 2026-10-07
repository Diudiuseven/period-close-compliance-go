# 按期结账与合规申报（period-close-compliance-go）

纯进程内、仅依赖本地持久化的会计期间结账与合规口径申报引擎。明细持续摄入，
期间可按期结账并产出**唯一确定**的合规口径结果；口径规则可版本化与按时间生效；
已结账期间可受控重开并以追加更正方式调整；全程可审计，崩溃/重启后结果可在本地
复现。

```go
import "github.com/Diudiuseven/period-close-compliance-go/periodclose"
```

## 快速验证

```bash
go test ./... -race -count=1   # 全部自动化测试（含竞态检测）
go run ./cmd/demo              # 端到端演示（结账→迟到/重复→重开调整→重启复现）
```

## 数据与持久化模型

- **金额**统一使用有符号整数（最小货币单位 / 分），全程不使用浮点数。
- 所有写操作先进入本地 **WAL（`journal.wal`，追加、CRC32 校验、整批一次 `write` +
  `fsync`）**，成功后再更新内存并原子落一份 `snapshot.json`（临时文件 + fsync +
  rename）作为可检查的检查点。
- **日志是唯一真相源**：重启时从 LSN 1 全量重放重建内存状态；快照只是旁路优化，
  正确性不依赖它，因此重放严格“恰好一次”，不重复应用也不跳过任何明细。
- 帧格式含 magic、类型、LSN、长度与 CRC。重启时遇到结尾撕裂/损坏帧会**截断到
  最后一条完整帧**，部分写入不会污染状态，后续追加仍连续。

## 结账语义

### 1. 期间归集、幂等与迟到/乱序
- 明细按 `Period`（`YYYYMM`）归集；以明细 **`ID` 为全局幂等键**。重复提交（同批
  或跨批、跨重启）只计一次，不报错地被确认。
- 乱序到达只影响摄入顺序，报告按 LSN 排序后确定性聚合，结果与到达顺序无关。
- 已结账期间拒绝新明细（迟到），该拒绝写入审计；结账结果**绝不**因迟到/重复/乱序
  而偏移。更正只能走“重开 + 追加调整”。

### 2. 结账原子性（两阶段提交）
- **阶段 1（持锁）**：快照该期间全部已接受行、选定当前生效规则，落一条
  `close-start` 意图（fsync），期间进入瞬态 `closing`。
- **计算（不持全局锁）**：基于已捕获输入做确定性报告计算——慢期间不阻塞其他期间
  的摄入与结账。
- **阶段 2（持锁）**：校验意图仍为同一版本，落不可变 `CloseVersion` 提交（fsync），
  随后原子发布。
- 读取方（读锁）只能看到“结账前的完整旧状态”或“提交后的完整新版本”，看不到
  半完成状态，也看不到字段级混合结果。

### 3. 规则版本化与生效时间
- `Rule` 带 `Version`（严格递增、唯一）与 `EffectiveFrom`。结账发生时，在
  `EffectiveFrom <= 结账时刻` 的规则中选择生效时间最新（并列取版本号更高）者。
- 规则可在新期间或指定时刻生效；**已结账期间钉住其 `RuleVersion`，规则更新后结果
  不变**；未结账期间按结账时刻最新生效规则结账。每个 `CloseVersion` 都记录所用
  规则版本与输入指纹，前后结果均可解释。
- 规则支持：类别重映射 `CategoryMap`、白名单 `IncludeCategories`（空为全部）、
  分类符号翻转 `Sign`。

### 4. 重开与追加调整
- 重开前置条件：期间当前必须为 `closed`；需要 **admin** 权限；重开本身落日志并
  审计，旧 `CloseVersion` 保留在版本历史中。
- 调整**只能追加**（`Adjustment`），不得修改或删除既有行；按调整 ID 幂等。
- 重新结账产出新版本号；提交前用“按全量口径从原始明细 + 全部调整重新计算”的结果
  做 **fingerprint 对齐校验**，不一致则拒绝提交，保证重结账 = 全量重算。

### 5. 并发
- 单把 `RWMutex` 保护派生状态，提交在临界区内完成，故任一读取都落在某个自洽完整
  版本上。
- 结账的昂贵计算在锁外进行，仅用每期间 busy 标志串行化同一期间的结账；不同期间的
  明细摄入与结账互不阻塞。同一期间并发结账只会有一个提交成功。

### 6. 失败与重启恢复
- 已提交（`closed`，含 `CloseVersion`）与进行中（`closing`，只有意图无版本）在
  重启后**可明确区分**。
- 进行中的结账没有任何已提交版本，`Recover(admin)` 将其确定性回滚为 `open`（落
  `close-abort` 帧并审计），随后可正常结账；已结账期间不受影响。
- 已 fsync 的批次要么整批可见、要么整批不可见；重复提交靠 ID 去重，不重复入账。

### 7. 审计与权限
- 角色：`submitter`（摄入）、`closer`（结账）、`admin`（规则/重开/调整/恢复）、
  `auditor`（读审计）、`reader`（读期间）。
- 每一次 `intake / close_start / close_commit / reopen / adjust / rule_change` 都
  与状态变更写在**同一个 fsynced 批次**里，审计与业务状态不可能分叉；审计序号
  单调递增。越权与前置条件失败被拒绝并记录 `denied`。审计仅追加，随状态同一把锁
  串行写入，不会被并发破坏或丢失。

## 配置项

| 配置 | 说明 |
| --- | --- |
| `Config.Dir` | 本地数据目录（必填）。存放 `journal.wal` 与 `snapshot.json`。 |

`Service` 的方法签名：

- `Open(Config) (*Service, error)` / `Close() error`
- `SubmitEntries(actor, ...Entry) error`（幂等；迟到/非开放期间拒绝并审计）
- `ClosePeriod(actor, Period) (*CloseVersion, error)`（两阶段原子结账）
- `ReopenPeriod(actor, Period, reason) error`（admin，closed 前置条件）
- `AddAdjustment(actor, Adjustment) error`（仅 reopen 态，追加、幂等）
- `AddRule(actor, Rule) (Rule, error)`（admin；版本/生效时间版本化）
- `GetPeriod(actor, Period) (PeriodView, error)`（任一自洽完整版本）
- `AuditLog(actor) ([]AuditRecord, error)`（auditor/admin）
- `Recover(actor) error`（admin；把中断的结账回滚为 open）

## 期间状态机

```
open ──close_start──▶ closing ──close_commit──▶ closed
  ▲                     │                         │
  │                     └─(crash)─▶ open          │ reopen(admin)
  │                                  (Recover)    ▼
  │                                            reopen ──(adjust 追加)──▶ close ──▶ closed(v+1)
```
