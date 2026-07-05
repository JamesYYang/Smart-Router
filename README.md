# SmartRouter

**SmartRouter** 是一个 OpenAI 协议兼容的 LLM 网关，将多家海内外模型供应商聚合到统一 API 之下，提供路由、故障切换、成本控制、缓存、护栏（Guardrails）和可观测性能力。项目由 [GoModel](https://github.com/ENTERPILOT/GoModel) 引导而来（详见 `NOTICE.md`），并计划在其基础上叠加"任务级智能模型路由"能力（见 `docs/design/`）。

## 特性

- **统一 API**：兼容 OpenAI 的 `/v1/chat/completions`、`/v1/responses`、`/v1/embeddings`、`/v1/conversations`、Batch API、Realtime（语音）等端点。
- **多供应商聚合**：内置支持 OpenAI、Anthropic、Gemini/Vertex、AWS Bedrock、Azure OpenAI、xAI、Groq、Fireworks、OpenRouter、DeepSeek、Z.ai、MiniMax、Xiaomi MiMo、阿里百炼（Bailian）、Oracle、Ollama、vLLM 及任意 OpenAI 兼容端点。
- **虚拟模型 / 智能路由**：支持别名、`round_robin` 与 `cost`（自动选最低成本可用目标）两种负载均衡策略。
- **故障切换（Failover）**：按模型配置回退链路，支持重试、退避与熔断。
- **成本与预算控制**：用量记录、定价覆盖、按 Key/路径的预算限额。
- **响应缓存**：精确匹配缓存与语义缓存（对接 Redis / Qdrant / pgvector / Pinecone / Weaviate）。
- **护栏（Guardrails）**：系统提示词注入、基于 LLM 的内容改写/脱敏等可插拔规则。
- **治理与可观测性**：审计日志、基于 Header 的请求打标签、Prometheus 指标、Admin 管理 API 与 Dashboard、实时日志推送。
- **透传路由**：`/p/{provider}/...` 直接透传到指定供应商的原生 API。
- **可扩展**：通过 `ext` 包注册自定义中间件、路由与请求改写器，构建定制化网关二进制文件。

## 快速开始

### 环境要求

- Go 1.26.4+

### 运行

无需任何配置文件即可启动（所有配置项都有合理默认值）：

```bash
go run ./cmd/gomodel
```

默认监听 `:8080`。设置至少一个供应商的 API Key 才能实际转发请求，例如：

```bash
export OPENAI_API_KEY=sk-...
go run ./cmd/gomodel
```

发送测试请求：

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-secret-key" \
  -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}'
```

### 构建

```bash
go build ./...
```

包含 Swagger 文档的构建（需要 `swagger` build tag）：

```bash
go build -tags=swagger ./...
```

### 命令行参数

```bash
go run ./cmd/gomodel --version          # 打印版本信息
go run ./cmd/gomodel --health           # 健康检查（liveness）后退出
go run ./cmd/gomodel --ready            # 就绪检查（readiness）后退出
```

## 配置

配置文件是可选的：复制 `config/config.example.yaml` 到 `config/config.yaml` 进行自定义，**环境变量始终优先于配置文件**。`config.example.yaml` 中每一项都注明了对应的环境变量名。

关键配置分区：

| 分区 | 说明 |
|---|---|
| `server` | 端口、Base Path、Master Key、Swagger/pprof 开关、透传路由 |
| `models` | 模型可见性与已配置模型列表的匹配策略（`fallback` / `allowlist`） |
| `tagging` | 基于请求 Header 的标签提取，用于用量统计和审计 |
| `virtual_models` | 虚拟模型：别名、负载均衡（`round_robin`/`cost`）、访问策略 |
| `cache` | 模型注册表缓存、精确/语义响应缓存 |
| `storage` | 存储后端：`sqlite`（默认）、`postgresql`、`mongodb` |
| `logging` | 审计日志（是否记录请求体/Header、保留天数） |
| `usage` / `budgets` | 用量记录、成本计费、按路径的预算限额 |
| `metrics` | Prometheus 指标端点 |
| `resilience` / `failover` | 重试退避策略、模型级故障切换规则 |
| `guardrails` | 系统提示词注入、LLM 改写等内容护栏规则 |
| `providers` | 各供应商的 API Key、Base URL 及模型列表 |

完整示例与逐项注释见 [`config/config.example.yaml`](config/config.example.yaml)。

## 项目结构

```
cmd/gomodel        # 主二进制入口（含 swagger 可选构建）
cmd/recordapi       # 辅助工具
run/                # 可复用的网关生命周期入口（CLI 解析、启停、探针）
ext/                # 扩展点注册表（自定义中间件/路由/请求改写器）
config/             # 配置加载与示例配置
internal/app        # 应用装配层：按配置初始化并串联所有模块
internal/core        # 共享领域模型与协议编解码
internal/gateway     # 推理编排：模型解析、分发、故障切换、用量记录
internal/server      # HTTP handler 层：路由、鉴权、中间件
internal/providers   # 供应商注册表、路由器及各供应商适配器（每个供应商一个子包）
internal/virtualmodels  # 虚拟模型（别名 / 负载均衡）
internal/failover    # 故障切换与熔断
internal/budget, usage, pricingoverrides  # 预算、用量、定价覆盖
internal/guardrails  # 内容护栏
internal/workflows   # 将护栏/改写规则编译为可执行的请求处理流水线
internal/responsecache  # 精确/语义响应缓存
internal/admin       # 管理 REST API 与 Dashboard
internal/observability  # Prometheus 指标
docs/design/         # 架构与设计文档
```

关于一次请求从进入到返回的完整处理链路（中间件顺序、Prepare/预算/缓存/推理编排/后处理各阶段），详见 [`docs/design/GoModel-Architecture.md`](docs/design/GoModel-Architecture.md)。产品方向与"任务级智能模型选择"设计见 [`docs/design/Smart-Router.md`](docs/design/Smart-Router.md) 与 [`docs/design/TaskRouting-Design.md`](docs/design/TaskRouting-Design.md)。

## 测试

```bash
go test ./...                                   # 全量测试
go test ./internal/gateway/...                  # 单个包
go test ./internal/gateway/... -run TestName -v # 单个测试
```

## 许可证

MIT License，见 [`LICENSE`](LICENSE)。本项目由 GoModel 引导而来，第三方版权声明见 [`NOTICE.md`](NOTICE.md)。
