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
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", id, base.Unix())))
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

		workers.Go(func() error {
			if ctx.Err() != nil {
				return nil
			}
			quotas, err := s.fetchAntigravityQuotas(ctx, cfg, a, token, now)
			entry := antigravityQuotaRefreshEntry{next: now.Add(time.Minute), epoch: a.RegistrationEpoch, tokenHash: tokenHash}
			if err == nil && ctx.Err() == nil && s.coreManager.ObserveAntigravityQuotas(a, quotas, now) {
				entry.next = now.Add(antigravityQuotaRefreshInterval)
				for _, quota := range quotas {
					if reset, errParse := time.Parse(time.RFC3339, quota.Signals["reset_time"]); errParse == nil && reset.After(now) {
						target := reset.Add(antigravityQuotaJitterFunc(a.ID, reset))
						if target.Before(entry.next) {
							entry.next = target
						}
					}
				}
				if weekly, ok := firstWeeklyResetTimeForAuth(a); ok && weekly.After(now) {
					target := weekly.Add(antigravityQuotaJitterFunc(a.ID, weekly))
					if target.Before(entry.next) {
						entry.next = target
					}
				}
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
	// retrieveUserQuota is an account endpoint on production by default.
	// Explicit base URLs still support private gateways and local test servers.
	baseURL := resolveAntigravityModelBaseURL(a)
	if baseURL == "" {
		baseURL = antigravityModelBaseURLProd
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1internal:retrieveUserQuota", strings.NewReader(`{}`))
	if err != nil {
		return nil, fmt.Errorf("invalid quota endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.AntigravityUserAgent())
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
