package usagestats

import (
	"fmt"
	"sort"
	"time"
)

// maxQueryRangeDays bounds QueryDaily so an unbounded from/to cannot make it
// open tens of thousands of files.
const maxQueryRangeDays = 400

// DailyModelStats is one aggregated (date, provider, model, account) bucket.
type DailyModelStats struct {
	Date     string `json:"date"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Account  string `json:"account"`

	// Records counts published usage records, not HTTP requests: a single
	// request can emit more than one record (e.g. additional-model reporting).
	Records  int64 `json:"records"`
	Failures int64 `json:"failures"`

	// Token fields mirror usage.TokenBreakdown's mutually-exclusive buckets
	// (see sdk/cliproxy/usage/accounting.go) so summing across providers never
	// double-counts input tokens that are also reported as cached.
	UncachedInputTokens      int64 `json:"uncached_input_tokens"`
	CacheReadTokens          int64 `json:"cache_read_tokens"`
	CacheWriteTokens         int64 `json:"cache_write_tokens"`
	NonReasoningOutputTokens int64 `json:"non_reasoning_output_tokens"`
	ReasoningTokens          int64 `json:"reasoning_tokens"`
	TotalTokens              int64 `json:"total_tokens"`
}

type statsKey struct {
	date     string
	provider string
	model    string
	account  string
}

// QueryDaily aggregates persisted usage-stats records under dir whose UTC date
// falls within [from, to] (inclusive, day granularity). A missing day file is
// treated as zero records for that day, not an error. A single malformed line
// within a file is skipped so it cannot break aggregation of the rest.
func QueryDaily(dir string, from, to time.Time) ([]DailyModelStats, error) {
	from = from.UTC().Truncate(24 * time.Hour)
	to = to.UTC().Truncate(24 * time.Hour)
	if to.Before(from) {
		from, to = to, from
	}
	if to.Sub(from) > maxQueryRangeDays*24*time.Hour {
		return nil, fmt.Errorf("usagestats: query range exceeds %d days", maxQueryRangeDays)
	}

	agg := make(map[statsKey]*DailyModelStats)

	err := forEachEntry(dir, from, to, func(day time.Time, e entry) bool {
		addEntry(agg, day.Format(dateLayout), e)
		return false
	})
	if err != nil {
		return nil, err
	}

	out := make([]DailyModelStats, 0, len(agg))
	for _, s := range agg {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Account < out[j].Account
	})
	return out, nil
}

func addEntry(agg map[statsKey]*DailyModelStats, dateStr string, e entry) {
	k := statsKey{date: dateStr, provider: e.Provider, model: e.Model, account: e.Account}
	s, ok := agg[k]
	if !ok {
		s = &DailyModelStats{Date: dateStr, Provider: e.Provider, Model: e.Model, Account: e.Account}
		agg[k] = s
	}
	s.Records++
	if e.Failed {
		s.Failures++
	}
	s.UncachedInputTokens += e.TokenBreakdown.Input.UncachedTokens
	s.CacheReadTokens += e.TokenBreakdown.Input.CacheReadTokens
	s.CacheWriteTokens += e.TokenBreakdown.Input.CacheWriteTokens
	s.NonReasoningOutputTokens += e.TokenBreakdown.Output.NonReasoningTokens
	s.ReasoningTokens += e.TokenBreakdown.Output.ReasoningTokens
	s.TotalTokens += e.TokenBreakdown.TotalTokens
}
