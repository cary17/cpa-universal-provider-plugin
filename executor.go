package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}
type hostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}
type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}
type streamEmit struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}
type streamClose struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	s := loadedState()
	if s == nil {
		return nil, fmt.Errorf("插件尚未配置")
	}
	up, err := prepareUpstream(s, &req)
	if err != nil {
		return rpcError("invalid_request", err.Error(), 400)
	}
	var resp pluginapi.HTTPResponse
	if err := callHost(pluginabi.MethodHostHTTPDo, up, &resp); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return rpcError("upstream_error", safeUpstreamError(resp.StatusCode, resp.Body), resp.StatusCode)
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers})
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.StreamID == "" {
		return nil, fmt.Errorf("stream_id 不能为空")
	}
	s := loadedState()
	if s == nil {
		return nil, fmt.Errorf("插件尚未配置")
	}
	up, err := prepareUpstream(s, &req)
	if err != nil {
		return rpcError("invalid_request", err.Error(), 400)
	}
	var resp hostHTTPStreamResponse
	if err := callHost(pluginabi.MethodHostHTTPDoStream, up, &resp); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = callHost(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": resp.StreamID}, nil)
		return rpcError("upstream_error", fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode), resp.StatusCode)
	}
	go forwardStream(resp.StreamID, req.StreamID)
	return okEnvelope(struct {
		Headers http.Header `json:"headers,omitempty"`
	}{Headers: resp.Headers})
}

func forwardStream(upstreamID, downstreamID string) {
	var finalErr string
	defer func() {
		_ = callHost(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": upstreamID}, nil)
		_ = callHost(pluginabi.MethodHostStreamClose, streamClose{StreamID: downstreamID, Error: finalErr}, nil)
	}()
	for {
		var chunk hostHTTPStreamReadResponse
		if err := callHost(pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": upstreamID}, &chunk); err != nil {
			finalErr = err.Error()
			return
		}
		if chunk.Error != "" {
			finalErr = chunk.Error
			return
		}
		if len(chunk.Payload) > 0 {
			if err := callHost(pluginabi.MethodHostStreamEmit, streamEmit{StreamID: downstreamID, Payload: chunk.Payload}, nil); err != nil {
				finalErr = err.Error()
				return
			}
		}
		if chunk.Done {
			return
		}
	}
}

func executorHTTPRequest(raw []byte) ([]byte, error) {
	var req struct {
		pluginapi.ExecutorHTTPRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	var resp pluginapi.HTTPResponse
	if err := callHost(pluginabi.MethodHostHTTPDo, hostHTTPRequest{HostCallbackID: req.HostCallbackID, Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body}, &resp); err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body})
}

func prepareUpstream(s *runtimeState, req *rpcExecutorRequest) (hostHTTPRequest, error) {
	m, ok := s.nativeModel(req.Model)
	if !ok {
		return hostHTTPRequest{}, fmt.Errorf("未配置模型 %q", req.Model)
	}
	if !s.Config.ImageGeneration && clearlyImageGeneration(req.Payload) {
		return hostHTTPRequest{}, fmt.Errorf("配置已禁用图像生成")
	}
	cred := s.pickCredential()
	if cred == nil {
		return hostHTTPRequest{}, fmt.Errorf("无可用 API key")
	}
	body, err := rewritePayload(req.Payload, m.Name, s.Config.Protocol, s.Config.ReasoningEffort, req.Stream)
	if err != nil {
		return hostHTTPRequest{}, err
	}
	headers := cloneHeader(req.Headers)
	for k, v := range s.Config.Headers {
		headers.Set(k, v)
	}
	headers.Set("Content-Type", "application/json")
	applyAuth(headers, s.Config.Protocol, cred.APIKey)
	return hostHTTPRequest{HostCallbackID: req.HostCallbackID, Method: http.MethodPost, URL: upstreamURL(s.Config.BaseURL, s.Config.Protocol, m.Name, req.Stream), Headers: headers, Body: body}, nil
}
func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}
func applyAuth(h http.Header, protocol, key string) {
	switch protocol {
	case "claude":
		// Authorization normally contains the CPA frontend credential. It is not
		// an Anthropic upstream credential and must never be forwarded.
		h.Del("Authorization")
		h.Del("x-goog-api-key")
		h.Set("x-api-key", key)
		if h.Get("anthropic-version") == "" {
			h.Set("anthropic-version", "2023-06-01")
		}
	case "gemini":
		h.Del("Authorization")
		h.Del("x-api-key")
		h.Set("x-goog-api-key", key)
	default:
		h.Del("x-api-key")
		h.Del("x-goog-api-key")
		h.Set("Authorization", "Bearer "+key)
	}
}
func upstreamURL(base, protocol, model string, stream bool) string {
	switch protocol {
	case "openai":
		return base + "/chat/completions"
	case "openai-response":
		return base + "/responses"
	case "claude":
		return base + "/messages"
	case "gemini":
		method := "generateContent"
		if stream {
			method = "streamGenerateContent"
		}
		endpoint := base + "/models/" + url.PathEscape(model) + ":" + method
		if stream {
			endpoint += "?alt=sse"
		}
		return endpoint
	}
	return base
}

func rewritePayload(raw []byte, model, protocol, effort string, stream bool) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象: %w", err)
	}
	root["model"] = model
	if protocol == "gemini" {
		delete(root, "model")
	}
	if protocol == "openai" || protocol == "openai-response" {
		root["stream"] = stream
	}
	if effort != "auto" {
		switch protocol {
		case "openai":
			root["reasoning_effort"] = effort
		case "openai-response":
			o, _ := root["reasoning"].(map[string]any)
			if o == nil {
				o = map[string]any{}
			}
			o["effort"] = effort
			root["reasoning"] = o
		case "claude":
			if effort == "none" {
				delete(root, "thinking")
			} else {
				root["thinking"] = claudeThinking(effort)
			}
		case "gemini":
			g, _ := root["generationConfig"].(map[string]any)
			if g == nil {
				g = map[string]any{}
			}
			t, _ := g["thinkingConfig"].(map[string]any)
			if t == nil {
				t = map[string]any{}
			}
			t["thinkingLevel"] = geminiThinking(effort)
			g["thinkingConfig"] = t
			root["generationConfig"] = g
		}
	}
	return json.Marshal(root)
}
func claudeThinking(e string) map[string]any {
	switch e {
	case "minimal", "low":
		return map[string]any{"type": "enabled", "budget_tokens": 1024}
	case "medium":
		return map[string]any{"type": "enabled", "budget_tokens": 4096}
	case "high":
		return map[string]any{"type": "enabled", "budget_tokens": 8192}
	case "xhigh":
		return map[string]any{"type": "enabled", "budget_tokens": 16384}
	}
	return map[string]any{"type": "adaptive"}
}
func geminiThinking(e string) string {
	if e == "none" {
		return "minimal"
	}
	if e == "xhigh" {
		return "high"
	}
	return e
}
func safeUpstreamError(status int, body []byte) string {
	var v map[string]any
	if json.Unmarshal(body, &v) == nil {
		if e, ok := v["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok && m != "" {
				return fmt.Sprintf("上游 HTTP %d: %s", status, m)
			}
		}
	}
	return fmt.Sprintf("上游返回 HTTP %d", status)
}
