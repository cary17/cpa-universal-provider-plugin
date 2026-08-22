package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func managementModelsState(t *testing.T) *runtimeState {
	t.Helper()
	s, err := decodeConfig([]byte(`
providers:
  - id: openai
    name: OpenAI
    enabled: true
    protocol: openai
    base-url: https://openai.example/custom/
    headers: {X-Provider: openai}
    api-key-entries: [{api-key: openai-fetch-secret}]
    models: [{name: openai-model}]
  - id: claude
    name: Claude
    enabled: true
    protocol: claude
    base-url: https://claude.example/v1
    headers: {Authorization: Bearer stale-manager-token, X-Provider: claude}
    api-key-entries: [{api-key: claude-fetch-secret}]
    models: [{name: claude-model}]
  - id: gemini
    name: Gemini
    enabled: true
    protocol: gemini
    base-url: https://gemini.example/api
    headers: {Authorization: Bearer stale-manager-token, X-Provider: gemini}
    api-key-entries: [{api-key: gemini-fetch-secret}]
    models: [{name: gemini-model}]
`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func decodeManagementModelsResponse(t *testing.T, raw []byte) pluginapi.ManagementResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("management response error envelope: %s", raw)
	}
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestManagementFetchesOpenAIModelsThroughHostCallback(t *testing.T) {
	state.Store(managementModelsState(t))
	old := managementHTTPDo
	t.Cleanup(func() { managementHTTPDo = old })

	var got hostHTTPRequest
	managementHTTPDo = func(request hostHTTPRequest, response *pluginapi.HTTPResponse) error {
		got = request
		*response = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"data":[{"id":"zebra","display_name":"Zebra"},{"id":"alpha"},{"id":"zebra","display_name":"Duplicate"}]}`)}
		return nil
	}

	raw, err := json.Marshal(rpcManagementRequest{
		ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/providers/models", Body: []byte(`{"id":"openai"}`)},
		HostCallbackID:    "callback-openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := managementHandle(raw)
	if err != nil {
		t.Fatal(err)
	}

	if got.HostCallbackID != "callback-openai" || got.Method != http.MethodGet || got.URL != "https://openai.example/custom/models" {
		t.Fatalf("host request=%#v", got)
	}
	if got.Headers.Get("Authorization") != "Bearer openai-fetch-secret" || got.Headers.Get("X-Provider") != "openai" {
		t.Fatalf("host request headers=%#v", got.Headers)
	}
	if len(got.Body) != 0 {
		t.Fatalf("models request must be GET without an upstream body: %q", got.Body)
	}

	response := decodeManagementModelsResponse(t, out)
	if response.StatusCode != http.StatusOK || strings.Contains(string(response.Body), "openai-fetch-secret") {
		t.Fatalf("unsafe management response=%#v", response)
	}
	var payload struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"display-name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Models) != 2 || payload.Models[0].Name != "alpha" || payload.Models[1].Name != "zebra" || payload.Models[1].DisplayName != "Zebra" {
		t.Fatalf("normalized models=%s", response.Body)
	}
}

func TestManagementFetchesClaudeAndGeminiModelsWithProtocolAuth(t *testing.T) {
	state.Store(managementModelsState(t))
	old := managementHTTPDo
	t.Cleanup(func() { managementHTTPDo = old })

	cases := []struct {
		id              string
		callbackID      string
		wantURL         string
		wantAuthHeader  string
		wantAuthValue   string
		wantModelsBody  string
		wantDisplayName string
	}{
		{
			id: "claude", callbackID: "callback-claude", wantURL: "https://claude.example/v1/models",
			wantAuthHeader: "X-Api-Key", wantAuthValue: "claude-fetch-secret",
			wantModelsBody: `{"data":[{"id":"claude-3-7-sonnet","display_name":"Claude 3.7 Sonnet"}]}`, wantDisplayName: "Claude 3.7 Sonnet",
		},
		{
			id: "gemini", callbackID: "callback-gemini", wantURL: "https://gemini.example/api/v1beta/models",
			wantAuthHeader: "X-Goog-Api-Key", wantAuthValue: "gemini-fetch-secret",
			wantModelsBody: `{"models":[{"name":"models/gemini-2.5-pro","displayName":"Gemini 2.5 Pro"}]}`, wantDisplayName: "Gemini 2.5 Pro",
		},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			var got hostHTTPRequest
			managementHTTPDo = func(request hostHTTPRequest, response *pluginapi.HTTPResponse) error {
				got = request
				*response = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(tc.wantModelsBody)}
				return nil
			}
			raw, err := json.Marshal(rpcManagementRequest{
				ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/providers/models", Body: []byte(`{"id":"` + tc.id + `"}`)},
				HostCallbackID:    tc.callbackID,
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := managementHandle(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.URL != tc.wantURL || got.HostCallbackID != tc.callbackID || got.Headers.Get(tc.wantAuthHeader) != tc.wantAuthValue || got.Headers.Get("Authorization") != "" || got.Headers.Get("X-Provider") != tc.id {
				t.Fatalf("host request=%#v", got)
			}
			if tc.id == "claude" && got.Headers.Get("Anthropic-Version") != "2023-06-01" {
				t.Fatalf("Claude headers=%#v", got.Headers)
			}
			response := decodeManagementModelsResponse(t, out)
			if response.StatusCode != http.StatusOK || strings.Contains(string(response.Body), tc.wantAuthValue) {
				t.Fatalf("unsafe management response=%#v", response)
			}
			if !strings.Contains(string(response.Body), tc.wantDisplayName) {
				t.Fatalf("normalized response=%s", response.Body)
			}
		})
	}
}

func TestManagementModelsProtocolEndpointSuffixes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
		baseURL  string
		want     string
	}{
		{name: "openai trailing slash", protocol: "openai", baseURL: "https://openai.example/v1/", want: "https://openai.example/v1/models"},
		{name: "openai response", protocol: "openai-response", baseURL: "https://responses.example/v1", want: "https://responses.example/v1/models"},
		{name: "claude without version", protocol: "claude", baseURL: "https://claude.example/api", want: "https://claude.example/api/v1/models"},
		{name: "claude versioned", protocol: "claude", baseURL: "https://claude.example/v1/", want: "https://claude.example/v1/models"},
		{name: "gemini versioned", protocol: "gemini", baseURL: "https://gemini.example/v1beta/", want: "https://gemini.example/v1beta/models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := managementModelsURL(tc.baseURL, tc.protocol); got != tc.want {
				t.Fatalf("managementModelsURL(%q, %q) = %q, want %q", tc.baseURL, tc.protocol, got, tc.want)
			}
		})
	}
}

func TestManagementModelsUpstreamErrorIsSafe(t *testing.T) {
	state.Store(managementModelsState(t))
	old := managementHTTPDo
	t.Cleanup(func() { managementHTTPDo = old })
	managementHTTPDo = func(_ hostHTTPRequest, response *pluginapi.HTTPResponse) error {
		*response = pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(`{"error":{"message":"Bearer openai-fetch-secret was rejected"}}`)}
		return nil
	}

	raw, _ := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/providers/models", Body: []byte(`{"id":"openai"}`)}})
	out, err := managementHandle(raw)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeManagementModelsResponse(t, out)
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(response.Body), "401") || strings.Contains(string(response.Body), "openai-fetch-secret") {
		t.Fatalf("upstream error leaked credentials or lacked status: %#v", response)
	}
}

func TestManagementModelsUnknownProviderDoesNotCallHost(t *testing.T) {
	state.Store(managementModelsState(t))
	old := managementHTTPDo
	t.Cleanup(func() { managementHTTPDo = old })
	called := false
	managementHTTPDo = func(_ hostHTTPRequest, _ *pluginapi.HTTPResponse) error {
		called = true
		return nil
	}

	raw, _ := json.Marshal(rpcManagementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/providers/models", Body: []byte(`{"id":"missing"}`)}})
	out, err := managementHandle(raw)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeManagementModelsResponse(t, out)
	if called || response.StatusCode != http.StatusNotFound || strings.Contains(string(response.Body), "fetch-secret") {
		t.Fatalf("unknown provider response=%#v called=%v", response, called)
	}
}
