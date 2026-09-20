package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// newAntigravityCountTokensUpstream mimics the observed behaviour of the real
// /v1internal:countTokens endpoint: it only tokenizes text parts found under
// request.contents and silently ignores request.systemInstruction and
// request.tools. One whitespace-separated word counts as one token.
func newAntigravityCountTokensUpstream(t *testing.T, captured *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != antigravityCountTokensPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, antigravityCountTokensPath)
		}
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read countTokens body: %v", errRead)
		}
		*captured = append([]byte(nil), body...)
		total := 0
		for _, content := range gjson.GetBytes(body, "request.contents").Array() {
			for _, part := range content.Get("parts").Array() {
				total += len(strings.Fields(part.Get("text").String()))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if total == 0 {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"totalTokens":` + strconv.Itoa(total) + `}`))
	}))
}

func TestAntigravityCountTokensCountsSystemPrompt(t *testing.T) {
	var upstreamBody []byte
	server := newAntigravityCountTokensUpstream(t, &upstreamBody)
	defer server.Close()

	// system = 4 words, user message = 1 word → a correct count is 5, not 1.
	payload := []byte(`{"model":"gemini-3.8-flash-high","system":"alpha beta gamma delta","messages":[{"role":"user","content":"hi"}]}`)
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	resp, errCount := exec.CountTokens(context.Background(), testAntigravityAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.8-flash-high",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		ResponseFormat:  sdktranslator.FormatClaude,
		OriginalRequest: payload,
	})
	if errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got != 5 {
		t.Fatalf("input_tokens = %d, want 5 (system prompt must be counted); upstream body=%s", got, upstreamBody)
	}
	if gjson.GetBytes(upstreamBody, "request.systemInstruction").Exists() {
		t.Fatalf("upstream countTokens body still carries request.systemInstruction, which upstream ignores: %s", upstreamBody)
	}
}

func TestAntigravityCountTokensCountsToolDeclarations(t *testing.T) {
	var upstreamBody []byte
	server := newAntigravityCountTokensUpstream(t, &upstreamBody)
	defer server.Close()

	payload := []byte(`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"read_file","description":"Read one file from disk","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}`)
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	resp, errCount := exec.CountTokens(context.Background(), testAntigravityAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.8-flash-high",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		ResponseFormat:  sdktranslator.FormatClaude,
		OriginalRequest: payload,
	})
	if errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got <= 1 {
		t.Fatalf("input_tokens = %d, want > 1 (tool declarations must contribute); upstream body=%s", got, upstreamBody)
	}
	if !strings.Contains(gjson.GetBytes(upstreamBody, "request.contents").Raw, "read_file") {
		t.Fatalf("tool declaration was not folded into request.contents: %s", upstreamBody)
	}
	if gjson.GetBytes(upstreamBody, "request.tools").Exists() {
		t.Fatalf("upstream countTokens body still carries request.tools, which upstream ignores: %s", upstreamBody)
	}
}

func TestAntigravityCountTokensWithoutSystemOrToolsIsUnchanged(t *testing.T) {
	var upstreamBody []byte
	server := newAntigravityCountTokensUpstream(t, &upstreamBody)
	defer server.Close()

	payload := []byte(`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"one two three"}]}`)
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	resp, errCount := exec.CountTokens(context.Background(), testAntigravityAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.8-flash-high",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		ResponseFormat:  sdktranslator.FormatClaude,
		OriginalRequest: payload,
	})
	if errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got != 3 {
		t.Fatalf("input_tokens = %d, want 3; upstream body=%s", got, upstreamBody)
	}
	if n := len(gjson.GetBytes(upstreamBody, "request.contents").Array()); n != 1 {
		t.Fatalf("request.contents length = %d, want 1 (no synthetic turn without system/tools); body=%s", n, upstreamBody)
	}
}
