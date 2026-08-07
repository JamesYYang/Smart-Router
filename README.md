# SmartRouter

**SmartRouter** 是一个 OpenAI 协议兼容的 LLM 网关，将多家海内外模型供应商聚合到统一 API 之下，提供路由、故障切换、成本控制、缓存、护栏（Guardrails）和可观测性能力。项目由 [GoModel](https://github.com/ENTERPILOT/GoModel) 引导而来（详见 `NOTICE.md`），并在其基础上叠加了"任务级智能模型路由"能力（P0/MVP 版本，规则引擎，见 `docs/design/`）以及基于子域名的**多租户 / SaaS 能力**（见下方[「多租户 / SaaS 模式」](#多租户--saas-模式)）。

## 特性

- **统一 API**：兼容 OpenAI 的 `/v1/chat/completions`、`/v1/responses`、`/v1/embeddings`、`/v1/conversations`、Batch API、Realtime（语音）等端点。
- **多供应商聚合**：内置支持 OpenAI、Anthropic、Gemini/Vertex、AWS Bedrock、Azure OpenAI、xAI、Groq、Fireworks、OpenRouter、DeepSeek、Z.ai、MiniMax、Xiaomi MiMo、阿里百炼（Bailian）、Oracle、Ollama、vLLM 及任意 OpenAI 兼容端点，完整列表见[下方「支持的模型供应商」](#支持的模型供应商)。
- **虚拟模型 / 智能路由**：支持别名、`round_robin` 与 `cost`（自动选最低成本可用目标）两种负载均衡策略。
- **任务级智能路由（TaskRouting，P0/MVP）**：对指定 entrypoint 虚拟模型的请求，基于规则引擎自动分类任务类型（`agent_tool_use`/`code`/`translation`/`title_generation`/`bulk_low_value`/`copywriting`/`data_extraction`/`general`），改写到对应的虚拟模型后复用现有虚拟模型解析/负载均衡/故障切换链路，默认关闭。
- **故障切换（Failover）**：按模型配置回退链路，支持重试、退避与熔断。
- **成本与预算控制**：用量记录、定价覆盖、按 Key/路径的预算限额。
- **响应缓存**：精确匹配缓存与语义缓存（对接 Redis / Qdrant / pgvector / Pinecone / Weaviate）。
- **护栏（Guardrails）**：系统提示词注入、基于 LLM 的内容改写/脱敏等可插拔规则。
- **治理与可观测性**：审计日志、基于 Header 的请求打标签、Prometheus 指标、Admin 管理 API 与 Dashboard、实时日志推送。
- **透传路由**：`/p/{provider}/...` 直接透传到指定供应商的原生 API。
- **多租户 / SaaS**：基于子域名的租户解析、平台/租户两级 Admin API 与 Dashboard、两级鉴权 Key、按租户隔离的数据与配置存储、租户级配额，可将网关以 SaaS 形式对外运营，默认关闭（不配置 `base_domain` 时行为与单租户部署完全一致）。
- **可扩展**：通过 `ext` 包注册自定义中间件、路由与请求改写器，构建定制化网关二进制文件。

## 支持的模型供应商

模型以 `provider/model-id` 的形式寻址（如 `openai/gpt-4o-mini`、`anthropic/claude-sonnet-4-6`）。每个供应商在 `providers` 配置中对应一个 `type`，具体模型列表默认通过供应商自身的 `/models` 接口动态发现（定价等元数据来自 [ai-model-list](https://github.com/ENTERPILOT/ai-model-list) 注册表，定期刷新），也可以在配置中显式声明 `models` 列表（`fallback` 或 `allowlist` 模式，见 [`models.configured_provider_models_mode`](#配置)）。

| 供应商 | `type` | 说明 |
|---|---|---|
| OpenAI | `openai` | 官方 API |
| Anthropic | `anthropic` | 原生 Messages API |
| Google Gemini | `gemini` | 默认走原生 `generateContent` API，可切换 `api_mode: openai_compatible` |
| Google Vertex AI | `vertex` | 支持 GCP ADC 或 Service Account 鉴权 |
| AWS Bedrock | `bedrock` | 无独立 API Key，走标准 AWS 凭证链，支持 Inference Profile |
| Azure OpenAI | `azure` | 需额外配置 `api_version` |
| xAI（Grok） | `xai` | |
| Groq | `groq` | |
| Fireworks AI | `fireworks` | |
| OpenRouter | `openrouter` | 聚合多家模型的第三方路由服务 |
| DeepSeek | `deepseek` | 无原生 Responses API，网关自动转译 `/v1/responses` 请求 |
| Z.ai（智谱 GLM） | `zai` | 可切换到 GLM Coding Plan 专用端点 |
| MiniMax | `minimax` | |
| 小米 MiMo（Xiaomi） | `xiaomi` | |
| 阿里百炼（Bailian/DashScope） | `bailian` | 支持新加坡/法兰克福/香港等多地域 Endpoint |
| Oracle | `oracle` | OpenAI 兼容端点 |
| Ollama | `ollama` | 本地/自建 Ollama 服务 |
| vLLM | `vllm` | 自建 vLLM 服务 |
| OpenCode Zen（Go 订阅） | `opencode_go` | 按模型自动路由到 OpenAI 兼容或 Anthropic 原生端点 |
| 任意 OpenAI 兼容端点 | `openai`（或 `vllm`） | 例如 LM Studio、自建网关，只需设置 `base_url` |

## 快速开始

### 环境要求

- Go 1.26.4+

### 运行

无需任何配置文件即可启动（所有配置项都有合理默认值）：

```bash
go run ./cmd/smartrouter
```

默认监听 `:8080`。设置至少一个供应商的 API Key 才能实际转发请求，例如：

```bash
export OPENAI_API_KEY=sk-...
go run ./cmd/smartrouter
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
go run ./cmd/smartrouter --version          # 打印版本信息
go run ./cmd/smartrouter --health           # 健康检查（liveness）后退出
go run ./cmd/smartrouter --ready            # 就绪检查（readiness）后退出
```

## 配置

配置文件是可选的：复制 `config/config.example.yaml` 到 `config/config.yaml` 进行自定义，**环境变量始终优先于配置文件**。`config.example.yaml` 中每一项都注明了对应的环境变量名。

关键配置分区：

| 分区 | 说明 |
|---|---|
| `server` | 端口、Base Path、Master Key、Swagger/pprof 开关、透传路由、多租户（`base_domain`/`platform_host`/`bootstrap_default_tenant`） |
| `models` | 模型可见性与已配置模型列表的匹配策略（`fallback` / `allowlist`） |
| `tagging` | 基于请求 Header 的标签提取，用于用量统计和审计 |
| `virtual_models` | 虚拟模型：别名、负载均衡（`round_robin`/`cost`）、访问策略 |
| `task_routing` | 任务级智能路由（P0/MVP）：entrypoint 列表、任务类型 → 虚拟模型映射，默认关闭 |
| `cache` | 模型注册表缓存、精确/语义响应缓存 |
| `storage` | 存储后端：`sqlite`（默认）、`postgresql`、`mongodb` |
| `logging` | 审计日志（是否记录请求体/Header、保留天数） |
| `usage` / `budgets` | 用量记录、成本计费、按路径的预算限额 |
| `metrics` | Prometheus 指标端点 |
| `resilience` / `failover` | 重试退避策略、模型级故障切换规则 |
| `guardrails` | 系统提示词注入、LLM 改写等内容护栏规则 |
| `providers` | 各供应商的 API Key、Base URL 及模型列表 |

完整示例与逐项注释见 [`config/config.example.yaml`](config/config.example.yaml)。

## 多租户 / SaaS 模式

设置 `server.base_domain` 后，网关从单租户模式切换为多租户 SaaS 模式：

- **子域名租户解析**：`<subdomain>.<base_domain>` 自动解析到对应租户；`<platform_host>.<base_domain>`（`server.platform_host`，默认 `app`）是平台管理域，不属于任何租户。
- **Host 分流（Host Guard）**：`/v1/*` 推理路由只在租户域下可用（平台域访问返回 404）；`/admin/tenants` 等租户管理路由只在平台域下可用（租户域访问返回 404）——同一套后端按请求 Host 分流到 `PlatformAdminHandler` 或 `TenantAdminHandler`。
- **两级鉴权 Key**：`master_key` 全局生效；平台管理员用 master key 为租户签发 tenant admin key，该 Key 只能在对应租户域下使用；租户下的普通 API Key 归属单一租户，不能跨租户访问数据。
- **数据与配置隔离**：虚拟模型、护栏、预算、用量、审计日志、鉴权 Key、会话/响应存储等均按 `tenant_id` 隔离（SQLite/PostgreSQL/MongoDB 三种存储后端均支持）。
- **租户级配额**：`/v1` 请求经配额中间件校验租户预算，超限返回 402。
- **Dashboard 角色感知**：平台域登录 Dashboard 可见「Tenants」管理页（新建/编辑/停用租户）；租户域登录仅能管理本租户数据；其余页面（Overview/Models/Usage/Budgets 等）两种角色共用。
- **空库自动 bootstrap**：首次启动且未配置 `base_domain` 或空库时，自动创建 `default` 租户，保证从单租户平滑过渡。
- **零配置兼容**：不设置 `base_domain` 时，以上机制完全不生效，行为与单租户部署完全一致（Dashboard 恒为平台视角）。

完整部署步骤（启动流程、检查清单、已知限制）见 [`docs/deployment/multi-tenant.md`](docs/deployment/multi-tenant.md)；设计文档见 [`docs/superpowers/specs/2026-08-02-saas-multi-tenant-design.md`](docs/superpowers/specs/2026-08-02-saas-multi-tenant-design.md)。

## HTTP 路由

> 所有路由均以 `server.base_path`（默认 `/`）为前缀。带 🔒 标记的路由需要 `Authorization: Bearer <master_key>` 认证；带 🏢 标记的路由仅在多租户模式的平台域（`platform_host.base_domain`）下可访问，租户域访问返回 404。

### 系统

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | Liveness 探针 |
| GET | `/health/ready` | Readiness 探针 |
| GET | `/metrics` | Prometheus 指标（需 `metrics.enabled: true`，路径可配置） |
| GET | `/swagger/*` | Swagger UI（需 `-tags=swagger` 构建且 `swagger_enabled: true`） |
| GET | `/debug/pprof*` | Go pprof 分析端点（需 `pprof_enabled: true`） |

### 供应商透传（Passthrough）

需 `enable_passthrough_routes: true`（默认开启），通过 `enabled_passthrough_providers` 配置允许的供应商列表。

| 方法 | 路径 | 说明 |
|------|------|------|
| GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS | `/p/:provider/*` | 🔒 透传到指定供应商原生 API，如 `/p/bailian/v1/chat/completions` |

### 推理 API（OpenAI 兼容）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET  | `/v1/models` | 🔒 列出所有可用模型 |
| POST | `/v1/chat/completions` | 🔒 Chat 对话补全（流式/非流式） |
| POST | `/v1/embeddings` | 🔒 文本向量化 |
| POST | `/v1/audio/speech` | 🔒 文字转语音（TTS） |
| POST | `/v1/audio/transcriptions` | 🔒 语音转文字（STT） |
| GET  | `/v1/realtime` | 🔒 Realtime 语音 WebSocket 升级（需 `realtime_enabled: true`） |

### Responses API

| 方法 | 路径 | 说明 |
|------|------|------|
| POST   | `/v1/responses` | 🔒 创建 Response |
| GET    | `/v1/responses/:id` | 🔒 获取 Response |
| DELETE | `/v1/responses/:id` | 🔒 删除 Response |
| POST   | `/v1/responses/:id/cancel` | 🔒 取消 Response |
| GET    | `/v1/responses/:id/input_items` | 🔒 列出 Response 的输入项 |
| POST   | `/v1/responses/input_tokens` | 🔒 预估输入 Token 数 |
| POST   | `/v1/responses/compact` | 🔒 压缩 Response 上下文 |

### Messages API（Anthropic 兼容）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/messages` | 🔒 Anthropic 风格消息补全 |
| POST | `/v1/messages/count_tokens` | 🔒 预估 Token 数 |

### Conversations API

| 方法 | 路径 | 说明 |
|------|------|------|
| POST   | `/v1/conversations` | 🔒 创建会话 |
| GET    | `/v1/conversations/:id` | 🔒 获取会话 |
| POST   | `/v1/conversations/:id` | 🔒 更新会话 |
| DELETE | `/v1/conversations/:id` | 🔒 删除会话 |

### Files API

| 方法 | 路径 | 说明 |
|------|------|------|
| POST   | `/v1/files` | 🔒 上传文件 |
| GET    | `/v1/files` | 🔒 列出文件 |
| GET    | `/v1/files/:id` | 🔒 获取文件元数据 |
| DELETE | `/v1/files/:id` | 🔒 删除文件 |
| GET    | `/v1/files/:id/content` | 🔒 下载文件内容 |

### Batch API

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/batches` | 🔒 创建批处理任务 |
| GET  | `/v1/batches` | 🔒 列出批处理任务 |
| GET  | `/v1/batches/:id` | 🔒 获取批处理任务状态 |
| POST | `/v1/batches/:id/cancel` | 🔒 取消批处理任务 |
| GET  | `/v1/batches/:id/results` | 🔒 获取批处理结果 |

### Admin API

需 `admin.endpoints_enabled: true`。挂载于 `/admin`（同时保留 `/admin/api/v1` 旧路径，已标记为 Deprecated）。多租户模式下，`/admin/tenants*` 仅平台域可访问，其余 `/admin/*` 路由按当前 Host 自动限定在对应租户范围内（详见[「多租户 / SaaS 模式」](#多租户--saas-模式)）。

| 方法 | 路径 | 说明 |
|------|------|------|
| GET/POST | `/admin/tenants` | 🏢 平台域：列出 / 创建租户 |
| GET/PATCH/DELETE | `/admin/tenants/:id` | 🏢 平台域：获取 / 更新 / 停用租户 |
| POST | `/admin/tenants/:id/admin-keys` | 🏢 平台域：为租户签发 tenant admin key |
| GET  | `/admin/runtime/config` | 当前运行时配置 |
| POST | `/admin/runtime/refresh` | 热更新运行时配置 |
| GET  | `/admin/cache/overview` | 缓存概览 |
| GET  | `/admin/live/logs` | SSE 实时日志流 |
| GET  | `/admin/providers/status` | 供应商状态 |
| GET  | `/admin/models` | 模型列表 |
| GET  | `/admin/models/categories` | 模型分类 |
| GET/PUT/DELETE | `/admin/virtual-models` | 虚拟模型管理 |
| GET/PUT/DELETE | `/admin/failover` | 故障切换规则管理 |
| POST | `/admin/failover/reset` | 重置故障切换规则 |
| POST | `/admin/failover/generate` | 自动生成故障切换规则 |
| GET/PUT/DELETE | `/admin/guardrails` | 护栏规则管理 |
| GET  | `/admin/guardrails/types` | 护栏类型列表 |
| GET/POST | `/admin/workflows` | 工作流管理 |
| GET  | `/admin/workflows/guardrails` | 工作流护栏列表 |
| GET  | `/admin/workflows/:id` | 获取工作流详情 |
| POST | `/admin/workflows/:id/deactivate` | 停用工作流 |
| GET/PUT/DELETE | `/admin/budgets` | 预算管理 |
| GET/PUT | `/admin/budgets/settings` | 预算全局设置 |
| POST | `/admin/budgets/reset-one` | 重置单条预算 |
| POST | `/admin/budgets/reset` | 重置所有预算 |
| GET/PUT/DELETE | `/admin/model-pricing-overrides` | 模型定价覆盖 |
| GET/PUT | `/admin/tagging/settings` | 标签配置 |
| GET  | `/admin/usage/summary` | 用量汇总 |
| GET  | `/admin/usage/daily` | 日用量 |
| GET  | `/admin/usage/models` | 按模型用量 |
| GET  | `/admin/usage/user-paths` | 按 User Path 用量 |
| GET  | `/admin/usage/labels` | 按标签用量 |
| GET  | `/admin/usage/log` | 用量明细日志 |
| GET  | `/admin/usage/throughput` | Token 吞吐量 |
| POST | `/admin/usage/recalculate-pricing` | 重新计算历史定价 |
| GET  | `/admin/audit/log` | 审计日志列表 |
| GET  | `/admin/audit/detail` | 审计日志详情 |
| GET  | `/admin/audit/conversation` | 审计日志会话还原 |
| GET/POST | `/admin/auth-keys` | 鉴权 Key 管理 |
| PUT  | `/admin/auth-keys/:id/labels` | 更新 Key 标签 |
| POST | `/admin/auth-keys/:id/deactivate` | 停用 Key |

### Admin Dashboard UI

需 `admin.ui_enabled: true`。

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/admin/dashboard` | Dashboard 首页 |
| GET | `/admin/dashboard/*` | Dashboard 子页面 |
| GET | `/admin/static/*` | Dashboard 静态资源 |

## 项目结构

```
cmd/smartrouter        # 主二进制入口（含 swagger 可选构建）
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
internal/taskrouting # 任务级智能路由（P0/MVP）：规则分类器 + 虚拟模型改写
internal/tenants     # 租户实体、子域名解析、Bootstrap；internal/admin 按 Host 拆分 Platform/Tenant Admin Handler
internal/failover    # 故障切换与熔断
internal/budget, usage, pricingoverrides  # 预算、用量、定价覆盖
internal/guardrails  # 内容护栏
internal/workflows   # 将护栏/改写规则编译为可执行的请求处理流水线
internal/responsecache  # 精确/语义响应缓存
internal/admin       # 管理 REST API 与 Dashboard
internal/observability  # Prometheus 指标
docs/design/         # 架构与设计文档
docs/deployment/     # 部署指南（含多租户部署）
```

关于一次请求从进入到返回的完整处理链路（中间件顺序、Prepare/预算/缓存/推理编排/后处理各阶段），详见 [`docs/design/GoModel-Architecture.md`](docs/design/GoModel-Architecture.md)。产品方向与"任务级智能模型路由"设计见 [`docs/design/Smart-Router.md`](docs/design/Smart-Router.md) 与 [`docs/design/TaskRouting-Design.md`](docs/design/TaskRouting-Design.md)；P0/MVP 版本（规则分类器、`internal/taskrouting`）已实现并接入 `internal/gateway` 与 `internal/batch` 的 ModelResolver。多租户 SaaS 改造的设计文档见 `docs/superpowers/specs/2026-08-02-saas-multi-tenant-design.md`，各阶段（P1-P7）实现记录见 `docs/superpowers/ROADMAP.md`。

## 测试

```bash
go test ./...                                   # 全量测试
go test ./internal/gateway/...                  # 单个包
go test ./internal/gateway/... -run TestName -v # 单个测试
```

## 许可证

MIT License，见 [`LICENSE`](LICENSE)。本项目由 GoModel 引导而来，第三方版权声明见 [`NOTICE.md`](NOTICE.md)。
