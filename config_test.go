package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func ptr(v int) *int { return &v }
func validYAML() []byte {
	return []byte(`
enabled: true
protocol: openai
base-url: https://api.example.com/v1
api-key-entries:
  - api-key: first-secret
    weight: 2
  - api-key: ignored
    weight: 0
  - api-key: second-secret
models:
  - name: upstream-model
    alias: public-model
    display-name: Public Model
    max-context-length: 128000
    input-modalities: [text, image]
    output-modalities: [text, image]
image-generation: false
reasoning-effort: high
`)
}

func TestDecodeConfigStrictAndFilters(t *testing.T) {
	s, err := decodeConfig(validYAML())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Credentials) != 2 || s.Credentials[1].Weight != 1 {
		t.Fatalf("credentials=%#v", s.Credentials)
	}
	m := s.ByPublicModel["public-model"]
	if !reflect.DeepEqual(m.OutputModalities, []string{"text"}) {
		t.Fatalf("output modalities=%v", m.OutputModalities)
	}
	for _, bad := range [][]byte{[]byte("protocol: typo\n"), append(validYAML(), []byte("unknown-field: true\n")...)} {
		if _, err := decodeConfig(bad); err == nil {
			t.Fatalf("expected invalid config: %s", bad)
		}
	}
}
func TestWeightLimit(t *testing.T) {
	raw := []byte(`protocol: openai
base-url: https://example.com/v1
api-key-entries: [{api-key: x, weight: 1000001}]
models: [{name: m}]
`)
	if _, err := decodeConfig(raw); err == nil {
		t.Fatal("expected weight limit error")
	}
}
func TestSmoothWeightedRoundRobin(t *testing.T) {
	s := &runtimeState{Credentials: []*credential{{ID: "a", Weight: 5}, {ID: "b", Weight: 1}}}
	got := []string{}
	for i := 0; i < 6; i++ {
		got = append(got, s.pickCredential().ID)
	}
	want := []string{"a", "a", "a", "b", "a", "a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
func TestReasoningMappings(t *testing.T) {
	cases := []struct {
		protocol, path string
		want           any
	}{{"openai", "reasoning_effort", "high"}, {"openai-response", "reasoning.effort", "high"}, {"claude", "thinking.budget_tokens", float64(8192)}, {"gemini", "generationConfig.thinkingConfig.thinkingLevel", "high"}}
	for _, tc := range cases {
		raw, err := rewritePayload([]byte(`{"model":"old","messages":[]}`), "native", tc.protocol, "high", false)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		if got := jsonPath(v, tc.path); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s got %#v want %#v body=%s", tc.protocol, got, tc.want, raw)
		}
	}
}
func TestAutoDoesNotOverride(t *testing.T) {
	raw, err := rewritePayload([]byte(`{"model":"old","reasoning_effort":"low"}`), "native", "openai", "auto", false)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	if v["reasoning_effort"] != "low" {
		t.Fatalf("auto overrode request: %s", raw)
	}
}
func TestImageDetection(t *testing.T) {
	if !clearlyImageGeneration([]byte(`{"modalities":["text","image"]}`)) {
		t.Fatal("missed modalities")
	}
	if clearlyImageGeneration([]byte(`{"messages":[{"content":"describe this image"}]}`)) {
		t.Fatal("false positive")
	}
}

func TestApplyAuthDoesNotForwardFrontendCredentials(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		want     string
	}{
		{protocol: "claude", want: "x-api-key"},
		{protocol: "gemini", want: "x-goog-api-key"},
		{protocol: "openai", want: "Authorization"},
		{protocol: "openai-response", want: "Authorization"},
	} {
		h := http.Header{
			"Authorization":  []string{"Bearer frontend-secret"},
			"X-Api-Key":      []string{"stale-anthropic-key"},
			"X-Goog-Api-Key": []string{"stale-gemini-key"},
		}
		applyAuth(h, tc.protocol, "upstream-key")
		if tc.protocol == "claude" || tc.protocol == "gemini" {
			if got := h.Get("Authorization"); got != "" {
				t.Fatalf("%s forwarded frontend Authorization: %q", tc.protocol, got)
			}
		}
		if got := h.Get(tc.want); got == "" || got == "Bearer frontend-secret" {
			t.Fatalf("%s auth header %s = %q", tc.protocol, tc.want, got)
		}
	}
}

func TestUpstreamURLs(t *testing.T) {
	cases := []struct {
		protocol string
		stream   bool
		want     string
	}{
		{"openai", false, "https://example.test/v1/chat/completions"},
		{"openai-response", false, "https://example.test/v1/responses"},
		{"claude", false, "https://example.test/v1/messages"},
		{"gemini", false, "https://example.test/v1/models/gemini-3:generateContent"},
		{"gemini", true, "https://example.test/v1/models/gemini-3:streamGenerateContent?alt=sse"},
	}
	for _, tc := range cases {
		if got := upstreamURL("https://example.test/v1", tc.protocol, "gemini-3", tc.stream); got != tc.want {
			t.Fatalf("%s stream=%v URL = %q, want %q", tc.protocol, tc.stream, got, tc.want)
		}
	}
}

func jsonPath(v map[string]any, path string) any {
	var x any = v
	for _, p := range split(path) {
		m, ok := x.(map[string]any)
		if !ok {
			return nil
		}
		x = m[p]
	}
	return x
}
func split(s string) []string {
	out := []string{}
	start := 0
	for i := range s {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
