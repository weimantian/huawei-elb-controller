# 最终方案设计：创建数据库时自动创建 ELB

> **基线文档**: `docs/design-auto-create-elb-with-database.md`（保留为设计过程备份）
> **状态**: 终稿
> **日期**: 2026-07-10

---

## 1. 背景与目标

### 1.1 当前流程（两步）

```
① 用户创建 LoadBalancerConfig（LBC）
② huawei-elb-controller 创建 ELB，写回 elb.id 到 LBC
③ 用户等待 LBC ready=true
④ 用户创建 DatabaseCluster（DBC），引用该 LBC
⑤ OpenEverest 创建 Service，复制 elb.id 到 Service
⑥ CCE CCM 绑定 ELB → Service 获得外部 IP
```

### 1.2 痛点

用户需两步操作、等待 LBC ready、理解 LBC 概念。对比 EKS/GKE 的体验——创建 `type: LoadBalancer` 的 Service 即自动获得负载均衡器，零额外配置。

### 1.3 目标

**用户只创建 DatabaseCluster，ELB 自动创建并绑定，无需手动建 LBC。** 体验对标 EKS/GKE。

---

## 2. 约束与关键事实

### 2.1 源码事实（OpenEverest 1.15.2，commit `a4519bd`）

| # | 事实 | 影响 |
|---|---|---|
| F1 | `loadBalancerConfigName` 留空时，OpenEverest 仍创建 LoadBalancer Service（带空注解） | 空值不阻塞 Service 创建 |
| F2 | `loadBalancerConfigName` 设了但 LBC 不存在时，Validating Webhook 在 CREATE 时拒绝 | DBC 创建时 LBC 必须先存在 |
| F3 | `ValidateUpdate` 不校验 LBC 存在性 | 更新时可指向不存在的 LBC |
| F4 | CEL 规则：`loadBalancerConfigName` 从 `""` 设为有值**允许** | 可后续 patch DBC 补该字段 |
| F5 | LBC 的 `spec.annotations` 变化后，OpenEverest 重新同步到 Service（非一次性拷贝） | LBC 注解后可补传播到 Service |
| F6 | LBC 是 cluster-scoped | 命名需全局唯一 |
| F7 | LBC 被 DBC 引用时获得 `everest.percona.com/in-use-protection` finalizer | 删除 LBC 前需先解除 DBC 引用 |
| F8 | OpenEverest 不直接建 Service，而是建 engine CR（PXC/PSMDB/PG），由 engine operator 建 Service | Service 命名由 engine operator 决定 |
| F9 | engine CR 与 DBC 同名、同 namespace | DBC 名 = engine CR 名 |
| F10 | `kubernetes.io/elb.id` 注解走 LBC → engine CR → Service 的传播链 | 注解传播路径已验证 |

### 2.2 CCE 平台实测结论（CCE 1.35.3 北京四）

| # | 问题 | 结论 | 状态 |
|---|---|---|---|
| Q1 | CCE CCM 对无注解 LoadBalancer Service 的行为 | CCE 内置 CCM **不** autocreate，Service 停在 `<pending>` | ✅ 实测 |
| Q2 | `loadBalancerConfigName` 能否留空 | 可选字段，CEL 不校验必填 | ✅ 实测 |
| Q4 | ELB 名称长度限制 | 66 字符创建成功，无 64 字符硬限制 | ✅ 实测 |
| Q7 | `loadBalancerSourceRanges` 行为 | CCE 1.35.3 CCM **完全忽略**该字段，不报错、不阻塞 | ✅ 实测 |
| F11 | `in-use-protection` 解除时序 | DBC 删除后，LBC controller 在 5s 内移除 finalizer | ✅ 实测 |
| R11 | HAProxy replicas 端口冲突 | primary Service 创建成功，replicas 被 CCE webhook 拒绝 | ✅ 实测 |
| R12 | sourceRanges 在 ELB 层面不生效 | 用户 UI 配置 `10.0.0.0/8`，Service 有该字段但 ELB 无访问控制 | ✅ 实测 |

**未验证**：

| # | 问题 | 说明 |
|---|---|---|
| U1 | CCE 1.33 `no access-control` 错误 | 仅客户报告，从未在任何 CCE 版本上复现 |

---

## 3. 最终方案：DBC Reconciler（当前方案 + 方案 1）

### 3.1 整体架构

```
┌──────────────────────────────────────────────────────────────┐
│  huawei-elb-controller（单 Deployment，两个 Reconciler）       │
│                                                              │
│  ┌──────────────────────────┐  ┌──────────────────────────┐ │
│  │ LBC Reconciler（现有）    │  │ DBC Reconciler（新增）    │ │
│  │                          │  │                          │ │
│  │ watch LBC CR             │  │ watch DBC CR             │ │
│  │ 探测 VPC/子网/AZ          │  │ 检测触发条件              │ │
│  │ 创建/删除 ELB             │  │ 建 primary LBC+replicas LBC│ │
│  │ 写 elb.id 到 LBC          │  │ patch DBC + patch PXC CR │ │
│  │ ACL 自动处理（D.8）       │  │ 删除时清理 LBC            │ │
│  └──────────┬───────────────┘  └───────────┬──────────────┘ │
│             │                              │                 │
│             └─────── 共享 ─────────────────┘                 │
│                 NetworkDetector（VPC/子网/AZ 探测）            │
│                 huaweicloud.ELBClient                        │
│                 huaweicloud.Credentials                      │
└──────────────────────────────────────────────────────────────┘
```

### 3.2 触发条件

DBC Reconciler 在以下条件全部满足时触发自动创建：

- `spec.proxy.expose.type == LoadBalancer`
- `spec.proxy.expose.loadBalancerConfigName == ""`
- DBC 不含其他云提供商注解

> **设计决策**：不要求 `auto-elb: "true"` 注解。OpenEverest UI 近期不支持自定义注解，要求注解意味着 UI 用户无法使用此功能。`loadBalancerConfigName` 为空即触发，与 `auto-elb` 注解 opt-in 等效（用户留空 = 有意让控制器处理）。

### 3.3 端到端流程

用户只操作步骤 ①，其余全自动。

```
步骤 ① 用户建 DBC（loadBalancerConfigName 留空）
  │
  ├─→ OpenEverest 建 engine CR + Service（空注解，裸奔窗口开始）
  │     CCE CCM 检查：无 elb.id，无 autocreate → 什么都不做，Service <pending>
  │
  └─→ DBC Reconciler（watch DBC CREATE 事件）
        │
        步骤 ② 检测触发条件 → 满足
        ├─ NetworkDetector 探测 VPC/子网/AZ
        ├─ 建 primary LBC: elb-<ns>-<dbc-name>
        ├─ 建 replicas LBC: elb-<ns>-<dbc-name>-replicas
        ├─ 给 DBC 加 annotation:
        │    huawei-elb.io/auto-lbc-name: "elb-<ns>-<dbc-name>"
        │    huawei-elb.io/auto-lbc-replicas-name: "elb-<ns>-<dbc-name>-replicas"
        ├─ 加 finalizer: huawei-elb.io/dbc-finalizer
        └─ return（不等 ELB ready）
              │
              步骤 ③ LBC Reconciler（watch LBC CREATE 事件，两个 LBC 各自触发）
              ├─ 探测 VPC/子网/AZ（缓存命中，跳过重复探测）
              ├─ 调华为云 ELB API 创建 ELB
              ├─ 轮询等待 STATUS=ACTIVE
              ├─ 写 elb.id 到 LBC.spec.annotations
              ├─ 写 ready=true、elb-status=ACTIVE、public-ip 到 LBC metadata.annotations
              ├─ 防御性注入 elb.acl-status=off
              └─ return
                    │
                    步骤 ④ DBC Reconciler（watch LBC UPDATE 事件，两个 LBC 各自触发）
                    │ 检测到 auto-lbc-name 注解 → 已建过
                    │ 检查 primary LBC.ready=true 且 replicas LBC.ready=true
                    │
                    ├─ patch DBC:
                    │    spec.proxy.expose.loadBalancerConfigName = "elb-<ns>-<dbc-name>"
                    │
                    └─ patch PXC CR:
                         spec.haproxy.exposePrimary.annotations:
                           kubernetes.io/elb.id: <primary-elb-id>
                         spec.haproxy.exposeReplicas.annotations:
                           kubernetes.io/elb.id: <replicas-elb-id>
                          │
                          步骤 ⑤ OpenEverest DBC Reconciler（watch DBC UPDATE 事件）
                          │ 检测到 loadBalancerConfigName 从空变为有值
                          │ GetAnnotations() → 从 primary LBC 读取 spec.annotations
                          │ → 写 engine CR ServiceExpose.Annotations
                          │
                          步骤 ⑥ PXC operator（watch PXC CR UPDATE 事件）
                          │ 检测到 exposePrimary/Replicas.annotations 变化
                          │ → 更新两个 Service 各自注入不同 elb.id
                          │ → Service 裸奔窗口结束
                          │
                          步骤 ⑦ CCE CCM（watch Service UPDATE 事件）
                          检测到 Service 有 elb.id
                          → 调华为云 API 绑定 ELB
                          → 写 Service.status.loadBalancer.ingress
                          两个 Service 各绑各的 ELB，端口 3306 不冲突 ✅
                          │
                          步骤 ⑧ LBC Reconciler — ACL 自动处理（watch Service 绑定完成）
                          │ Service 已绑定 ELB → LBC Reconciler 检测到关联 Service
                          │ 读 Service.spec.loadBalancerSourceRanges
                          │
                          ├─ 为空（用户未配 Source Range）
                          │   └─ 保持 elb.acl-status=off（已完成）
                          │
                          └─ 有值（如 10.0.0.0/8）
                              ├─ 调华为云 ELB API: CreateIpGroup(CIDR → IP 地址组)
                              ├─ 更新 LBC.spec.annotations:
                              │    kubernetes.io/elb.acl-status: "on"
                              │    kubernetes.io/elb.acl-type: "white"
                              │    kubernetes.io/elb.acl-id: <ipgroup-id>
                              ├─ OpenEverest 传播链 → Service.annotations
                              └─ CCM 绑定到 ELB listener → ACL 生效 ✅
```

### 3.4 时序详解

```
时间线（每个参与者独立 reconcile，靠 K8s watch 级联触发）:

T=0s    用户 kubectl apply DBC
          ├─ API Server 持久化 DBC
          ├─ watch DBC 的 controller 被触发（OpenEverest + 我们的）
          │
T=0.1s   OpenEverest → 建 engine CR → engine operator → 建 Service（空注解）
          裸奔窗口开始：Service type=LoadBalancer, annotations={}
          CCM 检查：无 elb.id → 什么都不做
          │
T=0.1s   我们的 DBC Reconciler:
          ├─ 检测触发条件 ✅
          ├─ NetworkDetector.Detect() → VPC/子网/AZ
          ├─ Create LBC "elb-everest-mysql-db"（primary）
          ├─ Create LBC "elb-everest-mysql-db-replicas"（replicas）
          ├─ Patch DBC annotations（auto-lbc-name + finalizer）
          └─ return
          │
T=0.2s   LBC Reconciler ×2（两个 LBC 各自触发）:
          ├─ 探测（缓存命中）
          ├─ CreateELB() → 轮询 ACTIVE（最长 120s）
          ├─ 写 elb.id 到 LBC.spec.annotations
          ├─ 写状态注解（ready=true, elb-status=ACTIVE, public-ip）
          └─ return
          │
T~15s    两个 LBC 都 ready
          ↓ LBC UPDATE 事件
T~15s    我们的 DBC Reconciler（第二次 reconcile）:
          ├─ auto-lbc-name 注解存在 → 已建过
          ├─ 两个 LBC 都 ready=true ✅
          ├─ Patch DBC: spec.proxy.expose.loadBalancerConfigName = "elb-everest-mysql-db"
          ├─ Patch PXC CR:
          │    exposePrimary.annotations:   {kubernetes.io/elb.id: <primary-elb-id>}
          │    exposeReplicas.annotations:  {kubernetes.io/elb.id: <replicas-elb-id>}
          └─ return
          │
          ↓ DBC UPDATE 事件 + PXC UPDATE 事件
T~15s    OpenEverest DBC Reconciler:
          GetAnnotations() → 读 primary LBC 注解 → 写 engine CR
          │
T~15s    PXC operator（PXC CR 变了）:
          更新 haproxy Service: annotations={kubernetes.io/elb.id: <primary-elb-id>}
          更新 haproxy-replicas Service: annotations={kubernetes.io/elb.id: <replicas-elb-id>}
          裸奔窗口结束
          │
          ↓ Service UPDATE 事件 ×2
T~15s    CCM（两个 Service 各自触发）:
          ├─ Service-A（primary）: 读 elb.id-A → 调 API 绑定 ELB-A → 写 status.ingress
          ├─ Service-B（replicas）: 读 elb.id-B → 调 API 绑定 ELB-B → 写 status.ingress
          └─ 两个 Service 各绑各的 ELB，端口 3306 不冲突 ✅
          │
T~18s   LBC Reconciler — ACL 自动处理（Service UPDATE 触发 LBC re-reconcile）:
          ├─ 读 Service.spec.loadBalancerSourceRanges
          ├─ 为空 → 保持 elb.acl-status=off（步骤 ③ 已注入）✅
          │
          └─ 有值（如用户配了 10.0.0.0/8）:
              ├─ 调 ELB API: CreateIpGroup(CIDR → IP 地址组)
              ├─ 更新 LBC.spec.annotations: acl-status=on, acl-type=white, acl-id=<id>
              ├─ OpenEverest 传播链 → Service.annotations
              └─ CCM 绑定到 ELB listener → ELB 仅放行 10.0.0.0/8 ✅
```

### 3.5 DBC Reconciler 详细流程

```
Reconcile(ctx, req)
  │
  ├─ Get DBC by name
  │   └─ NotFound → return
  │
  ├─ 删除处理（DeletionTimestamp != 0）
  │   ├─ 有 dbc-finalizer → reconcileDelete
  │   │   ├─ 轮询 primary LBC 的 finalizers（5s 间隔）
  │   │   │   等 in-use-protection 移除
  │   │   │   超时 10min → 写 huawei-elb.io/error 注解，继续重试
  │   │   ├─ 删 primary LBC → huawei-elb.io/finalizer 删 ELB
  │   │   ├─ 删 replicas LBC → huawei-elb.io/finalizer 删 ELB
  │   │   └─ 移除 dbc-finalizer → return
  │   └─ 无 dbc-finalizer → return
  │
  ├─ 触发条件检测
  │   ├─ expose.type != LoadBalancer → return
  │   ├─ loadBalancerConfigName != "" → return（走现有手动 LBC 流程）
  │   └─ hasForeignCloudAnnotations → return
  │
  ├─ 已自动建过？（auto-lbc-name 注解存在）
  │   ├─ 是 → 检查两个 LBC 状态
  │   │   ├─ 两者 ready=true
  │   │   │   ├─ DBC.loadBalancerConfigName 已设 → 完成（长轮询，requeue 5min）
  │   │   │   └─ DBC.loadBalancerConfigName 未设 → patch DBC + patch PXC CR
  │   │   └─ 任一 ready=false → requeue 5s（等 LBC Reconciler 建完 ELB）
  │   │
  │   └─ 否 → 首次处理
  │       ├─ NetworkDetector.Detect() → VPC/子网/AZ
  │       ├─ Create LBC "elb-<ns>-<dbc-name>"（primary）
  │       ├─ Create LBC "elb-<ns>-<dbc-name>-replicas"（replicas）
  │       ├─ Patch DBC:
  │       │    annotations:
  │       │      huawei-elb.io/auto-lbc-name: "elb-<ns>-<dbc-name>"
  │       │      huawei-elb.io/auto-lbc-replicas-name: "elb-<ns>-<dbc-name>-replicas"
  │       │    finalizers: [..., huawei-elb.io/dbc-finalizer]
  │       └─ return（requeue 5s，等 LBC Reconciler 建 ELB）
  │
  └─ Patch PXC CR（仅当两个 LBC 都 ready）
       kubectl patch pxc <dbc-name> --type='json' -p='[
         {"op":"add","path":"/spec/haproxy/exposePrimary/annotations",
          "value":{"kubernetes.io/elb.id":"<primary-elb-id>"}},
         {"op":"add","path":"/spec/haproxy/exposeReplicas/annotations",
          "value":{"kubernetes.io/elb.id":"<replicas-elb-id>"}}
       ]'
```

### 3.6 LBC Reconciler（现有，新增 ACL 自动处理）

```
Reconcile(ctx, req)  ← 现有逻辑不变，新增 D.8 ACL 处理
  │
  ├─ Get LBC
  │
  ├─ 删除处理（DeletionTimestamp != 0）
  │   └─ huawei-elb.io/finalizer → 删 ELB → 移除 finalizer
  │
  ├─ elb.id 已存在？
  │   ├─ 是 → 监控 ELB 状态，更新状态注解
  │   └─ 否 → 创建流程
  │       ├─ NetworkDetector.Detect() → VPC/子网/AZ
  │       ├─ 调华为云 API 创建 ELB
  │       ├─ 轮询 STATUS=ACTIVE
  │       ├─ 写 elb.id 到 LBC.spec.annotations
  │       ├─ 写状态注解（ready, elb-status, public-ip）
  │       └─ 防御性注入 elb.acl-status=off
  │
  └─ D.8 ACL 自动处理（新增）
       ├─ 通过 LBC 反查关联的 Service
       ├─ 读 Service.spec.loadBalancerSourceRanges
       ├─ 若有值 → 调 ELB API 创建 IP 地址组
       └─ 写 elb.acl-id/status/type 到 LBC.spec.annotations
          → 传播到 Service → CCM 绑定到 ELB listener
```

### 3.7 Replicas 独立 ELB 处理

PXC operator 为每个数据库集群创建两个 LoadBalancer Service（primary + replicas，同端口 3306）。EKS/GKE 上每个 Service 自动获得独立云 LB，端口不冲突。CCE 通过 `kubernetes.io/elb.id` 共享 ELB 时同端口冲突。

**方案**：DBC Reconciler 创建两个独立 LBC，primary 和 replicas 各绑定不同 ELB，如下：

```
                            ┌─────────────────────────────────┐
DBC 触发自动建               │                                 │
  │                         │  ┌──────────┐  ┌──────────┐   │
  ├─ Create primary LBC ────│→│ LBC-pri  │  │ LBC-rep  │   │
  │   命名: elb-<ns>-<name>  │  │ (primary)│  │(replicas)│   │
  │                         │  └────┬─────┘  └────┬─────┘   │
  └─ Create replicas LBC ───│→      ↓              ↓         │
      命名: elb-<ns>-<name>  │   ELB-1          ELB-2       │
              -replicas      │     │              │          │
                             │     ↓              ↓          │
两个 LBC 都 ready 后          │  PXC CR                      │
  │                         │  spec.haproxy:               │
  └─ Patch PXC CR:          │    exposePrimary:             │
       exposePrimary:        │      annotations:             │
         elb.id: <elb-id-1>  │        elb.id: <elb-id-1>    │
       exposeReplicas:       │    exposeReplicas:            │
         elb.id: <elb-id-2>  │      annotations:             │
                             │        elb.id: <elb-id-2>    │
                             └─────────────────────────────────┘
                                        │
                    ┌───────────────────┴───────────────────┐
                    ↓                                       ↓
          Service-A (primary)                   Service-B (replicas)
          annotations:                          annotations:
            elb.id: <elb-id-1>                    elb.id: <elb-id-2>
                    │                                       │
                    ↓                                       ↓
              CCM 绑定 ELB-1                         CCM 绑定 ELB-2
              端口 3306 ✅                           端口 3306 ✅
```

**对齐 EKS/GKE**：

| | EKS | GKE | CCE（实现本方案后） |
|---|---|---|---|
| Primary | NLB-1 | LB-1 | ELB-1（独立） |
| Replicas | NLB-2 | LB-2 | ELB-2（独立） |
| 端口冲突 | 不发生 | 不发生 | 不发生 |
| 计费 | 两个 LB | 两个 LB | 两个 ELB |

### 3.8 DBC 删除链路

```
用户删 DBC
  ↓
DBC Reconciler 检测到 DeletionTimestamp
  ↓
检查 DBC 有 dbc-finalizer
  ↓
轮询 primary LBC 的 finalizers（5s 间隔）
  等 in-use-protection 移除
  ↓
in-use-protection 已移除
  ↓
删 primary LBC → huawei-elb.io/finalizer 删 ELB-1
删 replicas LBC → huawei-elb.io/finalizer 删 ELB-2
  ↓
移除 dbc-finalizer
  ↓
DBC 被删
```

### 3.9 新增 RBAC

```yaml
# ClusterRole 新增
- apiGroups: ["everest.percona.com"]
  resources: ["databaseclusters"]
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: ["everest.percona.com"]
  resources: ["loadbalancerconfigs"]
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: ["pxc.percona.com"]
  resources: ["perconaxtradbclusters"]
  verbs: ["get", "watch", "patch"]
```

### 3.10 命名规则与常量

```
LBC 命名：
  primary:  elb-<namespace>-<dbc-name>
  replicas: elb-<namespace>-<dbc-name>-replicas

DBC 注解：
  huawei-elb.io/auto-lbc-name: "elb-<ns>-<dbc-name>"
  huawei-elb.io/auto-lbc-replicas-name: "elb-<ns>-<dbc-name>-replicas"

DBC Finalizer：
  huawei-elb.io/dbc-finalizer

LBC 默认注解（防御性）：
  kubernetes.io/elb.acl-status: "off"
```

---

## 4. CCE 平台问题专项分析

### 4.1 端口冲突：HAProxy Replicas Service（R11）

**现象**：PXC operator 创建两个 LoadBalancer Service（primary + replicas，同端口 3306），通过 `kubernetes.io/elb.id` 共享同一 ELB 时，CCE `validate.crd.service` webhook 拒绝第二个 Service：

```
protocol_port 3306 of the load Balancer xxx already has been usesd by other service
```

**实测**（2026-07-10，CCE 1.35.3）：primary Service 创建成功，replicas Service 被拒。

**根因**：CCE 支持多 Service 共享同一 ELB（通过 `elb.id` 注解），但共享模式下同端口冲突。EKS/GKE 不存在共享机制（每个 Service 独立 LB），因此不受影响。

**解决方案**：DBC Reconciler 为 replicas 独立创建 LBC + ELB，通过 patch PXC CR 分别指定不同 ELB ID。详见 §3.7。

### 4.2 ACL 访问控制：sourceRanges 不生效（R12）

**现象**（CCE 1.35.3 实测）：

```
UI 配置 Source Range: 10.0.0.0/8
  → DBC.spec.proxy.expose.ipSourceRanges: ["10.0.0.0/8"]
    → PXC CR spec.haproxy.exposePrimary.loadBalancerSourceRanges: ["10.0.0.0/8"]
      → Service.spec.loadBalancerSourceRanges: ["10.0.0.0/8"]
        → CCM: EnsuredLoadBalancer ✅（静默忽略）
        → ELB 层面：无访问控制（数据库公网全裸）
```

| 平台 | `loadBalancerSourceRanges` 是否生效 | 机制 |
|---|---|---|
| AWS EKS | ✅ | CCM 转成 NLB listener 规则 / 安全组 |
| Google GKE | ✅ | CCM 转成 VPC 防火墙规则 |
| 华为云 CCE | ❌ | CCM 不认此字段。需 `elb.acl-*` 注解 + 华为云 IP 地址组 |

**解决方案**：D.8 ACL 自动处理——LBC Reconciler 检测 Service 的 sourceRanges → 调华为云 ELB API 创建 IP 地址组 → 注入 `elb.acl-id/status/type` 到 LBC.spec.annotations，经传播链到 Service，CCM 绑定到 ELB listener。

**防御性措施**：创建 LBC 时默认注入 `elb.acl-status=off`，明确声明「无访问控制」。防止 CCE 1.33 的 `no access-control (source ranges enabled)` 错误（客户报告，未复现）。

### 4.3 `no access-control (source ranges enabled)` 错误（U1）

**状态**：⚠️ 未复现。仅 CCE 1.33 客户报告，从未在任何 CCE 版本上实际触发。

**已知事实**：

| CCE 版本 | `loadBalancerSourceRanges` 有值 | CCM 行为 |
|---|---|---|
| 1.33 | 客户报告报错 | 未知（未实测） |
| 1.35.3 | 实测 10+ 组合 | 静默忽略，EnsuredLoadBalancer |

**处理**：不假设 1.33 的行为，不做基于猜测的实现。防御性注入 `elb.acl-status=off` 作为低成本兜底。

---

## 5. 确认清单

| # | 项目 | 状态 | 备注 |
|---|---|---|---|
| Q1 | CCE CCM 不 autocreate | ✅ 已确认 | Service 无注解时停在 `<pending>` |
| Q2 | `loadBalancerConfigName` 可留空 | ✅ 已确认 | CEL 不校验必填 |
| Q4 | ELB 名长度无硬限制 | ✅ 已确认 | 66 字符测试通过 |
| Q7 | `loadBalancerSourceRanges` 静默忽略 | ✅ 已确认 | CCE 1.35.3 实测 |
| F11 | `in-use-protection` 解除时序 | ✅ 已确认 | 最后 DBC 删后 5s 内移除 |
| R11 | HAProxy replicas 端口冲突 | ✅ 已确认 | 已定方案（§3.7） |
| R12 | sourceRanges 在 ELB 不生效 | ✅ 已确认 | 已定方案（D.8） |
| U1 | CCE 1.33 `no access-control` 错误 | ⚠️ 未复现 | 防御性注入 `elb.acl-status=off` |
| D1 | **触发条件：是否需要 `auto-elb` 注解？** | ✅ 已确认 | 不要求注解，`loadBalancerConfigName` 为空即触发。理由：UI 不支持自定义注解，要求注解 = UI 用户无法使用 |
| D2 | **多 DB 共享 ELB 支持？** | ✅ 已确认 | 自动创建的 DBC 不共享 ELB（独立 LBC，对齐 EKS/GKE）。手动流程可共享：用户创建 DBC 时手动指定同一 `loadBalancerConfigName`。**限制**：华为云 ELB 不允许同一 listener 端口复用；PXC operator 的 primary 和 replicas Service 端口硬编码均为 3306，共享同一 ELB 必然触发端口冲突（同类型 DB 集群不能共享）。不同类型 DB 集群端口不一致（PXC=3306、PSMDB=27017、PG=5432），可共享同一 ELB。但共享模式下 replicas 端口冲突问题（§4.1）无法规避，详见 D4 |
| D3 | **UI 中 LBC 数量增加？** | ✅ 已确认 | 每个 auto DBC 产生 2 个 LBC（primary + replicas），UI 中可见可编辑。LBC 是配置面板，每个 DB 一个配置面板合理 |
| D4 | **手动流程下 replicas 端口冲突如何解决？** | 🔴 **需用户确认** | **问题**：D2 手动共享 LBC 路径与 replicas 端口冲突矛盾——单 LBC 模式下 replicas Service 必然创建失败。**唯一解法**：用户手动创建两个 LBC，分别给 primary 和 replicas；建完 DBC 后手动 patch PXC CR 为 `exposeReplicas.annotations` 指定第二个 LBC 的 ELB ID。**建议**：不提供自动关闭开关。不需要 replicas 外部接入的用户走手动单 LBC（接受冲突）；需要 replicas 外部接入的用户走自动流程（对齐 EKS/GKE） |

---

## 6. 方案优势

| 维度 | 当前手动方案 | 最终方案 |
|---|---|---|
| 用户步骤 | 2 步（建 LBC → 等 ready → 建 DBC） | 1 步（建 DBC） |
| 用户等待 | 必须等 LBC ready | 只需等 DBC ready |
| 前提知识 | 需理解 LBC 概念 | 零额外知识 |
| ELB 参数 | 需手动填（或自动探测） | 自动探测 |
| Primary 外部接入 | ✅ | ✅ |
| **Replicas 外部接入** | ❌（端口冲突） | ✅（独立 ELB，对齐 EKS/GKE） |
| **ACL 访问控制** | ❌（sourceRanges 不生效） | ✅（LBC Reconciler 自动转 elb.acl-*） |
| ELB 生命周期 | 控制器 finalizer | 同左 |
| 状态可见性 | LBC 注解（ready/error/public-ip） | 同左 |
| ELB 参数事后可变 | ✅（改 LBC 注解 → 调 API） | 同左 |
| 对齐 EKS/GKE | 不齐 | **对齐**（功能和费用） |

---

## 7. EKS / GKE / CCE 四平台对比

| 维度 | AWS EKS | Google GKE | CCE（当前手动方案） | CCE（最终 auto 方案） |
|---|---|---|---|---|
| **LB 名称** | NLB（Network Load Balancer） | Cloud Load Balancer（外部直通网络负载均衡器） | ELB（Elastic Load Balancer） | 同左 |
| **用户操作步骤** | 1 步：建 DBC | 1 步：建 DBC | 2 步：建 LBC → 等 ready → 建 DBC | 1 步：建 DBC |
| **LB 创建者** | AWS CCM 自动建 | GKE CCM 自动建 | huawei-elb-controller（提前建） | huawei-elb-controller（用户建 DBC 时触发） |
| **Service → LB 绑定方式** | CCM 调 AWS API 建 NLB，自动绑定 | CCM 调 GCP API 建 LB，自动绑定 | huawei-elb-controller 调 API 创建 ELB；Service 通过 `kubernetes.io/elb.id` 绑定已有 ELB，CCM 完成 listener/backend 绑定。**CCE 内置 CCM 只支持绑定已有 ELB，不支持自动创建（能力限制，非配置选择）** | 同左 |
| **每 DBC 的 LB 数量** | 2 个 NLB（每个单独计费） | 2 个 LB（每个单独计费） | 1 个 ELB（primary 和 replicas 共享，但 replicas 端口冲突不创建） | **2 个 ELB**（每个单独计费，对齐 EKS/GKE） |
| **replicas 外部接入** | ✅ NLB-2 独立外部 IP | ✅ LB-2 独立外部 IP | ❌ 端口冲突，不创建 | ✅ ELB-2 独立外部 IP |
| **Source Range / ACL** | ✅ CCM 转 NLB listener 规则 | ✅ CCM 转 VPC 防火墙规则 | ❌ CCM 忽略，需手动配 `elb.acl-*` | ✅ LBC Reconciler 自动转 `elb.acl-*`（D.8） |
| **LB 在 OpenEverest UI 中可见** | ❌ 不可见（NLB 非 K8s 资源，仅 Service external IP） | ❌ 不可见（同左） | ✅ 可见（LBC CR 出现在 Settings → Load Balancer Configuration） | ✅ 可见（同左，自动创建的 LBC 自动出现） |
| **LB 创建后参数可调** | ✅ 改 Service annotation → CCM 更新 | ✅ 改 Service annotation → CCM 更新 | ✅ 改 LBC annotation → controller 调华为云 API | ✅ 同左 |
| **LB 生命周期管理** | CCM 管理（删 Service → CCM 删 NLB） | CCM 管理 | 控制器 + finalizer（删 LBC → 调 API 删 ELB） | 同左 |
| **LB 共享** | 不支持（每 Service 独立 NLB） | 不支持 | 支持（多 Service 绑同一 ELB，端口不冲突则可用） | 自动：不支持（独立 ELB）；手动：同左 |
| **异常恢复** | CCM 处理，无孤儿风险 | 同左 | Compensation Goroutine 每 5min 扫描孤儿 LBC | 同左 |
| **前提知识** | 零 | 零 | 需理解 LBC 概念 | 零 |

---

## 8. 备选方案

### 8.1 方案 2：Service Reconciler（备用）

**思路**：新增 Service Reconciler，watch LoadBalancer Service，自动探测 VPC/子网/AZ，构造 `elb.autocreate` JSON 注入到 Service，让 CCE CCM 原生建 ELB。

**流程**：
```
① 用户建 DBC → OpenEverest 建 Service（空注解）
② Service Reconciler 检测到 → 探测 VPC/子网/AZ
③ patch Service 注入 autocreate + elb.class + elb.tags
④ CCM 检测到 autocreate → 建 ELB → 绑定 → 写 status.ingress
```

### 8.2 方案 2 与最终方案对比

| 维度 | 最终方案（DBC Reconciler） | 方案 2（Service Reconciler） |
|---|---|---|
| **架构接近 EKS/GKE** | 中（控制器建 ELB，CCM 只绑定） | **高**（CCM 原生建 ELB） |
| **代码量** | +400~500 行 | +200 行 |
| **Replicas 独立 ELB** | **支持**（独立 LBC + ELB） | 不支持（同 ELB 端口冲突） |
| **ACL 访问控制** | **支持**（LBC Reconciler，随 LBC 生命周期） | 支持（但独立于 LBC） |
| **ELB 创建后可调参** | **支持**（改 LBC 注解） | ❌ 不可变（autocreate 创建后不可改；华为云带宽计费场景是真实痛点） |
| **状态可见性** | **高**（LBC 注解: ready/error/public-ip） | 低（Service events + status.ingress） |
| **ELB 配置精细度** | **高**（全 ELB v3 API 参数） | 受限（autocreate 14 个字段） |
| **Region 覆盖** | **支持** | 不支持 |
| **用户操作** | 1 步（建 DBC） | 1 步（建 DBC） |
| **总评** | ⭐ 功能完整对齐 EKS/GKE | 架构优雅但不满足功能需求 |

**方案 2 不被选为最终方案的原因**：
1. 无法解决 replicas 端口冲突（autocreate 不支持指定独立 ELB）
2. ELB 创建后参数不可变（华为云带宽计费场景下是硬伤）
3. 状态可见性弱（无 LBC 结构化的 ready/error 注解）
