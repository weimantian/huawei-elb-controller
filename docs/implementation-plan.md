# 方案 2 实现计划

> **目标**: 实现 Service Reconciler（LBC 参数模板 + autocreate 创建 + API 更新）
> **基准文档**: `docs/design-plan2-recommended.md`
> **分支**: `design/auto-create-elb-with-database`

---

## 当前代码结构

```
cmd/main.go                              # 入口，注册 LBC Reconciler
internal/
  controller/
    loadbalancerconfig_controller.go     # LBC Reconciler（801 行）
  huaweicloud/
    elb.go                               # ELB API（创建、删除、查询、状态）
    client.go                            # 凭证加载
    vpc.go                               # VPC/子网/AZ 探测
cmd/list-vpcs/main.go                    # CLI 探测工具
```

---

## 实现步骤

### Phase 1: 重构共享组件

| # | 任务 | 详情 |
|---|---|---|
| P1-1 | 提取 `NetworkDetector` | 从 LBC Reconciler 的 `autoDetectParams` 方法提取为独立结构体 `internal/huaweicloud/detector.go`，返回 `(vpcID, subnetID, azs)` |
| P1-2 | 扩展 `ELBClient` | 在 `internal/huaweicloud/elb.go` 加更新方法：`UpdateELBBandwidth(name, size, chargeMode)` |
| P1-3 | 提取参数映射 | LBC 参数 → autocreate JSON 的映射逻辑，提取为 `internal/huaweicloud/params.go` |
| P1-4 | 提取 Service 工具函数 | Service 查询、验证（是否 OpenEverest 创建、是否有 elb.id）、批量查询 |

### Phase 2: 替换 Reconciler

| # | 任务 | 详情 |
|---|---|---|
| P2-1 | **新建** `internal/controller/service_controller.go` | Service Reconciler：watch Service CREATE/UPDATE/DELETE；创建路径：注入 autocreate；更新路径：调 ELB API；删除路径：CCM 原生 |
| P2-2 | **修改** `cmd/main.go` | 注册 Service Reconciler 替换 LBC Reconciler |
| P2-3 | **保留** `internal/controller/loadbalancerconfig_controller.go` | 保留文件但标记为 deprecated，若后续需要存量兼容可恢复 |

### Phase 3: 配置 + 部署

| # | 任务 | 详情 |
|---|---|---|
| P3-1 | 更新 RBAC | Service get/list/watch/patch，Node get/list/watch，移除 LBC delete 权限 |
| P3-2 | 更新 Helm chart | values.yaml 默认参数（带宽、EIP 类型等），新增 Service Reconciler 配置项 |
| P3-3 | 单元测试 | Service Reconciler 三个路径（创建/更新/删除）的 mock 测试 |
| P3-4 | 集成测试 | kind + mock 华为云 API 端到端测试 |

---

## Service Reconciler 核心逻辑

```
Reconcile(ctx, req) → Service
  │
  ├─ Service.type != LoadBalancer → skip
  ├─ 有 elb.id → skip（存量 CCM 绑定）
  │
  ├─ DELETION → CCM reclaim-policy: alwaysDelete → 已处理 ✅
  │             （加补偿扫描：删 Service 时确保 ELB 已删）
  │
  ├─ CREATE / UPDATE → 无 elb.autocreate → 创建路径
  │   ├─ 读 huawei-elb.io/* 参数（来自 LBC）
  │   │    无 → 用默认值（public/10M/traffic/5_bgp）
  │   ├─ NetworkDetector.Detect()
  │   ├─ 构造 autocreate JSON
  │   ├─ patch Service
  │   └─ return
  │
  └─ UPDATE → 已有 elb.autocreate → 更新路径
      ├─ 比较上次 reconcile 记录的 LBC 参数
      ├─ 有变化 → 调 ELB API 更新
      └─ return
```

---

## 预计改动量

| 文件 | 操作 | 行数 |
|------|------|------|
| `internal/huaweicloud/detector.go` | **新建** | +80 |
| `internal/huaweicloud/params.go` | **新建** | +60 |
| `internal/huaweicloud/elb.go` | **修改** | +40 |
| `internal/controller/service_controller.go` | **新建** | +300 |
| `cmd/main.go` | **修改** | -15/+15 |
| `internal/controller/loadbalancerconfig_controller.go` | **保留（不删）** | 0 |
| Helm chart | **修改** | -10/+15 |
| RBAC | **修改** | -5/+10 |
| 测试 | **新建** | +200 |
| **总计** | | **~700 行** |

---

## 确认项

| # | 问题 | 待确认 |
|---|---|---|
| C1 | 是否保留 LBC Reconciler 代码（标记 deprecation）？ | ✅ 保留文件，不注册 |
| C2 | 默认参数：public / 10M / traffic / 5_bgp 是否合适？ | 需确认 |
| C3 | ELB 参数更新是否同时支持带宽、EIP 共享类型等？ | 先实现带宽，后续扩展 |
| C4 | ACL（sourceRanges → elb.acl-*）是否在此阶段？ | P2 后续阶段 |
| C5 | Service 识别 OpenEverest 身份：通过 label 还是 ownerRef？ | 优先 label（`app.kubernetes.io/managed-by: percona-xtradb-cluster-operator`） |
