package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagestats"
)

func TestGetUsageBreakdown_ReturnsRowsAndEchoesParams(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	logsDir := t.TempDir()
	statsDir := usagestats.Dir(logsDir)
	if err := os.MkdirAll(statsDir, 0o755); err != nil {
		t.Fatalf("mkdir stats dir: %v", err)
	}
	line := `{"timestamp":"2026-09-18T10:15:00Z","provider":"antigravity","model":"gemini-3.8-flash-high","endpoint":"POST /v1/chat","account":"kevin","failed":false,"status_code":200,"token_breakdown":{"total_tokens":12}}` + "\n"
	if err := os.WriteFile(filepath.Join(statsDir, "usage-2026-09-18.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	h.logDir = logsDir

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-breakdown?dimension=model&from=2026-09-18T00:00:00Z&to=2026-09-18T23:59:59Z", nil)
	h.GetUsageBreakdown(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		From      string                    `json:"from"`
		To        string                    `json:"to"`
		Dimension string                    `json:"dimension"`
		Rows      []usagestats.BreakdownRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Dimension != "model" {
		t.Errorf("dimension = %s, want 'model'", payload.Dimension)
	}
	if len(payload.Rows) != 1 || payload.Rows[0].Key != "gemini-3.8-flash-high" || payload.Rows[0].TotalTokens != 12 {
		t.Errorf("rows = %+v, want 1 row with key gemini-3.8-flash-high and 12 tokens", payload.Rows)
	}

	// Test dimension=account returns cleaned account name
	recAcc := httptest.NewRecorder()
	ginCtxAcc, _ := gin.CreateTestContext(recAcc)
	ginCtxAcc.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-breakdown?dimension=account&from=2026-09-18T00:00:00Z&to=2026-09-18T23:59:59Z", nil)
	h.GetUsageBreakdown(ginCtxAcc)
	if recAcc.Code != http.StatusOK {
		t.Fatalf("account dimension status = %d, want 200", recAcc.Code)
	}
	var payloadAcc struct {
		Dimension string                    `json:"dimension"`
		Rows      []usagestats.BreakdownRow `json:"rows"`
	}
	if err := json.Unmarshal(recAcc.Body.Bytes(), &payloadAcc); err != nil {
		t.Fatalf("decode payloadAcc: %v", err)
	}
	if payloadAcc.Dimension != "account" || len(payloadAcc.Rows) != 1 || payloadAcc.Rows[0].Key != "kevin" {
		t.Errorf("expected 1 row with key 'kevin', got %+v", payloadAcc)
	}

	// Test invalid dimension returns 400
	recBad := httptest.NewRecorder()
	ginCtxBad, _ := gin.CreateTestContext(recBad)
	ginCtxBad.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-breakdown?dimension=invalid", nil)
	h.GetUsageBreakdown(ginCtxBad)
	if recBad.Code != http.StatusBadRequest {
		t.Errorf("invalid dimension status = %d, want 400", recBad.Code)
	}
}

func TestGetUsageTimeseries_RangeMode(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	logsDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	h.logDir = logsDir

	// exact mode returns range_mode in response
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-timeseries?range_mode=exact&from=2026-09-18T10:00:00Z&to=2026-09-18T11:00:00Z", nil)
	h.GetUsageTimeseries(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		RangeMode string `json:"range_mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.RangeMode != "exact" {
		t.Errorf("range_mode = %s, want 'exact'", payload.RangeMode)
	}

	// invalid range_mode returns 400
	recBad := httptest.NewRecorder()
	ginCtxBad, _ := gin.CreateTestContext(recBad)
	ginCtxBad.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-timeseries?range_mode=unknown", nil)
	h.GetUsageTimeseries(ginCtxBad)
	if recBad.Code != http.StatusBadRequest {
		t.Errorf("invalid range_mode status = %d, want 400", recBad.Code)
	}
}

func TestGetUsageRecords_SnapshotLifecycle(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	logsDir := t.TempDir()
	statsDir := usagestats.Dir(logsDir)
	if err := os.MkdirAll(statsDir, 0o755); err != nil {
		t.Fatalf("mkdir stats dir: %v", err)
	}
	line := `{"timestamp":"2026-09-18T10:00:00Z","model":"m1"}` + "\n"
	if err := os.WriteFile(filepath.Join(statsDir, "usage-2026-09-18.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	h.logDir = logsDir

	// 1. snapshot=new&offset=0 creates a snapshot
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-records?snapshot=new&offset=0&from=2026-09-18T00:00:00Z&to=2026-09-18T23:59:59Z", nil)
	h.GetUsageRecords(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.SnapshotID == "" {
		t.Fatalf("expected non-empty snapshot_id in response")
	}

	// 2. snapshot=new with offset > 0 returns 400
	recBad := httptest.NewRecorder()
	ginCtxBad, _ := gin.CreateTestContext(recBad)
	ginCtxBad.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-records?snapshot=new&offset=10", nil)
	h.GetUsageRecords(ginCtxBad)
	if recBad.Code != http.StatusBadRequest {
		t.Errorf("snapshot=new with offset>0 status = %d, want 400", recBad.Code)
	}

	// 3. invalid/expired snapshot returns 409
	recExp := httptest.NewRecorder()
	ginCtxExp, _ := gin.CreateTestContext(recExp)
	ginCtxExp.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-records?snapshot=snap_nonexistent", nil)
	h.GetUsageRecords(ginCtxExp)
	if recExp.Code != http.StatusConflict {
		t.Errorf("invalid snapshot status = %d, want 409 (conflict)", recExp.Code)
	}
}

func TestGetUsage_ModelAndAccountFiltering(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	logsDir := t.TempDir()
	statsDir := usagestats.Dir(logsDir)
	if err := os.MkdirAll(statsDir, 0o755); err != nil {
		t.Fatalf("mkdir stats dir: %v", err)
	}

	// 3 lines:
	// 1. account user-a.json, model gpt-4o, 100 tokens
	// 2. account user-a, model claude-3-7, 200 tokens
	// 3. account user-b.json, model gpt-4o, 400 tokens
	lines := `{"timestamp":"2026-09-18T10:00:00Z","model":"gpt-4o","account":"user-a.json","failed":false,"status_code":200,"token_breakdown":{"total_tokens":100}}` + "\n" +
		`{"timestamp":"2026-09-18T10:30:00Z","model":"claude-3-7","account":"user-a","failed":false,"status_code":200,"token_breakdown":{"total_tokens":200}}` + "\n" +
		`{"timestamp":"2026-09-18T11:00:00Z","model":"gpt-4o","account":"user-b.json","failed":false,"status_code":200,"token_breakdown":{"total_tokens":400}}` + "\n"

	if err := os.WriteFile(filepath.Join(statsDir, "usage-2026-09-18.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	h.logDir = logsDir

	// 1. Breakdown dimension=model filtered by account=user-a (should match both user-a.json and user-a)
	recModel := httptest.NewRecorder()
	ctxModel, _ := gin.CreateTestContext(recModel)
	ctxModel.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-breakdown?dimension=model&account=user-a&from=2026-09-18T00:00:00Z&to=2026-09-18T23:59:59Z", nil)
	h.GetUsageBreakdown(ctxModel)
	if recModel.Code != http.StatusOK {
		t.Fatalf("breakdown status = %d, want 200", recModel.Code)
	}
	var resModel struct {
		Rows []usagestats.BreakdownRow `json:"rows"`
	}
	if err := json.Unmarshal(recModel.Body.Bytes(), &resModel); err != nil {
		t.Fatalf("decode resModel: %v", err)
	}
	// user-a used both gpt-4o (100) and claude-3-7 (200) -> total 300 tokens across 2 rows
	if len(resModel.Rows) != 2 {
		t.Fatalf("expected 2 model rows for user-a, got %d", len(resModel.Rows))
	}

	// 2. Breakdown dimension=account filtered by model=gpt-4o
	recAcc := httptest.NewRecorder()
	ctxAcc, _ := gin.CreateTestContext(recAcc)
	ctxAcc.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-breakdown?dimension=account&model=gpt-4o&from=2026-09-18T00:00:00Z&to=2026-09-18T23:59:59Z", nil)
	h.GetUsageBreakdown(ctxAcc)
	if recAcc.Code != http.StatusOK {
		t.Fatalf("breakdown account status = %d, want 200", recAcc.Code)
	}
	var resAcc struct {
		Rows []usagestats.BreakdownRow `json:"rows"`
	}
	if err := json.Unmarshal(recAcc.Body.Bytes(), &resAcc); err != nil {
		t.Fatalf("decode resAcc: %v", err)
	}
	// gpt-4o was used by user-a (100) and user-b (400)
	if len(resAcc.Rows) != 2 {
		t.Fatalf("expected 2 account rows for gpt-4o, got %d", len(resAcc.Rows))
	}
	for _, r := range resAcc.Rows {
		if r.Key != "user-a" && r.Key != "user-b" {
			t.Errorf("unexpected account key: %s (should be cleaned without .json)", r.Key)
		}
	}

	// 3. Timeseries filtered by both model=gpt-4o and account=user-a
	recTs := httptest.NewRecorder()
	ctxTs, _ := gin.CreateTestContext(recTs)
	ctxTs.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-timeseries?model=gpt-4o&account=user-a&from=2026-09-18T10:00:00Z&to=2026-09-18T11:00:00Z&range_mode=exact", nil)
	h.GetUsageTimeseries(ctxTs)
	if recTs.Code != http.StatusOK {
		t.Fatalf("timeseries status = %d, want 200", recTs.Code)
	}
	var resTs struct {
		Buckets []usagestats.TimeseriesBucket `json:"buckets"`
	}
	if err := json.Unmarshal(recTs.Body.Bytes(), &resTs); err != nil {
		t.Fatalf("decode resTs: %v", err)
	}
	totalTokens := 0
	for _, b := range resTs.Buckets {
		totalTokens += int(b.TotalTokens)
	}
	if totalTokens != 100 {
		t.Errorf("expected 100 tokens for gpt-4o + user-a, got %d", totalTokens)
	}

	// 4. Records filtered by model=gpt-4o and account=user-b.json (with .json in query parameter)
	recRec := httptest.NewRecorder()
	ctxRec, _ := gin.CreateTestContext(recRec)
	ctxRec.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-records?model=gpt-4o&account=user-b.json&from=2026-09-18T00:00:00Z&to=2026-09-18T23:59:59Z", nil)
	h.GetUsageRecords(ctxRec)
	if recRec.Code != http.StatusOK {
		t.Fatalf("records status = %d, want 200", recRec.Code)
	}
	var resRec struct {
		Records []usagestats.UsageRecord `json:"records"`
	}
	if err := json.Unmarshal(recRec.Body.Bytes(), &resRec); err != nil {
		t.Fatalf("decode resRec: %v", err)
	}
	if len(resRec.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(resRec.Records))
	}
	if resRec.Records[0].Account != "user-b" {
		t.Errorf("expected cleaned account 'user-b', got %q", resRec.Records[0].Account)
	}
}
