# 备选方案 2 详细设计：Service Reconciler（autocreate 模式）

> **状态**: 备选，不被选为最终方案
> **基线**: `docs/design-final.md`
> **日期**: 2026-07-10

---

## 1. 核心思路

不再创建 LBC，直接 watch OpenEverest 创建出的 LoadBalancer Service，自动探测 VPC/子网/AZ，构造 `elb.autocreate` JSON 注入到 Service annotation，交给 CCE CCM 原生创建 ELB 并绑定。

**本质**：模仿 EKS/GKE 的行为——控制器只负责注入配置，CCM 负责建 LB。区别是 AWS/GCP CCM 能自动探测 VPC，CCE CCM 不能——需要本控制器补上探测这一步。

---

## 2. 架构

```
┌─────────────────────────────────────────────────┐
│  huawei-elb-controller（单 Deployment）           │
│                                                 │
│  ┌─────────────────────────────────────────┐    │
│  │ Service Reconciler（新增）                │    │
│  │                                         │    │
│  │ watch Service (type=LoadBalancer)       │    │
│  │ 识别 OpenEverest 创建的 Service           │    │
│  │ 探测 VPC/子网/AZ                        │    │
│  │ 构造 autocreate JSON                   │    │
│  │ patch Service 注入注解                  │    │
│  └──────────────┬──────────────────────────┘    │
│                 │                                │
│          NetworkDetector（VPC/子网/AZ 探测）       │
│          huaweicloud.Credentials                 │
└─────────────────────────────────────────────────┘
                        │
                        ▼
                  CCE CCM（原生）
           读 Service 的 elb.autocreate 注解
           调华为云 API 创建 ELB
           绑定 listener + backend
           写 Service.status.loadBalancer.ingress
```

---

## 3. 端到端流程

用户只操作步骤 ①，其余全自动。

```
步骤 ① 用户建 DBC（loadBalancerConfigName 留空）
  │
  ├─→ OpenEverest 建 engine CR + Service（空注解，裸奔窗口开始）
  │     CCE CCM 检查：无 elb.id，无 autocreate → 什么都不做，Service <pending>
  │
  └─→ Service Reconciler（watch Service CREATE 事件）
        │
        步骤 ② 识别触发条件
        ├─ Service.type == LoadBalancer ✅
        ├─ 无 kubernetes.io/elb.id ✅
        ├─ 无 kubernetes.io/elb.autocreate ✅
        ├─ 由 OpenEverest engine operator 创建（by label/ownerRef）✅
        └─ 不含其他云提供商注解 ✅
              │
              步骤 ③ 探测 + 构造
              ├─ NetworkDetector.Detect() → VPC/子网/AZ
              ├─ 构造 elb.autocreate JSON:
              │    {
              │      "name": "<dbc-name>-haproxy",
              │      "type": "public",
              │      "bandwidth_chargemode": "traffic",
              │      "bandwidth_size": 10,
              │      "bandwidth_sharetype": "PER",
              │      "eip_type": "5_bgp",
              │      "vip_subnet_cidr_id": "<detected>",
              │      "available_zone": ["<detected>"]
              │    }
              ├─ patch Service:
              │    metadata.annotations:
              │      kubernetes.io/elb.autocreate: <JSON>
              │      kubernetes.io/elb.class: "union"
              │      kubernetes.io/elb.tags: "managed-by=huawei-elb-controller"
              │      huawei-elb.io/auto: "true"
              └─ return（裸奔窗口结束）
                    │
                    步骤 ④ CCE CCM（watch Service UPDATE 事件）
                    检测到 Service 有 elb.autocreate 注解
                    ├─ 调华为云 API 创建 ELB（按 autocreate JSON 参数）
                    ├─ 创建 listener（基于 Service Ports）
                    ├─ 创建 backend members（基于 Endpoints）
                    ├─ 写 Service.status.loadBalancer.ingress: [{ip: <VIP>}]
                    └─ EXTERNAL-IP 从 <pending> 变为 VIP

                    第二个 Service（replicas）同样流程
                    CCM 创建第二个独立 ELB ✅
                    两个 ELB 各自端口 3306，互不冲突 ✅
```

---

## 4. 时序详解

```
T=0s    用户 kubectl apply DBC
          ├─ API Server 持久化 DBC
          │
T=0.1s   OpenEverest → 建 engine CR → engine operator → 建 Service
          ├─ Service-A（haproxy）
          ├─ Service-B（haproxy-replicas）
          裸奔窗口开始：两个 Service type=LoadBalancer, annotations={}
          CCM 检查：无注解 → 什么都不做
          │
T=0.1s   Service Reconciler（Service-A 触发）:
          ├─ 检测触发条件 ✅
          ├─ NetworkDetector.Detect() → VPC/子网/AZ
          ├─ 构造 autocreate JSON
          ├─ patch Service-A annotations（autocreate + class + tags）
          └─ return
          │
T=0.1s   Service Reconciler（Service-B 触发，并行）:
          ├─ 检测触发条件 ✅
          ├─ 探测（缓存命中）
          ├─ 构造 autocreate JSON（name 不同）
          ├─ patch Service-B annotations
          └─ return
          裸奔窗口结束（< 1s）
          │
          ↓ Service UPDATE 事件 ×2
T=0.2s   CCM（Service-A 触发）:
          读 elb.autocreate → 调 API 建 ELB-1 → 轮询 ACTIVE（≤120s）
          → 绑 listener + backend → 写 status.ingress
          │
T=0.2s   CCM（Service-B 触发，并行）:
          读 elb.autocreate → 调 API 建 ELB-2 → 轮询 ACTIVE（≤120s）
          → 绑 listener + backend → 写 status.ingress
          │
T~15s    两个 ELB 都 ACTIVE
          两个 Service 各绑各的 ELB，端口 3306 不冲突 ✅
          两个独立外部 IP
```

---

## 5. 触发条件

Service Reconciler 在以下条件全部满足时处理 Service：

| 条件 | 说明 |
|---|---|
| `Service.type == LoadBalancer` | 只有 LB 类型需要处理 |
| 无 `kubernetes.io/elb.id` | 未手动绑定 ELB |
| 无 `kubernetes.io/elb.autocreate` | 未注入过（幂等） |
| Service 由 OpenEverest engine operator 创建 | 过滤其他来源的 Service |
| 无其他云提供商注解 | 不干扰 EKS/GKE/其他云 |

**识别 OpenEverest Service 的方式**：检查 Service 的 `ownerReferences` 链或 label（`app.kubernetes.io/managed-by: percona-xtradb-cluster-operator` 等）。

---

## 6. `elb.autocreate` JSON 参数

| 参数 | 类型 | 必填 | 说明 | 默认值 |
|---|---|---|---|---|
| `name` | string | 否 | ELB 名称 | `cce-lb-<service.UID>` |
| `type` | string | 否 | `public` / `inner` | `inner` |
| `bandwidth_chargemode` | string | 公网必填 | `bandwidth` / `traffic` | - |
| `bandwidth_size` | int | 公网必填 | 1-2000 Mbit/s | - |
| `bandwidth_sharetype` | string | 公网必填 | `PER` | - |
| `eip_type` | string | 公网必填 | `5_bgp` / `5_sbgp` 等 | - |
| `vip_subnet_cidr_id` | string | 否 | IPv4 子网 ID | 集群同子网 |
| `available_zone` | []string | 独享必填 | 可用区 | - |
| `l4_flavor_name` | string | 否 | L4 flavor（独享） | - |

**不在 autocreate JSON 内的独立注解**：

| 注解 | 说明 |
|---|---|
| `kubernetes.io/elb.class` | `union`（共享）/ `performance`（独享），必须同时设置 |
| `kubernetes.io/elb.tags` | ELB 标签 |
| `kubernetes.io/elb.enterpriseID` | 企业项目 ID |
| `kubernetes.io/elb.instance-reclaim-policy` | `retain` / `alwaysDelete` |

---

## 7. ELB 删除

CCM 原生处理。Service 被删时，CCM 根据 `kubernetes.io/elb.instance-reclaim-policy` 决定保留或删除 ELB。**控制器无需 finalizer**。

| reclaim-policy | 行为 |
|---|---|
| `alwaysDelete`（v1.28.15-r60+） | 删 Service 时同步删 ELB |
| `retain` | 删 Service 时保留 ELB（需手动清理） |

控制器在注入 autocreate 时添加 `alwaysDelete` 注解。

---

## 8. 配置参数来源

| 参数 | 来源 |
|---|---|
| `vip_subnet_cidr_id` | NetworkDetector 从节点 IP 自动探测 |
| `available_zone` | 从节点 label `topology.kubernetes.io/zone` 读取 |
| `type`（公网/内网） | **无法自动探测**。默认 `public`，用户可通过 Service annotation `huawei-elb.io/public: "false"` 覆盖 |
| `bandwidth_*` | 默认 `traffic` / `10` Mbit/s / `5_bgp` |
| `name` | `<dbc-name>-<service-type>`（从 Service label 推导） |

---

## 9. 优势

| 维度 | 说明 |
|---|---|
| **架构接近 EKS/GKE** | CCM 原生建 ELB，控制器只做探测+注入。与 AWS/GCP CCM 的职责边界一致 |
| **代码量最少** | +200 行（单 Reconciler，无 ELB CRUD 逻辑，无 LBC 资源） |
| **无端口冲突** | 每 Service 独立 ELB，天然无端口冲突。replicas 和 primary 各绑各的，不需要任何额外处理 |
| **裸奔窗口极短** | <1s（探测 + 构造 + patch），远短于方案 1（建 LBC → 建 ELB → 等 ready → patch DBC → 更新 Service） |
| **无 LBC 概念** | 用户完全不需要理解 LBC。建 DBC → 等 DBC ready → 获得 IP。与 EKS/GKE 用户体验完全一致 |
| **ELB 生命周期由 CCM 管理** | 删 Service → CCM 删 ELB，无 finalizer 复杂性，无孤儿风险 |
| **无多 Reconciler 协调** | 不需要 DBC Reconciler + LBC Reconciler 的级联协调，单 Reconciler 自闭环 |
| **不 patch 用户 DBC** | 只 patch Service（系统资源），不修改用户创建的业务资源 |

---

## 10. 局限性（不被选为最终方案的原因）

| 局限性 | 说明 | 严重程度 |
|---|---|---|
| **autocreate 创建后参数不可变** | ELB 带宽、EIP 类型、公网/内网等参数创建后无法修改。华为云 ELB 带宽是核心计费变量（1-2000 Mbit/s），数据库流量变化后需要调参，现有 LBC 机制（改注解 → 调 API 更新）比 autocreate 灵活得多 | 🔴 硬伤 |
| **ELB 状态不可见** | 无 LBC 的结构化状态注解（ready/error/elb-status/public-ip），只有 Service events + `status.loadBalancer.ingress`。运维排查、监控依赖分散的信息源 | 🟡 中等 |
| **无 LBC UI 配置面板** | 用户在 OpenEverest UI 中看不到 LBC，无法通过 UI 编辑 ELB 参数。在华为云 ELB 高可配场景（14 个参数可调）下，失去唯一 UI 调参入口 | 🔴 硬伤 |
| **默认值不可覆盖** | autocreate JSON 的 14 个字段是该 Service 的硬编码配置。用户想换带宽大小、换 EIP 类型 → 无法在创建后修改 → 只能删 Service 重建 | 🔴 硬伤 |
| **ACL 需独立处理** | `loadBalancerSourceRanges` → `elb.acl-*` 的转换逻辑不在 LBC Reconciler 中，需单独实现 ACL handler。生命周期独立管理，不随 LBC 统一 | 🟡 中等 |
| **CCM 行为不可控** | autocreate 建 ELB 的具体参数、错误处理、重试逻辑完全由 CCE 闭源 CCM 控制。出现问题排查路径长（Service Reconciler → CCM → 华为云 API） | 🟡 中等 |

---

## 11. 与最终方案（DBC Reconciler）对比

| 维度 | 最终方案（DBC Reconciler + LBC） | 方案 2（Service Reconciler + autocreate） |
|---|---|---|
| **架构接近 EKS/GKE** | 中（控制器建 ELB，CCM 只绑定） | **高**（CCM 原生建 ELB + 绑定） |
| **代码量** | +400~500 行 | **+200 行** |
| **裸奔窗口** | ~15s（建 LBC → 建 ELB → patch DBC → 更新 Service） | **<1s**（探测 → inject → patch Service） |
| **端口冲突** | 需要额外处理（独立 replicas LBC） | **无冲突**（每 Service 独立 ELB，原生对齐 EKS/GKE） |
| **replicas 外部接入** | ✅（独立 ELB） | ✅（独立 ELB） |
| **ACL 访问控制** | **✅（LBC Reconciler，随 LBC 生命周期统一管理）** | ⚠️ 需独立 ACL handler |
| **ELB 创建后参数可调** | **✅（改 LBC 注解 → controller 调 API）** | ❌ 不可变（autocreate 一次性创建） |
| **ELB 状态可见性** | **✅（LBC 注解: ready/error/elb-status/public-ip）** | ⚠️ 仅 Service events + status.ingress |
| **UI 配置面板** | **✅（LBC 出现在 Settings → Load Balancer Configuration，用户可编辑参数）** | ❌ 无 LBC，无 UI 配置入口 |
| **ELB 生命周期** | 控制器 finalizer | **CCM 原生（reclaim-policy）** |
| **用户操作** | 1 步（建 DBC） | 1 步（建 DBC） |
| **ELB 配置精细度** | **✅ 完整（全 ELB v3 API 参数）** | 受限（autocreate 14 个字段） |
| **Region 覆盖** | **✅ 支持** | ❌ 不支持 |
| **平台适用** | CCE | CCE |

---

## 12. 结论

方案 2 架构最优雅——代码量最少、裸奔窗口最短、端口冲突天然不存在。**如果华为云 ELB 没有带宽计费这个高频变参场景，方案 2 是明显更优的选择。**

但在华为云 ELB 环境下，带宽大小、计费模式、EIP 类型是用户日常运营中频繁调整的参数，autocreate 的不可变性让方案 2 成为"一次创建、终生不变"的死配置——数据库流量波动时用户束手无策，唯一的调参方式（删 Service 重建 ELB）在生产环境不可接受。

**方案 2 保留为备用**，适用于：
- 环境配置定型后不再变化的长稳场景
- 不接受 LBC 概念、追求最小代码量的场景
- CCE autocreate 未来具备参数可变性后的首选方案
