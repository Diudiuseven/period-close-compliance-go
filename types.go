package compliance

import "time"

// Period 标识一个会计期间，例如 "2026-09"。
type Period string

// Status 是期间在其生命周期中的状态。
type Status string

const (
	StatusOpen     Status = "open"     // 未结账，可摄入明细
	StatusClosing  Status = "closing"  // 结账进行中（已写意图，尚未提交）
	StatusClosed   Status = "closed"   // 已结账，当前版本唯一确定
	StatusReopened Status = "reopened" // 已重开，只能以追加更正方式调整
)

// Entry 是一条日常账务明细。
type Entry struct {
	IdempotencyKey string    // 幂等键，重复提交不得被重复计入
	Period         Period    // 归属会计期间
	Category       string    // 口径分类
	Amount         int64     // 金额（最小货币单位，避免浮点误差）
	OccurredAt     time.Time // 业务发生时间
	ReceivedAt     time.Time // 系统接收时间
}

// Adjustment 是重开后追加的更正记录，只能追加，不能改写历史。
type Adjustment struct {
	ID               string
	Period           Period
	Category         string
	Amount           int64 // 可正可负
	Reason           string
	Author           string
	CreatedAt        time.Time
	ReversesEntryKey string // 可选：被冲销的原明细幂等键
}

// Rule 是某一版本的合规口径规则，发布后不可变。
type Rule struct {
	Version              int
	Name                 string
	EffectiveAt          time.Time // 生效时刻
	TaxRateBasisPoints   int       // 税率（基点，万分位）
	DeductibleCategories []string  // 可税前抵扣的分类
	PublishedBy          string
	PublishedAt          time.Time
}

// Balances 是按分类汇总的余额。
type Balances map[string]int64

// FilingResult 是合规申报口径结果。
type FilingResult struct {
	Gross       int64
	Deductible  int64
	Taxable     int64
	TaxDue      int64
	RuleVersion int
	ByCategory  map[string]int64
}

// CloseVersion 是一次结账提交后不可变的完整版本快照。
type CloseVersion struct {
	Period        Period
	Version       int // 第几次结账（重开后重新结账递增）
	RuleVersion   int // 本次结账使用的规则版本
	Balances      Balances
	Filing        FilingResult
	EntryKeys     []string // 计入的明细幂等键（确定顺序）
	AdjustmentIDs []string // 计入的追加更正 ID
	LastSeq       int64    // 结账时已提交 WAL 的最大序号
	ClosedAt      time.Time
	ClosedBy      string
}

// PeriodView 是读取方看到的期间自洽视图。
type PeriodView struct {
	Period          Period
	Status          Status
	Current         *CloseVersion // 当前已提交版本；结账中不会变成半成品
	History         []*CloseVersion
	EntryCount      int
	AdjustmentCount int
}

// AuditEvent 是一条不可篡改的审计记录。
type AuditEvent struct {
	Seq     int64
	Time    time.Time
	Actor   string
	Action  string
	Target  string
	Allowed bool
	Reason  string
	Detail  map[string]string
}
