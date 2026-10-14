package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const antigravityQuotaRefreshInterval = 5 * time.Minute

var antigravityQuotaJitterFunc = defaultAntigravityQuotaJitter

func defaultAntigravityQuotaJitter(id string, base time.Time) time.Duration {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", id, base.UnixNano())))
	secs := 60 + int(h[0])%120 // 60s ~ 179s (1 to 3 minutes)
	return time.Duration(secs) * time.Second
}

type antigravityQuotaRefreshEntry struct {
	next      time.Time
	epoch     uint64
	tokenHash [32]byte
}

// runAntigravityQuotaRefresh keeps probes off the request path. Strategy changes
// and newly loaded credentials are picked up on the next minute tick.
func (s *Service) runAntigravityQuotaRefresh(ctx context.Context, ticks <-chan time.Time) {
	next := make(map[string]antigravityQuotaRefreshEntry)
	s.refreshAntigravityQuotas(ctx, next, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			s.refreshAntigravityQuotas(ctx, next, now)
		}
	}
}

func (s *Service) refreshAntigravityQuotas(ctx context.Context, next map[string]antigravityQuotaRefreshEntry, now time.Time) {
	if s == nil || s.coreManager == nil || ctx.Err() != nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	enabled := cfg != nil && !cfg.Home.Enabled && normalizedRoutingRuntimeState(cfg).strategy == "quota-reset-first"
	s.cfgMu.RUnlock()
	if !enabled {
		clear(next)
		return
	}
	active := make(map[string]struct{})
	type probeResult struct {
		id    string
		entry antigravityQuotaRefreshEntry
	}
	auths := s.coreManager.List()
	results := make(chan probeResult, len(auths))
	var workers errgroup.Group
	workers.SetLimit(4)
	for _, a := range auths {
		if ctx.Err() != nil {
			break
		}
		if a.Disabled || a.Status == coreauth.StatusDisabled || !strings.EqualFold(a.Provider, "antigravity") {
			continue
		}
		token, _ := a.Metadata["access_token"].(string)
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		active[a.ID] = struct{}{}
		tokenHash := sha256.Sum256([]byte(token))
		entry := next[a.ID]
		if entry.epoch == a.RegistrationEpoch && entry.tokenHash == tokenHash && now.Before(entry.next) {
			continue
		}
		// Refreshable accounts must not be probed proactively on a periodic timer tick.
		// They are only probed when an incoming user request discovers the refreshable state.
		if entry.epoch == a.RegistrationEpoch && coreauth.IsAuthRefreshable(a, now) {
			continue
		}

		workers.Go(func() error {
			if ctx.Err() != nil {
				return nil
			}
			quotas, err := s.fetchAntigravityQuotas(ctx, cfg, a, token, now)
			entry := antigravityQuotaRefreshEntry{next: now.Add(time.Minute), epoch: a.RegistrationEpoch, tokenHash: tokenHash}
			if err == nil && ctx.Err() == nil && s.coreManager.ObserveAntigravityQuotas(a, quotas, now) {
				entry.next = now.Add(antigravityQuotaRefreshInterval)
			} else if err != nil && ctx.Err() == nil {
				// Errors contain status/parse details only, never the upstream body or credentials.
				log.WithField("auth_id", a.ID).Debugf("antigravity quota probe failed: %v", err)
			}
			results <- probeResult{id: a.ID, entry: entry}
			return nil
		})
	}
	_ = workers.Wait()
	close(results)
	for result := range results {
		next[result.id] = result.entry
	}

	for id := range next {
		if _, ok := active[id]; !ok {
			delete(next, id)
		}
	}
}

func (s *Service) fetchAntigravityQuotas(ctx context.Context, cfg *config.Config, a *coreauth.Auth, token string, now time.Time) (map[string]coreauth.QuotaState, error) {
	baseURL := resolveAntigravityModelBaseURL(a)
	if baseURL == "" {
		baseURL = antigravityModelBaseURLProd
	}
	proxyURL := a.ProxyURL
	if proxyURL == "" && cfg != nil {
		proxyURL = cfg.ProxyURL
	}
	transport, _, err := proxyutil.BuildHTTPTransport(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid quota proxy configuration")
	}
	client := &http.Client{}
	if transport != nil {
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}

	var (
		quotas        map[string]coreauth.QuotaState
		summaryQuotas map[string]coreauth.QuotaState
		errQuota      error
		errSummary    error
		g             errgroup.Group
	)

	projectID := antigravityProjectID(a)

	g.Go(func() error {
		quotas, errQuota = s.fetchAntigravityModelBuckets(ctx, client, baseURL, token, now)
		return nil
	})
	g.Go(func() error {
		summaryQuotas, errSummary = s.fetchAntigravityQuotaSummary(ctx, client, baseURL, token, projectID, now)
		return nil
	})
	_ = g.Wait()

	if errQuota != nil {
		return nil, errQuota
	}
	if quotas == nil {
		quotas = make(map[string]coreauth.QuotaState)
	}
	if errSummary == nil && len(summaryQuotas) > 0 {
		mergeAntigravityQuotaSummary(quotas, summaryQuotas)
	}
	return quotas, nil
}

func antigravityProjectID(a *coreauth.Auth) string {
	if a == nil {
		return ""
	}
	if a.Metadata != nil {
		if pid, ok := a.Metadata["project_id"].(string); ok && strings.TrimSpace(pid) != "" {
			return strings.TrimSpace(pid)
		}
	}
	if a.Attributes != nil {
		if pid := strings.TrimSpace(a.Attributes["project_id"]); pid != "" {
			return pid
		}
	}
	return ""
}

func (s *Service) fetchAntigravityModelBuckets(ctx context.Context, client *http.Client, baseURL, token string, now time.Time) (map[string]coreauth.QuotaState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1internal:retrieveUserQuota", strings.NewReader(`{}`))
	if err != nil {
		return nil, fmt.Errorf("invalid quota endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.AntigravityUserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota request failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("antigravity quota: close response body failed")
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("quota HTTP status %d", resp.StatusCode)
	}
	const maxBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return nil, fmt.Errorf("invalid quota response body")
	}
	return parseAntigravityQuotas(body, now)
}

func (s *Service) fetchAntigravityQuotaSummary(ctx context.Context, client *http.Client, baseURL, token, projectID string, now time.Time) (map[string]coreauth.QuotaState, error) {
	reqPayload := "{}"
	if projectID != "" {
		if b, err := json.Marshal(map[string]string{"project": projectID}); err == nil {
			reqPayload = string(b)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1internal:retrieveUserQuotaSummary", strings.NewReader(reqPayload))
	if err != nil {
		return nil, fmt.Errorf("invalid quota summary endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.AntigravityUserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota summary request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("antigravity quota summary: close response body failed")
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("quota summary HTTP status %d", resp.StatusCode)
	}
	const maxBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return nil, fmt.Errorf("invalid quota summary response body")
	}
	return parseAntigravityQuotaSummary(body, now)
}

func parseAntigravityQuotaSummary(body []byte, now time.Time) (map[string]coreauth.QuotaState, error) {
	var response struct {
		Groups []struct {
			DisplayName string `json:"displayName"`
			Buckets     []struct {
				BucketID          string   `json:"bucketId"`
				Window            string   `json:"window"`
				ResetTime         string   `json:"resetTime"`
				RemainingFraction *float64 `json:"remainingFraction"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Groups == nil || len(response.Groups) > 32 {
		return nil, fmt.Errorf("invalid quota summary response")
	}
	summary := make(map[string]coreauth.QuotaState)
	for _, group := range response.Groups {
		for _, bucket := range group.Buckets {
			if !strings.EqualFold(bucket.Window, "weekly") {
				continue
			}
			resetStr := strings.TrimSpace(bucket.ResetTime)
			if resetStr == "" {
				continue
			}
			resetTime, err := time.Parse(time.RFC3339, resetStr)
			if err != nil {
				continue
			}
			signals := map[string]string{
				"reset_time":            resetTime.UTC().Format(time.RFC3339Nano),
				"weekly_quota_reset_at": resetTime.UTC().Format(time.RFC3339Nano),
			}
			if bucket.RemainingFraction != nil && *bucket.RemainingFraction >= 0 && *bucket.RemainingFraction <= 1 {
				remStr := strconv.FormatFloat(*bucket.RemainingFraction, 'g', -1, 64)
				signals["remaining_fraction"] = remStr
				signals["weekly_remaining_fraction"] = remStr
			}
			bucketID := strings.ToLower(strings.TrimSpace(bucket.BucketID))
			if bucketID == "" {
				bucketID = strings.ToLower(strings.TrimSpace(group.DisplayName))
			}
			summary[bucketID] = coreauth.QuotaState{ObservedAt: now, Signals: signals}
		}
	}
	return summary, nil
}

func mergeAntigravityQuotaSummary(quotas, summaryQuotas map[string]coreauth.QuotaState) {
	var (
		geminiReset   string
		geminiRemain  string
		p3Reset       string
		p3Remain      string
		earliestReset time.Time
		earliestStr   string
	)

	for bucketID, qs := range summaryQuotas {
		resetStr := qs.Signals["weekly_quota_reset_at"]
		if resetStr == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, resetStr)
		if err == nil {
			if earliestReset.IsZero() || t.Before(earliestReset) {
				earliestReset = t
				earliestStr = resetStr
			}
		}
		if strings.Contains(bucketID, "gemini") {
			geminiReset = resetStr
			geminiRemain = qs.Signals["weekly_remaining_fraction"]
		} else if strings.Contains(bucketID, "3p") || strings.Contains(bucketID, "claude") || strings.Contains(bucketID, "gpt") {
			p3Reset = resetStr
			p3Remain = qs.Signals["weekly_remaining_fraction"]
		}
	}

	for model, qs := range quotas {
		modelLower := strings.ToLower(model)
		if strings.HasPrefix(modelLower, "gemini-") || strings.Contains(modelLower, "gemini") {
			if geminiReset != "" {
				if qs.Signals == nil {
					qs.Signals = make(map[string]string)
				}
				qs.Signals["weekly_quota_reset_at"] = geminiReset
				if geminiRemain != "" {
					qs.Signals["weekly_remaining_fraction"] = geminiRemain
				}
				quotas[model] = qs
			}
		} else if strings.HasPrefix(modelLower, "claude-") || strings.HasPrefix(modelLower, "gpt-") || strings.Contains(modelLower, "claude") || strings.Contains(modelLower, "gpt") {
			if p3Reset != "" {
				if qs.Signals == nil {
					qs.Signals = make(map[string]string)
				}
				qs.Signals["weekly_quota_reset_at"] = p3Reset
				if p3Remain != "" {
					qs.Signals["weekly_remaining_fraction"] = p3Remain
				}
				quotas[model] = qs
			}
		} else if earliestStr != "" {
			if qs.Signals == nil {
				qs.Signals = make(map[string]string)
			}
			qs.Signals["weekly_quota_reset_at"] = earliestStr
			quotas[model] = qs
		}
	}

	for k, v := range summaryQuotas {
		quotas[k] = v
	}
}

func parseAntigravityQuotas(body []byte, now time.Time) (map[string]coreauth.QuotaState, error) {
	var response struct {
		Buckets []struct {
			ModelID   string   `json:"modelId"`
			Remaining *float64 `json:"remainingFraction"`
			ResetTime string   `json:"resetTime"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Buckets == nil || len(response.Buckets) > 256 {
		return nil, fmt.Errorf("invalid quota buckets")
	}
	quotas := make(map[string]coreauth.QuotaState, len(response.Buckets))
	for _, bucket := range response.Buckets {
		model := normalizeAntigravityFetchedModelID(bucket.ModelID)
		// Fully unused buckets can omit resetTime; they have no ordering signal yet.
		if strings.TrimSpace(bucket.ResetTime) == "" {
			continue
		}
		reset, err := time.Parse(time.RFC3339, bucket.ResetTime)
		if model == "" || len(model) > 256 || err != nil || (bucket.Remaining != nil && (*bucket.Remaining < 0 || *bucket.Remaining > 1)) {
			return nil, fmt.Errorf("invalid model quota bucket")
		}
		if !reset.After(now) {
			continue
		}
		if previous, ok := quotas[model]; ok {
			previousReset, _ := time.Parse(time.RFC3339, previous.Signals["reset_time"])
			if !reset.Before(previousReset) {
				continue
			}
		}
		signals := map[string]string{"reset_time": reset.UTC().Format(time.RFC3339Nano)}
		if bucket.Remaining != nil {
			signals["remaining_fraction"] = strconv.FormatFloat(*bucket.Remaining, 'g', -1, 64)
		}
		quotas[model] = coreauth.QuotaState{ObservedAt: now, Signals: signals}
	}
	for model, quota := range quotas {
		if strings.HasSuffix(model, "-tiered") {
			base := strings.TrimSuffix(model, "-tiered")
			for _, alias := range []string{base + "-high", base} {
				if _, exists := quotas[alias]; !exists {
					quotas[alias] = quota
				}
			}
		}
	}
	return quotas, nil
}

func firstWeeklyResetTimeForAuth(a *coreauth.Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Quota.Signals != nil {
		if raw := a.Quota.Signals["weekly_quota_reset_at"]; raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				return t, true
			}
		}
	}
	if a.Attributes != nil {
		if raw := a.Attributes["weekly_quota_reset_at"]; raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				return t, true
			}
		}
	}
	if a.Metadata != nil {
		if raw, ok := a.Metadata["weekly_quota_reset_at"].(string); ok && raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func (s *Service) handleRefreshableAuth(a *coreauth.Auth, now time.Time) {
	if s == nil || s.coreManager == nil || a == nil {
		return
	}
	if !strings.EqualFold(a.Provider, "antigravity") {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	enabled := cfg != nil && !cfg.Home.Enabled && normalizedRoutingRuntimeState(cfg).strategy == "quota-reset-first"
	s.cfgMu.RUnlock()
	if !enabled {
		return
	}

	s.antigravityScheduledMu.Lock()
	if s.antigravityScheduled == nil {
		s.antigravityScheduled = make(map[string]time.Time)
	}
	targetTime, exists := s.antigravityScheduled[a.ID]
	if exists && now.Before(targetTime) {
		s.antigravityScheduledMu.Unlock()
		return
	}
	if exists && now.Sub(targetTime) < time.Minute {
		s.antigravityScheduledMu.Unlock()
		return
	}

	delay := antigravityQuotaJitterFunc(a.ID, now)
	targetTime = now.Add(delay)
	s.antigravityScheduled[a.ID] = targetTime
	s.antigravityScheduledMu.Unlock()

	s.antigravityProbeWg.Add(1)
	time.AfterFunc(delay, func() {
		defer s.antigravityProbeWg.Done()
		s.executeScheduledAntigravityProbe(a.ID)
	})
}

func (s *Service) executeScheduledAntigravityProbe(authID string) {
	if s == nil || s.coreManager == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	enabled := cfg != nil && !cfg.Home.Enabled && normalizedRoutingRuntimeState(cfg).strategy == "quota-reset-first"
	s.cfgMu.RUnlock()
	if !enabled {
		return
	}

	a, ok := s.coreManager.GetByID(authID)
	if !ok || a == nil || a.Disabled || a.Status == coreauth.StatusDisabled {
		return
	}
	token, _ := a.Metadata["access_token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}

	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	quotas, err := s.fetchAntigravityQuotas(ctx, cfg, a, token, now)
	if err == nil {
		s.coreManager.ObserveAntigravityQuotas(a, quotas, now)
	} else {
		log.WithField("auth_id", authID).Debugf("antigravity request-triggered quota probe failed: %v", err)
	}
}
