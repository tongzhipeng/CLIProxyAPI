package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CountTokens counts tokens for the given request using the Antigravity API.
func (e *AntigravityExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("antigravity")
	respCtx := context.WithValue(ctx, "alt", opts.Alt)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayloadSource, errValidate := validateAntigravityRequestSignatures(ctx, baseModel, from, originalPayloadSource)
	if errValidate != nil {
		return cliproxyexecutor.Response{}, errValidate
	}
	req.Payload = originalPayloadSource
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return cliproxyexecutor.Response{}, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}
	cliproxyauth.NotifyAccessTokenFingerprint(ctx, auth)
	if strings.TrimSpace(token) == "" {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	// Prepare payload once (doesn't depend on baseURL)
	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, Body: req.Payload, ModelInfo: modelInfo}
	payload := helps.TranslateRequestEnvelopeWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq).Body

	payload, err := helps.ApplyRequestThinking(payload, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	payload = e.obfuscateSensitiveWords(payload)
	payload = sanitizeAntigravityGeminiRequestSignatures(baseModel, payload)
	preparedPayload, _, errReplay := prepareAntigravityGeminiReasoningReplayPayload(ctx, baseModel, req, opts, payload)
	if errReplay != nil {
		return cliproxyexecutor.Response{}, errReplay
	}
	// Fold before the leading-user normalization: the synthetic prompt turn is a
	// user turn, so Gemini targets need no extra empty user turn in front of it.
	payload = foldAntigravityCountTokensPromptIntoContents(preparedPayload)
	payload = ensureAntigravityGeminiLeadingUserContent(baseModel, payload)

	payload = helps.DeleteJSONField(payload, "project")
	payload = helps.DeleteJSONField(payload, "model")
	payload = helps.DeleteJSONField(payload, "request.safetySettings")
	payload = helps.DeleteJSONField(payload, "request.toolConfig")
	payload = helps.DeleteJSONField(payload, "request.labels")
	payload = helps.DeleteJSONField(payload, "request.sessionId")

	base := resolveAntigravityRequestBaseURL(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(antigravityCountTokensPath)
	if opts.Alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(opts.Alt))
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(payload))
	if errReq != nil {
		return cliproxyexecutor.Response{}, errReq
	}
	// No httpReq.Close: keep the shared Antigravity connection pool usable.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       requestURL.String(),
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payload,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return cliproxyexecutor.Response{}, errDo
	}

	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errClose := httpResp.Body.Close(); errClose != nil {
		log.Errorf("antigravity executor: close response body error: %v", errClose)
	}
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return cliproxyexecutor.Response{}, errRead
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)

	if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
		count := gjson.GetBytes(bodyBytes, "totalTokens").Int()
		translated := sdktranslator.TranslateTokenCount(respCtx, to, responseFormat, count, bodyBytes)
		return cliproxyexecutor.Response{Payload: translated, Headers: httpResp.Header.Clone()}, nil
	}

	sErr := statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
	if httpResp.StatusCode == http.StatusTooManyRequests {
		closeAntigravityAuthIdleTransports(auth)
		if retryAfter, parseErr := helps.ParseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
			sErr.retryAfter = retryAfter
		}
	}
	return cliproxyexecutor.Response{}, sErr
}

// foldAntigravityCountTokensPromptIntoContents rewrites the count-only payload so
// the upstream /v1internal:countTokens endpoint sees everything that the
// generate path bills for.
//
// Verified against the real endpoint (2026-09-20): it tokenizes only
// request.contents and silently ignores request.systemInstruction and
// request.tools, so a request with a large system prompt and a short user
// message came back as totalTokens=1. Folding the system parts, and a JSON
// rendering of the tool declarations, into a synthetic leading user turn makes
// the returned count additive with the counted contents (system text counted
// as user text measured +1 token). The tool declarations are an
// approximation: upstream tool-token accounting is not exposed by the endpoint.
func foldAntigravityCountTokensPromptIntoContents(payload []byte) []byte {
	systemParts := gjson.GetBytes(payload, "request.systemInstruction.parts")
	tools := gjson.GetBytes(payload, "request.tools")

	parts := make([]string, 0, 4)
	if systemParts.IsArray() {
		for _, part := range systemParts.Array() {
			parts = append(parts, part.Raw)
		}
	}
	if tools.Exists() {
		toolPart, errTool := sjson.SetBytes([]byte(`{"text":""}`), "text", tools.Raw)
		if errTool == nil {
			parts = append(parts, string(toolPart))
		}
	}
	if len(parts) == 0 {
		return payload
	}

	synthetic := []byte(`{"role":"user","parts":[` + strings.Join(parts, ",") + `]}`)
	contents := gjson.GetBytes(payload, "request.contents")
	items := make([]string, 0, 1+len(contents.Array()))
	items = append(items, string(synthetic))
	if contents.IsArray() {
		for _, content := range contents.Array() {
			items = append(items, content.Raw)
		}
	}
	out, errSet := sjson.SetRawBytes(payload, "request.contents", []byte("["+strings.Join(items, ",")+"]"))
	if errSet != nil {
		return payload
	}
	out = helps.DeleteJSONField(out, "request.systemInstruction")
	out = helps.DeleteJSONField(out, "request.tools")
	return out
}
