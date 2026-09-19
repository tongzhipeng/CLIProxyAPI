package usagestats

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanAccount trims leading and trailing whitespace and strips any trailing ".json" extension.
func CleanAccount(account string) string {
	trimmed := strings.TrimSpace(account)
	return strings.TrimSuffix(trimmed, ".json")
}

const (
	scanInitBufSize = 64 * 1024
	scanMaxBufSize  = 1024 * 1024
)

// forEachEntry iterates over usage-YYYY-MM-DD.jsonl files for each UTC day from
// startDay to endDay (inclusive). Missing files are skipped. Empty lines and
// malformed JSON lines are skipped. If an I/O error or line exceeding 1MiB occurs,
// the error is returned immediately. If visit returns true, iteration stops early.
func forEachEntry(dir string, startDay, endDay time.Time, visit func(day time.Time, e entry) bool) error {
	start := startDay.UTC().Truncate(24 * time.Hour)
	end := endDay.UTC().Truncate(24 * time.Hour)
	if end.Before(start) {
		start, end = end, start
	}

	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		dateStr := day.Format(dateLayout)
		path := filepath.Join(dir, "usage-"+dateStr+".jsonl")

		f, errOpen := os.Open(path)
		if errOpen != nil {
			if os.IsNotExist(errOpen) {
				continue
			}
			return fmt.Errorf("usagestats: open %s: %w", path, errOpen)
		}

		stop := false
		errScan := func() error {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 0, scanInitBufSize), scanMaxBufSize)

			for scanner.Scan() {
				line := scanner.Bytes()
				if len(line) == 0 {
					continue
				}
				var e entry
				if errUnmarshal := json.Unmarshal(line, &e); errUnmarshal != nil {
					continue
				}
				e.Account = CleanAccount(e.Account)
				if visit(day, e) {
					stop = true
					return nil
				}
			}
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("usagestats: scan %s: %w", path, err)
			}
			return nil
		}()

		if errScan != nil {
			return errScan
		}
		if stop {
			return nil
		}
	}
	return nil
}

// MetricsAccumulator captures token and request counts, latency, and TTFT.
type MetricsAccumulator struct {
	Records                  int64 `json:"records"`
	Failures                 int64 `json:"failures"`
	UncachedInputTokens      int64 `json:"uncached_input_tokens"`
	CacheReadTokens          int64 `json:"cache_read_tokens"`
	CacheWriteTokens         int64 `json:"cache_write_tokens"`
	NonReasoningOutputTokens int64 `json:"non_reasoning_output_tokens"`
	ReasoningTokens          int64 `json:"reasoning_tokens"`
	TotalTokens              int64 `json:"total_tokens"`
	LatencyMsSum             int64 `json:"latency_ms_sum"`
	LatencySamples           int64 `json:"latency_samples"`
	TtftMsSum                int64 `json:"ttft_ms_sum"`
	TtftSamples              int64 `json:"ttft_samples"`
}

// Accumulate aggregates metrics from a single entry into the accumulator.
func (m *MetricsAccumulator) Accumulate(e entry) {
	m.Records++
	if e.Failed {
		m.Failures++
	}
	m.UncachedInputTokens += e.TokenBreakdown.Input.UncachedTokens
	m.CacheReadTokens += e.TokenBreakdown.Input.CacheReadTokens
	m.CacheWriteTokens += e.TokenBreakdown.Input.CacheWriteTokens
	m.NonReasoningOutputTokens += e.TokenBreakdown.Output.NonReasoningTokens
	m.ReasoningTokens += e.TokenBreakdown.Output.ReasoningTokens
	m.TotalTokens += e.TokenBreakdown.TotalTokens

	if e.LatencyMs > 0 {
		m.LatencyMsSum += e.LatencyMs
		m.LatencySamples++
	}
	if e.TtftMs > 0 {
		m.TtftMsSum += e.TtftMs
		m.TtftSamples++
	}
}
