package periodclose

import (
	"sort"
	"sync"
	"time"
)

// ruleBook holds every rule version. Selection: among rules whose
// EffectiveFrom <= at, the one with the greatest EffectiveFrom wins; ties
// are broken by the greater Version. Rules added later still govern any
// period that has not yet closed (selection happens at close time), while
// already-closed periods pin and keep their RuleVersion.
type ruleBook struct {
	mu    sync.RWMutex
	rules []Rule
}

func newRuleBook() *ruleBook { return &ruleBook{} }

// add registers a rule version. It must carry a strictly increasing
// non-zero Version and a finite EffectiveFrom.
func (b *ruleBook) add(r Rule) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Version <= 0 {
		return ErrBadArgument
	}
	for _, ex := range b.rules {
		if ex.Version == r.Version {
			return ErrBadArgument
		}
	}
	b.rules = append(b.rules, r)
	return nil
}

// all returns a copy ordered by (EffectiveFrom, Version).
func (b *ruleBook) allLocked() []Rule {
	out := make([]Rule, len(b.rules))
	copy(out, b.rules)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].EffectiveFrom.Equal(out[j].EffectiveFrom) {
			return out[i].EffectiveFrom.Before(out[j].EffectiveFrom)
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// effectiveAt returns the rule governing closes performed at time t.
func (b *ruleBook) effectiveAt(t time.Time) (Rule, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var best Rule
	found := false
	for _, r := range b.allLocked() {
		if r.EffectiveFrom.After(t) {
			continue
		}
		if !found || r.EffectiveFrom.After(best.EffectiveFrom) ||
			(r.EffectiveFrom.Equal(best.EffectiveFrom) && r.Version > best.Version) {
			best, found = r, true
		}
	}
	return best, found
}

// get returns a specific pinned version (used when re-reading closed reports).
func (b *ruleBook) get(version int64) (Rule, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, r := range b.rules {
		if r.Version == version {
			return r, true
		}
	}
	return Rule{}, false
}

// latest returns the highest version number regardless of effective time.
func (b *ruleBook) latest() (Rule, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var best Rule
	found := false
	for _, r := range b.rules {
		if !found || r.Version > best.Version {
			best, found = r, true
		}
	}
	return best, found
}

// snapshot returns a defensive copy of all registered rules.
func (b *ruleBook) snapshot() []Rule {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Rule, len(b.rules))
	copy(out, b.rules)
	return out
}

// validateNewRule normalizes Version (0 -> next) and checks uniqueness
// without registering the rule, so callers can validate before commit.
func validateNewRule(b *ruleBook, r Rule) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ex := range b.rules {
		if ex.Version == r.Version {
			return ErrBadArgument
		}
	}
	return nil
}
