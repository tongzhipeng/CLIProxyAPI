package usagestats

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const maxBreakdownRangeDays = 90

// BreakdownRow represents aggregated metrics grouped by a specific dimension.
type BreakdownRow struct {
	Key                      string `json:"key"`
	Records                  int64  `json:"records"`
	Failures                 int64  `json:"failures"`
	UncachedInputTokens      int64  `json:"uncached_input_tokens"`
	CacheReadTokens          int64  `json:"cache_read_tokens"`
	CacheWriteTokens         int64  `json:"cache_write_tokens"`
	NonReasoningOutputTokens int64  `json:"non_reasoning_output_tokens"`
	ReasoningTokens          int64  `json:"reasoning_tokens"`
	TotalTokens              int64  `json:"total_tokens"`
	LatencyMsSum             int64  `json:"latency_ms_sum"`
	LatencySamples           int64  `json:"latency_samples"`
	TtftMsSum                int64  `json:"ttft_ms_sum"`
	TtftSamples              int64  `json:"ttft_samples"`
}

// QueryBreakdown aggregates metrics grouped by dimension ("model", "endpoint", "provider", or "account")
// strictly within the closed time window [from, to].
func QueryBreakdown(dir string, from, to time.Time, dimension string, filter Filter) ([]BreakdownRow, error) {
	dim := strings.ToLower(strings.TrimSpace(dimension))
	switch dim {
	case "model", "endpoint", "provider", "account":
	default:
		return nil, fmt.Errorf("usagestats: invalid dimension %q, expected model, endpoint, provider, or account", dimension)
	}

	if from.After(to) {
		return nil, fmt.Errorf("usagestats: from must not be after to")
	}

	if to.Sub(from) > maxBreakdownRangeDays*24*time.Hour {
		return nil, fmt.Errorf("usagestats: breakdown query range exceeds %d days", maxBreakdownRangeDays)
	}

	utcStartDay := from.UTC().Truncate(24 * time.Hour)
	utcEndDay := to.UTC().Truncate(24 * time.Hour)

	providerFilter := strings.TrimSpace(filter.Provider)
	modelFilter := strings.TrimSpace(filter.Model)
	accountFilter := CleanAccount(filter.Account)

	agg := make(map[string]*BreakdownRow)

	errScan := forEachEntry(dir, utcStartDay, utcEndDay, func(day time.Time, e entry) bool {
		if e.Timestamp.Before(from) || e.Timestamp.After(to) {
			return false
		}

		if providerFilter != "" && e.Provider != providerFilter {
			return false
		}
		if modelFilter != "" && e.Model != modelFilter {
			return false
		}
		if accountFilter != "" && CleanAccount(e.Account) != accountFilter {
			return false
		}

		var key string
		switch dim {
		case "model":
			key = e.Model
		case "endpoint":
			key = e.Endpoint
		case "provider":
			key = e.Provider
		case "account":
			key = CleanAccount(e.Account)
		}

		row, exists := agg[key]
		if !exists {
			row = &BreakdownRow{Key: key}
			agg[key] = row
		}

		row.Records++
		if e.Failed {
			row.Failures++
		}
		row.UncachedInputTokens += e.TokenBreakdown.Input.UncachedTokens
		row.CacheReadTokens += e.TokenBreakdown.Input.CacheReadTokens
		row.CacheWriteTokens += e.TokenBreakdown.Input.CacheWriteTokens
		row.NonReasoningOutputTokens += e.TokenBreakdown.Output.NonReasoningTokens
		row.ReasoningTokens += e.TokenBreakdown.Output.ReasoningTokens
		row.TotalTokens += e.TokenBreakdown.TotalTokens

		if e.LatencyMs > 0 {
			row.LatencyMsSum += e.LatencyMs
			row.LatencySamples++
		}
		if e.TtftMs > 0 {
			row.TtftMsSum += e.TtftMs
			row.TtftSamples++
		}

		return false
	})
	if errScan != nil {
		return nil, errScan
	}

	out := make([]BreakdownRow, 0, len(agg))
	for _, r := range agg {
		out = append(out, *r)
	}

	// Sort: TotalTokens DESC -> Records DESC -> Key ASC
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Records != out[j].Records {
			return out[i].Records > out[j].Records
		}
		return out[i].Key < out[j].Key
	})

	return out, nil
}
