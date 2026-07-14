# 方案 2 详细设计：Service Reconciler（LBC 参数模板 + autocreate）

> **状态**: 方案 2

> **日期**: 2026-07-10

---

## 1. 核心思路

LBC 作为参数模板（对齐 EKS/GKE），存放 CCE ELB 配置参数而非 ELB ID。**创建通过 CCM autocreate，后续参数变更通过 controller 直接调华为云 API 更新**，两段互补：

```
EKS/GKE:
  LBC = {aws-load-balancer-type: nlb, scheme: internet-facing}  ← 参数模板
  Service 引用 LBC → CCM 建独立 NLB ✅
  参数变更 → 改 Service annotation → CCM 调 API 更新 ✅
  多个 Service 引用同一 LBC → 各自独立 NLB，零冲突 ✅

CCE 方案 2:
  LBC = {bandwidth_size: 20, eip_type: 5_bgp, public: true}   ← 参数模板
  Service 引用 LBC → Service Reconciler → autocreate → CCM 建独立 ELB ✅
  参数变更 → 改 LBC 注解 → Service Reconciler 调华为云 API 更新 ✅
  多个 Service 引用同一 LBC → 各自独立 ELB，零冲突 ✅
```

**两段式策略**：创建用 CCM（利用其 listener/backend 绑定逻辑），更新用 controller（绕过 autocreate 不可变限制）。效果完全等于 EKS/GKE：创建零配置，事后可调参。

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
│  │ 【创建路径】                                      │    │
│  │   识别由 OpenEverest 创建、引用了 LBC 的 Service    │    │
│  │   读 LBC 的参数（ELB 配置）→ 构造 autocreate JSON  │    │
│  │   探测 VPC/子网/AZ（补 CCM 能力缺失）              │    │
│  │   patch Service 注入 autocreate + elb.class      │    │
│  │   → CCM 读 autocreate → 建独立 ELB               │    │
│  │                                                  │    │
│  │ 【更新路径】                                      │    │
│  │   检测 Service 上的 LBC 参数变更                   │    │
│  │   调华为云 ELB API 更新 ELB（带宽/EIP/公网等）     │    │
│  │                                                  │    │
│  │ 【删除路径】                                      │    │
│  │   CCM 原生删 ELB（reclaim-policy: alwaysDelete）  │    │
│  └──────────────────────────────────────────────────┘    │
│                │                                         │
│         NetworkDetector（VPC/子网/AZ 探测）                │
│         huaweicloud.ELBClient（创建/更新）                 │
│         huaweicloud.Credentials                          │
└──────────────────────────────────────────────────────────┘
                       │
          ┌────────────┴────────────┐
          ▼                         ▼
    创建路径：CCM              更新路径：Controller
    autocreate → ELB           调 API → 更新 ELB 参数
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

CCE 方案 2 LBC:
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

步骤 ④ Service Reconciler — 创建路径（watch Service CREATE）
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

--- 参数变更流程 ---

步骤 ⑥ 用户修改 LBC 参数（如带宽 10M → 20M）
  LBC.spec.annotations: huawei-elb.io/bandwidth-size: "20"

步骤 ⑦ OpenEverest 同步 LBC annotation 到 Service
  Service annotation: huawei-elb.io/bandwidth-size: "20" (was "10")

步骤 ⑧ Service Reconciler — 更新路径（watch Service UPDATE）
  检测到 Service 已有 kubernetes.io/elb.autocreate ✅（说明 ELB 已创建）
  检测到 LBC 参数比上次 reconcile 时变了
  → 调华为云 ELB API: ModifyLoadBalancer（更新带宽）
  ✅
```

### 4.2 不使用 LBC（自动模式）

```
步骤 ① 用户建 DBC（loadBalancerConfigName 留空）

步骤 ② OpenEverest 建 Service（空注解）
  裸奔窗口开始

步骤 ③ Service Reconciler — 创建路径
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

T=0.1s  Service Reconciler（Service-A 创建路径）:
          ├─ 读 Service 上的 huawei-elb.io/* 参数
          ├─ NetworkDetector.Detect() → VPC/子网/AZ
          ├─ 构造 autocreate JSON（LBC 参数 + 探测的 VPC 信息）
          ├─ patch Service-A
          └─ return

T=0.1s  Service Reconciler（Service-B 创建路径，并行）:
          └─ 同上，构造另一个 autocreate（name 不同）

          裸奔窗口结束（< 1s）

T=0.2s  CCM ×2（并行）:
          Service-A：建 ELB-1 → ACTIVE → 绑 listener → 写 status.ingress
          Service-B：建 ELB-2 → ACTIVE → 绑 listener → 写 status.ingress

T~15s   两个 ELB 都 ACTIVE
         两个 Service 各绑各的 ELB，端口 3306 不冲突 ✅

--- 参数变更 ---

T=任意   用户改 LBC 带宽 10M → 20M
          → OpenEverest 同步到 Service
          → Service Reconciler 更新路径: 调 ELB API 更新带宽 ✅
```

---

## 6. 触发条件

| 条件 | 说明 |
|---|---|
| `Service.type == LoadBalancer` | 只处理 LB 类型 |
| 无 `kubernetes.io/elb.id` | 未手动绑定已有 ELB |
| Service 由 OpenEverest engine operator 创建 | 过滤其他来源 |
| 无其他云提供商注解 | 不干扰 EKS/GKE |

**两种子模式**：

| 模式 | Service 上是否有 huawei-elb.io/* 参数 | 参数来源 |
|---|---|---|
| **手动（引用 LBC）** | ✅ 有（OpenEverest 从 LBC 同步到 Service） | LBC.spec.annotations |
| **自动（无 LBC）** | ❌ 无 | 默认值（public / 10Mbit/s / traffic / 5_bgp） |

**两段式路径**：

| 路径 | 触发条件 | 动作 |
|---|---|---|
| **创建路径** | Service 无 `elb.autocreate` | 构造 autocreate JSON → patch Service → CCM 建 ELB |
| **更新路径** | Service 已有 `elb.autocreate` + LBC 参数和上次不一致 | 调华为云 ELB API 直接更新参数 |

---

## 7. LBC 参数映射与默认值

### 7.1 自动模式（无 LBC）—— 默认参数

用户不创建 LBC，UI 选择 "No configuration"。Service Reconciler 使用以下默认值构造 autocreate JSON：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| ELB 类型 | `public`（公网） | 与 GKE 一致，对外暴露。EKS 默认 `internal` |
| 带宽 | `10` Mbit/s | 华为云最小值 |
| 计费模式 | `traffic`（按流量） | 按实际流量付费 |
| EIP 类型 | `5_bgp` | BGP 多线，联通性最好 |
| 带宽共享类型 | `PER`（独享） | 独享带宽，不共享 |
| ELB 名称 | `cce-lb-<ns>-<svc>` | 自动生成，64 字符内截断 |
| VPC ID | NetworkDetector 探测 | 从节点 ECS 元数据获取 |
| 子网 ID | NetworkDetector 探测 | 从节点标签获取 |
| 可用区 | NetworkDetector 探测 | 从节点 zone 标签获取 |

### 7.2 手动模式（LBC 参数模板）—— 支持的注解

用户在 LBC 的 `spec.annotations` 中配置以下参数作为模板。OpenEverest 将其同步到 Service，Service Reconciler 读取后构造 autocreate JSON 或调 ELB API 更新：

| LBC annotation | 类型 | 参考值 | 默认值 | autocreate 字段 | API 更新 |
|----------------|------|--------|--------|-----------------|---------|
| `huawei-elb.io/public` | string | `"true"` / `"false"` | `"true"` -> public | `type` | ❌ 不可变（需重建） |
| `huawei-elb.io/bandwidth-size` | int | `1` – `2000`（Mbit/s） | `10` | `bandwidth_size` | ModifyEIPBandwidth ✅ |
| `huawei-elb.io/bandwidth-charge-mode` | string | `"traffic"` / `"bandwidth"` | `"traffic"` | `bandwidth_chargemode` | ModifyEIPBandwidth ✅ |
| `huawei-elb.io/eip-type` | string | `"5_bgp"` / `"5_sbgp"` / `"5_telcom"` / `"5_union"` | `"5_bgp"` | `eip_type` | ❌ 不可变（需重建） |
| `huawei-elb.io/bandwidth-share-type` | string | `"PER"` / `"WHOLE"` | `"PER"` | `bandwidth_sharetype` | — |
| `huawei-elb.io/name` | string | 自定义 ELB 名称（≤64 字符） | `cce-lb-<ns>-<svc>` | `name` | — |

**LBC 参数模板示例**：
```yaml
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: my-lbc-template
spec:
  annotations:
    huawei-elb.io/public: "true"
    huawei-elb.io/bandwidth-size: "20"
    huawei-elb.io/bandwidth-charge-mode: "traffic"
    huawei-elb.io/eip-type: "5_bgp"
```

> **注意**：`eip_type` 创建后不可变更（华为云 API 限制），一旦 ELB 创建，修改该参数需删除重建。此限制与 EKS/GKE 的 NLB/LB 类型不可切换一致。

### 7.3 三种平台对比

| | EKS | GKE | CCE 方案 2 |
|---|---|---|---|
| 默认公/内网 | internal（内网） | external（公网） | public（公网） |
| 默认带宽 | 无（自动扩展） | 无（自动扩展） | 10 Mbit/s |
| 参数配置入口 | Service annotations | Service annotations | LBC annotations → Service |
| 创建后可调参 | ✅ CCM 调 API | ✅ CCM 调 API | ✅ Service Reconciler 调 API |
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

CCM 原生处理。控制器注入 `kubernetes.io/elb.instance-reclaim-policy: alwaysDelete`，删 Service 时 CCM 同步删 ELB。ELB 本身无需 finalizer；但 ACL IP 组清理通过 `acl-cleanup` finalizer 阻塞 Service 删除直到 IP 组删除完成。

---

## 10. 方案优势与约束

### 10.1 优势

| 优势 | 说明 |
|---|---|
| **手动模式零端口冲突** | LBC 存参数不存 elb.id，每 Service 独立 ELB |
| **自动模式零端口冲突** | 同上 |
| **UI 无多 LBC 问题** | LBC 是参数模板，多 DBC 共享一个 LBC，不逐 DBC 新增条目 |
| **架构最接近 EKS/GKE** | LBC 是参数模板，创建用 CCM，更新用 controller |
| **ELB 参数事后可调** | 改 LBC 注解 → Service Reconciler 调 API 更新 ✅ |
| **裸奔窗口极短** | <1s（读参数 + 探测 + 构造 + patch） |
| **ELB 生命周期由 CCM 管理** | 删 Service -> CCM 删 ELB（ELB 无 finalizer）；ACL IP 组通过 acl-cleanup finalizer 清理 |
| **不 patch 用户 DBC** | 只 patch Service，不修改用户业务资源 |

### 10.2 约束

| 约束 | 说明 | 严重程度 |
|---|---|---|
| **实现复杂度高于预期** | Service Reconciler 需同时处理创建（autocreate）和更新（华为云 API），比最初 +200 行估算更高 | 🟡 中等 |
| **ELB 状态不可见** | 无 LBC 的结构化状态（ready/error/public-ip），仅 Service events + status.ingress | 🟡 中等 |
| **ACL 需独立处理** | `loadBalancerSourceRanges` → `elb.acl-*` 需单独实现 | 🟡 中等 |
| **CCM 行为不可控** | autocreate 建 ELB 由 CCE 闭源 CCM 控制，排查路径长 | 🟡 中等 |
| **EIP 类型不可变更** | `eip_type` 创建后不可变（华为云 API 限制），与 EKS/GKE NLB/LB 类型不可切换一致 | 🟢 平台限制，非方案问题 |

---

## 11. 方案对比（EKS / GKE / CCE）

### 11.1 手动模式：使用 LBC 创建数据库集群

| 平台 | LBC 角色 | 每 DBC 的 LB 数量 | Primary | Replicas | 端口冲突 | 说明 |
|---|---|---|---|---|---|---|
| **AWS EKS** | 参数模板 | 2 个 NLB（独立计费） | ✅ | ✅ | 不发生 | LBC 只是模板，每 Service 独立 NLB |
| **Google GKE** | 参数模板 | 2 个 LB（独立计费） | ✅ | ✅ | 不发生 | 同 EKS |
| **CCE 方案 1** | 实例引用（`elb.id`） | 1 个 ELB（共享） | ✅ | ❌ | 发生 | 手动单 LBC 不创 replicas LBC |
| **CCE 方案 2** | **参数模板**（`bandwidth/eip/...`） | **2 个 ELB**（独立计费） | ✅ | ✅ | 不发生 | 对齐 EKS/GKE，创建用 CCM，更新用 controller |

### 11.2 自动模式：不使用 LBC 创建数据库集群

| 平台 | 工作机制 | 每 DBC 的 LB 数量 | Primary | Replicas | 端口冲突 | 说明 |
|---|---|---|---|---|---|---|
| **AWS EKS** | CCM 原生 | 2 个 NLB（独立计费） | ✅ | ✅ | 不发生 | 零控制器 |
| **Google GKE** | CCM 原生 | 2 个 LB（独立计费） | ✅ | ✅ | 不发生 | 同 EKS |
| **CCE 方案 1** | DBC Reconciler → 双 LBC → 双 ELB | 2 个 ELB（独立计费） | ✅ | ✅ | 不发生 | 通过双 LBC 独立 ELB |
| **CCE 方案 2** | Service Reconciler 默认参数 autocreate | 2 个 ELB（独立计费） | ✅ | ✅ | 不发生 | CCM 原生建 ELB |

### 11.3 用户视角对比

| 维度 | EKS | GKE | CCE（方案 1） | CCE（方案 2） |
|---|---|---|---|---|
| **操作步骤** | 1 步 | 1 步 | 1 步 | 1 步 |
| **前提知识** | 零 | 零 | 零 | 零 |
| **LBC 是参数模板** | ✅ | ✅ | ❌（实例引用） | ✅ |
| **每 Service 独立 LB** | ✅ | ✅ | 自动 ✅ / 手动 ❌ | ✅（手动+自动） |
| **手动模式端口冲突** | 不发生 | 不发生 | 发生 | **不发生** |
| **自动模式端口冲突** | 不发生 | 不发生 | 不发生 | 不发生 |
| **UI LBC 条目** | 手动：用户自建；自动：0 | 手动：用户自建；自动：0 | 手动：用户自建；自动：每 DBC 2 条 | 手动：用户自建；自动：0 |
| **ELB 参数事后可调** | ✅（CCM 调 API） | ✅（CCM 调 API） | ✅（改 LBC → API） | ✅（改 LBC → Reconciler 调 API） |
| **体验对齐 EKS/GKE** | — | — | 自动模式对齐 | **手动+自动均对齐** ✅ |

### 11.4 实现层面差异

| 差异 | EKS/GKE | CCE（方案 2） | 原因 |
|---|---|---|---|
| **需要额外控制器** | ❌ CCM 全包 | **需要** Service Reconciler | CCE CCM 不会读 LBC 自定义参数，需控制器转为 `elb.autocreate` JSON |
| **创建路径** | CCM 调 API 建 LB | CCM 读 autocreate 建 ELB | 同上——控制器弥补 CCM 参数转换能力 |
| **更新路径** | CCM 调 API 更新 | **Service Reconciler 调 API 更新** | CCM 不参与 autocreate 创建后的参数更新，由 controller 绕过 |
| **需要探测 VPC/子网/AZ** | ❌ CCM 自动 | **需要** NetworkDetector | CCE CCM 的 `elb.autocreate` 不自动探测 |
| **ACL / Source Range** | ✅ CCM 原生 | ⚠️ 需单独实现 | CCE CCM 不认标准 K8s `loadBalancerSourceRanges` |

**实现层面差异的根源**：CCE CCM 的能力缺口——①不会自动探测 VPC/子网/AZ；②不认标准 K8s  字段；③不认自定义 LBC 参数；④autocreate 创建后不参与参数更新。这些缺口由 Service Reconciler + NetworkDetector 填补。EKS/GKE CCM 原生具备这些能力，不需要额外控制器。`loadBalancerSourceRanges`

### 11.5 总结

| 维度 | EKS | GKE | CCE（方案 1） | CCE（方案 2） |
|---|---|---|---|---|
| **手动模式 replicas** | ✅ | ✅ | ❌ | ✅ |
| **自动模式 replicas** | ✅ | ✅ | ✅ | ✅ |
| **手动 LBC 本质** | 参数模板 | 参数模板 | 实例引用 | **参数模板** ✅ |
| **与 EKS/GKE 行为对齐** | — | — | 自动对齐 | **手动+自动均对齐** ✅ |
| **ELB 创建后可调参** | ✅（CCM） | ✅（CCM） | ✅（API） | ✅（Reconciler → API） |
| **调参路径** | CCM | CCM | controller | controller |
| **需额外控制器** | ❌ | ❌ | ✅ | ✅ |
| **需探测 VPC** | ❌ | ❌ | ✅ | ✅ |
| **ACL 需额外处理** | ❌ | ❌ | ✅ | ✅ |

---

## 12. 结论

方案 2（LBC 参数模板 + 两段式：autocreate 创建 + API 更新）在所有维度都对齐 EKS/GKE：

- **手动模式**：LBC 是参数模板，多 Service 引用 → 各自独立 ELB，零端口冲突
- **自动模式**：默认参数 autocreate → 独立 ELB，零端口冲突
- **参数可调**：改 LBC 注解 → Service Reconciler 调 API 更新，效果等于 EKS/GKE CCM 调 API
- **用户体验**：与 EKS/GKE 完全一致

**与 EKS/GKE 的唯一差异在实现层面**：需要 Service Reconciler（填补 CCE CCM 的能力缺口），但这不影响用户使用体验。

## 12. 结论
