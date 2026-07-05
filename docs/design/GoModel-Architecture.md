# GoModel 架构现状分析

> 本文档面向 SmartRouter 改造评估，梳理 GoModel 当前的功能模块划分与一次请求从进入到返回所经过的完整处理链路，作为后续"保留/简化/移除"决策的依据。

---

## 1. 功能模块清单（`internal/` 目录）

按照职责分为六类：**网关核心**、**Provider 适配**、**路由与成本控制**、**治理与合规**、**存储与状态**、**运维与可观测性**。

### 1.1 网关核心（不可移除）

| 模块 | 职责 |
|---|---|
| `core` | 领域模型（ChatRequest/Response、Model、Workflow、Selector 等）与协议编解码，所有模块共享的基础类型 |
| `gateway` | 推理编排器（`InferenceOrchestrator`）：模型解析、请求分发、故障切换、用量记录的核心逻辑 |
| `server` | HTTP handler 层：路由注册、中间件编排、请求/响应组装，对接 `gateway` |
| `providers` | Provider 注册表 + 路由器（`Router`），按 selector 把请求分发到具体 Provider 适配器 |
| `app` | 应用装配层，负责按配置初始化并串联以上所有模块 |
| `httpclient` | 出站 HTTP 客户端（超时、连接池配置） |
| `storage` | 存储抽象层（SQLite/PostgreSQL/MongoDB 的统一入口） |

### 1.2 Provider 适配（`internal/providers/*`，可按需增减）

| 类别 | Provider |
|---|---|
| 美国/国际厂商 | `openai`、`anthropic`、`gemini`、`vertex`（Google Cloud）、`xai`、`groq`、`fireworks`、`bedrock`（AWS）、`azure`、`oracle`、`openrouter`、`opencodego` |
| 中国厂商 | `bailian`（阿里百炼）、`zai`（智谱/Z.ai）、`minimax`、`xiaomi`（MiMo）、`deepseek` |
| 自托管/兼容 | `ollama`、`vllm` |
| 支撑代码 | `factory.go`、`registry.go`、`registry_cache.go`（模型发现缓存）、`router.go`（selector → provider 路由）、`passthrough.go`（透传路由） |

### 1.3 路由与成本控制

| 模块 | 职责 |
|---|---|
| `virtualmodels` | 虚拟模型（别名 / 负载均衡重定向），支持 `round_robin` 与 `cost`（自动选最低成本可用 target）两种策略；是"任务级/成本导向选模"的现成执行层 |
| `modelselectors` | Selector 字符串（`provider/model`）的解析与归一化，供 `virtualmodels`、`budget` 等模块共用 |
| `pricingoverrides` | 模型定价覆盖（管理员可手工修正/补充某模型的单价），供成本路由和用量计费使用 |
| `budget` | 预算限额检查（按 key/用户/标签维度），请求分发前的硬性拦截 |
| `usage` | 请求用量与成本记录（token 数、金额），驱动预算检查和看板统计 |
| `failover` | Provider/模型级失败重试与熔断（circuit breaker） |

### 1.4 治理与合规（企业级功能，PRD 未要求）

| 模块 | 职责 | 规模 |
|---|---|---|
| `guardrails` | 内容护栏：请求/响应改写、敏感词过滤、LLM 自我审查等流水线 | 7470 行 / 30 文件 |
| `workflows` | 请求处理流水线编排（compiler + executor），把 guardrails/改写规则编译成可执行 workflow，**启动时会自动创建一个默认全局 workflow，无法整体关闭** | 4221 行 / 15 文件 |
| `tagging` | 基于 HTTP Header 的请求打标签，用于审计和用量细分 | 752 行 / 8 文件 |
| `auditlog` | 请求/响应审计日志（可选记录 body、header） | — |

### 1.5 存储与状态（OpenAI 兼容周边能力，PRD 未提及）

| 模块 | 职责 | 规模 |
|---|---|---|
| `responsecache` | 精确响应缓存 + 语义响应缓存（对接 Redis/Qdrant/pgvector/Pinecone/Weaviate） | 6352 行 / 23 文件 |
| `batch` | OpenAI Batch API（异步批处理任务） | 1107 行 / 9 文件 |
| `conversationstore` | `/v1/conversations` 会话状态持久化 | 541 行 / 3 文件 |
| `embedding` | Embeddings 端点的请求/响应编解码 | 293 行 / 2 文件 |
| `filestore` | Files API（上传文件供 batch/助手类接口引用） | — |
| `responsestore` | `/v1/responses` 结果快照存储（配合 `conversationstore`） | — |
| `realtime` | 语音实时网关（WebSocket 透明代理） | 274 行 / 2 文件 |
| `authkeys` | 托管 API Key 的签发/校验/标签管理 | — |

### 1.6 运维与可观测性

| 模块 | 职责 |
|---|---|
| `admin` / `admin/dashboard` | 管理 REST API + 前端 Dashboard（Provider 状态、用量看板、虚拟模型、guardrails、workflows、tagging 等的管理界面） |
| `live` | Dashboard 实时日志推送（SSE broker） |
| `observability` | Prometheus 指标导出 |
| `cache/modelcache` | Provider `/models` 发现结果的缓存 |
| `ext` | 扩展点注册表（自定义中间件/路由/请求改写器的挂载点） |

---

## 2. 配置开关一览（运行时可关，无需改代码）

| 功能 | 开关 |
|---|---|
| Guardrails | `GUARDRAILS_ENABLED` |
| Realtime 语音 | `REALTIME_ENABLED` |
| 精确/语义响应缓存 | `RESPONSE_CACHE_SIMPLE_ENABLED` / `SEMANTIC_CACHE_ENABLED` |
| Dashboard 实时日志 | `DASHBOARD_LIVE_LOGS_ENABLED` |
| Admin 后台/UI | `ADMIN_ENDPOINTS_ENABLED` / `ADMIN_UI_ENABLED` |
| Metrics | `METRICS_ENABLED` |
| Failover | `FAILOVER_ENABLED` |
| 审计日志 | `LOGGING_ENABLED` |
| 用量/预算 | `USAGE_ENABLED` / `BUDGETS_ENABLED` |
| 单个 Provider | 不配置对应 `*_API_KEY` 即不启用 |

**没有开关、代码里"常驻运行"的模块**：`tagging`、`workflows`、`batch`、`conversationstore`、`embedding`——这几个模块即使不使用也会被初始化、注册路由，若要精简必须删代码而非改配置。

---

## 3. 一次请求的完整处理流程

以最核心的 `POST /v1/chat/completions`（非流式）为例，梳理请求从进入到返回经过的每一层。

```
客户端请求
   │
   ▼
┌─────────────────────────────────────────────────────────────┐
│ 1. Echo 全局中间件链（internal/server/http.go，顺序固定）        │
│                                                               │
│   ① RequestLogger              请求访问日志                    │
│   ② Recover                    panic 兜底恢复                  │
│   ③ BodyLimit                  请求体大小限制（默认 10MB）        │
│   ④ RequestID                  生成/透传 X-Request-ID           │
│   ⑤ WriteDeadline              模型交互写超时保护                │
│   ⑥ RequestSnapshotCapture     原始请求快照（供后续中间件复用）    │
│   ⑦ TaggingCapture             [可选] 按配置 Header 提取标签      │
│   ⑧ PassthroughSemanticEnrich  [可选] 透传路由语义增强           │
│   ⑨ AuditLog Middleware        [可选] 审计日志采集（延迟写入）    │
│   ⑩ ExtraMiddleware            [可选] 扩展点自定义中间件          │
│   ⑪ AuthMiddleware             Master Key / 托管 Key 鉴权        │
│   ⑫ RequestRewriteMiddleware   [可选] 扩展点请求体改写（含 model）│
│   ⑬ WorkflowResolution         解析请求所属 Workflow（决定是否   │
│                                 走 guardrails/cache/budget 策略）│
└─────────────────────────────────────────────────────────────┘
   │
   ▼
┌─────────────────────────────────────────────────────────────┐
│ 2. Handler 层（internal/server/translated_inference_service.go）│
│                                                               │
│   a) 解析请求 JSON → core.ChatRequest                          │
│   b) PrepareChatRequest（internal/gateway）：                  │
│        - 模型 Selector 解析：拆出 provider/model，校验          │
│          该 Provider/Model 是否存在、是否对当前 Key 授权         │
│        - 解析/复用 Workflow（含 Guardrails 策略、缓存策略）      │
│        - TranslatedRequestPatcher：guardrails 对请求消息        │
│          做改写/过滤（若该 Workflow 启用了 guardrails）         │
│   c) enforceBudget：预算检查（BUDGETS_ENABLED 时），            │
│        超支直接 429/402 拦截，不再往下走                        │
│   d) handleWithCache：若 Workflow 允许缓存，先查                │
│        responsecache（精确 key 命中 或 语义相似度命中），        │
│        命中则直接返回，不再调用 Provider                        │
└─────────────────────────────────────────────────────────────┘
   │ 未命中缓存
   ▼
┌─────────────────────────────────────────────────────────────┐
│ 3. 推理编排（internal/gateway/inference_execute.go）            │
│                                                               │
│   - 按 Selector 解析出的 Provider 类型/名称，调用                │
│     internal/providers.Router 找到具体 Provider 客户端           │
│   - 若目标是 virtual model（别名/负载均衡）：                    │
│       internal/virtualmodels 按 round_robin 或 cost 策略        │
│       从多个 target 中选出一个真实 provider/model                │
│   - 执行 Provider 调用（HTTP 请求上游 API）                      │
│   - 失败时：internal/failover 按配置的重试次数/退避策略重试，     │
│     或切换到 failover model；连续失败触发熔断（circuit breaker） │
└─────────────────────────────────────────────────────────────┘
   │
   ▼
┌─────────────────────────────────────────────────────────────┐
│ 4. 响应后处理                                                  │
│                                                               │
│   - 响应规范化为 OpenAI 兼容格式（internal/core 编解码）         │
│   - Guardrails 对响应内容做审查/改写（若启用）                   │
│   - usage.Logger 记录本次请求的 token 用量与成本                 │
│     （调用 pricingoverrides 解析单价）                          │
│   - responsecache 写入本次响应（若启用缓存）                     │
│   - auditlog 中间件在响应返回后落盘完整审计记录（若启用）         │
│   - live broker 推送本次请求事件到 Dashboard 实时日志（若启用）   │
└─────────────────────────────────────────────────────────────┘
   │
   ▼
返回客户端（OpenAI 兼容 JSON，或 SSE 流）
```

### 流式请求（`stream: true`）的差异
- 中间件链、Prepare 阶段（模型解析/Workflow/Guardrails 请求侧改写）与预算检查完全一致。
- 命中 `CanFastPathStreamingChatPassthrough` 条件时（无请求改写、无需强制回传 usage）可走"快速透传"路径，跳过部分响应侧处理直接转发上游 SSE 流，降低延迟。
- 否则通过 `StreamChatCompletion` 逐块转发，并在流结束时（`response.done`/`[DONE]`）统一记录用量。

### 关键设计点
1. **Workflow 是策略聚合点**：guardrails 规则、缓存开关、用量策略并不是分散判断的，而是先解析成一个 `core.Workflow` 对象，后续各阶段只需读取 `workflow.XxxEnabled()`，这也是为什么 `workflows` 模块即使不需要自定义规则也会"默认创建一个全局 workflow"常驻运行。
2. **Budget 检查先于 Provider 调用，先于缓存查询之后还是之前**：代码顺序是 `enforceBudget` → `handleWithCache`，即**先查预算，再查缓存**——即使缓存命中本可以免费返回，也会先做一次预算校验（注意：命中缓存的请求通常不产生新的 provider 调用成本，这里的顺序是实现细节，简化时需注意是否要调整为"缓存命中直接放行"）。
3. **失败切换（failover）在 Provider 调用层，虚拟模型负载均衡在其上一层**：`virtualmodels` 决定"调用哪个 target"，`failover` 决定"这次调用失败后要不要换一个重试"，两者可以叠加。

---

## 4. 小结

- 请求处理链路上，**Guardrails/Workflow/Tagging/ResponseCache/AuditLog** 都是以"中间件或 Prepare 阶段的可选步骤"形式挂在主干链路上，理论上跳过它们不影响主干（模型解析 → 预算 → Provider 调用 → 用量记录）的正确性。
- 但 `workflows` 因为承担了"guardrails 策略是否生效"的开关角色，即使要精简 guardrails，也需要一并处理 `workflows` 里对应的默认策略逻辑，两者存在强耦合，建议一起评估、一起移除或一起保留。
- `batch`、`conversationstore`、`embedding`、`realtime`、`filestore`、`responsestore` 属于"OpenAI 兼容周边端点"，与主干请求链路（chat/completions）几乎没有代码耦合，是最容易独立摘除的一批。
