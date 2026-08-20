package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func providersYAML() []byte {
	return []byte(`
enabled: true
priority: 17
providers:
  - id: alpha
    name: Alpha
    enabled: true
    protocol: openai
    base-url: https://alpha.example/v1
    headers: {X-Tenant: alpha}
    api-key-entries:
      - {api-key: alpha-a, weight: 2}
      - {api-key: alpha-b, weight: 1}
    models:
      - {name: native-a, alias: shared, output-modalities: [text, image]}
    image-generation: false
    reasoning-effort: high
  - id: beta
    name: Beta
    enabled: true
    protocol: claude
    base-url: https://beta.example/v1
    api-key-entries: [{api-key: beta-a}]
    models: [{name: native-b, alias: other}]
    image-generation: true
    reasoning-effort: auto
`)
}

func TestDecodeProvidersAndLegacyMigration(t *testing.T) {
	s, err := decodeConfig(providersYAML())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Config.Providers) != 2 || s.Config.Enabled != true || s.Config.Priority != 17 {
		t.Fatalf("config=%#v", s.Config)
	}
	if got := s.ByPublicModel["shared"].Provider.ID; got != "alpha" {
		t.Fatalf("provider=%q", got)
	}
	legacy, err := decodeConfig(validYAML())
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy.Config.Providers) != 1 || legacy.Config.Providers[0].ID == "" || legacy.Config.Providers[0].Name == "" {
		t.Fatalf("migration=%#v", legacy.Config.Providers)
	}
}

func TestProvidersValidation(t *testing.T) {
	cases := []struct{ name, old, new string }{
		{"duplicate id", "id: beta", "id: alpha"},
		{"duplicate name", "name: Beta", "name: Alpha"},
		{"duplicate enabled alias", "alias: other", "alias: shared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(strings.Replace(string(providersYAML()), tc.old, tc.new, 1))
			if _, err := decodeConfig(raw); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	raw := []byte(strings.Replace(string(providersYAML()), "enabled: true\n    protocol: claude", "enabled: false\n    protocol: claude", 1))
	raw = []byte(strings.Replace(string(raw), "alias: other", "alias: shared", 1))
	if _, err := decodeConfig(raw); err != nil {
		t.Fatalf("disabled duplicate should be accepted: %v", err)
	}
}

func TestReservedInternalModelPrefixIsRejected(t *testing.T) {
	raw := []byte(strings.Replace(string(providersYAML()), "alias: shared", "alias: "+internalModelPrefix+"public", 1))
	if _, err := decodeConfig(raw); err == nil || !strings.Contains(err.Error(), "保留前缀") {
		t.Fatalf("expected reserved-prefix validation error, got %v", err)
	}
}

func TestRoutePinsProviderWithoutPublicModelLeak(t *testing.T) {
	s, err := decodeConfig(providersYAML())
	if err != nil {
		t.Fatal(err)
	}
	state.Store(s)
	raw, _ := json.Marshal(rpcModelRouteRequest{ModelRouteRequest: pluginapi.ModelRouteRequest{RequestedModel: "shared"}})
	out, err := routeModel(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.ModelRouteResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || !strings.HasPrefix(resp.TargetModel, internalModelPrefix) || strings.Contains(resp.TargetModel, "Alpha") {
		t.Fatalf("route=%#v", resp)
	}
	if _, ok := s.nativeModel(resp.TargetModel); !ok {
		t.Fatalf("executor cannot resolve %q", resp.TargetModel)
	}
	modelsRaw, _ := staticModels()
	if strings.Contains(string(modelsRaw), internalModelPrefix) {
		t.Fatal("internal target leaked in model list")
	}
}

func TestPerProviderSWRRAndPrepareUsesPinnedProvider(t *testing.T) {
	s, err := decodeConfig(providersYAML())
	if err != nil {
		t.Fatal(err)
	}
	binding := s.ByPublicModel["shared"]
	got := []string{}
	for i := 0; i < 3; i++ {
		got = append(got, binding.Provider.pickCredential().ID)
	}
	if !reflect.DeepEqual(got, []string{"alpha-key-1", "alpha-key-2", "alpha-key-1"}) {
		t.Fatalf("SWRR=%v", got)
	}
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: binding.InternalModel, Payload: []byte(`{"model":"shared","messages":[]}`), SourceFormat: "openai"}}
	up, err := prepareUpstream(s, &req)
	if err != nil {
		t.Fatal(err)
	}
	if up.URL != "https://alpha.example/v1/chat/completions" || up.Headers.Get("Authorization") == "" {
		t.Fatalf("upstream=%#v", up)
	}
}

func TestRegistrationAndManagementProvidersResource(t *testing.T) {
	r := pluginRegistration()
	if r.Metadata.Version != "0.1.1" || !r.Capabilities.ManagementAPI {
		t.Fatalf("registration=%#v", r)
	}
	want := []string{"openai"}
	if !reflect.DeepEqual(r.Capabilities.ExecutorInputFormats, want) || !reflect.DeepEqual(r.Capabilities.ExecutorOutputFormats, want) {
		t.Fatalf("formats=%v/%v", r.Capabilities.ExecutorInputFormats, r.Capabilities.ExecutorOutputFormats)
	}
	if len(r.Metadata.ConfigFields) != 1 || r.Metadata.ConfigFields[0].Name != "providers" {
		t.Fatalf("fields=%#v", r.Metadata.ConfigFields)
	}
	out, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "/providers") {
		t.Fatalf("management.register=%s", out)
	}
	req, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/providers"})
	out, err = handleMethod(pluginabi.MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	_ = json.Unmarshal(out, &e)
	var resp pluginapi.ManagementResponse
	_ = json.Unmarshal(e.Result, &resp)
	html := string(resp.Body)
	for _, needle := range []string{"Add provider", "Edit", "Copy", "Enable", "Delete", "confirm(", `type="password"`, "sessionStorage", "/v0/management/plugins/universal-provider/config", "textContent", "priority", "enabled"} {
		if !strings.Contains(html, needle) {
			t.Errorf("HTML missing %q", needle)
		}
	}
	for _, forbidden := range []string{"<script src=", "innerHTML", "localStorage.setItem"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("HTML contains unsafe %q", forbidden)
		}
	}
	if !strings.Contains(html, "Legacy provider") || !strings.Contains(html, "cfg.protocol") {
		t.Fatal("HTML does not migrate legacy singleton config")
	}
}

func TestManagementResourceRejectsMutation(t *testing.T) {
	req, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPut, Path: "/providers", Body: []byte(`{}`)})
	out, err := handleMethod(pluginabi.MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	_ = json.Unmarshal(out, &e)
	var resp pluginapi.ManagementResponse
	_ = json.Unmarshal(e.Result, &resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestCanonicalOpenAIRequestTranslatesToProviderProtocol(t *testing.T) {
	s, err := decodeConfig(providersYAML())
	if err != nil {
		t.Fatal(err)
	}
	binding := s.ByPublicModel["other"]
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: binding.InternalModel, Payload: []byte(`{"model":"other","messages":[]}`), SourceFormat: "openai"}}
	up, err := prepareUpstream(s, &req)
	if err != nil {
		t.Fatal(err)
	}
	if up.URL != "https://beta.example/v1/messages" {
		t.Fatalf("URL=%q", up.URL)
	}
	var body map[string]any
	if err := json.Unmarshal(up.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "native-b" {
		t.Fatalf("translated body=%s", up.Body)
	}
}

func TestNonOpenAIClientUsesCanonicalExecutorFormat(t *testing.T) {
	s, err := decodeConfig(providersYAML())
	if err != nil {
		t.Fatal(err)
	}
	binding := s.ByPublicModel["other"]
	canonical := []byte(`{"model":"other","messages":[{"role":"user","content":"hi"}]}`)
	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:           binding.InternalModel,
		Format:          "openai",
		SourceFormat:    "claude",
		Payload:         canonical,
		OriginalRequest: []byte(`{"model":"other","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`),
	}}
	up, err := prepareUpstream(s, &req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(up.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "native-b" {
		t.Fatalf("canonical payload was parsed using SourceFormat: %s", up.Body)
	}
	response := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"native-b","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	out := translateNonStreamResponse(req.Format, up.TargetFormat, up.UpstreamModel, req.OriginalRequest, up.Body, response)
	var translated map[string]any
	if err := json.Unmarshal(out, &translated); err != nil {
		t.Fatal(err)
	}
	if choices, ok := translated["choices"].([]any); !ok || len(choices) == 0 {
		t.Fatalf("response does not honor canonical OpenAI output: %s", out)
	}
}

func TestClaudeResponsesTranslateBackToCanonicalOpenAI(t *testing.T) {
	original := []byte(`{"model":"other","messages":[{"role":"user","content":"hi"}]}`)
	request := sdktranslator.TranslateRequest(sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "native-b", original, false)
	response := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"native-b","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	out := translateNonStreamResponse("openai", "claude", "other", original, request, response)
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("translated response: %v: %s", err, out)
	}
	if choices, ok := body["choices"].([]any); !ok || len(choices) == 0 {
		t.Fatalf("not OpenAI: %s", out)
	}
	var param any
	// The registered Claude→OpenAI stream translator consumes one `data:` SSE
	// record per call (matching CPA's executor read chunks), without an event line.
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"native-b","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`),
	}
	var frames [][]byte
	for _, chunk := range chunks {
		frames = append(frames, sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "other", original, request, chunk, &param)...)
	}
	if len(frames) == 0 || !strings.Contains(string(bytes.Join(frames, nil)), "hello") {
		t.Fatalf("stream frames=%q", frames)
	}
}

func TestReasoningMaxMappings(t *testing.T) {
	for _, protocol := range []string{"openai", "openai-response", "claude", "gemini"} {
		raw, err := rewritePayload([]byte(`{"model":"old","messages":[]}`), "native", protocol, "max", false)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		switch protocol {
		case "openai":
			if body["reasoning_effort"] != "max" {
				t.Fatalf("%s: %s", protocol, raw)
			}
		case "openai-response":
			if jsonPath(body, "reasoning.effort") != "max" {
				t.Fatalf("%s: %s", protocol, raw)
			}
		case "claude":
			if jsonPath(body, "thinking.budget_tokens") != float64(32768) {
				t.Fatalf("%s: %s", protocol, raw)
			}
		case "gemini":
			if jsonPath(body, "generationConfig.thinkingConfig.thinkingLevel") != "high" {
				t.Fatalf("%s: %s", protocol, raw)
			}
		}
	}
}
