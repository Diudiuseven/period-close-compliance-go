package compliance

import (
	"sort"
	"time"
)

// RuleBook 管理口径规则的版本化与生效时间。
//
// 语义：
//   - 版本号唯一且单调；发布后规则不可变（没有更新/删除接口）。
//   - EffectiveAt(t) 返回满足 EffectiveAt <= t 中版本号最大（即最新发布）的规则。
//   - 不允许发布 EffectiveAt 早于"历史上已存在的更晚生效时间"造成的二义性
//     这里采取简单确定的口径：按 (EffectiveAt, Version) 决定生效规则，
//     即同一时刻有多条规则时取版本号最大者，结果永远可解释。
type RuleBook struct {
	rules map[int]Rule
}

// NewRuleBook 创建空规则簿。
func NewRuleBook() *RuleBook {
	return &RuleBook{rules: map[int]Rule{}}
}

// Publish 发布一个新版本。版本号必须为正且唯一。
func (b *RuleBook) Publish(r Rule) error {
	if r.Version <= 0 {
		return ErrInvalidArgument
	}
	if r.Name == "" {
		return ErrInvalidArgument
	}
	if r.TaxRateBasisPoints < 0 || r.TaxRateBasisPoints > 10000 {
		return ErrInvalidArgument
	}
	if _, ok := b.rules[r.Version]; ok {
		return ErrRuleExists
	}
	b.rules[r.Version] = r
	return nil
}

// EffectiveAt 返回 t 时刻生效的规则：
// 在所有 EffectiveAt <= t 的规则中取版本号最大者。
func (b *RuleBook) EffectiveAt(t time.Time) (Rule, error) {
	best := Rule{Version: 0}
	found := false
	for _, r := range b.rules {
		if !r.EffectiveAt.After(t) && (!found || r.Version > best.Version) {
			best, found = r, true
		}
	}
	if !found {
		return Rule{}, ErrNoEffectiveRule
	}
	return best, nil
}

// Latest 返回版本号最大的规则。
func (b *RuleBook) Latest() (Rule, error) {
	best := Rule{Version: 0}
	found := false
	for _, r := range b.rules {
		if !found || r.Version > best.Version {
			best, found = r, true
		}
	}
	if !found {
		return Rule{}, ErrNoEffectiveRule
	}
	return best, nil
}

// Version 取指定版本规则。
func (b *RuleBook) Version(v int) (Rule, error) {
	r, ok := b.rules[v]
	if !ok {
		return Rule{}, ErrNotFound
	}
	return r, nil
}

// All 返回按版本号升序排列的全部规则。
func (b *RuleBook) All() []Rule {
	out := make([]Rule, 0, len(b.rules))
	for _, r := range b.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// applyFiling 按规则 r 对分类余额计算合规申报口径：
//   - Gross（收入总额）= 非抵扣分类的余额合计（允许因更正为负）；
//   - Deductible（可抵扣额）= 抵扣分类的正余额合计；
//   - Taxable = max(Gross - Deductible, 0)；
//   - TaxDue = Taxable * 税率（基点）/10000。
func applyFiling(r Rule, bal Balances) FilingResult {
	res := FilingResult{
		RuleVersion: r.Version,
		ByCategory:  map[string]int64{},
	}
	deductible := map[string]bool{}
	for _, c := range r.DeductibleCategories {
		deductible[c] = true
	}
	var gross, ded int64
	for cat, amt := range bal {
		res.ByCategory[cat] = amt
		if deductible[cat] {
			if amt > 0 {
				ded += amt
			}
		} else {
			gross += amt
		}
	}
	res.Gross = gross
	res.Deductible = ded
	res.Taxable = gross - ded
	if res.Taxable < 0 {
		res.Taxable = 0
	}
	res.TaxDue = res.Taxable * int64(r.TaxRateBasisPoints) / 10000
	return res
}
