# 备选方案 2 详细设计：Service Reconciler（LBC 参数模板 + autocreate）

> **状态**: 备选，不被选为最终方案
> **基线**: `docs/design-final.md`
> **日期**: 2026-07-10

---

## 1. 核心思路

LBC 作为参数模板（对齐 EKS/GKE），存放 CCE ELB 配置参数而非 ELB ID。Service Reconciler 读取 LBC 参数，构造 `elb.autocreate` JSON 注入到 Service，由 CCE CCM 为每个 Service 创建独立 ELB。

```
EKS/GKE:
  LBC = {aws-load-balancer-type: nlb, scheme: internet-facing}  ← 参数模板
  Service 引用 LBC → CCM 建独立 NLB ✅
  多个 Service 引用同一 LBC → 各自独立 NLB，零冲突 ✅

CCE 方案 2（改造后）:
  LBC = {bandwidth_size: 20, eip_type: 5_bgp, public: true}   ← 参数模板
  Service 引用 LBC → Service Reconciler → autocreate → CCM 建独立 ELB ✅
  多个 Service 引用同一 LBC → 各自独立 ELB，零冲突 ✅
```

**与 EKS/GKE 的区别**：CCE CCM 不认 LBC 参数，需要 Service Reconciler 多做一步参数转换（LBC 参数 → autocreate JSON）。其余行为完全对齐。

---

## 2. 架构

```
┌──────────────────────────────────────────────────────────┐
│  huawei-elb-controller（单 Deployment）                    │
│                                                          │
│  ┌──────────────────────────────────────────────────┐    │
│  │ Service Reconciler（新增）                         │    │
│  │                                                  │    │
│  │ watch Service (type=LoadBalancer)                │    │
│  │ 识别由 OpenEverest 创建、引用了 LBC 的 Service      │    │
│  │ 读 LBC 的参数（ELB 配置）→ 构造 autocreate JSON    │    │
│  │ 探测 VPC/子网/AZ（补 CCM 能力缺失）                │    │
│  │ patch Service 注入 autocreate + elb.class        │    │
│  │ 删除 Service 时 → CCM 原生删 ELB（reclaim-policy） │    │
│  └──────────────────────────────────────────────────┘    │
│                │                                         │
│         NetworkDetector（VPC/子网/AZ 探测）                │
│         huaweicloud.Credentials                          │
└──────────────────────────────────────────────────────────┘
                       │
                       ▼
                 CCE CCM（原生）
          读 Service 的 elb.autocreate 注解
          调华为云 API 创建独立 ELB
          绑定 listener + backend
          写 Service.status.loadBalancer.ingress

          每个 Service 一个独立 ELB ✅
          端口冲突不存在（同 EKS/GKE）✅
```

---

## 3. LBC 作为参数模板（对齐 EKS/GKE）

EKS/GKE 的 LBC 是一个 annotation 容器，不指向任何具体 LB：

```
EKS LBC:
  spec.annotations:
    service.beta.kubernetes.io/aws-load-balancer-type: "nlb"
    service.beta.kubernetes.io/aws-load-balancer-scheme: "internet-facing"
  ↑ 配置指令，不是实例指针。N 个 Service 引用 → N 个独立 NLB。

CCE 方案 2 LBC（改造后）:
  spec.annotations:
    huawei-elb.io/public: "true"
    huawei-elb.io/bandwidth-size: "20"
    huawei-elb.io/bandwidth-charge-mode: "traffic"
    huawei-elb.io/eip-type: "5_bgp"
  ↑ 配置指令，不是实例指针。N 个 Service 引用 → N 个独立 ELB。
```

**触发条件的变化**：

| 旧方案 2（elb.id 实例引用） | 新方案 2（参数模板） |
|---|---|
| LBC 存 `elb.id: "xxx"` | LBC 存 `public/bandwidth/eip` 参数 |
| 手动模式：多个 Service 共享 ELB → 端口冲突 | **手动模式：LBC 只是参数模板，每 Service 独立 ELB → 零冲突** ✅ |
| 自动模式（无 LBC）：控制器 autocreate | 自动模式（无 LBC）：控制器用默认参数 autocreate |

---

## 4. 端到端流程

### 4.1 使用 LBC（手动模式）

```
步骤 ① 用户创建 LBC（参数模板）
  spec.annotations:
    huawei-elb.io/public: "true"
    huawei-elb.io/bandwidth-size: "20"
    huawei-elb.io/eip-type: "5_bgp"

步骤 ② 用户建 DBC，引用该 LBC
  spec.proxy.expose.loadBalancerConfigName: "my-lbc"

步骤 ③ OpenEverest（现有流程，不改）
  读 LBC.spec.annotations → 写 engine CR ServiceExpose.Annotations
  → PXC operator 建 Service
  Service annotations 包含用户 LBC 参数:
    huawei-elb.io/public: "true"
    huawei-elb.io/bandwidth-size: "20"
    ...
  裸奔窗口开始：Service 有 LBC 参数，但无 elb.id 和 autocreate
  CCM 检查：无 elb.id，无 autocreate → 什么都不做

步骤 ④ Service Reconciler（watch Service CREATE 事件）
  ├─ Service 有 huawei-elb.io/* 参数 ✅（识别为 ELB 参数模板）
  ├─ 无 kubernetes.io/elb.id ✅
  ├─ 无 kubernetes.io/elb.autocreate ✅
  ├─ 读 Service 上的 LBC 参数
  ├─ NetworkDetector.Detect() → VPC/子网/AZ
  ├─ 构造 elb.autocreate JSON（参数来自 LBC，VPC 信息来自探测）
  ├─ patch Service:
  │    kubernetes.io/elb.autocreate: <JSON>
  │    kubernetes.io/elb.class: "union"
  │    kubernetes.io/elb.instance-reclaim-policy: "alwaysDelete"
  └─ return（裸奔窗口结束）

步骤 ⑤ CCE CCM（watch Service UPDATE）
  检测到 autocreate → 调华为云 API 创建独立 ELB
  → 绑 listener + backend → 写 status.ingress

第二个 Service（replicas）→ 同样流程 → CCM 创建第二个独立 ELB ✅
两个 ELB 各自端口 3306，互不冲突 ✅
```

### 4.2 不使用 LBC（自动模式）

```
步骤 ① 用户建 DBC（loadBalancerConfigName 留空）

步骤 ② OpenEverest 建 Service（空注解）
  裸奔窗口开始

步骤 ③ Service Reconciler
  ├─ Service 无 ELB 相关注解 ✅（自动模式）
  ├─ 使用默认参数构造 autocreate JSON:
  │    public: true, bandwidth: 10Mbit/s, traffic 计费, 5_bgp
  ├─ NetworkDetector.Detect() → VPC/子网/AZ
  └─ patch Service: elb.autocreate + class + reclaim-policy

步骤 ④ CCE CCM 创建独立 ELB
  两个 Service 各绑各的 ELB ✅
```

---

## 5. 时序详解

```
T=0s    用户建 LBC（参数模板）+ DBC 引用该 LBC

T=0.1s  OpenEverest → 读 LBC 参数 → 写 engine CR → 建 Service-A + Service-B
          两个 Service 都有 LBC 参数，无 elb.id/autocreate
          裸奔窗口开始

T=0.1s  Service Reconciler（Service-A 触发）:
          ├─ 读 Service 上的 huawei-elb.io/* 参数
          ├─ NetworkDetector.Detect() → VPC/子网/AZ
          ├─ 构造 autocreate JSON（LBC 参数 + 探测的 VPC 信息）
          ├─ patch Service-A
          └─ return

T=0.1s  Service Reconciler（Service-B 触发，并行）:
          └─ 同上，构造另一个 autocreate（name 不同）

          裸奔窗口结束（< 1s）

T=0.2s  CCM ×2（并行）:
          Service-A：建 ELB-1 → ACTIVE → 绑 listener → 写 status.ingress
          Service-B：建 ELB-2 → ACTIVE → 绑 listener → 写 status.ingress

T~15s   两个 ELB 都 ACTIVE
         两个 Service 各绑各的 ELB，端口 3306 不冲突 ✅
         两个独立外部 IP
```

---

## 6. 触发条件

| 条件 | 说明 |
|---|---|
| `Service.type == LoadBalancer` | 只处理 LB 类型 |
| 无 `kubernetes.io/elb.id` | 未手动绑定已有 ELB |
| 无 `kubernetes.io/elb.autocreate` | 未注入过（幂等） |
| Service 由 OpenEverest engine operator 创建 | 过滤其他来源 |
| 无其他云提供商注解 | 不干扰 EKS/GKE |

**两种子模式**：

| 模式 | Service 上是否有 huawei-elb.io/* 参数 | 参数来源 |
|---|---|---|
| **手动（引用 LBC）** | ✅ 有（OpenEverest 从 LBC 同步到 Service） | LBC.spec.annotations |
| **自动（无 LBC）** | ❌ 无 | 默认值（public / 10Mbit/s / traffic / 5_bgp） |

---

## 7. LBC 参数映射（LBC 参数 → autocreate JSON）

| LBC annotation | autocreate JSON 字段 | 默认值 |
|---|---|---|
| `huawei-elb.io/public` | `type` | `true` → `public`，`false` → `inner` |
| `huawei-elb.io/bandwidth-size` | `bandwidth_size` | `10` |
| `huawei-elb.io/bandwidth-charge-mode` | `bandwidth_chargemode` | `traffic` |
| `huawei-elb.io/eip-type` | `eip_type` | `5_bgp` |
| `huawei-elb.io/bandwidth-share-type` | `bandwidth_sharetype` | `PER` |
| `huawei-elb.io/name` | `name` | 自动生成 |
| （自动探测） | `vip_subnet_cidr_id` | NetworkDetector |
| （自动探测） | `available_zone` | 节点 zone label |

---

## 8. LBC 现用注解（`kubernetes.io/elb.id`）的处理

改造后 LBC 不再存放 `elb.id`，但存量 LBC 已有该注解。Service Reconciler 的处理：

| Service 上存在 | 处理 |
|---|---|
| `kubernetes.io/elb.id`（存量，OpenEverest 同步） | **跳过**——走现有 CCM 绑定流程，不干涉 |
| `huawei-elb.io/*` 参数（新 LBC） | 触发 autocreate |

存量 LBC 不受影响，新 LBC 走参数模板。

---

## 9. ELB 删除

CCM 原生处理。控制器注入 `kubernetes.io/elb.instance-reclaim-policy: alwaysDelete`，删 Service 时 CCM 同步删 ELB。**无需 finalizer**。

---

## 10. 方案优势与约束

### 10.1 优势

| 优势 | 说明 |
|---|---|
| **手动模式下零端口冲突** | LBC 存参数不存 elb.id，每个 Service 独立 ELB。primary 和 replicas 各自绑定独立 ELB，完全对齐 EKS/GKE |
| **自动模式下零端口冲突** | 同上，每个 Service 独立 ELB |
| **UI 无多 LBC 问题** | LBC 是参数模板，多个 DBC 共享一个 LBC（按参数分组），不会每 DBC 产生新 LBC 条目 |
| **架构最接近 EKS/GKE** | CCM 原生建 ELB + 绑定，控制器只做参数转换。LBC 是参数模板（对齐 EKS/GKE），不是实例指针 |
| **裸奔窗口极短** | <1s（读 LBC 参数 + 探测 + 构造 + patch） |
| **代码量最少** | +200 行，单 Reconciler 自闭环，无 ELB CRUD |
| **ELB 生命周期由 CCM 管理** | 删 Service → CCM 删 ELB，无 finalizer 复杂性 |
| **不 patch 用户 DBC** | 只 patch Service，不修改用户业务资源 |

### 10.2 约束（不被选为最终方案的原因）

| 约束 | 说明 | 严重程度 |
|---|---|---|
| **autocreate 创建后参数不可变** | ELB 带宽、EIP 类型、公网/内网等参数创建后无法修改。华为云 CCE ELB 多样化参数（带宽 1-2000 Mbit/s、计费模式 traffic/bandwidth）是高频变参场景 | 🔴 硬伤 |
| **ELB 状态不可见** | 无 LBC 的结构化状态（ready/error/public-ip），仅 Service events + status.ingress | 🟡 中等 |
| **ACL 需独立处理** | `loadBalancerSourceRanges` → `elb.acl-*` 需单独实现 | 🟡 中等 |
| **CCM 行为不可控** | autocreate 建 ELB 由 CCE 闭源 CCM 控制，排查路径长 | 🟡 中等 |

---

## 11. 方案对比（EKS / GKE / CCE）

### 11.1 手动模式：使用 LBC 创建数据库集群

| 平台 | LBC 角色 | 每 DBC 的 LB 数量 | Primary | Replicas | 端口冲突 | 说明 |
|---|---|---|---|---|---|---|
| **AWS EKS** | 参数模板 | 2 个 NLB（独立计费） | ✅ | ✅ | 不发生 | LBC 只是模板，每 Service 独立 NLB |
| **Google GKE** | 参数模板 | 2 个 LB（独立计费） | ✅ | ✅ | 不发生 | 同 EKS |
| **CCE 最终方案** | 实例引用（`elb.id`） | 1 个 ELB（共享） | ✅ | ❌ | 发生 | 手动单 LBC 不创 replicas LBC |
| **CCE 方案 2** | **参数模板**（`bandwidth/eip/...`） | **2 个 ELB**（独立计费） | ✅ | ✅ | 不发生 | 对齐 EKS/GKE，Service Reconciler 读 LBC 参数 → autocreate |

### 11.2 自动模式：不使用 LBC 创建数据库集群

| 平台 | 工作机制 | 每 DBC 的 LB 数量 | Primary | Replicas | 端口冲突 | 说明 |
|---|---|---|---|---|---|---|
| **AWS EKS** | CCM 原生 | 2 个 NLB（独立计费） | ✅ | ✅ | 不发生 | 零控制器 |
| **Google GKE** | CCM 原生 | 2 个 LB（独立计费） | ✅ | ✅ | 不发生 | 同 EKS |
| **CCE 最终方案** | DBC Reconciler → 双 LBC → 双 ELB | 2 个 ELB（独立计费） | ✅ | ✅ | 不发生 | 通过双 LBC 独立 ELB |
| **CCE 方案 2** | Service Reconciler 默认参数 autocreate | 2 个 ELB（独立计费） | ✅ | ✅ | 不发生 | CCM 原生建 ELB |

### 11.3 总结

| 维度 | EKS | GKE | CCE（最终方案） | CCE（方案 2） |
|---|---|---|---|---|
| **手动模式 LB 独立性** | 每 Service 独立 NLB | 同左 | 共享同一 ELB（冲突） | **每 Service 独立 ELB** ✅ |
| **手动模式 replicas** | ✅ | ✅ | ❌ | ✅ |
| **自动模式 replicas** | ✅ | ✅ | ✅ | ✅ |
| **手动 LBC 本质** | 参数模板 | 参数模板 | 实例引用 | **参数模板** ✅ |
| **与 EKS/GKE 行为对齐** | — | — | 自动模式对齐 | **手动+自动均对齐** ✅ |
| **ELB 创建后可调参** | ✅ | ✅ | ✅ | ❌（autocreate 不可变） |
| **UI 配置面板** | ❌ | ❌ | ✅（LBC 实例） | ✅（LBC 参数模板） |

---

## 12. 结论

方案 2（LBC 参数模板 + autocreate）在所有模式下都对齐 EKS/GKE：
- **手动模式**：LBC 是参数模板，多 Service 引用 → 各自独立 ELB，零端口冲突
- **自动模式**：默认参数 autocreate → 独立 ELB，零端口冲突
- **用户体验**：与 EKS/GKE 完全一致，无 LBC 实例概念，无端口冲突

**不被选为最终方案的唯一原因**：autocreate 创建后 ELB 参数不可变。华为云 ELB 带宽 1-2000 Mbit/s 是可计费变量，数据库流量波动时用户需要调参，autocreate 无法满足。最终方案保留 LBC 作为可编辑的配置面板，代价是手动模式下的端口冲突（需双 LBC 解决）。

**方案 2 保留为备用**，适用于：
- ELB 参数创建后无需调整的长稳场景
- CCE autocreate 未来支持参数可变后的首选方案
