package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}
type registrationCapabilities struct {
	ModelProvider         bool     `json:"model_provider"`
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	Scheduler             bool     `json:"scheduler"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		next, err := decodeConfig(req.ConfigYAML)
		if err != nil {
			return nil, err
		}
		state.Store(next)
		return okEnvelope(pluginRegistration(next.Config.Protocol))
	case pluginabi.MethodModelStatic:
		return staticModels()
	case pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginID})
	case pluginabi.MethodModelRoute:
		return routeModel(raw)
	case pluginabi.MethodSchedulerPick:
		return schedule(raw)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginID})
	case pluginabi.MethodExecutorExecute:
		return execute(raw)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(raw)
	case pluginabi.MethodExecutorCountTokens:
		return rpcError("not_supported", "universal-provider 不提供本地 token 计数", 501)
	case pluginabi.MethodExecutorHTTPRequest:
		return executorHTTPRequest(raw)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration(protocol string) registration {
	formats := []string{protocol}
	return registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: pluginapi.Metadata{Name: "Universal Provider", Version: "0.1.0", Author: "neverbehave", GitHubRepository: "https://github.com/neverbehave/cpa-universal-provider-plugin", ConfigFields: []pluginapi.ConfigField{
		{Name: "protocol", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"openai", "openai-response", "claude", "gemini"}, Description: "上游 CPA 协议标识。"},
		{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Description: "上游 API 根 URL。"}, {Name: "headers", Type: pluginapi.ConfigFieldTypeObject, Description: "附加请求头。"},
		{Name: "api-key-entries", Type: pluginapi.ConfigFieldTypeArray, Description: "静态 API key 与权重。"}, {Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "模型及模态元数据。"},
		{Name: "image-generation", Type: pluginapi.ConfigFieldTypeBoolean, Description: "允许图像输出声明和明显图像生成请求。"}, {Name: "reasoning-effort", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"auto", "none", "minimal", "low", "medium", "high", "xhigh"}, Description: "协议原生推理预设；auto 不覆盖请求。"},
	}}, Capabilities: registrationCapabilities{ModelProvider: true, ModelRouter: true, Executor: true, Scheduler: true, ExecutorModelScope: string(pluginapi.ExecutorModelScopeStatic), ExecutorInputFormats: formats, ExecutorOutputFormats: formats}}
}

func staticModels() ([]byte, error) {
	s := loadedState()
	if s == nil {
		return nil, fmt.Errorf("插件尚未配置")
	}
	models := make([]pluginapi.ModelInfo, 0, len(s.Config.Models))
	for _, m := range s.Config.Models {
		id := m.Alias
		if id == "" {
			id = m.Name
		}
		display := m.DisplayName
		if display == "" {
			display = id
		}
		generationMethods := []string{"chat"}
		if s.Config.Protocol == "gemini" {
			generationMethods = []string{"generateContent"}
		}
		models = append(models, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: pluginID, Name: m.Name, DisplayName: display, ContextLength: m.MaxContextLength, InputTokenLimit: m.MaxContextLength, SupportedGenerationMethods: generationMethods, SupportedInputModalities: append([]string(nil), m.InputModalities...), SupportedOutputModalities: append([]string(nil), m.OutputModalities...), Thinking: &pluginapi.ThinkingSupport{ZeroAllowed: true, DynamicAllowed: true, Levels: []string{"minimal", "low", "medium", "high", "xhigh"}}, UserDefined: true})
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: pluginID, Models: models})
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	s := loadedState()
	if s == nil {
		return nil, fmt.Errorf("插件尚未配置")
	}
	if _, ok := s.nativeModel(req.RequestedModel); !ok {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	if !s.Config.ImageGeneration && clearlyImageGeneration(req.Body) {
		return rpcError("image_generation_disabled", "配置已禁用图像生成", 400)
	}
	return okEnvelope(pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetSelf, Reason: "universal_provider_model"})
}

type schedulerCredential struct {
	id     string
	weight int
}

func schedule(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if !containsProvider(req.Provider, req.Providers, pluginID) {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	candidates := make([]schedulerCredential, 0)
	for _, x := range req.Candidates {
		if x.Provider != pluginID || strings.EqualFold(x.Status, "disabled") {
			continue
		}
		w := candidateWeight(x)
		if w > 0 {
			candidates = append(candidates, schedulerCredential{id: x.ID, weight: w})
		}
	}
	if len(candidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	best := schedulerSWRRPick(candidates)
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: best.id})
}

var schedulerState = struct {
	sync.Mutex
	current map[string]int64
}{current: make(map[string]int64)}

func schedulerSWRRPick(candidates []schedulerCredential) schedulerCredential {
	schedulerState.Lock()
	defer schedulerState.Unlock()
	active := make(map[string]bool, len(candidates))
	var best schedulerCredential
	var bestCurrent, total int64
	for _, candidate := range candidates {
		active[candidate.id] = true
		total += int64(candidate.weight)
		schedulerState.current[candidate.id] += int64(candidate.weight)
		current := schedulerState.current[candidate.id]
		if best.id == "" || current > bestCurrent {
			best, bestCurrent = candidate, current
		}
	}
	for id := range schedulerState.current {
		if !active[id] {
			delete(schedulerState.current, id)
		}
	}
	schedulerState.current[best.id] -= total
	return best
}
func containsProvider(p string, ps []string, want string) bool {
	if p == want {
		return true
	}
	for _, x := range ps {
		if x == want {
			return true
		}
	}
	return false
}
func candidateWeight(x pluginapi.SchedulerAuthCandidate) int {
	v := ""
	if x.Attributes != nil {
		v = x.Attributes["weight"]
	}
	if v == "" && x.Metadata != nil {
		v = fmt.Sprint(x.Metadata["weight"])
	}
	w := 1
	if v != "" {
		_, _ = fmt.Sscanf(v, "%d", &w)
	}
	if w > maxWeight {
		return 0
	}
	return w
}
