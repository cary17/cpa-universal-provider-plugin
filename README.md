# CPA Universal Provider 原生插件

`universal-provider` v0.1.3 是 CLIProxyAPI（CPA）的 Go `c-shared` 原生插件。它把公开模型别名路由到多个独立配置的 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 或 Gemini GenerateContent 兼容上游。

## 功能

- `providers` 列表；每项有唯一 `id`/`name`、启用状态、协议、Base URL、headers、加权 API keys、models、图像开关与 reasoning effort。
- 供应商级可选字段：`priority`（数值大的优先层先被选中，再在同层内加权轮询）、`weight`、`prefix`（公开别名统一加前缀）、`proxy-url`（供应商级代理；每个 API key 可用 `proxy-url` 覆盖，填 `Direct` 表示强制直连）、`websockets`（仅 Responses 协议的传输声明，当前 ABI 预留）、`disable-cooling`（冷却策略覆盖：true 禁用 / false 启用 / 缺省跟随全局）与 `excluded-models`（支持 `*` 通配符，命中者不注册为公开模型）。
- 启用供应商间公开模型 alias 必须全局唯一；禁用供应商不注册模型。
- 路由把供应商 ID 编码在只供 executor 使用的内部 `TargetModel`，公开模型列表不会泄漏该定位值。
- 每个供应商拥有独立 API-key SWRR 池；凭据级冷却状态机在多次失败后临时跳过故障 key（`disable-cooling: true` 可关闭）。
- 管理 API：`POST /v0/management/plugins/universal-provider/providers/models`（按 id 经 host 回调拉取上游模型列表，返回脱敏结果）与 `POST .../providers/test`（连通性测试，返回 HTTP 状态、延迟与模型数）。
- 专属 `/v0/resource/plugins/universal-provider/providers` 管理页支持添加、编辑、复制、启停、删除（有二次确认）。资源路由只返回静态页面，不能修改配置；页面仅通过带 management Bearer key 的 CPA `GET/PUT /v0/management/plugins/universal-provider/config` 保存并触发 CPA 热重载。页面自动读取同源管理面板已保存的登录凭据（无需手动输入密钥），通过面板代理以保存的 CPA 管理密钥访问配置；支持官方风格的模型拉取（搜索/勾选/去重合并）、逐条模型编辑、连通性测试与排除模型。API key 输入不回显已保存值也不写日志，页面不加载第三方脚本并以 DOM `textContent` 渲染用户值。
- PUT 基于 GET 返回对象替换 `providers`，因此保留宿主的 `enabled`、`priority` 和未知宿主字段。

## 配置

参见 [`config.example.yaml`](config.example.yaml)。旧版单供应商的 `protocol`、`base-url`、`headers`、`api-key-entries`、`models`、`image-generation`、`reasoning-effort` 会在内存中迁移为 ID `legacy` 的单项 providers 配置；下次从管理 UI 保存时写成新结构。

四种协议标识为 `openai`、`openai-response`、`claude`、`gemini`。插件对 CPA 声明 canonical `openai` executor 输入/输出：CPA 先把不同客户端协议转换为 OpenAI，插件再使用 CPA SDK builtin translator 转为选中供应商协议；非流式与流式响应反向转回 OpenAI，最后由 CPA 转回客户端协议。

`reasoning-effort` 支持 `auto`、`none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max`。OpenAI/Responses 原样写入 `max`；Claude `max` 映射到 32768 thinking budget tokens；Gemini 没有对应 `max` 等级，因此映射为 `high`。`auto` 不覆盖请求。

关闭 `image-generation` 会从注册模型移除 image 输出模态，并拒绝明显的图像生成请求。插件不注册单独的 `/v1/images` 路由。

## 构建和验证

本环境若 `/tmp` 为 `noexec`，必须设置项目内 `GOTMPDIR`：

```bash
mkdir -p .gotmp
export GOTMPDIR="$PWD/.gotmp"
gofmt -w *.go
go test -count=1 ./...
go vet ./...
make build
make test-abi
```

本地 Linux 产物为 `bin/universal-provider.so`。插件只通过 CPA host HTTP callbacks 请求上游，不直接创建 `http.Client`；不会记录 API keys。

## 插件商店安装和更新

仓库包含 `.github/workflows/release.yml`。未来推送严格的 `v0.1.3` tag 时，Actions 为受支持平台构建：

```text
universal-provider_0.1.3_<goos>_<goarch>.zip
checksums.txt
```

每个 zip 根目录直接包含 `universal-provider.so`（Linux）或 `universal-provider.dylib`（macOS），符合 CPA `github-release` 插件商店解析规则。

将 [`registry-entry.example.json`](registry-entry.example.json) 的插件条目提交到 CPA 官方 registry，或把该 schema-v1 JSON 托管在固定 HTTPS URL 作为第三方 registry。CPA 配置中的插件 registry URL 列表加入该 URL 后，即可在 Plugin Store 在线安装；后续发布更高 semver tag 及同名平台归档和 `checksums.txt`，商店即可检测并执行更新。私有仓库需按 CPA 插件商店的 GitHub 认证配置提供访问权限。

## 安全与限制

- management resource 路由未认证，所以它只返回 UI；所有读写均走 CPA 已认证 management API。
- 页面从同源 localStorage 读取管理面板保存的登录凭据（CPAMP `cli-proxy-auth` 混淆格式）；凭据不存在或失效时仅提示重新登录面板，页面本身不收集、不存储任何密钥。
- API key 权重省略为 1；`<=0` 排除，最大 1,000,000；每个供应商至少一个有效 key。
- 配置严格使用 YAML `KnownFields` 校验。重配置成功后才原子替换 runtime state，失败时旧状态继续运行。
- 没有真实付费 API 集成测试；测试覆盖配置、迁移、路由、SWRR、协议转换、管理注册和 UI 安全字符串。

项目仓库：<https://github.com/cary17/cpa-universal-provider-plugin>
