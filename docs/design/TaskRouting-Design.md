# TaskRouting 模块设计文档（智能任务级模型路由）

> 对应 PRD《SmartRouter》第 4.2 节"智能任务级模型选择"。本设计的目标是：**用一个新增模块 + 少量挂载点改动，复用 GoModel 现有的模型解析、虚拟模型、成本路由、故障切换等全部基础设施**，不改动 `providers`、`virtualmodels`、`failover` 等核心包的内部逻辑。

---

## 1. 目标与非目标

**目标**
- 客户端不指定具体模型（或显式声明"自动模式"）时，网关根据请求内容自动判断任务类型，并路由到最适合该任务类型、成本最优的模型。
- 复用现有 `virtualmodels` 的多目标（Target）+ 策略（`round_robin` / `cost`）执行层，新增模块只负责"选出一个虚拟模型名字"，不重新实现负载均衡、故障切换、可用性探测。
- 分类逻辑可插拔：MVP 用规则版（关键词/长度/结构特征），后续可平滑升级为小模型分类（对应 PRD 8. 风险应对"逐步引入 LLM 自我分类"）。

**非目标**
- 不负责最终的 Provider 调用、重试、熔断（交给 `failover`）。
- 不负责定价/成本计算本身（交给 `pricingoverrides` + `usage`），只消费成本数据做分类决策的参考。
- 不改变 OpenAI 兼容的请求/响应协议，任务分类是网关内部行为，对客户端透明。

---

## 2. 现有代码中的挂载点

请求的模型解析链路（`internal/gateway/request_model_resolution.go`）：

```
PrepareChatRequest（gateway/inference_prepare.go）
   │
   ▼
ResolveRequestModelWithAuthorizer
   │
   ▼
ResolveExecutionSelector   ← ★ 本模块的挂载点
   │
   ├─ resolver.ResolveModel(requested)   // resolver = virtualmodels.Service
   │     内部按 redirect 表把 "source name" 解析成
   │     一个或多个 Target，再按 round_robin/cost 策略
   │     选出最终 core.ModelSelector
   │
   └─ provider.ResolveModel(requested)   // 若 provider 自身也实现别名解析
```

`virtualmodels.Service` 已经实现了 `ModelResolver` 接口（`ResolveModel` / `ResolveModelForUserPath`），本质是一张 `source name → []Target` 的重定向表，支持多目标 + 策略选择。**这正是 PRD 4.3"模型级路由"要的能力，已经现成可用**。

TaskRouting 要做的，只是在 `resolver.ResolveModel` 被调用**之前**，多一步："如果请求命中的是一个需要智能分类的虚拟模型名，先分类任务类型，把 `requested.Model` 改写成分类结果对应的虚拟模型名，再交给原有的 `virtualmodels` 解析流程。"

---

## 3. 触发约定

新增一个约定的"智能路由"selector 前缀/别名，两种方式二选一（建议 A，改动最小）：

**方案 A：约定虚拟模型名触发（推荐）**
客户端像调用普通模型一样传：
```json
{ "model": "smart-router/auto", "messages": [...] }
```
`smart-router/auto`、`smart-router/auto-code`、`smart-router/auto-translate` 等都是**在 `virtualmodels` 里注册的普通虚拟模型名**，唯一区别是它们被标记为"需要任务分类"（见下方 §5 数据模型的 `TaskAware` 标记）。命中这些名字时才触发分类器；其余请求（客户端已指定具体模型）完全不受影响，零性能开销。

**方案 B：显式参数触发**
在请求体扩展一个非标准字段（如 `"routing": "auto"`），网关识别后忽略 `model` 字段直接跑分类器。
- 优点：语义更明确。
- 缺点：非 OpenAI 标准字段，SDK 透传行为不可控，Postel 法则下建议只作为方案 A 的补充，不作为主路径。

**结论：采用方案 A**，与现有 `virtualmodels` 别名机制形式一致，客户端心智成本最低。

---

## 4. 模块结构（新增 `internal/taskrouting`）

```
internal/taskrouting/
  ├── types.go            // TaskType、Classification、Rule、Mapping 等类型定义
  ├── classifier.go        // Classifier 接口 + 组合调度（规则优先，规则未命中可选降级到 LLM 分类器）
  ├── rules.go              // 规则版分类器实现（MVP）
  ├── llm_classifier.go     // LLM 自我分类实现（V2，可选开关）
  ├── resolver.go           // 实现 gateway.ModelResolver，包装 virtualmodels.Service
  ├── config.go             // config.yaml / env 配置解析
  ├── factory.go            // 依赖装配，供 internal/app 调用
  └── *_test.go
```

### 4.1 核心类型（`types.go`）

```go
package taskrouting

// TaskType is a coarse task category used to pick a cost/quality tier.
type TaskType string

const (
	TaskTitleGeneration TaskType = "title_generation"
	TaskCopywriting     TaskType = "copywriting"
	TaskTranslation     TaskType = "translation"
	TaskCode            TaskType = "code"
	TaskDataExtraction  TaskType = "data_extraction"
	TaskAgentToolUse    TaskType = "agent_tool_use"
	TaskBulkLowValue    TaskType = "bulk_low_value"
	TaskGeneral         TaskType = "general" // fallback when no rule matches
)

// Classification is the classifier's decision for one request.
type Classification struct {
	Task       TaskType
	Confidence float64 // 0..1, reserved for LLM classifier; rule classifier reports 1.0
	Reason     string  // short human-readable rule/heuristic name, for audit/debug
}

// Mapping declares which virtual model name a task type routes to.
// The virtual model itself (with its Targets + Strategy) already lives in
// virtualmodels — Mapping only says "which virtual model name to resolve to".
type Mapping struct {
	Task               TaskType `yaml:"task"`
	VirtualModelSource string   `yaml:"virtual_model"` // e.g. "smart-router/tier-simple"
}
```

### 4.2 分类器接口（`classifier.go`）

```go
// Classifier decides a TaskType for one chat/responses request.
type Classifier interface {
	Classify(ctx context.Context, req ClassificationInput) (Classification, error)
}

// ClassificationInput carries only the signals a classifier needs — never the
// full request struct, so classifiers stay decoupled from core.ChatRequest.
type ClassificationInput struct {
	Messages       []Message // role + content, truncated to last N turns
	HasToolCalls   bool
	ToolNames      []string
	RequestedModel string // the literal "smart-router/xxx" alias requested, for logging
}
```

MVP 用**规则链**（`rules.go`）：按顺序尝试一组 `Rule`，第一个命中的生效，都不命中则落到 `TaskGeneral`：

```go
type Rule struct {
	Name  string
	Match func(ClassificationInput) bool
	Task  TaskType
}
```

初始规则示例（对应 PRD 4.2 的任务列表）：
| 规则 | 判定依据 |
|---|---|
| `agent_tool_use` | `HasToolCalls == true` |
| `code` | 消息含代码块围栏 ``` 或典型代码关键字（`func`/`def`/`class`/`import` 等）密度高 |
| `translation` | 消息包含"翻译成/translate to"等短语，或检测到输入输出语言不同 |
| `title_generation` | 消息很短（如 < 50 字）且包含"标题/title"关键字 |
| `bulk_low_value` | 请求携带批量标签（复用 `internal/tagging` 已提取的 label，如 `batch=true`） |
| `copywriting` / `data_extraction` | 关键字规则，按需扩展 |
| 兜底 | `TaskGeneral` |

规则版对性能友好（纯字符串/正则匹配，微秒级），且**规则集本身可以做成配置文件**（`taskrouting.rules` in `config.yaml`），运营侧可以不改代码调整规则。

V2 可选：`llm_classifier.go` 实现同一个 `Classifier` 接口，调用一个小模型（如 Qwen-Turbo）做分类，返回结构化 JSON。两个分类器可以链式组合（`ChainClassifier`）：规则优先，规则给出 `TaskGeneral` 时再调用 LLM 兜底，兼顾成本和准确率。

### 4.3 Resolver（`resolver.go`）—— 唯一对接现有链路的地方

```go
// Resolver wraps virtualmodels.Service, intercepting only task-aware virtual
// model names; everything else passes through untouched.
type Resolver struct {
	next       gateway.ModelResolver // the real virtualmodels.Service
	classifier Classifier
	mappings   map[TaskType]string   // task -> virtual model source name
	taskAware  map[string]struct{}   // set of "smart-router/xxx" names that trigger classification
}

func (r *Resolver) ResolveModel(requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	if _, ok := r.taskAware[requested.Model]; ok {
		classification, err := r.classifier.Classify(ctx, buildInput(requested))
		if err != nil {
			// Fail open: fall back to the literal "smart-router/auto" virtual
			// model's own default target rather than failing the request.
			return r.next.ResolveModel(requested)
		}
		if target, ok := r.mappings[classification.Task]; ok {
			requested = core.NewRequestedModelSelector(target, requested.ProviderHint)
		}
	}
	return r.next.ResolveModel(requested) // delegates to virtualmodels as usual
}

func (r *Resolver) ResolveModelForUserPath(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	// same interception, then delegates to r.next.ResolveModelForUserPath
}
```

`Resolver` 实现了 `gateway.ModelResolver`（及 `UserPathModelResolver`），在 `internal/app` 装配阶段，用 `taskrouting.Resolver` **包一层** `virtualmodels.Service` 再传给 `gateway.InferenceConfig.ModelResolver`，其余装配代码不需要改。

---

## 5. 数据模型与配置

### 5.1 虚拟模型侧（复用现有 `virtualmodels`，零改动）
运营在 `config.yaml`（或 Dashboard）里像平时一样声明分档虚拟模型，比如：

```yaml
virtual_models:
  - source: smart-router/tier-simple
    strategy: cost
    targets:
      - { provider: bailian, model: qwen-turbo }
      - { provider: zai, model: glm-4-flash }
  - source: smart-router/tier-medium
    strategy: cost
    targets:
      - { provider: bailian, model: qwen-plus }
  - source: smart-router/tier-quality
    strategy: cost
    targets:
      - { provider: bailian, model: qwen-max }
      - { provider: deepseek, model: deepseek-v3 }
  - source: smart-router/tier-code
    targets:
      - { provider: deepseek, model: deepseek-coder }
  - source: smart-router/auto        # 客户端实际调用的入口
    targets:
      - { provider: bailian, model: qwen-plus }   # 分类失败时的兜底默认档
```

### 5.2 TaskRouting 侧新增配置（`config/taskrouting.go`）

```yaml
task_routing:
  enabled: true
  entrypoints:                # 哪些虚拟模型名会触发任务分类
    - smart-router/auto
  mappings:                   # 任务类型 -> 目标虚拟模型
    - task: title_generation
      virtual_model: smart-router/tier-simple
    - task: bulk_low_value
      virtual_model: smart-router/tier-simple
    - task: copywriting
      virtual_model: smart-router/tier-medium
    - task: code
      virtual_model: smart-router/tier-code
    - task: agent_tool_use
      virtual_model: smart-router/tier-quality
    - task: general
      virtual_model: smart-router/tier-medium   # 兜底档
  classifier: rule            # rule | llm | chain
```

对应 env：`TASK_ROUTING_ENABLED`（默认 `true`，可整体关闭退化为普通虚拟模型解析）。

---

## 6. 更新后的请求流程（局部）

```
① 客户端: { "model": "smart-router/auto", ... }
② Auth / Budget 中间件（不变）
③ PrepareChatRequest
     └─ ResolveExecutionSelector
          └─ taskrouting.Resolver.ResolveModel("smart-router/auto")
               ├─ 命中 entrypoints → 触发 Classifier.Classify(messages)
               │     → 返回 TaskType = "code"
               ├─ mappings["code"] = "smart-router/tier-code"
               └─ 改写 requested.Model = "smart-router/tier-code"
                    └─ 委托 virtualmodels.Service.ResolveModel(...)
                         └─ 按 strategy=cost 挑最便宜的可用 target
                              → provider=deepseek, model=deepseek-coder
④ Guardrails / Cache / Dispatch / Failover / Usage（完全不变）
```

分类结果（`Classification.Task` + `Reason`）建议写入 `core.Workflow` 或请求 context，供 `auditlog`/`usage` 记录，方便后续统计"任务自动路由命中率"（PRD 第 7 节 KPI：命中率 80%+）。

---

## 7. 模型选择算法（任务 → 模型的打分与决策）

§4.3 给出的是"任务类型 → 固定虚拟模型分档"的**静态映射**，实现简单但本质上还是人工分层（跟 PRD 4.2 举例的"简单任务→7B，中等→32B，高质量→72B"一样，是死规则）。这里给出一个可以替代/增强静态映射的**动态打分算法**，让"同一任务类型下选哪个具体模型"这件事也能被量化决策，而不是纯靠人工排列 Target 顺序。

### 7.1 输入：候选池（Candidate Pool）

每个 `TaskType` 对应一组候选模型（而不是一个固定虚拟模型），候选池在 `config.yaml` 里声明：

```yaml
task_routing:
  candidate_pools:
    code:
      - { provider: deepseek, model: deepseek-coder }
      - { provider: bailian, model: qwen-coder-plus }
      - { provider: zai, model: glm-4-plus }
    tier_simple:
      - { provider: bailian, model: qwen-turbo }
      - { provider: zai, model: glm-4-flash }
      - { provider: xiaomi, model: mimo-7b }
```

候选池比"单一虚拟模型"更宽——它允许算法跨 Provider、跨规格挑选，而不是被提前锁死在某一档。

### 7.2 硬性过滤（Filter）——不满足直接淘汰，不参与打分

对候选池做三层过滤，任一不满足即淘汰：

1. **可用性**：`catalog.ModelAvailable(model)` 为 false（Provider 探活失败/库存过期）的候选直接剔除——复用 `virtualmodels` 已有的可用性探测，不重新实现。
2. **能力硬约束**：根据任务类型要求的能力位（`ModelMetadata.Capabilities`，如 `tool_use`、`vision`、`json_mode`）做过滤。例如 `agent_tool_use` 任务要求 `capabilities["tool_use"] == true`；请求里带图片输入时要求 `capabilities["vision"] == true`。
3. **上下文长度硬约束**：估算本次请求的 token 数（见 §8.3 的估算方法），若超过候选模型的 `ModelMetadata.ContextWindow`，剔除。

过滤后候选池为空时，**降级**：放宽能力约束（只保留可用性过滤），若仍为空则退回该 Provider 集合中的默认模型（fail-open，保证请求不因为筛选过严而失败）。

### 7.3 打分：质量分与成本分

对过滤后剩下的每个候选 `m`，计算两个 0~1 的归一化分数，再加权求和：

```
Score(m) = w_quality(task) × QualityNorm(m, task) − w_cost(task) × CostNorm(m, request)
```

**QualityNorm(m, task)** —— 模型在该任务类型下的相对质量，取以下两种数据源之一（按优先级）：

1. **公开榜单打分**（数据已在 `ModelMetadata.Rankings map[string]ModelRanking`，如 `{"lmarena": {Elo: 1280}, "livecodebench": {Rank: 5}}`）：
   - 为每个 `TaskType` 配置一份"相关榜单权重表"，例如 `code` 任务只看 `livecodebench`/`humaneval`，`general`/`copywriting` 看 `lmarena`/`arena_hard`，中文类任务额外看 `cmmlu`/`c-eval`。
   - 把 Elo/Rank 换算成候选池内的相对分：`QualityNorm = (elo - min(elo in pool)) / (max(elo in pool) - min(elo in pool))`（Rank 类似,取倒数排名）。
   - 若某候选完全没有相关榜单数据，进入下一优先级。
2. **运营人工评级兜底**（新增轻量配置 `task_routing.quality_overrides`，类似 `pricingoverrides` 的思路，但只有一个 0-100 的分数字段，不需要单独建模块）：
   ```yaml
   task_routing:
     quality_overrides:
       - { provider: deepseek, model: deepseek-coder, task: code, score: 88 }
       - { provider: bailian,  model: qwen-coder-plus, task: code, score: 82 }
   ```
   这是 MVP 阶段最实际的数据来源——中国模型在国际榜单上的覆盖率不稳定，运营基于真实评测（PRD 5.3"我们自己是模型的重度使用者，能真实评估模型"正是这个数据的来源）手工打分更可靠。
3. **参数规模粗分级兜底**（两种数据源都没有时的最后兜底）：按模型名里的规模标识（7B/9B → 0.3，32B/34B → 0.6，72B/满血旗舰/DeepSeek-V3 级 → 0.9）粗略给分，保证系统在冷启动、无数据阶段也能跑。

**CostNorm(m, request)** —— 见 §8，先算出预估调用成本 `EstimatedCost(m, request)`，再在候选池内做 min-max 归一化：
```
CostNorm(m) = (EstimatedCost(m) − min(EstimatedCost in pool)) / (max(EstimatedCost in pool) − min(EstimatedCost in pool))
```
候选池只有一个候选、或所有候选价格相同时，`CostNorm` 记为 0（不惩罚，此时纯看质量/直接选中）。

**权重 `w_quality(task)` / `w_cost(task)`**：按任务类型配置，体现"用最少的钱办成事"的核心诉求——越是低价值/批量任务，成本权重越高；越是高价值/正确性敏感任务，质量权重越高：

| 任务类型 | w_quality | w_cost | 直觉 |
|---|---|---|---|
| `bulk_low_value` / `title_generation` | 0.2 | 0.8 | 结果差异不敏感，优先省钱 |
| `translation` / `data_extraction` | 0.4 | 0.6 | 中等敏感 |
| `copywriting` / `general` | 0.5 | 0.5 | 均衡 |
| `code` / `agent_tool_use` | 0.75 | 0.25 | 正确性敏感，愿意多花钱 |

### 7.4 决策与执行

1. 计算候选池内所有候选的 `Score`，取最高分为最终选择。
2. **平局/近似打分**：若最高分与次高分差距 < 阈值（如 0.05），优先选成本更低的一个（保证"同等质量下必须选便宜的"，直接落实 PRD"以最低成本获得最优效果"）。
3. 选中的候选是**具体 `(provider, model)`**，不是虚拟模型名，因此后续解析可以直接构造 `core.RequestedModelSelector` 并标记 `ExplicitProvider = true`，跳过 `virtualmodels` 的重定向解析（§2 提到 `resolveRequested` 对显式 provider 直接短路返回），但仍然完整走后续的 Guardrails/Budget/Cache/Dispatch/Failover 流程。
4. **失败回退**：若选中的候选在实际调用时失败（`failover` 判定需要重试/切换），按 §7.3 打分结果的**次优候选**顺序重试，而不是随机换——即打分结果同时也是一份"按性价比排序的 failover 候选列表"，一次计算两用。

### 7.5 与静态映射（§4.3）的关系

`candidate_pools + 打分算法` 是**通用版**，`virtual_model 静态映射`是它在"候选池只有一个人工预选模型"时的**退化特例**。工程落地建议：
- **P0（MVP）**：先上静态映射（§4.3），验证任务分类准确率和整体链路；
- **P1**：候选池打分算法上线，静态映射降级为"候选池只有一个候选"的边界情况，配置格式不用推倒重来。

---

## 8. 成本计算方法

### 8.1 现状：GoModel 已有的定价数据结构

`core.ModelPricing`（`internal/core/types.go`）已经定义了完整的按 token 计费字段：`InputPerMtok`、`OutputPerMtok`、`CachedInputPerMtok`、`ReasoningOutputPerMtok`、`BatchInputPerMtok/BatchOutputPerMtok` 等（单位：每百万 token 价格）。数据来源两层，后者覆盖前者：

1. **模型注册表**（`internal/modeldata`，即 ai-model-list 的 `models.json`，随 `CACHE_REFRESH_INTERVAL` 定期拉取）——大部分国际厂商模型已有现成定价，中国模型覆盖率参差不齐。
2. **运营手工覆盖**（`internal/pricingoverrides`）——针对没有定价数据或需要修正的模型，运营在 Dashboard/`config.yaml` 里补录 `input_per_mtok`/`output_per_mtok`，`mergePricing` 按字段级 override 合并，不需要整模型覆盖。

`virtualmodels.targetCost()` 已经实现了"单模型综合成本"的取数逻辑：**输入单价 + 输出单价的加和**作为一个可比较的相对成本数字（详见 `internal/virtualmodels/balancer.go` 的 `targetCost`），`cheapestTarget` 就是照这个数字挑最便宜的。TaskRouting 直接复用这个数据源，不需要重新接入定价数据。

### 8.2 事后精确成本 vs 事前预估成本——两者用途不同

| | 事后精确成本（已有） | 事前预估成本（TaskRouting 新增需求） |
|---|---|---|
| 计算时机 | 请求返回后 | 请求发出前，用于选模型 |
| 数据来源 | Provider 返回的真实 `usage.prompt_tokens`/`completion_tokens` | 请求内容的 token 数**估算** |
| 用途 | 计费、预算扣减、Dashboard 用量统计（`internal/usage`） | 路由决策（选哪个模型） |
| 精确度 | 精确（除了 Provider 未返回 usage 的边缘情况） | 近似，够用于"比较候选之间谁更便宜"即可，不需要绝对精确 |

TaskRouting 只需要"预估成本"，且只需要**候选之间的相对大小**（用于打分归一化），不需要绝对精确到分。

### 8.3 预估成本公式

```
EstimatedInputTokens  = EstimateTokens(request.messages)         // 见下方估算方法
EstimatedOutputTokens = ExpectedOutputTokens(task)                // 按任务类型给经验值，而非猜测

EstimatedCost(m, request) =
      (EstimatedInputTokens  / 1_000_000) × pricing(m).InputPerMtok
    + (EstimatedOutputTokens / 1_000_000) × pricing(m).OutputPerMtok
```

**`EstimateTokens`**：不引入分词器依赖（保持轻量），用字符数近似——英文按 `len(text) / 4`，中文按 `len(text) / 1.5`（中文字符 token 密度更高），按输入文本语言检测结果择一或加权平均；已有 `internal/core` 的语言/字符处理工具可以直接复用（避免新增分词依赖，符合 KISS 原则）。

**`ExpectedOutputTokens(task)`**：按任务类型给一个经验系数，可配置：

| 任务类型 | 预期输出 token 数（经验值） |
|---|---|
| `title_generation` | 20 |
| `translation` | ≈ 输入 token 数 × 1.2 |
| `copywriting` | 500 |
| `code` | 800 |
| `agent_tool_use` | 300（多轮平均，工具调用场景可能被多次触发，这里只估算单次） |
| `bulk_low_value` | 50 |
| `general`（兜底） | 300 |

这些经验值上线后可以用 `internal/usage` 里同任务类型的历史真实 `completion_tokens` 均值来**校准**（P2 阶段的优化项，不阻塞 MVP）。

### 8.4 无定价数据时的兜底

若某候选模型在 `ModelMetadata.Pricing` 里 `InputPerMtok`/`OutputPerMtok` 均为空（既没有注册表数据也没有人工 override）：
- 打分阶段：该候选的 `CostNorm` 记为 1（视为池内最贵，除非质量分极高，否则很难胜出）——**没有定价数据的模型默认被认为"不划算"，倒逼运营尽快补录定价**，而不是默认当作免费导致算法误选。
- 同时在日志/Dashboard 里对这类候选打一个"缺少定价数据"的告警标记，方便运营发现遗漏。

---

## 9. 可观测性与 KPI 对接

- 在 `usage.UsageEntry` 增加可选字段 `TaskType`（或复用现有 `labels` 机制，把分类结果作为一个 label 写入，**完全不改 usage 表结构**）。
- Dashboard 用量看板按 label 已经支持"by-label breakdown"（`GET /admin/usage/labels`），分类结果可以直接复用这条现成的看板能力，不需要新开发页面。
- 规则命中率、LLM 兜底调用次数可以作为 Prometheus 指标（`observability` 模块）新增，用于监控 PRD KPI「任务自动路由命中率 80%+」。

---

## 10. 分阶段落地（对应 PRD 6.1 MVP 范围）

| 阶段 | 内容 |
|---|---|
| **P0** | 新增 `internal/taskrouting`，先只实现规则分类器 + `Resolver`，接入 `internal/app` 装配；虚拟模型分档靠 `config.yaml` 手工声明（3–5 个中国模型） |
| **P1** | 补充腾讯混元/百度千帆/火山方舟等 Provider adapter，丰富分档 target |
| **P2** | 引入 LLM 自我分类（`llm_classifier.go`），规则兜底改为"规则优先，低置信度再调小模型复核" |
| **P3** | Dashboard 增加任务路由命中率可视化、分类规则可视化编辑（可选，非必需） |

---

## 11. 风险与应对（延续 PRD 第 8 节）

| 风险 | 应对 |
|---|---|
| 规则分类误判 | 分类失败或低置信度时 `Resolver` fail-open，退回 `smart-router/auto` 自身的默认 target，不影响可用性 |
| 分类器成为新的性能瓶颈 | 规则版是纯 CPU 字符串匹配，微秒级；LLM 分类器需要异步/缓存优化（可对短请求做分类结果的语义缓存，复用现有 `responsecache` 语义缓存能力） |
| 与现有 Guardrails/Workflow 顺序冲突 | `taskrouting.Resolver` 只发生在 `ResolveExecutionSelector` 阶段，早于 Workflow 解析对 guardrails 的处理，改写后的 selector 会正常参与后续 Workflow 解析，无需额外适配 |
