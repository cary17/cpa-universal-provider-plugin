package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func managementRegister() ([]byte, error) {
	return okEnvelope(pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/providers"},
			{Method: http.MethodPost, Path: "/providers/models"},
			{Method: http.MethodPost, Path: "/providers/test"},
		},
		Resources: []pluginapi.ResourceRoute{{
			Path:        "/providers",
			Menu:        "通用供应商",
			Description: "管理多协议 AI 上游供应商。",
		}},
	})
}

type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementProviderSummary struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Enabled         bool     `json:"enabled"`
	Protocol        string   `json:"protocol"`
	BaseURL         string   `json:"base-url"`
	Priority        *int     `json:"priority,omitempty"`
	Prefix          string   `json:"prefix,omitempty"`
	Websockets      bool     `json:"websockets,omitempty"`
	CoolingDisabled bool     `json:"cooling-disabled,omitempty"`
	KeyCount        int      `json:"key-count"`
	ModelCount      int      `json:"model-count"`
	Models          []string `json:"models"`
	ImageGeneration bool     `json:"image-generation"`
	ReasoningEffort string   `json:"reasoning-effort"`
}

type managementProviderList struct {
	Providers []managementProviderSummary `json:"providers"`
}

type managementModelsRequest struct {
	ID string `json:"id"`
}

type managementModel struct {
	Name        string `json:"name"`
	DisplayName string `json:"display-name,omitempty"`
}

type managementModelsResponse struct {
	Models []managementModel `json:"models"`
}

// managementHTTPDo is a test seam. Production requests always use the host
// callback, so the plugin never creates a private HTTP client.
var managementHTTPDo = func(request hostHTTPRequest, response *pluginapi.HTTPResponse) error {
	return callHost(pluginabi.MethodHostHTTPDo, request, response)
}

func providersResourcePath(path string) bool {
	return path == "/providers" || path == "/v0/resource/plugins/"+pluginID+"/providers"
}

func providersManagementPath(path string) bool {
	return path == "/providers" || path == "/v0/management/plugins/"+pluginID+"/providers"
}

func providersTestManagementPath(path string) bool {
	return path == "/providers/test" || path == "/v0/management/plugins/"+pluginID+"/providers/test"
}
func providersModelsManagementPath(path string) bool {
	return path == "/providers/models" || path == "/v0/management/plugins/"+pluginID+"/providers/models"
}

func managementJSON(status int, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, Body: body})
}

func managementProviderSummaries(s *runtimeState) managementProviderList {
	if s == nil {
		return managementProviderList{Providers: []managementProviderSummary{}}
	}
	providers := make([]managementProviderSummary, 0, len(s.Providers))
	for _, provider := range s.Providers {
		models := make([]string, 0, len(provider.Config.Models))
		for _, model := range provider.Config.Models {
			name := strings.TrimSpace(model.Alias)
			if name == "" {
				name = strings.TrimSpace(model.Name)
			}
			if name != "" {
				models = append(models, name)
			}
		}
		sort.Strings(models)
		providers = append(providers, managementProviderSummary{
			ID:              provider.Config.ID,
			Name:            provider.Config.Name,
			Enabled:         provider.Config.Enabled,
			Protocol:        provider.Config.Protocol,
			BaseURL:         provider.Config.BaseURL,
			Priority:        provider.Config.Priority,
			Prefix:          provider.Config.Prefix,
			Websockets:      provider.Config.Websockets,
			CoolingDisabled: provider.Config.DisableCooling != nil && *provider.Config.DisableCooling,
			KeyCount:        len(provider.Credentials),
			ModelCount:      len(models),
			Models:          models,
			ImageGeneration: provider.Config.ImageGeneration,
			ReasoningEffort: provider.Config.ReasoningEffort,
		})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return managementProviderList{Providers: providers}
}

func managementModelsURL(baseURL, protocol string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch protocol {
	case "claude":
		if strings.HasSuffix(baseURL, "/v1") {
			return baseURL + "/models"
		}
		return baseURL + "/v1/models"
	case "gemini":
		if strings.HasSuffix(baseURL, "/v1beta") {
			return baseURL + "/models"
		}
		return baseURL + "/v1beta/models"
	default: // openai and openai-response
		return baseURL + "/models"
	}
}

func managementModelsHeaders(provider *providerRuntime, key string) http.Header {
	headers := make(http.Header, len(provider.Config.Headers)+3)
	for name, value := range provider.Config.Headers {
		headers.Set(name, value)
	}
	applyAuth(headers, provider.Config.Protocol, key)
	return headers
}

func safeManagementUpstreamStatus(status int) (int, string) {
	// HTTP responses must use a valid status code; a malformed host response is
	// represented as Bad Gateway rather than reflecting an unusable code.
	if status < http.StatusContinue || status > 599 {
		return http.StatusBadGateway, "upstream returned an invalid HTTP status"
	}
	return status, "upstream returned HTTP " + http.StatusText(status) + " (" + strconv.Itoa(status) + ")"
}

func normalizeManagementModels(protocol string, raw []byte) (managementModelsResponse, error) {
	var payload struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return managementModelsResponse{}, err
	}
	entries := payload.Data
	if protocol == "gemini" {
		entries = payload.Models
	}
	byName := make(map[string]managementModel, len(entries))
	for _, rawEntry := range entries {
		var entry struct {
			ID               string `json:"id"`
			Name             string `json:"name"`
			DisplayName      string `json:"display_name"`
			CamelDisplayName string `json:"displayName"`
		}
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			continue
		}
		name := strings.TrimSpace(entry.ID)
		if name == "" {
			name = strings.TrimSpace(entry.Name)
		}
		if protocol == "gemini" {
			name = strings.TrimPrefix(name, "models/")
		}
		if name == "" {
			continue
		}
		displayName := strings.TrimSpace(entry.DisplayName)
		if displayName == "" {
			displayName = strings.TrimSpace(entry.CamelDisplayName)
		}
		if current, ok := byName[name]; !ok || (current.DisplayName == "" && displayName != "") {
			byName[name] = managementModel{Name: name, DisplayName: displayName}
		}
	}
	models := make([]managementModel, 0, len(byName))
	for _, model := range byName {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return managementModelsResponse{Models: models}, nil
}

// managementTestProvider performs a lightweight upstream connectivity probe via
// the host HTTP callback and returns a secret-free summary. It reuses the model
// listing endpoint so the check validates URL, credential, and protocol headers.
func managementTestProvider(req rpcManagementRequest) ([]byte, error) {
	var request managementModelsRequest
	if err := json.Unmarshal(req.Body, &request); err != nil || strings.TrimSpace(request.ID) == "" {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid_provider_id"})
	}
	s := loadedState()
	if s == nil {
		return managementJSON(http.StatusServiceUnavailable, map[string]string{"error": "provider_state_unavailable"})
	}
	provider, ok := s.Providers[strings.TrimSpace(request.ID)]
	if !ok || provider == nil {
		return managementJSON(http.StatusNotFound, map[string]string{"error": "provider_not_found"})
	}
	if len(provider.Credentials) == 0 || provider.Credentials[0] == nil || strings.TrimSpace(provider.Credentials[0].APIKey) == "" {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "provider_has_no_credential"})
	}

	upstreamRequest := hostHTTPRequest{
		HostCallbackID: req.HostCallbackID,
		Method:         http.MethodGet,
		URL:            managementModelsURL(provider.Config.BaseURL, provider.Config.Protocol),
		Headers:        managementModelsHeaders(provider, provider.Credentials[0].APIKey),
	}
	started := timeNowMillis()
	var upstream pluginapi.HTTPResponse
	errDo := managementHTTPDo(upstreamRequest, &upstream)
	latency := timeNowMillis() - started
	result := struct {
		OK        bool   `json:"ok"`
		Status    int    `json:"status"`
		LatencyMs int64  `json:"latency-ms"`
		Models    int    `json:"models"`
		Error     string `json:"error,omitempty"`
	}{LatencyMs: latency}
	if errDo != nil {
		result.Error = "upstream_request_failed"
		return managementJSON(http.StatusBadGateway, result)
	}
	result.Status = upstream.StatusCode
	if upstream.StatusCode < http.StatusOK || upstream.StatusCode >= http.StatusMultipleChoices {
		status, message := safeManagementUpstreamStatus(upstream.StatusCode)
		result.Error = message
		return managementJSON(status, result)
	}
	models, err := normalizeManagementModels(provider.Config.Protocol, upstream.Body)
	if err != nil {
		result.Error = "invalid_upstream_models_response"
		return managementJSON(http.StatusBadGateway, result)
	}
	result.OK = true
	result.Models = len(models.Models)
	return managementJSON(http.StatusOK, result)
}

func managementFetchModels(req rpcManagementRequest) ([]byte, error) {
	var request managementModelsRequest
	if err := json.Unmarshal(req.Body, &request); err != nil || strings.TrimSpace(request.ID) == "" {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid_provider_id"})
	}
	s := loadedState()
	if s == nil {
		return managementJSON(http.StatusServiceUnavailable, map[string]string{"error": "provider_state_unavailable"})
	}
	provider, ok := s.Providers[strings.TrimSpace(request.ID)]
	if !ok || provider == nil {
		return managementJSON(http.StatusNotFound, map[string]string{"error": "provider_not_found"})
	}
	if len(provider.Credentials) == 0 || provider.Credentials[0] == nil || strings.TrimSpace(provider.Credentials[0].APIKey) == "" {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "provider_has_no_credential"})
	}

	var upstream pluginapi.HTTPResponse
	upstreamRequest := hostHTTPRequest{
		HostCallbackID: req.HostCallbackID,
		Method:         http.MethodGet,
		URL:            managementModelsURL(provider.Config.BaseURL, provider.Config.Protocol),
		Headers:        managementModelsHeaders(provider, provider.Credentials[0].APIKey),
	}
	if err := managementHTTPDo(upstreamRequest, &upstream); err != nil {
		// Host callback errors can contain implementation details. Do not return
		// them to the management caller because headers carry provider credentials.
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "upstream_request_failed"})
	}
	if upstream.StatusCode < http.StatusOK || upstream.StatusCode >= http.StatusMultipleChoices {
		status, message := safeManagementUpstreamStatus(upstream.StatusCode)
		return managementJSON(status, map[string]string{"error": message})
	}
	models, err := normalizeManagementModels(provider.Config.Protocol, upstream.Body)
	if err != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "invalid_upstream_models_response"})
	}
	return managementJSON(http.StatusOK, models)
}

func managementHandle(raw []byte) ([]byte, error) {
	var req rpcManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if providersResourcePath(req.Path) {
		if req.Method != http.MethodGet {
			return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed, Headers: http.Header{"Allow": []string{http.MethodGet}}, Body: []byte("resource is read-only")})
		}
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Content-Security-Policy": []string{"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'"}}, Body: []byte(providersHTML)})
	}
	if providersTestManagementPath(req.Path) {
		if req.Method != http.MethodPost {
			return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed, Headers: http.Header{"Allow": []string{http.MethodPost}}, Body: []byte("method not allowed")})
		}
		return managementTestProvider(req)
	}
	if providersModelsManagementPath(req.Path) {
		if req.Method != http.MethodPost {
			return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed, Headers: http.Header{"Allow": []string{http.MethodPost}}, Body: []byte("method not allowed")})
		}
		return managementFetchModels(req)
	}
	if !providersManagementPath(req.Path) {
		return managementJSON(http.StatusNotFound, map[string]string{"error": "not_found"})
	}
	switch req.Method {
	case http.MethodGet:
		return managementJSON(http.StatusOK, managementProviderSummaries(loadedState()))
	default:
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed, Headers: http.Header{"Allow": []string{http.MethodGet}}, Body: []byte("method not allowed")})
	}
}

const providersHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>供应商管理</title><style>
body{margin:0;padding:20px;background:var(--bg-primary,#f6f8fb);color:var(--text-primary,#2c3e50);font-family:var(--cpamp-plugin-font-family,Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif);font-size:14px}
.toolbar{display:flex;gap:8px;align-items:center;margin-bottom:14px;flex-wrap:wrap}
h1{font-size:18px;margin:0 12px 0 0}
#status{color:var(--text-secondary,#5f6c7b);margin-right:auto}
button{min-height:32px;padding:6px 14px;border:1px solid var(--border-color,rgba(15,23,42,.08));border-radius:var(--app-radius-md,10px);background:var(--app-surface,#fff);color:var(--text-primary,#2c3e50);cursor:pointer;font:inherit}
button:hover:not(:disabled){border-color:var(--border-hover,rgba(64,158,255,.28));color:var(--primary-color,#409eff)}
button.primary{background:var(--primary-color,#409eff);border-color:var(--primary-color);color:#fff}
button.primary:hover:not(:disabled){background:var(--primary-hover,#79bbff);color:#fff}
button.danger{color:var(--danger-color,#f56c6c)}
button:disabled{opacity:.55;cursor:not-allowed}
table{width:100%;border-collapse:collapse;background:var(--app-surface,var(--bg-primary))}
th,td{text-align:left;padding:10px 8px;border-bottom:1px solid var(--border-color,rgba(15,23,42,.08));font-size:13px}
th{color:var(--text-secondary,#5f6c7b)}
.badge{display:inline-block;padding:2px 10px;border-radius:999px;font-size:12px;background:color-mix(in srgb,var(--primary-color,#409eff) 12%,transparent);color:var(--primary-color,#409eff)}
.toggle{appearance:none;-webkit-appearance:none;width:36px;height:20px;border-radius:999px;background:var(--border-color,#cbd5e1);position:relative;cursor:pointer;transition:.15s;margin:0}
.toggle:checked{background:var(--success-color,#67c23a)}
.toggle::before{content:"";position:absolute;top:2px;left:2px;width:16px;height:16px;border-radius:50%;background:#fff;transition:.15s}
.toggle:checked::before{transform:translateX(16px)}
.empty{padding:48px;text-align:center;color:var(--text-tertiary,#8b95a6)}
dialog{width:min(92vw,880px);max-height:88vh;border:1px solid var(--border-color,rgba(15,23,42,.08));border-radius:var(--app-radius-lg,14px);background:var(--app-surface,var(--bg-primary,#f6f8fb));color:inherit;padding:18px}
dialog::backdrop{background:rgba(15,23,42,.45)}
.grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}
label{display:grid;gap:4px;font-size:13px;color:var(--text-secondary,#5f6c7b)}
input,select,textarea{min-height:34px;border:1px solid var(--border-color,rgba(15,23,42,.08));border-radius:var(--app-radius-sm,8px);background:var(--app-input-bg,transparent);color:var(--text-primary,#2c3e50);padding:6px 10px;font:inherit}
textarea{min-height:74px}
.keyrow,.modelrow{display:flex;gap:8px;align-items:center;margin-top:6px}
.testpanel{border-top:1px dashed var(--border-color,rgba(15,23,42,.08));margin-top:10px;padding-top:8px}
.statusbadge{display:inline-block;margin-top:6px;padding:3px 10px;border-radius:999px;font-size:12px;border:1px solid transparent}
.statusbadge.success{color:var(--success-color,#67c23a);background:color-mix(in srgb,var(--success-color,#67c23a) 12%,transparent);border-color:color-mix(in srgb,var(--success-color,#67c23a) 30%,transparent)}
.statusbadge.error{color:var(--danger-color,#f56c6c);background:color-mix(in srgb,var(--danger-color,#f56c6c) 12%,transparent);border-color:color-mix(in srgb,var(--danger-color,#f56c6c) 30%,transparent)}
.statusbadge.muted{color:var(--text-secondary,#5f6c7b);background:var(--bg-tertiary,#eef3f9);border-color:var(--border-color,rgba(15,23,42,.08))}
.msection{border:1px solid var(--border-color,rgba(15,23,42,.08));border-radius:var(--app-radius-md,10px);padding:12px;background:color-mix(in srgb,var(--bg-tertiary,#eef3f9) 35%,transparent)}
.mhead{display:flex;align-items:center;justify-content:space-between;margin-bottom:6px}
.mtitle{font-weight:600;font-size:13px;color:var(--text-primary,#2c3e50)}
.mtools{display:flex;gap:8px}
.mentry{display:flex;flex-direction:column;gap:4px;border:1px solid var(--border-color,rgba(15,23,42,.08));border-radius:8px;padding:8px 10px;margin-top:6px;background:var(--app-surface,var(--bg-primary))}
.mentry-top{display:flex;gap:8px;align-items:center}
.mentry-top input:first-child{flex:1}
.mentry-arrow{color:var(--text-tertiary,#8b95a6)}
.mentry-adv{display:flex;gap:8px;flex-wrap:wrap}
.mentry-adv input{min-height:28px;font-size:12px;flex:1;min-width:110px}
.mentry-x{border:none;background:transparent;color:var(--text-secondary);cursor:pointer;font-size:14px;line-height:1;padding:4px 6px;border-radius:6px}
.mentry-x:hover{color:var(--danger-color,#f56c6c);background:color-mix(in srgb,var(--danger-color,#f56c6c) 10%,transparent)}
.mchip{display:inline-flex;align-items:center;gap:4px;padding:2px 10px;border-radius:999px;font-size:11px;background:color-mix(in srgb,var(--primary-color,#409eff) 12%,transparent);color:var(--primary-color,#409eff)}
.disc-row{display:flex;gap:8px;align-items:flex-start;width:100%;padding:8px 10px;border:1px solid var(--border-color,rgba(15,23,42,.08));border-radius:8px;background:var(--bg-primary,var(--bg-primary,#fff));cursor:pointer;margin-bottom:6px}
.disc-row:hover{border-color:var(--primary-color,#409eff)}
.disc-row input[type=checkbox]{margin-top:2px}
.disc-meta{display:flex;flex-direction:column;gap:2px;min-width:0}
.disc-name{display:flex;align-items:center;justify-content:space-between;gap:8px;font-weight:600;color:var(--text-primary,#2c3e50)}
.disc-desc{font-size:12px;color:var(--text-secondary,#5f6c7b)}
.model-list-old{max-height:240px;overflow:auto;border:1px solid var(--border-color,rgba(15,23,42,.08));border-radius:8px;margin-top:8px}
.model-row{display:flex;gap:8px;align-items:center;padding:6px 8px;border-bottom:1px dashed var(--border-color,rgba(15,23,42,.08))}
.dlg-actions{display:flex;gap:8px;justify-content:flex-end;margin-top:14px}
.hint{color:var(--text-tertiary,#8b95a6);font-size:12px;margin:4px 0 0}
@media(max-width:720px){.grid{grid-template-columns:1fr}}
</style></head><body>
<div class="toolbar"><h1>供应商管理</h1><span id="status" role="status"></span><button id="refresh">刷新</button><button id="add">新增供应商</button><button id="save" class="primary" disabled>保存更改</button></div>
<table><thead><tr><th>类型</th><th>名称</th><th>Base URL</th><th>模型</th><th>密钥数量</th><th>状态</th><th>操作</th></tr></thead><tbody id="rows"></tbody></table>
<p id="empty" class="empty" hidden>尚未配置供应商，点击“新增供应商”开始。</p>
<dialog id="editor"><form method="dialog" novalidate><h2 id="editor-title" style="margin:0 0 12px;font-size:16px">编辑供应商</h2><div class="grid">
<label>标识 ID<input name="id" required placeholder="例如 official-deepseek"></label>
<label>显示名称<input name="name" required placeholder="例如 官方 DeepSeek"></label>
<label>协议<select name="protocol"><option value="openai">OpenAI 兼容</option><option value="openai-response">OpenAI Responses</option><option value="claude">Anthropic Messages</option><option value="gemini">Gemini GenerateContent</option></select></label>
<label>Base URL<input name="base-url" required placeholder="https://api.example.com/v1"></label>
<label>优先级 (可选)<input name="priority" type="number" placeholder="留空为 0，数值大的优先"></label>
<label>权重 (可选)<input name="weight" type="number" min="0" placeholder="默认 1"><p class="hint">优先级会先筛选。在加权轮询中，正数决定流量比例。</p></label>
<label>前缀 (可选)<input name="prefix" placeholder="例如 teamA-（公开别名加前缀）"></label>
<label>代理 (可选)<input name="proxy-url" placeholder="http:// 或 socks5://；密钥可单独覆盖"><p class="hint">每个 API 密钥可单独配置代理：不填跟随全局/此处值，填 Direct 表示直连。</p></label>
<label>冷却策略<select name="disable-cooling"><option value="inherit">跟随全局</option><option value="enable">启用冷却</option><option value="disable">禁用冷却</option></select></label>
<label id="ws-row" hidden>Websockets<label class="keyrow" style="gap:6px"><input type="checkbox" name="websockets" style="width:auto"><span style="font-size:13px">开启 Responses API 的 websocket 传输。仅限使用 Responses API 时</span></label></label>
<label>思考强度<select name="reasoning-effort"><option value="auto">auto</option><option value="none">none</option><option value="minimal">minimal</option><option value="low">low</option><option value="medium">medium</option><option value="high">high</option><option value="xhigh">xhigh</option><option value="max">max</option></select></label>
<label>图像生成<select name="image-generation"><option value="false">禁用</option><option value="true">启用</option></select></label>
<label style="grid-column:1/-1">自定义请求头（每行“名称: 值”）<textarea name="headers"></textarea></label>
<label style="grid-column:1/-1">API 密钥与权重<div id="keys"></div><div class="keyrow"><button type="button" id="add-key">新增 API 密钥</button></div><p class="hint">已保存的密钥不会显示；留空表示保持不变。权重：正数决定流量比例，非正数会排除此凭据。代理：可填 URL 或 Direct（直连），留空继承供应商设置。</p></label>
<div class="msection" style="grid-column:1/-1"><div class="mhead"><span class="mtitle">模型</span><span class="mtools"><button type="button" id="add-model">添加模型</button><button type="button" id="fetch-models">拉取模型</button></span></div><p class="hint">上游模型名 → 公开别名（可选）。点击“拉取模型”可从 /v1/models 获取。</p><div id="model-entries"></div><div class="testpanel"><div class="mhead" style="margin-bottom:4px"><span class="mtitle">连通性测试</span></div><p class="hint">发送一次测试请求，确认当前配置是否可用。</p><div class="keyrow"><select id="test-model" style="flex:1"></select><button type="button" id="test-provider">测试连通性</button></div><div id="test-status" class="statusbadge muted" hidden></div></div></div><label style="grid-column:1/-1">排除的模型 (可选)<textarea name="excluded-models" placeholder="每行或逗号分隔，支持通配符，例如 gemini-*-preview"></textarea><p class="hint">命中排除规则的模型不会出现在公开模型列表。</p></label><input type="hidden" name="models">
</div><p class="dlg-actions"><button value="cancel">取消</button><button id="apply" class="primary" value="default">应用</button></p></form></dialog>
<dialog id="models-dlg"><form method="dialog"><h2 style="margin:0 0 10px;font-size:16px">拉取模型</h2><p id="models-status" role="status" class="hint"></p><div class="keyrow"><input id="model-search" placeholder="搜索模型" style="flex:1"><button type="button" id="sel-visible">全选可见</button><button type="button" id="clear-sel">清空选择</button></div><p id="models-count" class="hint"></p><div id="model-box" class="model-list"></div><p class="dlg-actions"><button value="cancel">关闭</button><button id="merge-models" class="primary" value="default">添加所选</button></p></form></dialog>
<script>
(()=>{'use strict';
const CFG='/v0/management/plugins/universal-provider/config';
const MODELS='/v0/management/plugins/universal-provider/providers/models';
const MODELS_TEST='/v0/management/plugins/universal-provider/providers/test';
const AUTH_KEY='cli-proxy-auth';
const SALT='cli-proxy-api-webui::secure-storage';
const PROTO={'openai':'OpenAI 兼容','openai-response':'OpenAI Responses','claude':'Anthropic','gemini':'Gemini'};
let auth=null,cfg=null,providers=[],editing=-1,fetched=[];
const $=i=>document.getElementById(i);
const st=m=>{$('status').textContent=m};
function el(t,x){const n=document.createElement(t);if(x!==undefined)n.textContent=x;return n}
function lines(v){return String(v||'').split('\n').map(s=>s.trim()).filter(Boolean)}
function xor(a,k){const o=new Uint8Array(a.length);for(let i=0;i<a.length;i++)o[i]=a[i]^k[i%k.length];return o}
function deobf(raw){
 if(raw.startsWith('enc::v1::')){
  const b=Uint8Array.from(atob(raw.slice(9)),c=>c.charCodeAt(0));
  const k=new TextEncoder().encode(SALT+'|'+location.host+'|'+navigator.userAgent);
  return new TextDecoder().decode(xor(b,k));
 }
 return raw;
}
function readAuth(){
 try{
  const raw=localStorage.getItem(AUTH_KEY);
  if(!raw)return null;
  const s=(JSON.parse(deobf(raw))||{}).state||{};
  if(!s.managementKey)return null;
  const base=String(s.apiBase||location.origin).replace(/\/+$/,'').replace(/\/v0\/management$/,'');
  return {base:base,key:String(s.managementKey)};
 }catch(e){return null}
}
async function call(method,path,body){
 const r=await fetch(auth.base+path,{method:method,credentials:'same-origin',
  headers:Object.assign({Authorization:'Bearer '+auth.key},body?{'Content-Type':'application/json'}:{}),
  body:body?JSON.stringify(body):undefined});
 const t=await r.text();
 if(!r.ok){
  if(r.status===401)throw Error('登录凭据无效或已过期：请在管理面板重新登录后刷新本页。');
  throw Error('HTTP '+r.status+' '+t.slice(0,200));
 }
 return t?JSON.parse(t):{};
}
function legacy(c){return {id:'legacy',name:'旧版供应商',enabled:true,protocol:c.protocol||'openai','base-url':c['base-url']||'',headers:c.headers||{},'api-key-entries':c['api-key-entries']||[],models:c.models||[],'image-generation':!!c['image-generation'],'reasoning-effort':c['reasoning-effort']||'auto'}}
async function load(){
 st('正在加载…');
 try{cfg=await call('GET',CFG)}catch(e){st('加载失败：'+e.message);$('save').disabled=true;return}
 providers=Array.isArray(cfg.providers)?structuredClone(cfg.providers):((cfg.protocol||cfg['base-url']||Array.isArray(cfg.models))?[legacy(cfg)]:[]);
 render();$('save').disabled=false;
 st('已加载 '+providers.length+' 个供应商。');
}
function render(){
 const tb=$('rows');tb.replaceChildren();
 $('empty').hidden=providers.length>0;
 providers.forEach((p,i)=>{
  const tr=document.createElement('tr');
  const c1=document.createElement('td');const bd=el('span',PROTO[p.protocol]||p.protocol);bd.className='badge';c1.append(bd);tr.append(c1);
  tr.append(el('td',(p.name||'')+'（'+(p.id||'')+'）'));
  tr.append(el('td',p['base-url']||''));
  tr.append(el('td',String((p.models||[]).length)));
  tr.append(el('td',String((p['api-key-entries']||[]).length)));
  const c6=document.createElement('td');const tg=document.createElement('input');tg.type='checkbox';tg.className='toggle';tg.title=p.enabled?'已启用，点击停用':'已停用，点击启用';tg.checked=!!p.enabled;tg.addEventListener('change',()=>{p.enabled=tg.checked});c6.append(tg);tr.append(c6);
  const c7=document.createElement('td');c7.className='keyrow';
  const eb=el('button','编辑');eb.addEventListener('click',()=>openEditor(i));
  const db=el('button','删除');db.className='danger';db.addEventListener('click',()=>{if(confirm('确定删除供应商「'+(p.name||p.id)+'」？点击“保存更改”后生效。')){providers.splice(i,1);render()}});
  c7.append(eb,db);tr.append(c7);tb.append(tr);
 });
}
function keyRow(orig){
 const row=document.createElement('div');row.className='keyrow';row._orig=orig||null;
 if(orig)row.append(el('span','已有密钥'));
 const pwd=document.createElement('input');pwd.type='password';pwd.autocomplete='new-password';pwd.placeholder=orig?'留空保持已保存密钥不变':'API 密钥';pwd.style.flex='1';row._pwd=pwd;
 const px=document.createElement('input');px.placeholder='代理/Direct(可选)';px.style.width='150px';row._proxy=px;px.value=(orig&&orig['proxy-url'])||'';
 const w=document.createElement('input');w.type='number';w.min='0';w.max='1000000';w.title='权重：正数决定流量比例，非正数会排除此凭据';w.style.width='72px';w.value=(orig&&orig.weight)||1;row._weight=w;
 const rm=el('button','移除');rm.type='button';rm.addEventListener('click',()=>row.remove());
 row.append(pwd,px,w,rm);
 return row;
}

function modelEntryRow(m){
 const row=document.createElement('div');row.className='mentry';
 const top=document.createElement('div');top.className='mentry-top';
 const name=document.createElement('input');name.placeholder='上游模型名';name.value=m&&m.name||'';row._name=name;
 const arrow=el('span','→');arrow.className='mentry-arrow';
 const alias=document.createElement('input');alias.placeholder='公开别名（可选）';alias.value=m&&m.alias||'';row._alias=alias;
 const x=el('button','✕');x.type='button';x.className='mentry-x';x.title='移除';
 x.addEventListener('click',()=>row.remove());
 top.append(name,arrow,alias,x);
 const adv=document.createElement('div');adv.className='mentry-adv';
 const disp=document.createElement('input');disp.placeholder='显示名（可选）';disp.value=m&&m['display-name']||'';row._disp=disp;
 const ctx=document.createElement('input');ctx.type='number';ctx.placeholder='上下文长度';ctx.min='0';ctx.value=(m&&m['max-context-length'])||'';row._ctx=ctx;
 const inmo=document.createElement('input');inmo.placeholder='输入模态: text,image';inmo.value=((m&&m['input-modalities'])||[]).join(',');row._inmo=inmo;
 const outmo=document.createElement('input');outmo.placeholder='输出模态: text,image';outmo.value=((m&&m['output-modalities'])||[]).join(',');row._outmo=outmo;
 adv.append(disp,ctx,inmo,outmo);
 row.append(top,adv);
 name.addEventListener('input',refreshTestModels);
 return row;
}
function readModelEntries(){
 return [].slice.call($('model-entries').children).map(row=>({
  name:row._name.value.trim(),
  alias:row._alias.value.trim(),
  'display-name':row._disp.value.trim(),
  'max-context-length':Number(row._ctx.value)||0,
  'input-modalities':row._inmo.value.split(',').map(x=>x.trim()).filter(Boolean),
  'output-modalities':row._outmo.value.split(',').map(x=>x.trim()).filter(Boolean)
 })).filter(m=>m.name);
}
function renderModelEntries(models){
 const box=$('model-entries');box.replaceChildren();
 (models&&models.length?models:[{name:''}]).forEach(m=>box.append(modelEntryRow(m)));
}

const editorForm=()=>$('editor').querySelector('form');
function openEditor(i){
 editing=i;
 const p=i<0?{id:'',name:'',enabled:true,protocol:'openai','base-url':'',headers:{},'api-key-entries':[],models:[],'image-generation':false,'reasoning-effort':'auto'}:providers[i];
 const f=editorForm();
 $('editor-title').textContent=i<0?'新增供应商':'编辑供应商';
 f.elements.id.value=p.id||'';f.elements.name.value=p.name||'';f.elements.protocol.value=p.protocol||'openai';f.elements['base-url'].value=p['base-url']||'';
 f.elements['reasoning-effort'].value=p['reasoning-effort']||'auto';f.elements['image-generation'].value=String(!!p['image-generation']);
 f.elements.priority.value=(p.priority===undefined||p.priority===null)?'':p.priority;
 f.elements.weight.value=(p.weight===undefined||p.weight===null)?'':p.weight;
 f.elements.prefix.value=p.prefix||'';f.elements['proxy-url'].value=p['proxy-url']||'';
 f.elements['disable-cooling'].value=(function(v){return v===true?'disable':v===false?'enable':'inherit'})(p['disable-cooling']);
 f.elements.websockets.checked=!!p.websockets;$('ws-row').hidden=f.elements.protocol.value!=='openai-response';
 f.elements['excluded-models'].value=(p['excluded-models']||[]).join('\n');
 f.elements.headers.value=Object.entries(p.headers||{}).map(kv=>kv[0]+': '+kv[1]).join('\n');
 const kc=$('keys');kc.replaceChildren.apply(kc,(p['api-key-entries']||[]).map(keyRow));
 renderModelEntries(p.models||[]);
 refreshTestModels();
 const ts=$("test-status");ts.hidden=true;ts.textContent="";
 $('editor').showModal();
}
$('add').addEventListener('click',()=>openEditor(-1));
editorForm().elements.protocol.addEventListener('change',()=>{$('ws-row').hidden=editorForm().elements.protocol.value!=='openai-response'});
$('add-key').addEventListener('click',()=>$('keys').append(keyRow(null)));
$('add-model').addEventListener('click',()=>{$('model-entries').append(modelEntryRow({name:''}));refreshTestModels()});
$('refresh').addEventListener('click',load);
$('save').addEventListener('click',async()=>{
 if(!cfg){st('配置尚未加载。');return}
 try{await call('PUT',CFG,Object.assign({},cfg,{providers:providers}))}catch(e){st('保存失败：'+e.message);return}
 st('已保存，CPA 正在异步热重载插件配置，稍后自动刷新。');
 setTimeout(load,1600);
});
$('apply').addEventListener('click',e=>{
 e.preventDefault();
 const f=editorForm(),g=n=>f.elements[n].value.trim();
 const headers={};lines(f.elements.headers.value).forEach(x=>{const i=x.indexOf(':');if(i>0)headers[x.slice(0,i).trim()]=x.slice(i+1).trim()});
 const keys=[].slice.call($('keys').children).map(row=>{
  const typed=row._pwd.value.trim();
  const keyVal=typed||(row._orig&&row._orig['api-key'])||'';
  if(!keyVal)return null;
  const entry=Object.assign({},row._orig||{});
  entry['api-key']=keyVal;
  const wv=Math.max(0,Number(row._weight.value));entry.weight=Number.isFinite(wv)&&wv>0?wv:(row._orig&&row._orig.weight)||1;
  const pv=row._proxy.value.trim();if(pv)entry['proxy-url']=pv;else delete entry['proxy-url'];
  return entry;
 }).filter(Boolean);
 if(!keys.length){st('请至少填写一条 API 密钥。');return}
 const models=readModelEntries();
 if(!models.length){st('请至少填写一个上游模型名。');return}
 if(editing>=0&&providers.some((q,i)=>i!==editing&&q.id===g('id'))){st('标识 ID 已被其他供应商使用。');return}
 if(editing>=0&&providers[editing].id!==g('id')&&!confirm('修改标识 ID 会同时更换公开模型别名前缀，确定继续？')){return}
 const p={id:g('id'),name:g('name'),enabled:editing<0?true:providers[editing].enabled,protocol:f.elements.protocol.value,'base-url':g('base-url'),priority:g('priority')===''?undefined:Number(g('priority')),'weight':g('weight')===''?undefined:Number(g('weight')),'prefix':g('prefix'),'proxy-url':g('proxy-url'),'disable-cooling':(function(v){return v==='disable'?true:v==='enable'?false:undefined})(f.elements['disable-cooling'].value),websockets:f.elements.websockets.checked,'excluded-models':lines(f.elements['excluded-models'].value.replace(/,/g,'\n')),'api-key-entries':keys,models:models,'image-generation':f.elements['image-generation'].value==='true','reasoning-effort':f.elements['reasoning-effort'].value};
 if(editing<0)providers.push(p);else providers[editing]=p;
 $('editor').close();render();st('已更新列表，点击“保存更改”生效。');
});
$('fetch-models').addEventListener('click',async()=>{
 fetched=[];$('model-box').replaceChildren();picked.clear();
 $('models-status').textContent='正在通过 CPA 拉取上游模型…';
 try{const r=await call('POST',MODELS,{id:editorForm().elements.id.value.trim()});fetched=r.models||[]}catch(e){$('models-status').textContent='拉取失败：'+e.message;return}
 $('models-status').textContent='共获取 '+fetched.length+' 个上游模型，勾选后点击“添加所选”。';
 drawFetched($('model-search').value.trim().toLowerCase());
 $('models-dlg').showModal();
});
function drawFetched(q){
 const box=$('model-box');box.replaceChildren();
 const existing=new Set(readModelEntries().map(m=>m.name));
 let shown=0;
 fetched.forEach(m=>{
  if(q&&(m.name+' '+(m['display-name']||'')).toLowerCase().indexOf(q)<0)return;
  shown++;
  const row=el('label');row.className='disc-row';row._name=m.name;
  const cb=document.createElement('input');cb.type='checkbox';cb.disabled=existing.has(m.name);
  cb.checked=!cb.disabled&&picked.has(m.name);
  cb.addEventListener('change',()=>{cb.checked?picked.add(m.name):picked.delete(m.name)});
  const meta=el('span');meta.className='disc-meta';
  const nameLine=el('div');nameLine.className='disc-name';
  nameLine.append(el('span',m.name));
  if(existing.has(m.name)){const chip=el('span','已添加');chip.className='mchip';nameLine.append(chip)}
  meta.append(nameLine);
  if(m['display-name']){const d=el('div',m['display-name']);d.className='disc-desc';meta.append(d)}
  row.append(cb,meta);
  box.append(row);
 });
 $('models-count').textContent=fetched.length?('共 '+fetched.length+' 个，当前显示 '+shown+' 个，已选 '+picked.size+' 个'):'';
 if(!box.children.length)box.append(el('p','无匹配模型。'));
}
function refreshTestModels(){
 const sel=$('test-model');
 const cur=sel.value;
 const names=readModelEntries().map(m=>m.name);
 sel.replaceChildren(...names.map(n=>{const o=document.createElement('option');o.value=n;o.textContent=n;return o}));
 if(names.includes(cur))sel.value=cur;
}
function setTestStatus(kind,msg){
 const el2=$('test-status');
 el2.hidden=false;
 el2.className='statusbadge '+kind;
 el2.textContent=msg;
}
$('test-provider').addEventListener('click',async()=>{
 const id=editorForm().elements.id.value.trim();
 const model=$('test-model').value.trim();
 if(!model){setTestStatus('error','请先添加要测试的模型');return}
 const btn=$('test-provider');btn.disabled=true;
 $('test-status').hidden=false;$('test-status').className='statusbadge muted';$('test-status').textContent='测试中…';
 try{
  const r=await call('POST',MODELS_TEST,{id:id,model:model});
  if(r.ok)setTestStatus('success','连通性测试成功 · HTTP '+r.status+' · '+r['latency-ms']+'ms · '+r.models+' 个模型');
  else setTestStatus('error','失败：'+(r.error||('HTTP '+r.status)));
 }catch(e){setTestStatus('error','失败：'+e.message)}
 finally{btn.disabled=false}
});
$('model-search').addEventListener('input',()=>drawFetched($('model-search').value.trim().toLowerCase()));
$('sel-visible').addEventListener('click',()=>{[].slice.call($('model-box').querySelectorAll('.disc-row input[type=checkbox]:not(:disabled)')).forEach(cb=>{cb.checked=true;picked.add(cb.closest('.disc-row')._name)});drawFetched($('model-search').value.trim().toLowerCase())});
$('clear-sel').addEventListener('click',()=>{picked.clear();drawFetched($('model-search').value.trim().toLowerCase())});
$('merge-models').addEventListener('click',()=>{
 const have=new Set(readModelEntries().map(m=>m.name));
 const box=$('model-entries');
 [].slice.call(box.children).forEach(row=>{if(!row._name.value.trim())row.remove()});
 const add=[...picked].filter(n=>!have.has(n)).sort();
 add.forEach(n=>box.append(modelEntryRow({name:n})));
 picked.clear();drawFetched($('model-search').value.trim().toLowerCase());
 $('models-status').textContent=add.length?('已添加 '+add.length+' 个模型。'):'所选模型均已存在。';
 refreshTestModels();
});
$('editor').addEventListener('close',()=>{picked.clear()});
let booted=false;
function boot(){
 if(booted)return;booted=true;
 auth=readAuth();
 if(!auth){st('未能自动读取管理面板登录凭据：请登录 CPA 管理面板后重新打开本页。');return}
 load();
}
boot();
})();
</script></body></html>`
