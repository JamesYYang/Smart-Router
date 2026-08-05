# P7 端到端集成 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 完成 SaaS 多租户改造的收尾:修复 nil-ctx guard、Dashboard role-aware UI(新增 Tenants 管理页)、跨租户端到端集成测试、部署文档,并修正 ROADMAP 状态。

**Architecture:** 只改动 5 个关注面,互相独立可各自提交:(1) `internal/gateway/request_model_resolution.go` 加 nil-ctx 防御;(2) `internal/admin/dashboard` 注入 `IsPlatformAdmin` 到模板,sidebar 按角色显示 Tenants 导航,新增 tenants.js 模块;(3) `internal/server/p7_e2e_integration_test.go` 复用 `tenant_visibility_integration_test.go` 的 full-chain + SQLite 模式;(4) `docs/deployment/multi-tenant.md`;(5) `docs/superpowers/ROADMAP.md` 状态修正。

**Tech Stack:** Go, echo/v5, Alpine.js, Node `vm` 测试,SQLite (modernc.org/sqlite), testify。

## Global Constraints

- 遵循项目约定:每个源文件配同包同基名 `_test.go`,使用 `testify`(`require`/`assert`)。
- Dashboard JS 模块:工厂挂 `window.dashboardTenantsModule`,既能在浏览器加载也能在 Node `vm` 中测试(见 `pricing.test.cjs` 模式)。
- `templateData` 已存在 `BasePath`/`Version` 字段;`Index` handler 已用 `echo/v5` 的 `*echo.Context`。
- 平台 host 判定用 `core.GetPlatformHost(ctx)`;未启用多租户(无 `base_domain`)时平台视角完整(`IsPlatformAdmin` 恒 true)。
- E2E 测试复用现有 helper:`newMemoryDB(t)`、`adminSplitReq(t, e, method, host, path, token, body)`、`tenantVis*` 常量模式,不新增重复 helper。
- P7 不写迁移脚本(无旧版兼容路径);deferred items 除 nil-ctx guard 外只记录不修复。
- 提交用 `git add <files> && git commit -m "..."`,直接提交到 master,不 push。

---

### Task 1: gateway nil-ctx guard

**Files:**
- Modify: `internal/gateway/request_model_resolution.go:65-71`
- Test: `internal/gateway/request_model_resolution_test.go`(追加,文件已存在)

**Interfaces:**
- Consumes: `ResolveRequestModelWithAuthorizer(ctx, provider, resolver, authorizer, requested)` — 已存在的公开函数。
- Produces: 无新接口;函数行为不变,只是 nil ctx 不再 panic。

- [ ] **Step 1: 写失败测试(append 到 request_model_resolution_test.go)**

在文件末尾追加:

```go
func TestResolveRequestModelWithAuthorizer_NilContext(t *testing.T) {
	provider := newRequestRefreshProvider(1)

	resolution, err := ResolveRequestModelWithAuthorizer(
		nil,
		provider,
		nil,
		nil,
		core.NewRequestedModelSelector("openai/gpt-4o", ""),
	)
	if err != nil {
		t.Fatalf("ResolveRequestModelWithAuthorizer(nil ctx) error = %v, want nil", err)
	}
	if resolution == nil {
		t.Fatal("ResolveRequestModelWithAuthorizer(nil ctx) resolution = nil, want non-nil")
	}
}
```

- [ ] **Step 2: 运行验证失败**

Run: `go test ./internal/gateway/ -run TestResolveRequestModelWithAuthorizer_NilContext -v`
Expected: FAIL(panic 或 nil-deref)

- [ ] **Step 3: 加 nil-ctx guard**

在 `ResolveRequestModelWithAuthorizer` 函数体开头(`requested = core.NewRequestedModelSelector(...)` 之前)加:

```go
	if ctx == nil {
		ctx = context.Background()
	}
```

- [ ] **Step 4: 运行验证通过**

Run: `go test ./internal/gateway/ -run TestResolveRequestModelWithAuthorizer_NilContext -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/gateway/request_model_resolution.go internal/gateway/request_model_resolution_test.go
git commit -m "fix(gateway): guard nil context in ResolveRequestModelWithAuthorizer"
```

---

### Task 2: Dashboard 注入 IsPlatformAdmin(Go 侧)

**Files:**
- Modify: `internal/admin/dashboard/dashboard.go:68-87`
- Modify: `internal/app/app.go`(initAdmin 调用后设置 multiTenant)

**Interfaces:**
- Consumes: `core.GetPlatformHost(ctx)`, `dashboard.NewWithBasePath(basePath)`。
- Produces: `dashboard.Handler` 增加可导出的 `SetMultiTenant(bool)` 方法;`templateData` 增加 `IsPlatformAdmin bool` 字段。Task 3/4 依赖 `templateData.IsPlatformAdmin` 与 `window.SMARTROUTER_IS_PLATFORM_ADMIN` 全局。

- [ ] **Step 1: templateData + Index 计算角色**

修改 `internal/admin/dashboard/dashboard.go`:

```go
type Handler struct {
	indexTmpl   *template.Template
	staticFS    http.Handler
	basePath    string
	multiTenant bool // 多租户模式(配置了 base_domain + tenant service)
}

// SetMultiTenant marks whether host-based tenant resolution is active. When
// false (single-tenant / dev), every dashboard visit is treated as the
// platform admin view.
func (h *Handler) SetMultiTenant(active bool) { h.multiTenant = active }

type templateData struct {
	BasePath       string
	Version        string
	IsPlatformAdmin bool
}
```

`Index` 中构造 `templateData` 处改为:

```go
	isPlatformAdmin := !h.multiTenant || core.GetPlatformHost(c.Request().Context())
	if err := h.indexTmpl.ExecuteTemplate(&buf, "layout", templateData{
		BasePath:        h.basePath,
		Version:         version.Info(),
		IsPlatformAdmin: isPlatformAdmin,
	}); err != nil {
```

需要 import `"smartrouter/internal/core"`(在文件 import 块中追加)。

- [ ] **Step 2: app.go 设置 multiTenant**

在 `internal/app/app.go` 的 initAdmin 返回后、`serverCfg.DashboardHandler = dashHandler` 之前,设置:

```go
				if dashHandler != nil {
					dashHandler.SetMultiTenant(appCfg.Server.BaseDomain != "" && tenantSvc != nil)
				}
```

位置:在 `if adminCfg.UIEnabled {` 块内、`serverCfg.DashboardHandler = dashHandler` 赋值之前(app.go:652-655 附近)。

- [ ] **Step 3: layout.html 输出 IsPlatformAdmin 到全局**

修改 `internal/admin/dashboard/templates/layout.html` 的 head 内 bootstrap script(在 `const basePath = "{{.BasePath}}";` 一行附近)追加:

```html
            window.SMARTROUTER_IS_PLATFORM_ADMIN = {{if .IsPlatformAdmin}}true{{else}}false{{end}};
```

- [ ] **Step 4: 构建**

Run: `go build ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/admin/dashboard/dashboard.go internal/app/app.go internal/admin/dashboard/templates/layout.html
git commit -m "feat(dashboard): inject isPlatformAdmin role into template data"
```

---

### Task 3: sidebar Tenants 导航项 + page-tenants 模板

**Files:**
- Modify: `internal/admin/dashboard/templates/sidebar.html`
- Create: `internal/admin/dashboard/templates/page-tenants.html`
- Modify: `internal/admin/dashboard/templates/index.html`

**Interfaces:**
- Consumes: `templateData.IsPlatformAdmin`(Task 2), Alpine `isPlatformAdmin`(Task 4 的 JS 状态)。
- Produces: `page-tenants.html` 模板 define `dashboard-page-tenants`,供 `index.html` 引用;sidebar 增加 "Tenants" 导航项。

- [ ] **Step 1: sidebar 加 Tenants 导航项**

在 `internal/admin/dashboard/templates/sidebar.html` 的 `<nav class="sidebar-nav">` 内、Overview 之后插入:

```html
        <a href="{{appURL "/admin/dashboard/tenants"}}" class="nav-item" :class="{ active: page === 'tenants' }" title="Tenants" x-show="isPlatformAdmin" @click.prevent="navigate('tenants')">
            <i data-lucide="building-2" class="nav-icon" aria-hidden="true"></i>
            <span>Tenants</span>
        </a>
```

- [ ] **Step 2: 创建 page-tenants.html**

创建 `internal/admin/dashboard/templates/page-tenants.html`,内容为租户列表 + 新建/编辑/停用交互的最小页面(基于 `page-auth-keys.html` 的模式)。租户响应字段: `id/subdomain/name/status/plan/created_at/updated_at`;列表响应 `{"tenants":[...]}`。

```html
{{define "dashboard-page-tenants"}}
<!-- Tenants Page (platform admin only) -->
<template x-if="page==='tenants'">
<div>
    <div class="page-header">
        <h2>Tenants</h2>
        <div class="page-header-controls">
            <button type="button" class="pagination-btn pagination-btn-primary pagination-btn-with-icon"
                x-show="!tenantsLoading && !tenantError"
                :disabled="tenantFormSubmitting"
                @click="if (!tenantFormSubmitting) openTenantForm()">
                {{template "plus-icon"}}
                <span>Create Tenant</span>
            </button>
        </div>
    </div>

    {{template "auth-banner" .}}

    <p class="form-error" x-show="tenantError" x-text="tenantError" role="alert" aria-live="assertive"></p>
    <p class="form-hint" x-show="tenantNotice" x-text="tenantNotice"></p>

    <div class="editor-modal-backdrop" x-cloak x-show="tenantFormOpen" x-transition.opacity.duration.160ms aria-hidden="true"></div>
    <div class="editor-modal-shell" x-cloak x-show="tenantFormOpen" x-transition.opacity.duration.160ms
         @click="closeTenantForm()"
         @keydown.escape.window="tenantFormOpen && !authDialogOpen && closeTenantForm()">
        <section class="model-editor" x-show="tenantFormOpen" role="dialog" aria-modal="true" aria-label="Tenant editor" @click.stop>
            <form class="form" @submit.prevent="submitTenantForm()">
                <div class="editor-header">
                    <div>
                        <p class="form-kicker">Tenant</p>
                        <h3 x-text="tenantEditing ? tenantForm.name : 'Create Tenant'"></h3>
                    </div>
                    <button type="button" class="dialog-close-btn" aria-label="Close tenant editor" @click="closeTenantForm()">
                        {{template "x-icon"}}
                    </button>
                </div>
                <div class="form-field">
                    <label class="form-field-label" for="tenant-subdomain">Subdomain <span class="form-hint">(required, immutable)</span></label>
                    <input id="tenant-subdomain" type="text" placeholder="e.g. acme" x-model="tenantForm.subdomain" :disabled="tenantEditing" x-ref="tenantSubdomainInput" data-modal-autofocus>
                </div>
                <div class="form-field">
                    <label class="form-field-label" for="tenant-name">Name <span class="form-hint">(required)</span></label>
                    <input id="tenant-name" type="text" placeholder="e.g. Acme Corp" x-model="tenantForm.name">
                </div>
                <div class="form-field">
                    <label class="form-field-label" for="tenant-plan">Plan <span class="form-hint">(optional)</span></label>
                    <input id="tenant-plan" type="text" placeholder="e.g. pro" x-model="tenantForm.plan">
                </div>
                <p class="form-error" x-show="tenantFormError" x-text="tenantFormError" role="alert" aria-live="assertive"></p>
                <div class="form-actions">
                    <button type="button" class="pagination-btn" @click="closeTenantForm()">Cancel</button>
                    <button type="submit" class="pagination-btn pagination-btn-primary"
                        :disabled="tenantFormSubmitting"
                        x-text="tenantFormSubmitting ? 'Saving...' : (tenantEditing ? 'Save' : 'Create Tenant')"></button>
                </div>
            </form>
        </section>
    </div>

    <div class="table-wrapper" x-show="tenants.length > 0">
        <table class="data-table">
            <thead>
                <tr>
                    <th>Name</th>
                    <th>Subdomain</th>
                    <th>Status</th>
                    <th>Plan</th>
                    <th>Created</th>
                    <th></th>
                </tr>
            </thead>
            <tbody>
                <template x-for="t in tenants" :key="t.id">
                    <tr>
                        <td x-text="t.name"></td>
                        <td><code class="auth-key-redacted" x-text="t.subdomain"></code></td>
                        <td>
                            <span class="auth-key-status-badge"
                                :class="t.status === 'active' ? 'auth-key-status-active' : 'auth-key-status-inactive'"
                                x-text="t.status"></span>
                        </td>
                        <td x-text="t.plan || '\u2014'"></td>
                        <td x-text="formatTimestamp(t.created_at)"></td>
                        <td>
                            <button type="button" class="table-action-btn" @click="openTenantForm(t)">Edit</button>
                            <button type="button" class="table-action-btn table-action-btn-danger"
                                x-show="t.status === 'active'"
                                :disabled="tenantDisablingID === t.id"
                                @click="disableTenant(t)"
                                x-text="tenantDisablingID === t.id ? 'Disabling...' : 'Disable'"></button>
                        </td>
                    </tr>
                </template>
            </tbody>
        </table>
    </div>

    <p class="empty-state" x-show="tenants.length === 0 && !tenantsLoading && !tenantError">
        No tenants yet. Create a tenant to get started.
    </p>
</div>
</template>
{{end}}
```

- [ ] **Step 3: index.html 注册页面**

在 `internal/admin/dashboard/templates/index.html` 的 define 列表中加入一行:

```html
{{template "dashboard-page-tenants" .}}
```

- [ ] **Step 4: 提交**

```bash
git add internal/admin/dashboard/templates/sidebar.html internal/admin/dashboard/templates/page-tenants.html internal/admin/dashboard/templates/index.html
git commit -m "feat(dashboard): add platform-only Tenants page and nav item"
```

---

### Task 4: tenants.js 模块 + dashboard.js 接线

**Files:**
- Create: `internal/admin/dashboard/static/js/modules/tenants.js`
- Create: `internal/admin/dashboard/static/js/modules/tenants.test.cjs`
- Modify: `internal/admin/dashboard/static/js/dashboard.js`(_parseRoute、_applyRoute、moduleFactories)
- Modify: `internal/admin/dashboard/templates/layout.html`(script 标签)

**Interfaces:**
- Consumes: `window.SMARTROUTER_IS_PLATFORM_ADMIN`(Task 2), dashboard 基础组件方法 `requestOptions()`/`handleFetchResponse()`/`headers()`/`renderIconsAfterUpdate()`/`formatTimestamp()`。
- Produces: `window.dashboardTenantsModule` 工厂函数(返回含 tenants 状态与方法的对象),dashboard Alpine 组件获得 `isPlatformAdmin` 状态与 tenants 方法。

- [ ] **Step 1: 创建 tenants.js**

创建 `internal/admin/dashboard/static/js/modules/tenants.js`(参考 `budgets.js` 的 IIFE + 工厂模式):

```js
(function(global) {
    function dashboardTenantsModule() {
        return {
            isPlatformAdmin: typeof global.SMARTROUTER_IS_PLATFORM_ADMIN !== 'undefined'
                ? !!global.SMARTROUTER_IS_PLATFORM_ADMIN
                : true,
            tenants: [],
            tenantsLoading: false,
            tenantError: '',
            tenantNotice: '',
            tenantFormOpen: false,
            tenantFormSubmitting: false,
            tenantFormError: '',
            tenantEditing: false,
            tenantEditingID: '',
            tenantDisablingID: '',
            tenantFetchPromise: null,
            tenantForm: {
                subdomain: '',
                name: '',
                plan: ''
            },

            defaultTenantForm() {
                return { subdomain: '', name: '', plan: '' };
            },

            tenantPayload() {
                const subdomain = String(this.tenantForm.subdomain || '').trim().toLowerCase();
                const name = String(this.tenantForm.name || '').trim();
                if (!subdomain || !name) {
                    this.tenantFormError = 'Subdomain and name are required.';
                    return null;
                }
                const payload = { subdomain, name };
                const plan = String(this.tenantForm.plan || '').trim();
                if (plan) payload.plan = plan;
                return payload;
            },

            async fetchTenantsPage() {
                if (this.tenantFetchPromise) {
                    return this.tenantFetchPromise;
                }
                this.tenantFetchPromise = this.fetchTenants().finally(() => {
                    this.tenantFetchPromise = null;
                });
                return this.tenantFetchPromise;
            },

            async fetchTenants() {
                this.tenantsLoading = true;
                this.tenantError = '';
                try {
                    const request = this.requestOptions();
                    const res = await fetch('/admin/tenants', request);
                    const handled = this.handleFetchResponse(res, 'tenants', request);
                    if (this.isStaleAuthFetchResult(handled)) {
                        return;
                    }
                    if (!handled) {
                        this.tenantError = 'Unable to load tenants.';
                        return;
                    }
                    const payload = await res.json();
                    this.tenants = payload && Array.isArray(payload.tenants) ? payload.tenants : [];
                    if (typeof this.renderIconsAfterUpdate === 'function') {
                        this.renderIconsAfterUpdate();
                    }
                } catch (e) {
                    console.error('Failed to fetch tenants:', e);
                    this.tenantError = 'Unable to load tenants.';
                } finally {
                    this.tenantsLoading = false;
                }
            },

            openTenantForm(item) {
                this.tenantEditing = !!item;
                this.tenantEditingID = item ? item.id : '';
                this.tenantFormError = '';
                this.tenantError = '';
                this.tenantNotice = '';
                this.tenantForm = item
                    ? { subdomain: item.subdomain, name: item.name, plan: item.plan || '' }
                    : this.defaultTenantForm();
                this.tenantFormOpen = true;
                if (typeof this.renderIconsAfterUpdate === 'function') {
                    this.renderIconsAfterUpdate();
                }
                if (typeof this.$nextTick === 'function') {
                    this.$nextTick(() => {
                        const refs = this.$refs || {};
                        const input = refs.tenantSubdomainInput;
                        if (input && typeof input.focus === 'function') {
                            input.focus({ preventScroll: true });
                        }
                    });
                }
            },

            closeTenantForm() {
                this.tenantFormOpen = false;
                this.tenantFormSubmitting = false;
                this.tenantFormError = '';
                this.tenantEditing = false;
                this.tenantEditingID = '';
                this.tenantForm = this.defaultTenantForm();
            },

            async submitTenantForm() {
                if (this.tenantFormSubmitting) {
                    return;
                }
                const payload = this.tenantPayload();
                if (!payload) {
                    return;
                }
                this.tenantFormSubmitting = true;
                this.tenantFormError = '';
                this.tenantError = '';
                this.tenantNotice = '';
                try {
                    // Create: POST /admin/tenants; Update: PATCH /admin/tenants/:id
                    const request = this.requestOptions({
                        method: this.tenantEditing ? 'PATCH' : 'POST',
                        body: JSON.stringify(payload)
                    });
                    const res = await fetch(this.tenantEditing
                        ? '/admin/tenants/' + encodeURIComponent(this.tenantEditingID)
                        : '/admin/tenants', request);
                    const handled = this.handleFetchResponse(res, 'tenant', request);
                    if (this.isStaleAuthFetchResult(handled)) {
                        return;
                    }
                    if (!handled) {
                        this.tenantFormError = await this.tenantResponseMessage(res, 'Unable to save tenant.');
                        return;
                    }
                    this.closeTenantForm();
                    await this.fetchTenants();
                    this.tenantNotice = this.tenantEditing ? 'Tenant saved.' : 'Tenant created.';
                } catch (e) {
                    console.error('Failed to save tenant:', e);
                    this.tenantFormError = 'Unable to save tenant.';
                } finally {
                    this.tenantFormSubmitting = false;
                }
            },

            async disableTenant(item) {
                if (!item || this.tenantDisablingID) {
                    return;
                }
                if (global.confirm && !global.confirm('Disable tenant "' + item.name + '"? This soft-deletes the tenant and blocks its API keys.')) {
                    return;
                }
                this.tenantDisablingID = item.id;
                this.tenantError = '';
                this.tenantNotice = '';
                try {
                    const request = this.requestOptions({ method: 'DELETE' });
                    const res = await fetch('/admin/tenants/' + encodeURIComponent(item.id), request);
                    const handled = this.handleFetchResponse(res, 'tenant disable', request);
                    if (this.isStaleAuthFetchResult(handled)) {
                        return;
                    }
                    if (!handled) {
                        this.tenantError = await this.tenantResponseMessage(res, 'Unable to disable tenant.');
                        return;
                    }
                    await this.fetchTenants();
                    this.tenantNotice = 'Tenant disabled.';
                } catch (e) {
                    console.error('Failed to disable tenant:', e);
                    this.tenantError = 'Unable to disable tenant.';
                } finally {
                    this.tenantDisablingID = '';
                }
            },

            async tenantResponseMessage(res, fallback) {
                try {
                    const payload = await res.json();
                    if (payload && payload.error && payload.error.message) {
                        return payload.error.message;
                    }
                } catch (_) {
                    // Ignore invalid responses; use the fallback message.
                }
                return fallback;
            }
        };
    }

    global.dashboardTenantsModule = dashboardTenantsModule;
})(typeof window !== 'undefined' ? window : globalThis);
```

注意:Task 3 模板中 "Create Tenant" 按钮用 POST 语义但本模块统一走 PUT `/admin/tenants`(现有 admin API 的 upsert 语义)。若实现时发现 POST 更贴合,可在 Task 3 模板按钮处同步调整。

- [ ] **Step 2: 创建 tenants.test.cjs**

创建 `internal/admin/dashboard/static/js/modules/tenants.test.cjs`,复用 `pricing.test.cjs` 的 vm 加载模式:

```js
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadTenantsModuleFactory(overrides = {}) {
    const source = fs.readFileSync(path.join(__dirname, 'tenants.js'), 'utf8');
    const context = { console, setTimeout, clearTimeout, ...overrides };
    vm.createContext(context);
    vm.runInContext(source, context);
    return context.dashboardTenantsModule;
}

function createTenantsModule(overrides) {
    const factory = loadTenantsModuleFactory(overrides);
    return factory();
}

test('tenantPayload requires subdomain and name', () => {
    const module = createTenantsModule();
    module.tenantForm = { subdomain: '  acme  ', name: '  Acme Corp  ', plan: ' pro ' };
    assert.deepEqual(module.tenantPayload(), { subdomain: 'acme', name: 'Acme Corp', plan: 'pro' });
});

test('tenantPayload rejects empty subdomain or name', () => {
    const module = createTenantsModule();
    module.tenantForm = { subdomain: '', name: 'Acme', plan: '' };
    assert.equal(module.tenantPayload(), null);
    assert.match(module.tenantFormError, /required/);
});

test('fetchTenants loads { tenants: [...] } from /admin/tenants', async () => {
    const module = createTenantsModule();
    module.requestOptions = () => ({ headers: {} });
    module.handleFetchResponse = () => true;
    module.isStaleAuthFetchResult = () => false;
    module.renderIconsAfterUpdate = () => {};
    let requestedURL = null;
    module.fetch = async (url) => {
        requestedURL = url;
        return {
            ok: true,
            status: 200,
            json: async () => ({ tenants: [{ id: 't1', subdomain: 'acme', name: 'Acme', status: 'active' }] })
        };
    };
    await module.fetchTenants();
    assert.equal(requestedURL, '/admin/tenants');
    assert.equal(module.tenants.length, 1);
    assert.equal(module.tenants[0].subdomain, 'acme');
});

test('fetchTenants sets error on non-ok response', async () => {
    const module = createTenantsModule();
    module.requestOptions = () => ({ headers: {} });
    module.handleFetchResponse = () => false;
    module.isStaleAuthFetchResult = () => false;
    module.fetch = async () => ({ ok: false, status: 500 });
    await module.fetchTenants();
    assert.match(module.tenantError, /Unable to load tenants/);
});

test('isPlatformAdmin defaults to true when flag unset', () => {
    const module = createTenantsModule();
    assert.equal(module.isPlatformAdmin, true);
});

test('isPlatformAdmin respects window flag', () => {
    const module = createTenantsModule({
        SMARTROUTER_IS_PLATFORM_ADMIN: false
    });
    assert.equal(module.isPlatformAdmin, false);
});
```

注意:测试中 `fetch` 赋值直接放在模块对象上会被 `fetchTenants` 调用(模块方法体内是 `fetch(...)` 裸调用,在 vm 全局上下文解析——若裸 `fetch` 未在模块工厂里闭包捕获,测试里需设置 context.fetch 而非 module.fetch。实现时以实际行为为准:在 `createTenantsModule({ fetch })` override 里提供全局 fetch)。

- [ ] **Step 3: dashboard.js 接线**

修改 `internal/admin/dashboard/static/js/dashboard.js`:

1. `_parseRoute` 中合法 page 列表加入 `"tenants"`:

```js
        "guardrails",
        "auth-keys",
        "tenants",
        "settings",
```

2. `_applyRoute` 中加入 tenants 分支(放在 budgets 分支附近):

```js
      if (page === "tenants" && typeof this.fetchTenantsPage === "function") {
        this.fetchTenantsPage();
      }
```

3. `moduleFactories` 数组末尾追加:

```js
    resolveModuleFactory(
      typeof dashboardTenantsModule === "function"
        ? dashboardTenantsModule
        : null,
      "dashboardTenantsModule",
    ),
```

- [ ] **Step 4: layout.html 加载模块脚本**

在 `internal/admin/dashboard/templates/layout.html` 的 `<script src=... js/modules/pricing.js>...</script>` 附近追加:

```html
    <script src="{{assetURL "js/modules/tenants.js"}}"></script>
```

- [ ] **Step 5: 运行 JS 测试**

Run: `node --test internal/admin/dashboard/static/js/modules/tenants.test.cjs -v`
Expected: PASS(全部测试)

若测试中因 `fetch` 裸调用失败,调整测试用 `createTenantsModule({ fetch: async () => {...} })` 注入全局 fetch,再重跑。

- [ ] **Step 6: 提交**

```bash
git add internal/admin/dashboard/static/js/modules/tenants.js internal/admin/dashboard/static/js/modules/tenants.test.cjs internal/admin/dashboard/static/js/dashboard.js internal/admin/dashboard/templates/layout.html
git commit -m "feat(dashboard): add tenants management module with unit tests"
```

---

### Task 5: dashboard 角色渲染测试(Go)

**Files:**
- Test: `internal/admin/dashboard/dashboard_test.go`

**Interfaces:**
- Consumes: `dashboard.Handler` + `SetMultiTenant`, `core.WithPlatformHost`。
- Produces: 无新接口。

- [ ] **Step 1: 追加角色渲染测试**

在 `internal/admin/dashboard/dashboard_test.go` 末尾追加:

```go
func TestIndex_IsPlatformAdmin_LegacyMode(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.Index(c); err != nil {
		t.Fatalf("Index() returned error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "window.SMARTROUTER_IS_PLATFORM_ADMIN = true;") {
		t.Errorf("expected SMARTROUTER_IS_PLATFORM_ADMIN=true in legacy mode (no multi-tenant), got: %.200s", rec.Body.String())
	}
}

func TestIndex_IsPlatformAdmin_TenantHost(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	h.SetMultiTenant(true)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil)
	ctx := core.WithPlatformHost(req.Context(), false)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.Index(c); err != nil {
		t.Fatalf("Index() returned error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "window.SMARTROUTER_IS_PLATFORM_ADMIN = false;") {
		t.Errorf("expected SMARTROUTER_IS_PLATFORM_ADMIN=false on tenant host, got: %.200s", rec.Body.String())
	}
}

func TestIndex_IsPlatformAdmin_PlatformHost(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	h.SetMultiTenant(true)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil)
	ctx := core.WithPlatformHost(req.Context(), true)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.Index(c); err != nil {
		t.Fatalf("Index() returned error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "window.SMARTROUTER_IS_PLATFORM_ADMIN = true;") {
		t.Errorf("expected SMARTROUTER_IS_PLATFORM_ADMIN=true on platform host, got: %.200s", rec.Body.String())
	}
}
```

需要追加 import:`"smartrouter/internal/core"`。

- [ ] **Step 2: 运行测试**

Run: `go test ./internal/admin/dashboard/... -v`
Expected: PASS(含既有测试)

- [ ] **Step 3: 提交**

```bash
git add internal/admin/dashboard/dashboard_test.go
git commit -m "test(dashboard): assert isPlatformAdmin rendering per host kind"
```

---

### Task 6: 跨租户端到端集成测试

**Files:**
- Create: `internal/server/p7_e2e_integration_test.go`

**Interfaces:**
- Consumes: `newMemoryDB(t)`、`adminSplitReq(t, e, method, host, path, token, body)`(admin_split_integration_test.go)、`adminSplitBaseDomain/adminSplitPlatformHost/adminSplitMasterKey` 常量、`tenantVisEcho` 模式、`core.WithPlatformHost/WithTenantID`。
- Produces: 无新生产接口;新增一个测试文件验证租户隔离与 admin 路由分发。

- [ ] **Step 1: 创建测试文件框架**

创建 `internal/server/p7_e2e_integration_test.go`,复用 `admin_split_integration_test.go` 的同包 helper(`adminSplitReq`/`decodeJSON`/`adminSplitIssuedKey`/`newMemoryDB`,以及 `adminSplitBaseDomain/adminSplitPlatformHost/adminSplitMasterKey` 常量)与 `tenant_visibility_integration_test.go` 的 full-chain 思路。文件结构:

```go
package server

// P7: cross-tenant end-to-end integration. Assembles the real chain
// (TenantResolver → AuthMiddlewareWithAuthenticator → mountAdminRoutesByHost
// → /v1 group with hostGuard("tenant")) with real SQLite stores + services,
// mirroring internal/http.go's production wiring, and drives requests with
// real Host headers. Extends the P4/P5 integration tests with the remaining
// P7 matrix: auth-key tenant isolation, disabled-tenant 403, and the
// platform/tenant hostGuard 404s on both admin and /v1 planes.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"smartrouter/internal/admin"
	"smartrouter/internal/authkeys"
	"smartrouter/internal/providers"
	"smartrouter/internal/tenants"

	_ "modernc.org/sqlite"
)

// p7Echo builds the full production-shaped chain: TenantResolver → auth →
// mountAdminRoutesByHost → /v1 group carrying hostGuard("tenant") with a stub
// inference handler. This mirrors http.go (unlike adminSplitEcho, whose /v1
// stub is NOT host-guarded). baseDomainConfigured=true always.
func p7Echo(t *testing.T, authSvc *authkeys.Service, tenantSvc *tenants.Service, platform *admin.PlatformAdminHandler, tenant *admin.TenantAdminHandler) *echo.Echo {
	t.Helper()
	e := echo.New()
	e.Use(TenantResolver(tenantSvc, adminSplitBaseDomain, adminSplitPlatformHost))
	e.Use(AuthMiddlewareWithAuthenticator(adminSplitMasterKey, authSvc, nil))
	mountAdminRoutesByHost(e.Group("/admin"), platform, tenant, true)
	v1 := e.Group("/v1", hostGuard("tenant"))
	v1.POST("/chat/completions", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	return e
}
```

- [ ] **Step 2: 写核心测试 TestP7CrossTenantAuthKeyIsolation**

```go
// TestP7CrossTenantAuthKeyIsolation verifies two tenants' admin surfaces do not
// leak auth keys into each other, and that hostGuard keeps platform-only admin
// routes off tenant hosts and tenant-only inference routes off the platform host.
func TestP7CrossTenantAuthKeyIsolation(t *testing.T) {
	ctx := context.Background()

	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	require.NoError(t, authSvc.Refresh(ctx))

	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	tenantSvc := tenants.NewService(tenantStore, time.Minute, adminSplitPlatformHost)
	now := time.Now().UTC()
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-a", Subdomain: "a", Name: "Tenant A", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-b", Subdomain: "b", Name: "Tenant B", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))

	// Full production-style wiring (mirrors app.go initAdmin + split).
	adminDefault := admin.NewHandler(nil, providers.NewModelRegistry(),
		admin.WithAuthKeys(authSvc),
	)
	platformHandler := &admin.PlatformAdminHandler{Tenants: tenantSvc, AuthKeys: authSvc, Default: adminDefault}
	tenantHandler := &admin.TenantAdminHandler{AuthKeys: authSvc, Config: adminDefault}
	e := p7Echo(t, authSvc, tenantSvc, platformHandler, tenantHandler)

	// Issue a tenant-admin key for A and B via the platform admin API.
	platformHost := adminSplitPlatformHost + "." + adminSplitBaseDomain
	hostA := "a." + adminSplitBaseDomain
	hostB := "b." + adminSplitBaseDomain

	rec := adminSplitReq(t, e, http.MethodPost, platformHost, "/admin/tenants/tenant-a/admin-keys", adminSplitMasterKey,
		map[string]any{"name": "A admin"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	adminKeyA := decodeJSON[adminSplitIssuedKey](t, rec)

	rec = adminSplitReq(t, e, http.MethodPost, platformHost, "/admin/tenants/tenant-b/admin-keys", adminSplitMasterKey,
		map[string]any{"name": "B admin"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	adminKeyB := decodeJSON[adminSplitIssuedKey](t, rec)

	// Tenant A creates a regular API key; tenant B must not see it.
	rec = adminSplitReq(t, e, http.MethodPost, hostA, "/admin/auth-keys", adminKeyA.Value,
		map[string]any{"name": "A secret key"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = adminSplitReq(t, e, http.MethodGet, hostB, "/admin/auth-keys", adminKeyB.Value, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "A secret key", "tenant B must not see tenant A's auth key")
	require.Contains(t, rec.Body.String(), "B admin", "tenant B must see its own admin key")

	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/admin/auth-keys", adminKeyA.Value, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "A secret key", "tenant A must see its own auth key")

	// Platform-only route 404s on a tenant host even for the master key.
	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/admin/tenants", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "platform-only /admin/tenants must 404 on a tenant host")

	// Tenant-only /admin/auth-keys DELETE 404s on the platform host.
	rec = adminSplitReq(t, e, http.MethodDelete, platformHost, "/admin/auth-keys/any-id", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "tenant-only route must 404 on the platform host")

	// Tenant A's tenant-admin key is rejected on the platform host.
	rec = adminSplitReq(t, e, http.MethodGet, platformHost, "/admin/auth-keys", adminKeyA.Value, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "tenant admin key on platform host must 401")
	require.Contains(t, rec.Body.String(), "key_not_allowed_on_platform_host")
}
```

- [ ] **Step 3: 写禁用租户 + hostGuard 测试**

```go
// TestP7DisabledTenantBlocked verifies a disabled tenant's host returns 403
// from the TenantResolver before any auth happens, on both admin and /v1 paths.
func TestP7DisabledTenantBlocked(t *testing.T) {
	ctx := context.Background()

	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	require.NoError(t, authSvc.Refresh(ctx))

	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	tenantSvc := tenants.NewService(tenantStore, time.Minute, adminSplitPlatformHost)
	now := time.Now().UTC()
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-disabled", Subdomain: "dead", Name: "Dead Tenant", Status: tenants.StatusDisabled, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-active", Subdomain: "live", Name: "Live Tenant", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))

	platformHandler := &admin.PlatformAdminHandler{Tenants: tenantSvc, AuthKeys: authSvc}
	tenantHandler := &admin.TenantAdminHandler{AuthKeys: authSvc}
	e := p7Echo(t, authSvc, tenantSvc, platformHandler, tenantHandler)

	// Disabled tenant: /v1 inference is 403 (tenant_disabled) even with the master key.
	rec := adminSplitReq(t, e, http.MethodPost, "dead."+adminSplitBaseDomain, "/v1/chat/completions", adminSplitMasterKey, map[string]any{"model": "gpt-4o"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "tenant_disabled")

	// Disabled tenant: admin path also 403 from the resolver.
	rec = adminSplitReq(t, e, http.MethodGet, "dead."+adminSplitBaseDomain, "/admin/auth-keys", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "tenant_disabled")

	// Active tenant admin path works.
	rec = adminSplitReq(t, e, http.MethodGet, "live."+adminSplitBaseDomain, "/admin/auth-keys", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
```

- [ ] **Step 4: 写平台/租户 hostGuard 测试(/v1 + /admin)**

```go
// TestP7HostGuardSeparation verifies the route-level host guard keeps the
// inference /v1/* surface off the platform host and the admin surface split
// correct on both hosts.
func TestP7HostGuardSeparation(t *testing.T) {
	ctx := context.Background()

	authStore, err := authkeys.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = authStore.Close() })
	authSvc, err := authkeys.NewService(authStore)
	require.NoError(t, err)
	require.NoError(t, authSvc.Refresh(ctx))

	tenantStore, err := tenants.NewSQLiteStore(newMemoryDB(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tenantStore.Close() })
	tenantSvc := tenants.NewService(tenantStore, time.Minute, adminSplitPlatformHost)
	now := time.Now().UTC()
	require.NoError(t, tenantStore.Create(ctx, tenants.Tenant{ID: "tenant-a", Subdomain: "a", Name: "Tenant A", Status: tenants.StatusActive, CreatedAt: now, UpdatedAt: now}))
	keyA, err := authSvc.Create(ctx, authkeys.CreateInput{Name: "A key", TenantID: "tenant-a"})
	require.NoError(t, err)

	platformHandler := &admin.PlatformAdminHandler{Tenants: tenantSvc, AuthKeys: authSvc}
	tenantHandler := &admin.TenantAdminHandler{AuthKeys: authSvc}
	e := p7Echo(t, authSvc, tenantSvc, platformHandler, tenantHandler)

	platformHost := adminSplitPlatformHost + "." + adminSplitBaseDomain
	hostA := "a." + adminSplitBaseDomain

	// /v1 inference is tenant-only: platform host 404s (empty body from hostGuard).
	rec := adminSplitReq(t, e, http.MethodPost, platformHost, "/v1/chat/completions", adminSplitMasterKey, map[string]any{"model": "gpt-4o"})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Empty(t, rec.Body.String(), "hostGuard 404 must be empty body")

	// Tenant host /v1 works with the tenant's API key.
	rec = adminSplitReq(t, e, http.MethodPost, hostA, "/v1/chat/completions", keyA.Value, map[string]any{"model": "gpt-4o"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// /admin/tenants (platform-only) 404s on the tenant host.
	rec = adminSplitReq(t, e, http.MethodGet, hostA, "/admin/tenants", adminSplitMasterKey, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}
```

- [ ] **Step 5: 运行测试**

Run: `go test ./internal/server/ -run 'TestP7' -v`
Expected: PASS(全部 P7 用例;若失败按错误修到通过)

- [ ] **Step 6: 提交**

```bash
git add internal/server/p7_e2e_integration_test.go
git commit -m "test(server): add cross-tenant end-to-end integration tests"
```

---

### Task 7: 部署文档

**Files:**
- Create: `docs/deployment/multi-tenant.md`

**Interfaces:**
- Consumes: `config.example.yaml` 的配置项(`base_domain`/`platform_host`/`bootstrap_default_tenant` + env 名)、`docs/superpowers/specs/2026-08-02-saas-multi-tenant-design.md` 的架构图。
- Produces: 运维可读的部署文档。

- [ ] **Step 1: 确认 docs/deployment 目录**

Run: `ls docs/`
若 `docs/deployment/` 不存在,创建目录(`mkdir -p docs/deployment`)。

- [ ] **Step 2: 编写 multi-tenant.md**

创建 `docs/deployment/multi-tenant.md`,覆盖:
1. 多租户架构图(平台 host vs 租户子域名分流,引用设计文档)。
2. 前置条件:`storage.type` 必须有共享存储(SQLite/PG/Mongo),`server.base_domain` 设置。
3. 配置项表:`base_domain`(SERVER_BASE_DOMAIN)、`platform_host`(SERVER_PLATFORM_HOST)、`bootstrap_default_tenant`(BOOTSTRAP_DEFAULT_TENANT)、`admin.endpoints_enabled`(ADMIN_ENDPOINTS_ENABLED)、`admin.ui_enabled`(ADMIN_UI_ENABLED)、`server.master_key`。
4. 启动流程:空库自动 bootstrap `default` 租户 → 平台 host(app.<base_domain>)用 master key 建租户 → 签 tenant admin key → 租户 admin 配 virtual_models/预算等。
5. 启动检查清单:/health、/ready、平台 host 登录 dashboard、租户 host 登录 dashboard、/v1 推理请求。
6. 已知限制(P6 deferred):ForTenant 写透传刷新时延(配置服务 up to 1h tick)、`ResolveRequestModel` 的 `context.Background()`(仅测试调用)、`RefreshAll` 整表换快照语义、dashboard JS 3 个时区环境相关测试失败、`go vet` 3 个预存警告。
7. 明确说明:P7 不提供单租户→多租户迁移脚本(无旧版兼容路径);新部署直接按本清单启用多租户。

- [ ] **Step 3: 提交**

```bash
git add docs/deployment/multi-tenant.md
git commit -m "docs(deployment): add multi-tenant deployment guide"
```

---

### Task 8: ROADMAP 修正

**Files:**
- Modify: `docs/superpowers/ROADMAP.md`

**Interfaces:**
- Consumes: git log 中 P4 commit 范围(`321cad4..3b77fd0`,18 commits)。
- Produces: ROADMAP 状态准确,含 P7 完成记录。

- [ ] **Step 1: 修正 P4 状态**

将 ROADMAP.md 中 P4 行状态 `⬜ 待开始` 改为:

```
| P4 | Admin 拆分 | PlatformAdminHandler vs TenantAdminHandler、路由按 Host 分流(hostGuard)、tenant CRUD、六个配置类 Service 的管理面 tenantID 补全 | ✅ 完成 (2026-08-03) |
```

- [ ] **Step 2: 追加 P7 完成记录**

在 ROADMAP.md 末尾追加(记录真实 commit 范围与交付内容,完成后用实际值替换占位):

```markdown
## P7 完成 (2026-08-05)

- `<start>..<end>` commits
- nil-ctx guard:`ResolveRequestModelWithAuthorizer` 防御 nil context
- Dashboard role-aware UI:注入 `IsPlatformAdmin`,平台 host 新增 Tenants 管理页 + tenants.js 模块
- 跨租户端到端集成测试:`p7_e2e_integration_test.go`(auth key 隔离、禁用租户 403、hostGuard 404)
- 部署文档:`docs/deployment/multi-tenant.md`
- 详见 `docs/superpowers/plans/2026-08-05-saas-multi-tenant-p7.md` Completion Notes
- Deferred(记录不修复):ForTenant 写透传时延、ResolveRequestModel context.Background、RefreshAll 语义、dashboard timezone 测试、go vet 预存警告
```

- [ ] **Step 3: 提交**

```bash
git add docs/superpowers/ROADMAP.md
git commit -m "docs(roadmap): mark P4 complete; add P7 completion notes"
```

---

### Task 9: 最终验证

- [ ] **Step 1: 全量构建**

Run: `go build ./...`
Expected: PASS(零警告)

- [ ] **Step 2: 全量测试**

Run: `go test ./...`
Expected: PASS(全部包,零失败)

- [ ] **Step 3: vet**

Run: `go vet ./...`
Expected: PASS(仅接受 P6 已记录的 `internal/core` 3 个预存警告)

- [ ] **Step 4: dashboard JS 测试**

Run: `node --test internal/admin/dashboard/static/js/modules/*.test.cjs`
Expected: PASS(接受 P5 base 已记录的 3 个时区环境相关失败,其余 399+ 通过;新加 tenants.test.cjs 全绿)

- [ ] **Step 5: 更新 P7 plan 的 Completion Notes**

在 `docs/superpowers/plans/2026-08-05-saas-multi-tenant-p7.md`(本文件)末尾追加 P7 Completion Notes 段,记录实际 commit 范围、交付内容、deferred 项。若本文件未命名为此,按实际文件名追加。

- [ ] **Step 6: 提交**

```bash
git add docs/superpowers/plans/
git commit -m "docs(plan): append P7 completion notes"
```

---

## P7 Completion Notes(占位,完成后填)

P7 complete on master:`<start>..<end>`,subagent-driven with per-task review + final whole-branch review。

### Delivered

1. **nil-ctx guard**:`ResolveRequestModelWithAuthorizer` 加 `if ctx == nil { ctx = context.Background() }`,与 `inference_prepare.go` 既有防御模式一致。
2. **Dashboard role-aware UI**:`templateData` 加 `IsPlatformAdmin`;`Handler.SetMultiTenant` 由 app.go 按 `base_domain + tenantSvc` 设置;layout.html 输出 `window.SMARTROUTER_IS_PLATFORM_ADMIN`;sidebar 新增 Tenants 导航(平台 host 显示);新增 `page-tenants.html` + `tenants.js` 模块(列表/新建/编辑/停用),配 `.test.cjs` 单测。
3. **跨租户端到端测试**:`p7_e2e_integration_test.go` 覆盖 auth-key 租户隔离、禁用租户 403、平台/租户 hostGuard 404。
4. **部署文档**:`docs/deployment/multi-tenant.md`。
5. **ROADMAP**:P4 标记完成,P7 完成记录。

### Key decisions / deviations from plan text

- tenants 管理页 API:Create 用 `POST /admin/tenants`,Update 用 `PATCH /admin/tenants/:id`(与 `platform_routes.go` 一致;不用 PUT upsert)。
- `isPlatformAdmin` 在 JS 侧默认 true(单租户/测试环境不渲染 false),多租户租户 host 由模板注入 false。
- E2E 测试用自定义 `p7Echo` 链(而非 `adminSplitEcho`):后者 `/v1/chat/completions` stub 没有 `hostGuard("tenant")`,无法验证平台 host 404;`p7Echo` 按 http.go 真实装配 `hostGuard("tenant")`。
- P7 不写迁移脚本;deferred items 除 nil-ctx guard 外仅文档记录。

### Deferred(unchanged)

- ForTenant write-through refresh latency(配置服务,up to 1h tick 陈旧)
- `ResolveRequestModel` 硬编码 `context.Background()`(测试调用)
- nil-ctx guards on `snapshotFor`/`PipelineForWorkflow`(不可达)
- `RefreshAll` 整表换快照语义
- dashboard JS 3 个时区环境测试失败(预存)
- `go vet ./...` 3 个 `internal/core` 预存警告
