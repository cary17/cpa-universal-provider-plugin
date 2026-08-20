package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

const (
	pluginID  = "universal-provider"
	maxWeight = 1_000_000
)

var state atomic.Pointer[runtimeState]

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}
type config struct {
	Enabled         bool              `yaml:"enabled"`
	Priority        int               `yaml:"priority"`
	Protocol        string            `yaml:"protocol"`
	BaseURL         string            `yaml:"base-url"`
	Headers         map[string]string `yaml:"headers"`
	APIKeyEntries   []apiKeyEntry     `yaml:"api-key-entries"`
	Models          []modelConfig     `yaml:"models"`
	ImageGeneration bool              `yaml:"image-generation"`
	ReasoningEffort string            `yaml:"reasoning-effort"`
}
type apiKeyEntry struct {
	APIKey string `yaml:"api-key"`
	Weight *int   `yaml:"weight"`
}
type modelConfig struct {
	Name             string   `yaml:"name"`
	Alias            string   `yaml:"alias"`
	DisplayName      string   `yaml:"display-name"`
	MaxContextLength int64    `yaml:"max-context-length"`
	InputModalities  []string `yaml:"input-modalities"`
	OutputModalities []string `yaml:"output-modalities"`
}
type credential struct {
	ID      string
	APIKey  string
	Weight  int
	Current int64
}
type runtimeState struct {
	Config        config
	ByPublicModel map[string]modelConfig
	Credentials   []*credential
	mu            sync.Mutex
}

func decodeConfig(raw []byte) (*runtimeState, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var cfg config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("配置 YAML 无效: %w", err)
	}
	cfg.Protocol = strings.ToLower(strings.TrimSpace(cfg.Protocol))
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.ReasoningEffort = strings.ToLower(strings.TrimSpace(cfg.ReasoningEffort))
	if cfg.ReasoningEffort == "" {
		cfg.ReasoningEffort = "auto"
	}
	allowedProtocols := map[string]bool{"openai": true, "openai-response": true, "claude": true, "gemini": true}
	if !allowedProtocols[cfg.Protocol] {
		return nil, fmt.Errorf("protocol 必须是 openai、openai-response、claude 或 gemini")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("base-url 必须是绝对 http(s) URL")
	}
	allowedEffort := map[string]bool{"auto": true, "none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true}
	if !allowedEffort[cfg.ReasoningEffort] {
		return nil, fmt.Errorf("reasoning-effort 无效")
	}
	if len(cfg.Models) == 0 {
		return nil, fmt.Errorf("models 不能为空")
	}
	byModel := make(map[string]modelConfig, len(cfg.Models))
	names := make(map[string]bool)
	for i := range cfg.Models {
		m := &cfg.Models[i]
		m.Name = strings.TrimSpace(m.Name)
		m.Alias = strings.TrimSpace(m.Alias)
		m.DisplayName = strings.TrimSpace(m.DisplayName)
		if m.Name == "" {
			return nil, fmt.Errorf("models[%d].name 不能为空", i)
		}
		if m.MaxContextLength < 0 {
			return nil, fmt.Errorf("models[%d].max-context-length 不能为负", i)
		}
		public := m.Alias
		if public == "" {
			public = m.Name
		}
		if names[public] {
			return nil, fmt.Errorf("模型标识 %q 重复", public)
		}
		names[public] = true
		byModel[public] = *m
		if !cfg.ImageGeneration {
			m.OutputModalities = removeImage(m.OutputModalities)
			byModel[public] = *m
		}
	}
	creds := make([]*credential, 0, len(cfg.APIKeyEntries))
	for i, e := range cfg.APIKeyEntries {
		key := strings.TrimSpace(e.APIKey)
		weight := 1
		if e.Weight != nil {
			weight = *e.Weight
		}
		if weight > maxWeight {
			return nil, fmt.Errorf("api-key-entries[%d].weight 不能超过 %d", i, maxWeight)
		}
		if weight <= 0 {
			continue
		}
		if key == "" {
			return nil, fmt.Errorf("api-key-entries[%d].api-key 不能为空", i)
		}
		creds = append(creds, &credential{ID: fmt.Sprintf("config-key-%d", i+1), APIKey: key, Weight: weight})
	}
	if len(creds) == 0 {
		return nil, fmt.Errorf("至少需要一个 weight > 0 的 api-key-entry")
	}
	return &runtimeState{Config: cfg, ByPublicModel: byModel, Credentials: creds}, nil
}

func removeImage(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if strings.ToLower(strings.TrimSpace(v)) != "image" {
			out = append(out, v)
		}
	}
	return out
}
func loadedState() *runtimeState { return state.Load() }
func (s *runtimeState) pickCredential() *credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *credential
	var total int64
	for _, c := range s.Credentials {
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
func (s *runtimeState) nativeModel(public string) (modelConfig, bool) {
	m, ok := s.ByPublicModel[strings.TrimSpace(public)]
	return m, ok
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
