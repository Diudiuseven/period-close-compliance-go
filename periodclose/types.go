// Package periodclose implements in-process period closing, compliance
// reporting, versioned reporting rules, reopen/adjustment workflow, audit
// and crash-recovery on top of a local append-only journal plus snapshots.
package periodclose

import "time"

// Period identifies an accounting period as a 6-digit YYYYMM key.
type Period string

// Role names used for authorization.
const (
	RoleSubmitter = "submitter"
	RoleCloser    = "closer"
	RoleAdmin     = "admin"
	RoleAuditor   = "auditor"
	RoleReader    = "reader"
)

// Actor identifies the caller of an operation.
type Actor struct {
	ID    string
	Roles []string
}

func (a Actor) hasRole(role string) bool {
	for _, r := range a.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Entry is one accounting detail line attributed to a period.
// Amount is expressed in integer minor units (cents) to stay exact.
type Entry struct {
	ID         string // client-supplied idempotency key, globally unique
	Period     Period // attributed accounting period
	Category   string // reporting category
	Amount     int64  // signed amount in cents
	OccurredAt time.Time
}

// StoredEntry records an entry together with intake bookkeeping.
type StoredEntry struct {
	Entry
	Seq        int64 // global journal sequence at intake
	ReceivedAt time.Time
}

// Rule is a versioned compliance/口径 rule. Only one rule version is
// effective at a point in time (by EffectiveFrom); higher Version numbers
// win for ties.
type Rule struct {
	Version     int64
	Description string
	// EffectiveFrom is the wall-clock instant from which this version
	// governs closing of still-open periods.
	EffectiveFrom time.Time
	// IncludeCategories restricts reported categories; empty means all.
	IncludeCategories []string
	// CategoryMap remaps source categories to reported categories.
	CategoryMap map[string]string
	// Sign flips the reported sign for a reported category (multiplier ±1).
	Sign map[string]int64
}

// Adjustment is an append-only correction attached to a reopened period.
type Adjustment struct {
	ID        string
	Period    Period
	Category  string
	Amount    int64
	Reason    string
	Seq       int64
	CreatedAt time.Time
}

// PeriodStatus is the lifecycle state of a period.
type PeriodStatus string

const (
	StatusOpen    PeriodStatus = "open"
	StatusClosing PeriodStatus = "closing" // an intent exists but no committed close (crash only)
	StatusClosed  PeriodStatus = "closed"
	StatusReopen  PeriodStatus = "reopen" // closed then reopened for adjustments
)

// CategoryFigure is one reported line.
type CategoryFigure struct {
	Category string
	Entries  int
	Amount   int64
}

// Report is the uniquely determined compliance output of a close version.
type Report struct {
	Period      Period
	RuleVersion int64
	GeneratedAt time.Time
	Totals      map[string]CategoryFigure
	Net         int64
	// Fingerprint deterministically summarizes inputs + rule + adjustments.
	Fingerprint string
}

// CloseVersion is one immutable committed close result of a period.
type CloseVersion struct {
	Period      Period
	Version     int64 // 1-based close version for the period
	RuleVersion int64
	ClosedAt    time.Time
	EntrySeqs   []int64 // global seqs incorporated (entries + adjustments)
	Report      Report
}

// PeriodView is the self-consistent state a reader can observe at one instant.
type PeriodView struct {
	Period Period
	Status PeriodStatus
	// CloseVersion is nil unless status is closed.
	CloseVersion *CloseVersion
	EntryCount   int
	EntryAmount  int64 // raw balance including all accepted lines (open/reopen only)
}

// OpKind enumerates audited operations.
type OpKind string

const (
	OpIntake      OpKind = "intake"
	OpCloseStart  OpKind = "close_start"
	OpCloseCommit OpKind = "close_commit"
	OpReopen      OpKind = "reopen"
	OpAdjust      OpKind = "adjust"
	OpRuleChange  OpKind = "rule_change"
	OpDeny        OpKind = "denied"
)

// AuditRecord is one immutable audit trail entry.
type AuditRecord struct {
	Seq          int64
	At           time.Time
	ActorID      string
	Op           OpKind
	Period       Period
	RuleVersion  int64
	CloseVersion int64
	Detail       string
	Allowed      bool
}

// Sentinel errors.
var (
	ErrUnauthorized   = errString("periodclose: unauthorized")
	ErrPeriodClosed   = errString("periodclose: period already closed")
	ErrNotClosed      = errString("periodclose: period is not closed")
	ErrAlreadyOpen    = errString("periodclose: period already open")
	ErrClosing        = errString("periodclose: close already in progress")
	ErrDuplicateEntry = errString("periodclose: duplicate entry id")
	ErrRuleNotFound   = errString("periodclose: no effective rule")
	ErrBadArgument    = errString("periodclose: bad argument")
	ErrNoAdvance      = errString("periodclose: nothing to recover")
)

type errString string

func (e errString) Error() string { return string(e) }
