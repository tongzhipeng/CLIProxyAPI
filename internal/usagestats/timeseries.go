package usagestats

import (
	"fmt"
	"strings"
	"time"
)

const (
	maxHourlyRangeHours = 31 * 24 // 31 days max for hourly step
	maxDailyRangeDays   = 90      // 90 days max for daily step

	RangeModeBucket = "bucket"
	RangeModeExact  = "exact"
)

// Filter specifies optional filter dimensions for queries.
type Filter struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Account  string `json:"account,omitempty"`
}

// TimeseriesBucket holds token and request counts for one time interval.
type TimeseriesBucket struct {
	Timestamp                string `json:"timestamp"`
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

// QueryTimeseries aggregates usage stats into continuous, zero-filled time buckets
// using the legacy bucket range mode.
func QueryTimeseries(dir string, from, to time.Time, step string, filter Filter) ([]TimeseriesBucket, error) {
	return QueryTimeseriesWithOptions(dir, from, to, step, filter, RangeModeBucket)
}

// QueryTimeseriesWithOptions aggregates usage stats into continuous, zero-filled time buckets
// with support for range_mode ("bucket" or "exact").
func QueryTimeseriesWithOptions(dir string, from, to time.Time, step string, filter Filter, rangeMode string) ([]TimeseriesBucket, error) {
	rangeMode = strings.ToLower(strings.TrimSpace(rangeMode))
	if rangeMode == "" {
		rangeMode = RangeModeBucket
	}
	if rangeMode != RangeModeBucket && rangeMode != RangeModeExact {
		return nil, fmt.Errorf("usagestats: invalid range_mode %q, expected %s or %s", rangeMode, RangeModeBucket, RangeModeExact)
	}

	if rangeMode == RangeModeExact {
		if from.After(to) {
			return nil, fmt.Errorf("usagestats: from must not be after to")
		}
	} else {
		if to.Before(from) {
			from, to = to, from
		}
	}

	loc := from.Location()
	step = strings.ToLower(strings.TrimSpace(step))
	if step != "day" {
		step = "hour"
	}

	if rangeMode == RangeModeExact {
		span := to.Sub(from)
		if step == "hour" && span > maxHourlyRangeHours*time.Hour {
			return nil, fmt.Errorf("usagestats: hourly query range exceeds %d hours", maxHourlyRangeHours)
		}
		if step == "day" && span > maxDailyRangeDays*24*time.Hour {
			return nil, fmt.Errorf("usagestats: daily query range exceeds %d days", maxDailyRangeDays)
		}
	}

	var buckets []TimeseriesBucket
	var bucketIndex map[string]int
	var effectiveStart, effectiveEnd time.Time

	if step == "hour" {
		start := from.Truncate(time.Hour)
		end := to.Truncate(time.Hour)
		effectiveStart = start
		effectiveEnd = end.Add(time.Hour)
		hours := int(end.Sub(start) / time.Hour)
		if rangeMode == RangeModeBucket && hours > maxHourlyRangeHours {
			return nil, fmt.Errorf("usagestats: hourly query range exceeds %d hours", maxHourlyRangeHours)
		}
		bucketCount := hours + 1
		buckets = make([]TimeseriesBucket, 0, bucketCount)
		bucketIndex = make(map[string]int, bucketCount)
		for t := start; !t.After(end); t = t.Add(time.Hour) {
			tsStr := t.Format(time.RFC3339)
			bucketIndex[tsStr] = len(buckets)
			buckets = append(buckets, TimeseriesBucket{Timestamp: tsStr})
		}
	} else {
		start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
		end := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, loc)
		effectiveStart = start
		effectiveEnd = end.AddDate(0, 0, 1)
		days := int(end.Sub(start) / (24 * time.Hour))
		if rangeMode == RangeModeBucket && days > maxDailyRangeDays {
			return nil, fmt.Errorf("usagestats: daily query range exceeds %d days", maxDailyRangeDays)
		}
		bucketCount := days + 1
		buckets = make([]TimeseriesBucket, 0, bucketCount)
		bucketIndex = make(map[string]int, bucketCount)
		for t := start; !t.After(end); t = t.AddDate(0, 0, 1) {
			tsStr := t.Format(time.RFC3339)
			bucketIndex[tsStr] = len(buckets)
			buckets = append(buckets, TimeseriesBucket{Timestamp: tsStr})
		}
	}

	utcStartDay := effectiveStart.UTC().Truncate(24 * time.Hour)
	utcEndDay := effectiveEnd.UTC().Truncate(24 * time.Hour)

	providerFilter := strings.TrimSpace(filter.Provider)
	modelFilter := strings.TrimSpace(filter.Model)
	accountFilter := CleanAccount(filter.Account)

	errScan := forEachEntry(dir, utcStartDay, utcEndDay, func(day time.Time, e entry) bool {
		if rangeMode == RangeModeExact {
			if e.Timestamp.Before(from) || e.Timestamp.After(to) {
				return false
			}
		} else {
			if e.Timestamp.Before(effectiveStart) || !e.Timestamp.Before(effectiveEnd) {
				return false
			}
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

		var bKey string
		if step == "hour" {
			bKey = e.Timestamp.In(loc).Truncate(time.Hour).Format(time.RFC3339)
		} else {
			tLoc := e.Timestamp.In(loc)
			bKey = time.Date(tLoc.Year(), tLoc.Month(), tLoc.Day(), 0, 0, 0, 0, loc).Format(time.RFC3339)
		}

		idx, found := bucketIndex[bKey]
		if !found {
			return false
		}

		b := &buckets[idx]
		b.Records++
		if e.Failed {
			b.Failures++
		}
		b.UncachedInputTokens += e.TokenBreakdown.Input.UncachedTokens
		b.CacheReadTokens += e.TokenBreakdown.Input.CacheReadTokens
		b.CacheWriteTokens += e.TokenBreakdown.Input.CacheWriteTokens
		b.NonReasoningOutputTokens += e.TokenBreakdown.Output.NonReasoningTokens
		b.ReasoningTokens += e.TokenBreakdown.Output.ReasoningTokens
		b.TotalTokens += e.TokenBreakdown.TotalTokens

		if e.LatencyMs > 0 {
			b.LatencyMsSum += e.LatencyMs
			b.LatencySamples++
		}
		if e.TtftMs > 0 {
			b.TtftMsSum += e.TtftMs
			b.TtftSamples++
		}
		return false
	})
	if errScan != nil {
		return nil, errScan
	}

	return buckets, nil
}
