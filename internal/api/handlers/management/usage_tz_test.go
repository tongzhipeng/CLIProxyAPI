package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestGetUsageTimeseries_PreservesLocalOffset verifies that a percent-encoded
// +08:00 offset in the from/to query params (as sent by the frontend, e.g.
// "2026-09-21T00:00:00.000%2B08:00") round-trips through parseUsageTime and
// is echoed back in the response with the +08:00 offset preserved, not
// silently normalized to UTC/Z.
func TestGetUsageTimeseries_PreservesLocalOffset(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	logsDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	h.logDir = logsDir

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet,
		"/v0/management/usage-timeseries?range_mode=exact&step=hour"+
			"&from=2026-09-21T00:00:00.000%2B08:00&to=2026-09-21T12:00:00.000%2B08:00", nil)
	h.GetUsageTimeseries(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	if got := payload.From; len(got) < 6 || got[len(got)-6:] != "+08:00" {
		t.Errorf("from = %q, want it to preserve +08:00 offset (not Z)", got)
	}
	if got := payload.To; len(got) < 6 || got[len(got)-6:] != "+08:00" {
		t.Errorf("to = %q, want it to preserve +08:00 offset (not Z)", got)
	}
}
