# SmartRouter SaaS 多租户改造 — P1-P7 项目计划

> 设计文档: `docs/superpowers/specs/2026-08-02-saas-multi-tenant-design.md`
> 各阶段 plan: `docs/superpowers/plans/2026-08-02-saas-multi-tenant-p<N>.md`

## 阶段总览

| 阶段 | 名称 | 核心内容 | 状态 |
|------|------|---------|------|
| P1 | 租户基座 | Tenant 实体、子域名解析中间件、context 传播、bootstrap default 租户 | ✅ 完成 |
| P2 | 认证与两级 Key | auth_keys 加 tenant_id/is_tenant_admin、中间件强制的租户匹配+角色 | ✅ 完成 |
| P3 | Store 隔离 | 12 个 Store 接口加 tenantID 参数、ListEffective 合并、隔离测试 | ✅ 完成 |
| P4 | Admin 拆分 | PlatformAdminHandler vs TenantAdminHandler、路由按 Host 分流(hostGuard)、tenant CRUD、六个配置类 Service 的管理面 tenantID 补全 | ⬜ 待开始 |
| P5 | 路由与 Host Guard | /v1/* host guard、provider 按租户可见性、配额中间件 | ✅ 完成 (2026-08-03) |
| P6 | 内存 Store DB 化 | conversationstore SQL/Mongo 实现、多实例横向扩展 | ✅ 完成 (2026-08-04) |
| P7 | 端到端集成 | 跨租户端到端测试、dashboard role-aware UI、部署文档、迁移脚本 | ⬜ 待开始 |

## 执行约定

- 直接在 master 上执行(单人开发,不开 feature 分支)
- 用 subagent-driven-development 执行
- 不主动 push,推送时机由用户决定
- 每阶段 deferred/minor 项记入 plan 的 Completion Notes

## 换电脑恢复

```bash
git clone <repo-url>
cd SmartRouter
# 读本文件了解进度,读对应 plan 文件了解细节
# 说 "继续实现 P4" 开始下一阶段
```

## P3 完成 (2026-08-02)

- 20 commits (`d080768..d6daef4`)
- 12/12 stores: 6 data (WHERE filter) + 6 config (ListEffective merge)
- All 3 backends (SQLite/PG/MongoDB)
- `go build ./...` + `go test ./...` all pass
- Deferred: MongoDB sort, PG/Mongo tests, per-tenant Service wiring

## P6 完成 (2026-08-04)

- 22 commits (`45530ee..3bed4a5`)
- conversationstore + responsestore 三后端 DB 化(SQLite/PG/Mongo),tenant_id 隔离,factory + app 装配
- 配额中间件 `/v1` 组挂载,`user_path="*"` 表示租户级聚合预算,超限 402
- 17 个新 SQLite-backed store 测试 + PG 聚合分支 env-gated 测试
- 详见 `docs/superpowers/plans/2026-08-04-saas-multi-tenant-p6.md` Completion Notes
- Deferred: P5/P4 minor 项(ForTenant write-through 时延、ResolveRequestModel context.Background 等),见 P6 plan
