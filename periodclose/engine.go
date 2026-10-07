package periodclose

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// calcLine is one normalized input line (original entry or correction).
type calcLine struct {
	seq      int64
	category string // reported category after remap
	amount   int64  // signed after applying the rule sign
	raw      int64  // signed amount before sign, after remap
}

// normalize turns entries+adjustments into ordered, rule-shaped lines.
func normalize(entries []StoredEntry, adjustments []Adjustment, rule Rule) []calcLine {
	lines := make([]calcLine, 0, len(entries)+len(adjustments))
	allowed := rule.includeSet()
	for _, e := range entries {
		rep := mapCategory(e.Category, rule.CategoryMap)
		if allowed != nil && !allowed[rep] {
			continue
		}
		lines = append(lines, calcLine{
			seq:      e.Seq,
			category: rep,
			raw:      e.Amount,
			amount:   e.Amount * signFor(rep, rule.Sign),
		})
	}
	for _, a := range adjustments {
		rep := mapCategory(a.Category, rule.CategoryMap)
		if allowed != nil && !allowed[rep] {
			continue
		}
		lines = append(lines, calcLine{
			seq:      a.Seq,
			category: rep,
			raw:      a.Amount,
			amount:   a.Amount * signFor(rep, rule.Sign),
		})
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].seq < lines[j].seq })
	return lines
}

func (r Rule) includeSet() map[string]bool {
	if len(r.IncludeCategories) == 0 {
		return nil // nil means "all"
	}
	m := make(map[string]bool, len(r.IncludeCategories))
	for _, c := range r.IncludeCategories {
		m[c] = true
	}
	return m
}

func mapCategory(src string, m map[string]string) string {
	if dst, ok := m[src]; ok && dst != "" {
		return dst
	}
	return src
}

func signFor(category string, m map[string]int64) int64 {
	if s, ok := m[category]; ok && s == -1 {
		return -1
	}
	return 1
}

// fingerprint deterministically summarizes the exact line set and rule, so
// a re-close can be proven equal to a from-scratch recomputation.
func fingerprint(p Period, ruleVersion int64, lines []calcLine) string {
	h := sha256.New()
	h.Write([]byte("v1|"))
	h.Write([]byte(string(p)))
	h.Write([]byte("|r"))
	h.Write([]byte(strconv.FormatInt(ruleVersion, 10)))
	h.Write([]byte("\n"))
	for _, l := range lines {
		var b strings.Builder
		b.WriteString(strconv.FormatInt(l.seq, 10))
		b.WriteByte('=')
		b.WriteString(l.category)
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(l.raw, 10))
		b.WriteByte('\n')
		h.Write([]byte(b.String()))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// recalc deterministically recomputes the compliance report for a period
// from the FULL set of accepted lines under the given (pinned) rule.
// It is a pure function: same inputs always yield the same report, which is
// the basis for the "re-close equals full recomputation" guarantee.
func recalc(p Period, entries []StoredEntry, adjustments []Adjustment, rule Rule) Report {
	lines := normalize(entries, adjustments, rule)
	totals := make(map[string]CategoryFigure)
	var net int64
	for _, l := range lines {
		c := totals[l.category]
		c.Category = l.category
		c.Entries++
		c.Amount += l.amount
		totals[l.category] = c
		net += l.amount
	}
	return Report{
		Period:      p,
		RuleVersion: rule.Version,
		Totals:      totals,
		Net:         net,
		Fingerprint: fingerprint(p, rule.Version, lines),
	}
}
