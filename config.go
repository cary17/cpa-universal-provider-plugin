package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

const (
	pluginID            = "universal-provider"
	maxWeight           = 1_000_000
	internalModelPrefix = "__universal_provider_v1__"
)

var state atomic.Pointer[runtimeState]

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}
type config struct {
	Enabled   bool             `yaml:"enabled" json:"enabled"`
	Priority  int              `yaml:"priority" json:"priority"`
	Providers []providerConfig `yaml:"providers" json:"providers"`
	// Legacy v0.1 fields, accepted only to migrate an existing singleton config.
	Protocol        string            `yaml:"protocol,omitempty" json:"-"`
	BaseURL         string            `yaml:"base-url,omitempty" json:"-"`
	Headers         map[string]string `yaml:"headers,omitempty" json:"-"`
	APIKeyEntries   []apiKeyEntry     `yaml:"api-key-entries,omitempty" json:"-"`
	Models          []modelConfig     `yaml:"models,omitempty" json:"-"`
	ImageGeneration bool              `yaml:"image-generation,omitempty" json:"-"`
	ReasoningEffort string            `yaml:"reasoning-effort,omitempty" json:"-"`
}
type providerConfig struct {
	ID              string            `yaml:"id" json:"id"`
	Name            string            `yaml:"name" json:"name"`
	Enabled         bool              `yaml:"enabled" json:"enabled"`
	Protocol        string            `yaml:"protocol" json:"protocol"`
	BaseURL         string            `yaml:"base-url" json:"base-url"`
	Headers         map[string]string `yaml:"headers" json:"headers"`
	APIKeyEntries   []apiKeyEntry     `yaml:"api-key-entries" json:"api-key-entries"`
	Models          []modelConfig     `yaml:"models" json:"models"`
	ImageGeneration bool              `yaml:"image-generation" json:"image-generation"`
	ReasoningEffort string            `yaml:"reasoning-effort" json:"reasoning-effort"`
}
type apiKeyEntry struct {
	APIKey string `yaml:"api-key" json:"api-key"`
	Weight *int   `yaml:"weight" json:"weight,omitempty"`
}
type modelConfig struct {
	Name             string   `yaml:"name" json:"name"`
	Alias            string   `yaml:"alias" json:"alias,omitempty"`
	DisplayName      string   `yaml:"display-name" json:"display-name,omitempty"`
	MaxContextLength int64    `yaml:"max-context-length" json:"max-context-length,omitempty"`
	InputModalities  []string `yaml:"input-modalities" json:"input-modalities,omitempty"`
	OutputModalities []string `yaml:"output-modalities" json:"output-modalities,omitempty"`
}
type credential struct {
	ID, APIKey string
	Weight     int
	Current    int64
}
type providerRuntime struct {
	providerConfig
	Config      providerConfig
	Credentials []*credential
	mu          sync.Mutex
}
type modelBinding struct {
	modelConfig
	Model         modelConfig
	Provider      *providerRuntime
	InternalModel string
}
type runtimeState struct {
	Config          config
	ByPublicModel   map[string]modelBinding
	ByInternalModel map[string]modelBinding
	Providers       map[string]*providerRuntime
	Credentials     []*credential
}

func decodeConfig(raw []byte) (*runtimeState, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var cfg config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("配置 YAML 无效: %w", err)
	}
	if len(cfg.Providers) == 0 && (cfg.Protocol != "" || cfg.BaseURL != "" || len(cfg.Models) > 0) {
		cfg.Providers = []providerConfig{{ID: "legacy", Name: "Legacy provider", Enabled: true, Protocol: cfg.Protocol, BaseURL: cfg.BaseURL, Headers: cfg.Headers, APIKeyEntries: cfg.APIKeyEntries, Models: cfg.Models, ImageGeneration: cfg.ImageGeneration, ReasoningEffort: cfg.ReasoningEffort}}
	}
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("providers 不能为空")
	}
	cfg.Protocol = ""
	cfg.BaseURL = ""
	cfg.Headers = nil
	cfg.APIKeyEntries = nil
	cfg.Models = nil
	cfg.ImageGeneration = false
	cfg.ReasoningEffort = ""
	s := &runtimeState{Config: cfg, ByPublicModel: map[string]modelBinding{}, ByInternalModel: map[string]modelBinding{}, Providers: map[string]*providerRuntime{}}
	ids, names, aliases := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := range s.Config.Providers {
		p := &s.Config.Providers[i]
		p.ID = strings.TrimSpace(p.ID)
		p.Name = strings.TrimSpace(p.Name)
		p.Protocol = strings.ToLower(strings.TrimSpace(p.Protocol))
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		p.ReasoningEffort = strings.ToLower(strings.TrimSpace(p.ReasoningEffort))
		if p.ReasoningEffort == "" {
			p.ReasoningEffort = "auto"
		}
		if p.ID == "" || p.Name == "" {
			return nil, fmt.Errorf("providers[%d].id/name 不能为空", i)
		}
		if ids[p.ID] {
			return nil, fmt.Errorf("provider id %q 重复", p.ID)
		}
		if names[p.Name] {
			return nil, fmt.Errorf("provider name %q 重复", p.Name)
		}
		ids[p.ID] = true
		names[p.Name] = true
		if !map[string]bool{"openai": true, "openai-response": true, "claude": true, "gemini": true}[p.Protocol] {
			return nil, fmt.Errorf("providers[%d].protocol 无效", i)
		}
		u, e := url.Parse(p.BaseURL)
		if e != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("providers[%d].base-url 必须是绝对 http(s) URL", i)
		}
		if !map[string]bool{"auto": true, "none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}[p.ReasoningEffort] {
			return nil, fmt.Errorf("providers[%d].reasoning-effort 无效", i)
		}
		if len(p.Models) == 0 {
			return nil, fmt.Errorf("providers[%d].models 不能为空", i)
		}
		pr := &providerRuntime{providerConfig: *p, Config: *p}
		for k, e := range p.APIKeyEntries {
			key := strings.TrimSpace(e.APIKey)
			w := 1
			if e.Weight != nil {
				w = *e.Weight
			}
			if w > maxWeight {
				return nil, fmt.Errorf("providers[%d].api-key-entries[%d].weight 过大", i, k)
			}
			if w <= 0 {
				continue
			}
			if key == "" {
				return nil, fmt.Errorf("providers[%d].api-key-entries[%d].api-key 不能为空", i, k)
			}
			pr.Credentials = append(pr.Credentials, &credential{ID: fmt.Sprintf("%s-key-%d", p.ID, k+1), APIKey: key, Weight: w})
		}
		if len(pr.Credentials) == 0 {
			return nil, fmt.Errorf("providers[%d] 至少需要一个有效 API key", i)
		}
		s.Providers[p.ID] = pr
		for j := range p.Models {
			m := p.Models[j]
			m.Name = strings.TrimSpace(m.Name)
			m.Alias = strings.TrimSpace(m.Alias)
			m.DisplayName = strings.TrimSpace(m.DisplayName)
			if m.Name == "" {
				return nil, fmt.Errorf("providers[%d].models[%d].name 不能为空", i, j)
			}
			if m.MaxContextLength < 0 {
				return nil, fmt.Errorf("max-context-length 不能为负")
			}
			public := m.Alias
			if public == "" {
				public = m.Name
			}
			if strings.HasPrefix(public, internalModelPrefix) {
				return nil, fmt.Errorf("providers[%d].models[%d] 的公开模型标识使用了保留前缀", i, j)
			}
			if p.Enabled && aliases[public] {
				return nil, fmt.Errorf("启用供应商模型标识 %q 重复", public)
			}
			if p.Enabled {
				aliases[public] = true
			}
			if !p.ImageGeneration {
				m.OutputModalities = removeImage(m.OutputModalities)
			}
			internal := encodeInternalModel(p.ID, public)
			b := modelBinding{modelConfig: m, Model: m, Provider: pr, InternalModel: internal}
			if p.Enabled {
				s.ByPublicModel[public] = b
				s.ByInternalModel[internal] = b
			}
		}
	}
	if len(s.Config.Providers) == 1 {
		s.Credentials = s.Providers[s.Config.Providers[0].ID].Credentials
	}
	return s, nil
}
func encodeInternalModel(providerID, public string) string {
	return internalModelPrefix + base64.RawURLEncoding.EncodeToString([]byte(providerID)) + "." + base64.RawURLEncoding.EncodeToString([]byte(public))
}
func removeImage(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !strings.EqualFold(strings.TrimSpace(v), "image") {
			out = append(out, v)
		}
	}
	return out
}
func loadedState() *runtimeState { return state.Load() }
func (p *providerRuntime) pickCredential() *credential {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *credential
	var total int64
	for _, c := range p.Credentials {
		total += int64(c.Weight)
		c.Current += int64(c.Weight)
		if best == nil || c.Current > best.Current {
			best = c
		}
	}
	if best != nil {
		best.Current -= total
	}
	return best
}
func (s *runtimeState) pickCredential() *credential {
	if len(s.Config.Providers) == 1 && s.Providers[s.Config.Providers[0].ID] != nil {
		return s.Providers[s.Config.Providers[0].ID].pickCredential()
	}
	p := &providerRuntime{Credentials: s.Credentials}
	return p.pickCredential()
}
func (s *runtimeState) nativeModel(name string) (modelBinding, bool) {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, internalModelPrefix) {
		b, ok := s.ByInternalModel[name]
		return b, ok
	}
	b, ok := s.ByPublicModel[name]
	return b, ok
}
func clearlyImageGeneration(body []byte) bool {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	return walkImageIntent(v, "")
}
func walkImageIntent(v any, key string) bool {
	lk := strings.ToLower(key)
	if lk == "modalities" {
		if a, ok := v.([]any); ok {
			for _, x := range a {
				if strings.EqualFold(fmt.Sprint(x), "image") {
					return true
				}
			}
		}
	}
	if lk == "tool" || lk == "type" || lk == "name" {
		x := strings.ToLower(fmt.Sprint(v))
		if strings.Contains(x, "image_generation") || strings.Contains(x, "generate_image") {
			return true
		}
	}
	switch x := v.(type) {
	case map[string]any:
		for k, z := range x {
			if walkImageIntent(z, k) {
				return true
			}
		}
	case []any:
		for _, z := range x {
			if walkImageIntent(z, key) {
				return true
			}
		}
	}
	return false
}
