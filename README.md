# CPA Universal Provider 原生插件

`universal-provider` 是 CLIProxyAPI（CPA）的 Go `c-shared` 原生插件。它把一组用户声明的模型路由到一个 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 或 Gemini GenerateContent 兼容上游，并由插件自己的静态 API Key 池执行请求。

## 能力

插件同时声明 CPA 的 `model_provider`、`model_router`、`executor` 与 `scheduler` 能力：

- `model_provider` 注册配置中的模型、别名、显示名称、上下文长度和输入/输出模态；
- `model_router` 仅接管已配置模型并路由到自身 executor；
- `executor` 生成协议正确的 URL 和认证头，通过 CPA host callback 发起 HTTP；
- `scheduler` 可处理真实 host auth 候选；配置内静态凭据则由 executor 内部的平滑加权轮询（SWRR）选择，因为 CPA ABI **没有**把插件配置内凭据注册成 host auth candidate 的虚构接口。

## 构建与验证

要求 Go（SDK v7.2.137 会按 Go toolchain 机制使用 Go 1.26）和 C 编译工具链：

```bash
go mod tidy
gofmt -w *.go
go test ./...
go vet ./...
make build
make test-abi
```

产物为 `bin/universal-provider.so`。将 `.so` 复制到 CPA `plugins.dir`，并按 `config.example.yaml` 合并配置。文件基本名即插件 ID，必须是 `universal-provider`。

## 配置

- `protocol`：严格四选一：`openai`、`openai-response`、`claude`、`gemini`；这些是 CPA SDK 的官方格式标识。
- `base-url`：绝对 `http(s)` API 根 URL，不以 `/` 结尾。
- `headers`：附加上游请求头。认证头最终由插件覆盖，避免客户端伪造认证。
- `api-key-entries`：`api-key` 与可选 `weight`。省略权重为 1，`<=0` 排除，超过 1,000,000 拒绝。配置内有效 key 至少一个。
- `models`：`name` 是上游模型名；`alias` 是客户端模型名（省略则等于 `name`）；还支持 `display-name`、`max-context-length`、`input-modalities`、`output-modalities`。
- `image-generation`：关闭时注册模型会去掉 `image` 输出模态，并拒绝 `modalities:["image"]` 或显式 `image_generation` / `generate_image` 工具等明显图像生成请求；开启时允许图像输出声明和这类请求。
- `reasoning-effort`：`auto`、`none`、`minimal`、`low`、`medium`、`high`、`xhigh`。`auto` 不改写已有推理字段。其他值写入：OpenAI `reasoning_effort`、Responses `reasoning.effort`、Claude `thinking`、Gemini `generationConfig.thinkingConfig.thinkingLevel`。Claude 的等级映射为禁用或 1024/4096/8192/16384 token 预算；Gemini `none` 合理降级为 `minimal`。

配置解析使用 YAML `KnownFields` 严格校验，同时接受 CPA 注入的 `enabled` 和 `priority`。热重载先完整解析和验证新快照，成功后才原子替换旧配置；失败时旧配置继续工作。实现不写日志，因此不会把 API Key 输出到日志。

## 协议 URL 与认证

| protocol | URL 后缀 | 认证 |
|---|---|---|
| `openai` | `/chat/completions` | `Authorization: Bearer <api-key>` |
| `openai-response` | `/responses` | `Authorization: Bearer <api-key>` |
| `claude` | `/messages` | `x-api-key` + 默认 `anthropic-version: 2023-06-01` |
| `gemini` | `/models/{model}:generateContent`；流式为 `streamGenerateContent?alt=sse` | `x-goog-api-key` |

非流式请求只调用 `host.http.do`。流式请求调用 `host.http.do_stream`，循环 `host.http.stream_read`，始终 `host.http.stream_close`；下游使用 `host.stream.emit` 与 `host.stream.close`。插件没有直接使用 `net/http.Client`。

插件不会把 CPA 前端认证凭据转发给第三方上游：Claude/Gemini 模式会清除入站 `Authorization`，随后只写入对应协议的上游认证头；OpenAI 两种模式会覆盖 `Authorization`。

## 关键限制

1. CPA 当前插件 ABI 没有“从插件配置注册静态 auth”的接口，所以这些 key 不会出现在 CPA auth 管理列表；SWRR 在 executor 内完成。插件的 scheduler 只对 CPA 实际提供的 auth candidates 做选择。
2. 本插件只声明模型并执行已有模型入口请求，**不注册 `/v1/images`**；CPA 插件 API 也未提供这种前端模型端点注册能力。`image-generation` 只控制模型输出声明和对明显图像生成意图的许可。
3. executor 声明单一配置协议作为输入/输出格式。跨协议客户端请求能否使用，取决于 CPA 对该协议组合是否有请求及响应转换器。
4. 上游响应体只在结构化错误中摘取 `error.message`，不回显或记录凭据。项目未配置真实上游密钥，因此测试使用纯单元/ABI 构建验证，不声称完成真实付费 API 调用。

项目仓库：<https://github.com/cary17/cpa-universal-provider-plugin>
