package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// closeIntent 是 close_start WAL 事件体：结账意图一旦写入即持久，
// 重启后可据此识别"进行中的结账"。
type closeIntent struct {
	Period      Period    `json:"period"`
	RuleVersion int       `json:"rule_version"`
	At          time.Time `json:"at"`
}

type reopenBody struct {
	Period Period    `json:"period"`
	Reason string    `json:"reason"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
}

// periodState 是单个会计期间的进程内状态，由 WAL 回放重建。
type periodState struct {
	// lock 是期间级互斥：结账慢只锁住本期间，不阻塞其他期间摄入/结账。
	lock        sync.Mutex
	period      Period
	status      Status
	entries     []Entry
	adjustments []Adjustment
	current     *CloseVersion
	history     []*CloseVersion
	intent      *closeIntent
}

// Service 是按期结账与合规申报链路的入口。
type Service struct {
	cfg  Config
	w    *wal // nil 表示纯内存模式
	aud  *AuditLog
	auth Authorizer

	// mu 只保护 periods 映射与全局幂等键表，不跨任何 fsync。
	mu      sync.RWMutex
	periods map[Period]*periodState
	keys    map[string]Period // 全局幂等键 -> 期间；同一键永远只计入一次
	rules   *RuleBook

	closed bool
}

// New 创建服务并从 DataDir 的 WAL/审计文件恢复。空 DataDir 为纯内存模式。
func New(cfg Config) (*Service, error) {
	s := &Service{
		cfg:     cfg,
		auth:    NewStaticAuthorizer(),
		periods: map[Period]*periodState{},
		keys:    map[string]Period{},
		rules:   NewRuleBook(),
	}

	if cfg.DataDir != "" {
		w, err := openWAL(cfg.DataDir, cfg.Fsync)
		if err != nil {
			return nil, err
		}
		s.w = w
		l, err := OpenAuditLog(cfg.DataDir)
		if err != nil {
			_ = w.close()
			return nil, err
		}
		s.aud = l
	} else {
		s.aud = NewAuditLog()
	}

	if err := s.recover(); err != nil {
		_ = s.Close()
		return nil, err
	}
	s.seedRules()
	return s, nil
}

// ---------- 恢复 ----------

// recover 重放全部 WAL 记录重建状态，并处理重启时的孤儿结账。
func (s *Service) recover() error {
	if s.w == nil {
		return nil
	}
	recs, err := s.w.readAll()
	if err != nil {
		return err
	}
	for _, r := range recs {
		if _, err := s.applyRec(r); err != nil {
			return err
		}
	}
	// 重启后"已结账"与"进行中的结账"必须可区分：
	// 完整重放后仍停留在 closing（意图存在但没有 commit/abort）的才是孤儿。
	var orphans []*periodState
	for _, ps := range s.periods {
		if ps.status == StatusClosing && ps.intent != nil {
			orphans = append(orphans, ps)
		}
	}
	// 重启后"已结账"与"进行中的结账"必须可区分：
	// close_start 之后没有 close_commit/abort 的期间就是孤儿结账。
	for _, ps := range orphans {
		if s.cfg.RecoverOrphanClose {
			s.rollbackIntent(ps, "recover-orphan-close", "system")
			s.audit("system", "recover_abort_close", string(ps.period), true,
				"crash recovery rolled back an uncommitted close")
		} else {
			s.audit("system", "detect_orphan_close", string(ps.period), true,
				"close in progress after restart; manual intervention required")
		}
	}
	return nil
}

// applyRec 把一条 WAL 记录应用到内存状态（确定性回放，不做 IO）。
func (s *Service) applyRec(r rec) (*periodState, error) {
	switch r.Kind {
	case evEntry:
		var e Entry
		if err := unmarshalBody(r, &e); err != nil {
			return nil, err
		}
		ps := s.getOrCreate(e.Period)
		ps.entries = append(ps.entries, e)
		s.keys[e.IdempotencyKey] = e.Period
		return ps, nil
	case evAdjustment:
		var a Adjustment
		if err := unmarshalBody(r, &a); err != nil {
			return nil, err
		}
		ps := s.getOrCreate(a.Period)
		ps.adjustments = append(ps.adjustments, a)
		return ps, nil
	case evRule:
		var ru Rule
		if err := unmarshalBody(r, &ru); err != nil {
			return nil, err
		}
		if err := s.rules.Publish(ru); err != nil {
			return nil, err
		}
		return nil, nil
	case evCloseStart:
		var in closeIntent
		if err := unmarshalBody(r, &in); err != nil {
			return nil, err
		}
		ps := s.getOrCreate(in.Period)
		ps.status = StatusClosing
		cp := in
		ps.intent = &cp
		return ps, nil
	case evCloseCommit:
		var cv CloseVersion
		if err := unmarshalBody(r, &cv); err != nil {
			return nil, err
		}
		ps := s.getOrCreate(cv.Period)
		cp := cv
		ps.current = &cp
		ps.history = append(ps.history, &cp)
		ps.status = StatusClosed
		ps.intent = nil
		return ps, nil
	case evCloseAbort:
		var in closeIntent
		if err := unmarshalBody(r, &in); err != nil {
			return nil, err
		}
		ps := s.getOrCreate(in.Period)
		ps.intent = nil
		if len(ps.history) > 0 {
			ps.status = StatusReopened
		} else {
			ps.status = StatusOpen
		}
		return ps, nil
	case evReopen:
		var b reopenBody
		if err := unmarshalBody(r, &b); err != nil {
			return nil, err
		}
		ps := s.getOrCreate(b.Period)
		ps.status = StatusReopened
		return ps, nil
	default:
		return nil, fmt.Errorf("%w: unknown wal event %q", ErrInvalidArgument, r.Kind)
	}
}

func unmarshalBody(r rec, v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("%w: corrupt %s record", ErrInvalidArgument, r.Kind)
	}
	return nil
}

// seedRules 在规则簿为空时发布初始规则（同时写 WAL，保证重启一致）。
func (s *Service) seedRules() {
	if len(s.rules.All()) > 0 {
		return
	}
	seed := Rule{
		Version:     1,
		Name:        "default",
		EffectiveAt: time.Time{}, // 零值 = 自始生效
		PublishedBy: "system",
		PublishedAt: time.Now().UTC(),
	}
	if len(s.cfg.DefaultRules) > 0 {
		for _, r := range s.cfg.DefaultRules {
			_ = s.persistRule(r)
		}
		return
	}
	_ = s.persistRule(seed)
}

// ---------- 摄入 ----------

// Ingest 摄入明细：幂等键去重；只接受未结账期间；结账中/已结账/已重开一律拒绝。
func (s *Service) Ingest(ctx context.Context, u User, e Entry) error {
	if !s.auth.Authorize(u, PermIngest) {
		return s.deny(u, "ingest", string(e.Period))
	}
	if s.isClosed() {
		return ErrClosed
	}
	if e.IdempotencyKey == "" || e.Period == "" {
		return ErrInvalidArgument
	}
	e.ReceivedAt = time.Now().UTC()

	// 全局幂等：先占位，杜绝并发/乱序/迟到的重复计入。
	s.mu.Lock()
	if _, dup := s.keys[e.IdempotencyKey]; dup {
		s.mu.Unlock()
		s.audit(u.Name, "ingest_duplicate", e.IdempotencyKey, true,
			"duplicate idempotency key ignored")
		return ErrDuplicateEntry
	}
	s.keys[e.IdempotencyKey] = e.Period
	ps := s.getOrCreateLocked(e.Period)
	s.mu.Unlock()

	ps.lock.Lock()
	defer ps.lock.Unlock()

	switch ps.status {
	case StatusClosed:
		s.releaseKey(e.IdempotencyKey)
		s.audit(u.Name, "ingest_rejected", string(e.Period), true, "period already closed")
		return ErrPeriodClosed
	case StatusClosing:
		s.releaseKey(e.IdempotencyKey)
		return ErrPeriodClosing
	case StatusReopened:
		// 已重开期间只能追加更正，不能再进普通明细。
		s.releaseKey(e.IdempotencyKey)
		s.audit(u.Name, "ingest_rejected", string(e.Period), true,
			"period reopened; adjustments only")
		return ErrNotReopened
	}

	if err := s.appendRec(evEntry, e); err != nil {
		s.releaseKey(e.IdempotencyKey)
		return err
	}
	ps.entries = append(ps.entries, e)
	s.audit(u.Name, "ingest", e.IdempotencyKey, true, "")
	return nil
}

// ---------- 结账 ----------

// ClosePeriod 原子结账。成功后期间恰好处于一个完整已提交版本；
// 失败（含中途崩溃）不会留下半个版本。
func (s *Service) ClosePeriod(ctx context.Context, u User, p Period, at time.Time) (*CloseVersion, error) {
	if !s.auth.Authorize(u, PermClose) {
		return nil, s.deny(u, "close", string(p))
	}
	if s.isClosed() {
		return nil, ErrClosed
	}
	if p == "" {
		return nil, ErrInvalidArgument
	}
	ps := s.getOrCreate(p)
	if !lockPeriod(&ps.lock, s.cfg.CloseTimeout) {
		return nil, ErrPeriodClosing
	}
	defer ps.lock.Unlock()

	if ps.status == StatusClosed {
		return nil, ErrPeriodClosed
	}
	if ps.status == StatusClosing {
		return nil, ErrPeriodClosing
	}
	// 未结账期间必须按最新生效规则结账；规则版本在意图阶段就固定。
	rule, err := s.rules.EffectiveAt(at)
	if err != nil {
		return nil, err
	}
	intent := closeIntent{Period: p, RuleVersion: rule.Version, At: at.UTC()}

	// 阶段 1：持久化结账意图 -> 期间进入 closing（读取方仍只看到旧的完整版本）。
	if err := s.appendRec(evCloseStart, intent); err != nil {
		return nil, err
	}
	ps.status = StatusClosing
	ps.intent = &intent

	// 阶段 2：计算并提交。任何失败都回滚意图，期间恢复 open/reopened。
	cv, err := s.computeVersion(ps, rule, u.Name, at)
	if err != nil {
		s.rollbackIntent(ps, "close-compute-failed", u.Name)
		return nil, err
	}
	if err := s.appendRec(evCloseCommit, cv); err != nil {
		s.rollbackIntent(ps, "close-commit-failed", u.Name)
		return nil, err
	}
	ps.current = cv
	ps.history = append(ps.history, cv)
	ps.status = StatusClosed
	ps.intent = nil

	s.checkpoint(cv.LastSeq)
	s.audit(u.Name, "close", string(p), true,
		fmt.Sprintf("committed version=%d rule=%d", cv.Version, cv.RuleVersion))
	return cloneVersion(cv), nil
}

// AbortClose 放弃一个进行中的结账（权限：reopen），用于人工干预。
func (s *Service) AbortClose(ctx context.Context, u User, p Period, reason string) error {
	if !s.auth.Authorize(u, PermReopen) {
		return s.deny(u, "abort_close", string(p))
	}
	if s.isClosed() {
		return ErrClosed
	}
	ps, ok := s.lookup(p)
	if !ok {
		return ErrNotFound
	}
	ps.lock.Lock()
	defer ps.lock.Unlock()
	if ps.status != StatusClosing {
		return ErrPeriodNotClosed
	}
	s.rollbackIntent(ps, reason, u.Name)
	s.audit(u.Name, "abort_close", string(p), true, reason)
	return nil
}

// rollbackIntent 必须在持有 ps 期间锁时调用（恢复路径除外，那时单线程）。
func (s *Service) rollbackIntent(ps *periodState, reason, actor string) {
	if ps.intent != nil {
		_ = s.appendRec(evCloseAbort, *ps.intent)
	}
	ps.intent = nil
	if len(ps.history) > 0 {
		ps.status = StatusReopened
	} else {
		ps.status = StatusOpen
	}
}

// computeVersion 基于期间内全部明细与追加更正做确定性全量计算。
// 重开后的重新结账也走这里，因此结果天然与"按全量口径重算"对齐。
func (s *Service) computeVersion(ps *periodState, rule Rule, actor string, at time.Time) (*CloseVersion, error) {
	ents := make([]Entry, len(ps.entries))
	copy(ents, ps.entries)
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].OccurredAt.Equal(ents[j].OccurredAt) {
			return ents[i].IdempotencyKey < ents[j].IdempotencyKey
		}
		return ents[i].OccurredAt.Before(ents[j].OccurredAt)
	})
	adjs := make([]Adjustment, len(ps.adjustments))
	copy(adjs, ps.adjustments)
	sort.Slice(adjs, func(i, j int) bool { return adjs[i].ID < adjs[j].ID })

	bal := Balances{}
	keys := make([]string, 0, len(ents))
	for _, e := range ents {
		bal[e.Category] += e.Amount
		keys = append(keys, e.IdempotencyKey)
	}
	adjIDs := make([]string, 0, len(adjs))
	for _, a := range adjs {
		bal[a.Category] += a.Amount // 调整以追加更正方式叠加，不改写历史
		adjIDs = append(adjIDs, a.ID)
	}

	version := 1
	if len(ps.history) > 0 {
		version = ps.history[len(ps.history)-1].Version + 1
	}
	lastSeq := s.currentSeq()
	return &CloseVersion{
		Period:        ps.period,
		Version:       version,
		RuleVersion:   rule.Version,
		Balances:      bal,
		Filing:        applyFiling(rule, bal),
		EntryKeys:     keys,
		AdjustmentIDs: adjIDs,
		LastSeq:       lastSeq,
		ClosedAt:      at.UTC(),
		ClosedBy:      actor,
	}, nil
}

// ---------- 重开与调整 ----------

// Reopen 重开已结账期间。前置条件：期间当前为 closed；调用方需 reopen 权限。
// 已提交版本保留在历史中不受影响；期间只能通过 AppendAdjustment 追加更正。
func (s *Service) Reopen(ctx context.Context, u User, p Period, reason string) error {
	if !s.auth.Authorize(u, PermReopen) {
		return s.deny(u, "reopen", string(p))
	}
	if s.isClosed() {
		return ErrClosed
	}
	ps, ok := s.lookup(p)
	if !ok {
		return ErrNotFound
	}
	ps.lock.Lock()
	defer ps.lock.Unlock()
	if ps.status != StatusClosed {
		return ErrPeriodNotClosed
	}
	body := reopenBody{Period: p, Reason: reason, Actor: u.Name, At: time.Now().UTC()}
	if err := s.appendRec(evReopen, body); err != nil {
		return err
	}
	ps.status = StatusReopened
	s.audit(u.Name, "reopen", string(p), true, reason)
	return nil
}

// AppendAdjustment 在已重开期间上追加更正（不得改写或删除既有明细/版本）。
func (s *Service) AppendAdjustment(ctx context.Context, u User, a Adjustment) (*Adjustment, error) {
	if !s.auth.Authorize(u, PermAdjust) {
		return nil, s.deny(u, "adjust", string(a.Period))
	}
	if s.isClosed() {
		return nil, ErrClosed
	}
	if a.Period == "" {
		return nil, ErrInvalidArgument
	}
	ps, ok := s.lookup(a.Period)
	if !ok {
		return nil, ErrNotFound
	}
	ps.lock.Lock()
	defer ps.lock.Unlock()
	if ps.status != StatusReopened {
		return nil, ErrNotReopened
	}
	if a.ID == "" {
		a.ID = fmt.Sprintf("adj-%d", len(ps.adjustments)+1)
	} else {
		for _, ex := range ps.adjustments {
			if ex.ID == a.ID {
				return nil, ErrDuplicateEntry
			}
		}
	}
	a.Author = u.Name
	a.CreatedAt = time.Now().UTC()
	if err := s.appendRec(evAdjustment, a); err != nil {
		return nil, err
	}
	ps.adjustments = append(ps.adjustments, a)
	s.audit(u.Name, "adjust", a.ID, true, a.Reason)
	cp := a
	return &cp, nil
}

// ---------- 规则 ----------

// PublishRule 发布新版本口径规则。规则发布后不可变；新版本只影响之后的结账，
// 已结账期间的版本里固定了 RuleVersion，结果永远不变。
func (s *Service) PublishRule(ctx context.Context, u User, r Rule) error {
	if !s.auth.Authorize(u, PermRule) {
		return s.deny(u, "publish_rule", fmt.Sprintf("v%d", r.Version))
	}
	if s.isClosed() {
		return ErrClosed
	}
	if r.Version <= 0 || r.Name == "" {
		return ErrInvalidArgument
	}
	if _, err := s.rules.Version(r.Version); err == nil {
		return ErrRuleExists
	}
	r.PublishedBy = u.Name
	r.PublishedAt = time.Now().UTC()
	if err := s.persistRule(r); err != nil {
		return err
	}
	s.audit(u.Name, "publish_rule", fmt.Sprintf("v%d", r.Version), true,
		fmt.Sprintf("effective_at=%s rate=%dbp", r.EffectiveAt.Format(time.RFC3339), r.TaxRateBasisPoints))
	return nil
}

func (s *Service) persistRule(r Rule) error {
	if s.w != nil {
		if err := s.appendRec(evRule, r); err != nil {
			return err
		}
	}
	return s.rules.Publish(r)
}

// EffectiveRule 返回 t 时刻生效的规则（只读）。
func (s *Service) EffectiveRule(ctx context.Context, u User, t time.Time) (Rule, error) {
	if !s.auth.Authorize(u, PermRead) {
		return Rule{}, s.deny(u, "read", "rule")
	}
	return s.rules.EffectiveAt(t)
}

// ---------- 读取 ----------

// GetPeriod 返回期间的自洽完整视图；结账进行中只会看到旧版本或无版本，
// 绝不会看到字段级混合的半成品。
func (s *Service) GetPeriod(ctx context.Context, u User, p Period) (*PeriodView, error) {
	if !s.auth.Authorize(u, PermRead) {
		return nil, s.deny(u, "read", string(p))
	}
	ps, ok := s.lookup(p)
	if !ok {
		return nil, ErrNotFound
	}
	return s.view(ps), nil
}

// ListPeriods 列出全部期间视图。
func (s *Service) ListPeriods(ctx context.Context, u User) ([]*PeriodView, error) {
	if !s.auth.Authorize(u, PermRead) {
		return nil, s.deny(u, "read", "*")
	}
	s.mu.RLock()
	pss := make([]*periodState, 0, len(s.periods))
	for _, ps := range s.periods {
		pss = append(pss, ps)
	}
	s.mu.RUnlock()
	sort.Slice(pss, func(i, j int) bool { return pss[i].period < pss[j].period })
	out := make([]*PeriodView, 0, len(pss))
	for _, ps := range pss {
		out = append(out, s.view(ps))
	}
	return out, nil
}

func (s *Service) view(ps *periodState) *PeriodView {
	ps.lock.Lock()
	defer ps.lock.Unlock()
	v := &PeriodView{
		Period:          ps.period,
		Status:          ps.status,
		EntryCount:      len(ps.entries),
		AdjustmentCount: len(ps.adjustments),
	}
	if ps.current != nil {
		v.Current = cloneVersion(ps.current)
	}
	if len(ps.history) > 0 {
		v.History = make([]*CloseVersion, len(ps.history))
		for i, h := range ps.history {
			v.History[i] = cloneVersion(h)
		}
	}
	return v
}

// ---------- 审计 ----------

// AuditEvents 返回 seq 之后的审计记录（需要 audit 权限）。
func (s *Service) AuditEvents(ctx context.Context, u User, sinceSeq int64) ([]AuditEvent, error) {
	if !s.auth.Authorize(u, PermAudit) {
		return nil, s.deny(u, "audit", "*")
	}
	return s.aud.Since(sinceSeq), nil
}

func (s *Service) audit(actor, action, target string, allowed bool, reason string) {
	ev := AuditEvent{Actor: actor, Action: action, Target: target, Allowed: allowed, Reason: reason}
	_, _ = s.aud.Append(ev)
}

func (s *Service) deny(u User, action, target string) error {
	s.audit(u.Name, action, target, false, "permission denied")
	return ErrPermissionDenied
}

// ---------- 内部工具 ----------

func (s *Service) appendRec(kind string, v any) error {
	if s.w == nil {
		return nil
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	_, err := s.w.append(kind, v)
	return err
}

func (s *Service) currentSeq() int64 {
	if s.w == nil {
		return 0
	}
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	return s.w.seq
}

// checkpoint 尽力写一份原子快照（检查点）；恢复仍以完整 WAL 为准，
// 因此快照写失败不影响正确性。
func (s *Service) checkpoint(lastSeq int64) {
	if s.w == nil {
		return
	}
	_ = writeSnapshot(s.cfg.DataDir, snapshot{LastSeq: lastSeq, State: map[string]any{
		"committed_at": time.Now().UTC().Format(time.RFC3339),
	}})
}

func (s *Service) getOrCreate(p Period) *periodState {
	s.mu.RLock()
	ps, ok := s.periods[p]
	s.mu.RUnlock()
	if ok {
		return ps
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getOrCreateLocked(p)
}

func (s *Service) getOrCreateLocked(p Period) *periodState {
	if ps, ok := s.periods[p]; ok {
		return ps
	}
	ps := &periodState{period: p, status: StatusOpen}
	s.periods[p] = ps
	return ps
}

func (s *Service) lookup(p Period) (*periodState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps, ok := s.periods[p]
	return ps, ok
}

func (s *Service) releaseKey(k string) {
	s.mu.Lock()
	delete(s.keys, k)
	s.mu.Unlock()
}

func (s *Service) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// lockPeriod 尝试在 timeout 内获取期间锁；timeout<=0 表示阻塞直到获取。
func lockPeriod(mu *sync.Mutex, timeout time.Duration) bool {
	if timeout <= 0 {
		mu.Lock()
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if mu.TryLock() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return mu.TryLock()
}

func cloneVersion(cv *CloseVersion) *CloseVersion {
	cp := *cv
	cp.Balances = Balances{}
	for k, v := range cv.Balances {
		cp.Balances[k] = v
	}
	cp.Filing.ByCategory = map[string]int64{}
	for k, v := range cv.Filing.ByCategory {
		cp.Filing.ByCategory[k] = v
	}
	cp.EntryKeys = append([]string(nil), cv.EntryKeys...)
	cp.AdjustmentIDs = append([]string(nil), cv.AdjustmentIDs...)
	return &cp
}

// Close 落盘并关闭。
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.closed = true
	s.mu.Unlock()

	var err error
	if s.w != nil {
		err = s.w.close()
	}
	if e := s.aud.Close(); err == nil {
		err = e
	}
	return err
}

var _ io.Closer = (*Service)(nil)
