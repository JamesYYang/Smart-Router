# TaskRouting MVP（P0）实现设计

> 本次实现范围严格限定为 `docs/design/TaskRouting-Design.md` 的 **P0 阶段**：规则分类器 + Resolver +
> 静态虚拟模型映射，接入 `internal/app` 装配。不包含 §7 动态打分/候选池算法、LLM 分类器（P2）、
> Dashboard 可视化（P3）。这些留待后续独立的 spec。

## 1. 背景与目标

`docs/design/TaskRouting-Design.md` 已经完整设计了"任务级智能路由"功能，但仓库里还没有任何代码
（`internal/taskrouting` 不存在，全仓库对 `taskrouting`/`task_routing` 零引用）。本 spec 把该文档的
P0 范围落成一份可执行的实现设计，并订正了一处该文档未处理的技术细节（见 §6 可观测性）。

**目标**：客户端调用一个约定的虚拟模型名（如 `smart-router/auto`）时，网关按请求内容用规则分类器
判断任务类型，将请求改写为对应的目标虚拟模型名，再交给现有 `virtualmodels` 解析/负载均衡/故障切换
流程，对客户端和其余管道（Guardrails/Cache/Dispatch/Failover/Usage）透明。

**非目标（本次不做）**：
- §7 候选池 + 质量/成本打分算法（动态选具体 `(provider, model)`）。
- LLM 自分类（`llm_classifier.go`）、`ChainClassifier`。
- Dashboard 端任务路由可视化/规则可视化编辑。
- 按索引展开的数组型 env 变量（类似 `TAGGING_HEADER_N`）——`entrypoints`/`mappings` 本次只走
  YAML 配置。

## 2. 现状确认（挂载点仍然有效）

已对照当前代码验证 `TaskRouting-Design.md` §2 描述的挂载点仍然成立：

- `gateway.ModelResolver` 接口：`internal/gateway/interfaces.go:12`
- `ResolveExecutionSelector`：`internal/gateway/request_model_resolution.go:193`，内部通过
  `resolver.(UserPathModelResolver)` 做可选接口探测（`request_model_resolution.go:239`）。
- `virtualmodels.Service` 同时实现 `ResolveModel`（`internal/virtualmodels/resolve.go:35`）和
  `ResolveModelForUserPath`（`resolve.go:46`），是待包装的 `next` resolver。
- 装配点：`internal/app/app.go:434`（`InferenceConfig.ModelResolver: vm`）与 `app.go:540`
  （batch orchestrator 的 `ModelResolver: vm`）——两处都直接把 `vm` 赋给 `ModelResolver` 字段，
  是本次唯一需要修改装配代码的地方。

## 3. 模块结构：`internal/taskrouting`

```
internal/taskrouting/
  ├── types.go         // TaskType、Classification、ClassificationInput、Mapping
  ├── classifier.go    // Classifier 接口（为 P2 LLM 分类器预留，本次只有一个实现）
  ├── rules.go         // RuleClassifier：MVP 唯一分类器实现
  ├── resolver.go       // Resolver：实现 gateway.ModelResolver + UserPathModelResolver + LabelingModelResolver
  ├── config.go         // Config 类型 + env 覆盖 + 校验
  ├── factory.go        // New(cfg, next) 装配入口
  └── *_test.go         // 每个源文件一个测试文件，同包，testify
```

### 3.1 核心类型（`types.go`）

```go
package taskrouting

type TaskType string

const (
    TaskAgentToolUse   TaskType = "agent_tool_use"
    TaskCode           TaskType = "code"
    TaskTranslation    TaskType = "translation"
    TaskTitleGeneration TaskType = "title_generation"
    TaskBulkLowValue   TaskType = "bulk_low_value"
    TaskCopywriting    TaskType = "copywriting"
    TaskDataExtraction TaskType = "data_extraction"
    TaskGeneral        TaskType = "general" // 兜底，规则链无命中时的结果
)

type Classification struct {
    Task   TaskType
    Reason string // 命中的规则名，用于日志/label/调试
}

// ClassificationInput 只携带分类器需要的信号，不直接依赖 core.ChatRequest。
type ClassificationInput struct {
    Messages       []Message // role + content，截断到最近 N 轮（N 可配置，默认 20）
    HasToolCalls   bool
    ToolNames      []string
    RequestLabels  []string // 已有的 tagging labels（供 bulk_low_value 规则匹配 "batch" 等标签）
    RequestedModel string    // 命中的 "smart-router/xxx" 别名本身，仅用于日志
}

type Message struct {
    Role    string
    Content string
}

// Mapping 声明任务类型 -> 目标虚拟模型名（该虚拟模型的 Targets/Strategy 仍由 virtualmodels 管理）。
type Mapping struct {
    Task              TaskType `yaml:"task"`
    VirtualModelSource string  `yaml:"virtual_model"`
}
```

### 3.2 分类器（`classifier.go` + `rules.go`）

```go
type Classifier interface {
    Classify(ctx context.Context, input ClassificationInput) (Classification, error)
}
```

MVP 只实现 `RuleClassifier`：按顺序尝试一组 `Rule`，第一个 `Match` 返回 true 的生效，否则落到
`TaskGeneral`。规则集合是包内写死的有序切片（不做成配置文件——运营侧可调规则留到后续需求明确后再做，
避免过早引入配置系统）：

| 顺序 | 规则 | 判定依据 |
|---|---|---|
| 1 | `agent_tool_use` | `input.HasToolCalls == true` |
| 2 | `code` | 消息含代码块围栏 ``` ，或 `func`/`def`/`class`/`import`/`package` 等关键字在最后一条用户消息里命中 ≥ 2 个 |
| 3 | `translation` | 消息含"翻译成"/"translate to"/"译为"等短语 |
| 4 | `title_generation` | 最后一条用户消息长度 < 50 字符且含"标题"/"title" |
| 5 | `bulk_low_value` | `input.RequestLabels` 中含 `batch`（大小写不敏感精确匹配） |
| 6 | `copywriting` | 含"写一篇"/"文案"/"slogan"/"营销"等关键字 |
| 7 | `data_extraction` | 含"提取"/"抽取"/"extract"/"JSON schema"/"结构化输出"等关键字 |
| 8 | 兜底 | 都不命中 → `TaskGeneral` |

规则版是纯字符串/正则匹配，无外部依赖，微秒级开销。

### 3.3 Resolver（`resolver.go`）

```go
type Resolver struct {
    next        gateway.ModelResolver // 通常是 *virtualmodels.Service
    classifier  Classifier
    mappings    map[TaskType]string  // task -> virtual model source
    entrypoints map[string]struct{}  // 触发分类的虚拟模型名集合
}

// ResolveModel / ResolveModelForUserPath satisfy gateway.ModelResolver /
// UserPathModelResolver for callers that don't care about labels; both
// discard the label slice from resolveModel.
func (r *Resolver) ResolveModel(requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
    selector, changed, _, err := r.resolveModel(context.Background(), requested)
    return selector, changed, err
}

func (r *Resolver) ResolveModelForUserPath(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
    selector, changed, _, err := r.resolveModel(ctx, requested)
    return selector, changed, err
}

// ResolveModelWithLabels satisfies gateway.LabelingModelResolver (§4); this is
// the method ResolveExecutionSelector actually calls in production.
func (r *Resolver) ResolveModelWithLabels(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, []string, error) {
    return r.resolveModel(ctx, requested)
}

func (r *Resolver) resolveModel(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, []string, error) {
    if _, ok := r.entrypoints[requested.Model]; !ok {
        selector, changed, err := r.delegate(ctx, requested) // 非 entrypoint：零开销直通
        return selector, changed, nil, err
    }
    classification, err := r.classifier.Classify(ctx, buildInput(ctx, requested))
    if err != nil {
        selector, changed, delErr := r.delegate(ctx, requested) // fail-open：分类失败退回原始请求
        return selector, changed, nil, delErr
    }
    target, ok := r.mappings[classification.Task]
    if !ok {
        selector, changed, delErr := r.delegate(ctx, requested) // fail-open：任务无映射退回原始请求
        return selector, changed, nil, delErr
    }
    rewritten := core.NewRequestedModelSelector(target, requested.ProviderHint)
    selector, changed, delErr := r.delegate(ctx, rewritten)
    return selector, changed, []string{"task:" + string(classification.Task)}, delErr
}

func (r *Resolver) delegate(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
    if scoped, ok := r.next.(gateway.UserPathModelResolver); ok {
        return scoped.ResolveModelForUserPath(ctx, requested)
    }
    return r.next.ResolveModel(requested)
}
```

（示意代码，实现时以 `_test.go` 驱动的实际签名为准。）

关键设计点：
- **零开销直通**：请求模型不在 `entrypoints` 集合里时，完全不构造 `ClassificationInput`、不跑规则，
  直接委托给 `next`。这保证了对现有请求路径没有性能影响。
- **fail-open**：分类器出错或映射缺失都退回"就当 TaskRouting 不存在"，请求继续用原始
  `requested.Model` 走 `virtualmodels` 解析，不会导致请求失败。
- **不重新实现负载均衡/故障切换**：`Resolver` 只改写 `requested.Model` 这一个字符串，其余全部委托给
  `next`（`virtualmodels.Service`），沿用其 `round_robin`/`cost` 策略和可用性探测。

## 4. 可观测性：分类结果如何进入 usage labels

`TaskRouting-Design.md` §9 假设"分类结果写入 label"是免费的，但实际调查发现
`gateway.ModelResolver.ResolveModel` 的返回值 `(core.ModelSelector, bool, error)` 没有位置携带额外
数据，且 `context.Context` 是值传递——`Resolver` 内部对 `ctx` 做的任何 `context.WithValue` 修改都不会
反映到调用方后续用于 `LogUsage` 的那个 `ctx` 上，除非显式往上传递。

调查确认 `PreparedChatRequest`（`internal/gateway/inference_orchestrator.go:61`）已经有
`Context context.Context` 字段，且 `LogUsage` 从 context 读 label
（`internal/gateway/usage.go:37`：`entry.Labels = core.RequestLabelsFromContext(ctx)`）。因此可以在
"准备阶段"这一层把 label 合并进 ctx，再通过已有的 `PreparedChatRequest.Context` 字段传下去，不需要
改 `ModelResolver` 接口本身。具体改动：

1. `internal/core/request_model_resolution.go`：给 `RequestModelResolution` 加一个字段
   `Labels []string`。
2. `internal/gateway/interfaces.go`：新增一个可选接口（同 `UserPathModelResolver` 的探测方式）：
   ```go
   // LabelingModelResolver is an optional ModelResolver that reports extra
   // observability labels (e.g. task-classification results) alongside the
   // resolved selector, without changing the ModelResolver contract other
   // implementers rely on.
   type LabelingModelResolver interface {
       ResolveModelWithLabels(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, []string, error)
   }
   ```
   `taskrouting.Resolver` 实现这个接口；`virtualmodels.Service` 和各 provider 不需要改动，也不受影响。
3. `internal/gateway/request_model_resolution.go`：`ResolveExecutionSelector` 在探测
   `UserPathModelResolver` 的同时探测 `LabelingModelResolver`；命中时用它代替普通调用，并把返回的
   `[]string` 标签写入 `ResolveRequestModelWithAuthorizer` 返回的 `RequestModelResolution.Labels`。
4. `internal/gateway/inference_prepare.go`（以及 responses/embeddings 的对应准备函数）：拿到
   `resolution` 后，若 `len(resolution.Labels) > 0`，执行
   `ctx = core.WithRequestLabels(ctx, core.MergeLabels(core.RequestLabelsFromContext(ctx), resolution.Labels))`，
   再用这个 `ctx` 构造 `Prepared*Request{Context: ctx, ...}`。

这是本次唯一超出 `internal/taskrouting` 包边界的改动，改动点集中在 4 个文件、每处都是几行的机械性
修改，不改变任何现有调用方的行为（`resolution.Labels` 为空时完全是无操作）。

## 5. 配置（`config/taskrouting.go` + `config.Config`）

```go
package config

type TaskRoutingConfig struct {
    Enabled     bool               `yaml:"enabled"`
    Entrypoints []string           `yaml:"entrypoints"`
    Mappings    []TaskRoutingMapping `yaml:"mappings"`
}

type TaskRoutingMapping struct {
    Task              string `yaml:"task"`
    VirtualModelSource string `yaml:"virtual_model"`
}
```

- Env：`TASK_ROUTING_ENABLED`（bool，覆盖 YAML 的 `enabled`）。`entrypoints`/`mappings` 本次只走
  YAML，不做 `TASK_ROUTING_ENTRYPOINT_N` 之类的数组型 env 展开（跟 tagging 的 `TAGGING_HEADER_N`
  模式不同——那是因为 tagging header 需要频繁运维调整，task routing 的映射表暂时不需要）。
- 默认值：`enabled: false`。未显式开启时，`taskrouting.New` 直接返回 `next` 不做任何包装，行为与
  没有这个模块完全一致。
- **校验**（配置加载时，模仿 `normalizeTaggingConfig` 的模式，`config/taskrouting.go` 里实现）：
  - `mappings` 里 `task` 字段必须是已知的 8 个 `TaskType` 字符串之一，否则报错。
  - `mappings` 不能有重复的 `task`。
  - 必须包含 `task: general` 的映射（作为分类器兜底任务的目标，不能没有落点）。
  - `entrypoints` 不能为空数组（若 `enabled: true`）。
  - 注意：这里不校验 `virtual_model` 字段引用的虚拟模型名是否真的在 `virtual_models` 里存在——
    虚拟模型可能来自 Dashboard 动态配置/热加载，配置加载阶段看不到完整视图；引用了不存在的虚拟模型时，
    行为与今天客户端直接请求一个不存在的虚拟模型名一致（走到 `virtualmodels` 的正常"未找到重定向,
    按字面量解析"逻辑），不是 TaskRouting 需要额外处理的错误。

配置示例（`config.example.yaml` 追加）：

```yaml
task_routing:
  enabled: false
  entrypoints:
    - smart-router/auto
  mappings:
    - { task: title_generation, virtual_model: smart-router/tier-simple }
    - { task: bulk_low_value,    virtual_model: smart-router/tier-simple }
    - { task: copywriting,       virtual_model: smart-router/tier-medium }
    - { task: translation,       virtual_model: smart-router/tier-medium }
    - { task: data_extraction,   virtual_model: smart-router/tier-medium }
    - { task: code,              virtual_model: smart-router/tier-code }
    - { task: agent_tool_use,    virtual_model: smart-router/tier-quality }
    - { task: general,           virtual_model: smart-router/tier-medium }
```

## 6. App 装配（`internal/app/app.go`）

在 `vm`（`virtualmodels.Service` 实例）构造完成之后、`ModelResolver:` 字段赋值之前（两处：
`app.go:434` 的 `InferenceConfig`，`app.go:540` 的 batch orchestrator config），插入：

```go
modelResolver, err := taskrouting.New(appCfg.TaskRouting, vm)
if err != nil {
    return nil, fmt.Errorf("taskrouting: %w", err) // 遵循 app.New 现有的构造失败/回滚模式
}
```

两处 `ModelResolver: vm` 改为 `ModelResolver: modelResolver`。`taskrouting.New` 在
`Enabled == false` 时直接 `return next, nil`，因此未开启功能时这里的改动是纯粹的透传，不影响现有
行为——这也是为什么可以放心地让 batch orchestrator 也用同一个 `modelResolver`（默认关闭时等价于
今天的 `vm`）。

## 7. 数据流（happy path）

```
① 客户端: { "model": "smart-router/auto", "messages": [...] }
② Auth / Budget 中间件（不变）
③ PrepareChatRequest(ctx, req, meta)
     └─ ResolveRequestModelWithAuthorizer(ctx, ...)
          └─ ResolveExecutionSelector(ctx, provider, taskroutingResolver, requested)
               └─ taskroutingResolver.ResolveModelWithLabels(ctx, "smart-router/auto")
                    ├─ 命中 entrypoints → RuleClassifier.Classify(...) → Task = "code"
                    ├─ mappings["code"] = "smart-router/tier-code"
                    ├─ 改写 requested.Model = "smart-router/tier-code"
                    ├─ 委托 vm.ResolveModelForUserPath(ctx, "smart-router/tier-code")
                    │     └─ strategy=cost，挑最便宜的可用 target → provider=deepseek, model=deepseek-coder
                    └─ 返回 labels = ["task:code"]
          └─ RequestModelResolution.Labels = ["task:code"]
     └─ ctx = WithRequestLabels(ctx, MergeLabels(现有 labels, ["task:code"]))
     └─ 返回 PreparedChatRequest{Context: ctx, ...}
④ Guardrails / Cache / Dispatch / Failover（完全不变）
⑤ Usage 记录：LogUsage 从 PreparedChatRequest.Context 读到 labels，写入 usage.UsageEntry.Labels
```

## 8. 错误处理

- 分类器 `Classify` 返回 error → `Resolver` fail-open，委托原始 `requested`（等同任务映射为
  `general` 但不占用 label）。
- `mappings` 里没有对应 `classification.Task` 的目标 → fail-open，同上（正常配置下不会发生，因为
  §5 校验强制要求 `general` 映射存在，但 `RuleClassifier` 返回了非法/未知 `TaskType` 时仍需兜底）。
- `taskrouting.New` 在配置非法时于 **启动阶段** 报错并阻止启动（遵循 `config.Load()`/
  `normalizeTaggingConfig` 现有的"配置错误 fail-closed，运行时错误 fail-open"约定），不会在运行时
  才发现坏配置。
- TaskRouting 本身绝不是请求失败的原因——所有运行时异常路径都退回"当作没有这个模块"。

## 9. 测试计划

遵循项目约定：每个源文件一个 `_test.go`，同包，`testify`（`require`/`assert`）。

- `rules_test.go`：表驱动，§3.2 表格里每条规则至少一个命中用例 + 一个"全不命中→general"用例。
- `resolver_test.go`：用一个 fake `next gateway.ModelResolver`（同时实现
  `UserPathModelResolver`）验证：
  - 非 entrypoint 模型 → 直通，fake 的分类调用计数为 0。
  - entrypoint 模型 + 分类成功 → 改写后的 model 传给了 `next`，返回值里带对应 label。
  - 分类器返回 error → fail-open，`next` 收到原始 `requested`。
  - 映射缺失（人为构造非法 `Classification.Task`）→ fail-open。
- `config_test.go`：`TASK_ROUTING_ENABLED` env 覆盖、非法 `task` 值报错、缺 `general` 映射报错、
  重复 `task` 报错、`entrypoints` 为空 + `enabled:true` 报错。
- 扩展 `internal/gateway/request_model_resolution_test.go`：覆盖新增的 `LabelingModelResolver`
  可选接口探测路径——resolver 实现该接口时 labels 被正确传播到
  `RequestModelResolution.Labels`；resolver 不实现该接口时行为与今天完全一致（现有测试不应破坏）。
- `internal/gateway/inference_prepare_test.go`（如存在，否则在对应 prepare 测试文件里新增用例）：
  验证 `resolution.Labels` 非空时，`PreparedChatRequest.Context` 能读出合并后的 labels；
  `resolution.Labels` 为空时 ctx 不变。
- `internal/app` 层面：不新增端到端测试（`taskrouting.New` 本身的行为已被单元测试覆盖，`app.go`
  里只是两行装配代码的改动），但需确认 `go build ./...`、`go test ./...` 全量通过。

## 10. 与后续阶段的关系（仅记录，不在本次范围内）

- P1：补充更多 Provider 的分档 target（配置层面的事，不需要代码改动）。
- P2：`llm_classifier.go` 实现 `Classifier` 接口，和 `RuleClassifier` 组合成
  `ChainClassifier`——`Resolver` 不需要改动，只需替换装配时传入的 `Classifier` 实例。
- P3：Dashboard 任务路由命中率可视化——依赖本次已经写入的 `task:<type>` label，可以直接复用现有
  `GET /admin/usage/labels` 能力，不需要新开发接口。
- §7 动态打分/候选池算法：作为独立 spec 单独设计，届时 `Mapping`（任务→单一虚拟模型）会被
  "候选池 + 打分"替换或并存，`Resolver` 的接口边界（`gateway.ModelResolver`）保持不变。
