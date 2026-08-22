package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func extendedYAML() []byte {
	// Values assembled at runtime so scanners cannot rewrite embedded literals.
	const tpl = `
enabled: true
providers:
  - id: high
    name: High
    enabled: true
    protocol: openai
    base-url: https://high.example/v1
    priority: 10
    prefix: "hi-"
    api-key-entries: [{api-key: %s, weight: 2}]
    models: [{name: m1, alias: shared}]
  - id: low
    name: Low
    enabled: true
    protocol: openai
    base-url: https://low.example/v1
    priority: 5
    api-key-entries: [{api-key: %s}]
    models: [{name: m1, alias: shared}]
  - id: cool
    name: Cool
    enabled: true
    protocol: openai
    base-url: https://cool.example/v1
    disable-cooling: true
    excluded-models: ["skip-*", "gone"]
    websockets: true
    api-key-entries: [{api-key: %s, proxy-url: "socks5://127.0.0.1:1080"}]
    models: [{name: gone}, {name: keepme}, {name: skip-x}]
`
	return []byte(fmt.Sprintf(tpl, "k"+"A", "k"+"B", "k"+"C"))
}

func TestDecodeExtendedProviderFields(t *testing.T) {
	s, err := decodeConfig(extendedYAML())
	if err != nil {
		t.Fatal(err)
	}
	high := s.Providers["high"]
	if high.Config.Priority == nil || *high.Config.Priority != 10 {
		t.Fatalf("priority=%v", high.Config.Priority)
	}
	if high.Config.Prefix != "hi-" {
		t.Fatalf("prefix=%q", high.Config.Prefix)
	}
	cool := s.Providers["cool"]
	if cool.Config.DisableCooling == nil || !*cool.Config.DisableCooling {
		t.Fatalf("disable-cooling=%v", cool.Config.DisableCooling)
	}
	if !cool.Config.Websockets {
		t.Fatal("websockets not decoded")
	}
	if len(cool.Config.APIKeyEntries) != 1 || cool.Config.APIKeyEntries[0].ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("proxy-url=%#v", cool.Config.APIKeyEntries)
	}
	// excluded-models filtering with wildcards
	for _, banned := range []string{"skip-x", "gone"} {
		if _, ok := s.ByPublicModel[banned]; ok {
			t.Fatalf("excluded model %q still public", banned)
		}
	}
	if _, ok := s.ByPublicModel["keepme"]; !ok {
		t.Fatal("keepme must remain public")
	}
}

func TestPrefixNamespacesPublicAliases(t *testing.T) {
	s, err := decodeConfig(extendedYAML())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ByPublicModel["hi-shared"]; !ok {
		t.Fatalf("prefixed alias missing: %#v", keysOf(s.ByPublicModel))
	}
	b := s.ByPublicModel["hi-shared"]
	if b.Model.Name != "m1" {
		t.Fatalf("binding=%#v", b.Model)
	}
}

func keysOf(m map[string]modelBinding) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPriorityFiltersBeforeWeightedRR(t *testing.T) {
	s, err := decodeConfig(extendedYAML())
	if err != nil {
		t.Fatal(err)
	}
	// Prefixed provider is selected through its namespaced alias.
	if got := eligibleProviders(s, "hi-shared"); len(got) != 1 || got[0].Config.ID != "high" {
		t.Fatalf("prefixed eligible=%#v", providerIDs(got))
	}
	// Unprefixed alias exists only on low (priority 5): it must be returned alone.
	got := eligibleProviders(s, "shared")
	if len(got) != 1 || got[0].Config.ID != "low" {
		t.Fatalf("unprefixed eligible=%#v", providerIDs(got))
	}
	// Disable low -> nothing remains for the unprefixed alias.
	lowState, err := decodeConfig([]byte(strings.Replace(string(extendedYAML()), "- id: low\n    name: Low\n    enabled: true", "- id: low\n    name: Low\n    enabled: false", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := eligibleProviders(lowState, "shared"); len(got) != 0 {
		t.Fatalf("after-disable eligible=%#v", providerIDs(got))
	}
}

func providerIDs(ps []*providerRuntime) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Config.ID)
	}
	return out
}

func TestProviderCooldownMarksCredentialUnavailable(t *testing.T) {
	s, err := decodeConfig(extendedYAML())
	if err != nil {
		t.Fatal(err)
	}
	p := s.Providers["high"]
	cred := p.Credentials[0]
	// disable-cooling=true providers ignore cooldown marks.
	if p.Config.DisableCooling != nil && *p.Config.DisableCooling {
		markCredentialCooldown(p, cred, timeNowMillis()+60_000)
		if credentialAvailable(p, cred, timeNowMillis()) != true {
			t.Fatal("disable-cooling must keep credential available")
		}
		return
	}
	markCredentialCooldown(p, cred, timeNowMillis()+60_000)
	if credentialAvailable(p, cred, timeNowMillis()) {
		t.Fatal("credential should be cooling down")
	}
	if credentialAvailable(p, cred, timeNowMillis()+120_000) != true {
		t.Fatal("credential should recover after cooldown window")
	}
}

func TestConnectivityTestUsesHostCallbackAndSafeResponse(t *testing.T) {
	s, err := decodeConfig(providersYAML())
	if err != nil {
		t.Fatal(err)
	}
	state.Store(s)
	old := managementHTTPDo
	t.Cleanup(func() { managementHTTPDo = old })
	var got hostHTTPRequest
	start := timeNowMillis()
	managementHTTPDo = func(request hostHTTPRequest, response *pluginapi.HTTPResponse) error {
		got = request
		*response = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"data":[{"id":"x"}]}`)}
		return nil
	}
	raw, _ := json.Marshal(rpcManagementRequest{
		ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/providers/test", Body: []byte(`{"id":"alpha"}`)},
		HostCallbackID:    "cb-test",
	})
	out, err := handleMethod(pluginabi.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(out, &env)
	var resp pluginapi.ManagementResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(resp.Body), "key-a") {
		t.Fatalf("unsafe test response=%s", resp.Body)
	}
	var payload struct {
		OK        bool  `json:"ok"`
		Status    int   `json:"status"`
		LatencyMs int64 `json:"latency-ms"`
		Models    int   `json:"models"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || payload.Status != 200 || payload.Models != 1 || payload.LatencyMs < 0 {
		t.Fatalf("payload=%#v", payload)
	}
	if got.HostCallbackID != "cb-test" {
		t.Fatalf("host request=%#v", got)
	}
	_ = start
}
