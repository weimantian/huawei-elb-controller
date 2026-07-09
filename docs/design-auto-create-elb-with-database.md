# 设计：创建数据库时自动创建 ELB

> **分支**: `design/auto-create-elb-with-database`
> **状态**: 设计阶段，待确认关键问题后进入实现
> **作者**: weimantian
> **日期**: 2026-07-08

---

## 1. 背景与问题

### 1.1 当前流程（两步）

```
① 用户创建 LoadBalancerConfig（LBC）
② huawei-elb-controller 创建 ELB，写回 elb.id 到 LBC
③ 用户等待 LBC ready=true
④ 用户创建 DatabaseCluster（DBC），引用该 LBC
⑤ OpenEverest 创建 Service，复制 elb.id 到 Service
⑥ CCE CCM 绑定 ELB -> Service 获得外部 IP
```

### 1.2 痛点

用户嫌两步太麻烦。对比 EKS/GKE 的体验--创建 `type: LoadBalancer` 的 Service 即自动获得负载均衡器，零额外配置。当前方案要求用户先理解 LBC 概念、创建它、等待 ready、再回来创建数据库。

### 1.3 目标

**用户只创建 DatabaseCluster，ELB 自动创建并绑定，无需手动建 LBC。** 体验对标 EKS/GKE。

---

## 2. 约束与关键事实（已通过源码确认）

以下事实来自 `openeverest/openeverest-operator` 源码（commit `a4519bd`）研究，是方案设计的基础：

| # | 事实 | 源码位置 | 影响 |
|---|---|---|---|
| F1 | `loadBalancerConfigName` 留空时，OpenEverest 仍创建 LoadBalancer Service（带空注解） | `helper.go` L698-L711 | 空值不阻塞 Service 创建 |
| F2 | `loadBalancerConfigName` 设了名字但 LBC 不存在时，**Validating Webhook 在 CREATE 时拒绝** | `databasecluster_webhook.go` L309-L325 | DBC 创建时 LBC 必须已存在 |
| F3 | `ValidateUpdate` **不校验** LBC 存在性 | `databasecluster_webhook.go` L145-L188 | 更新时可指向不存在的 LBC（但 reconcile 会报错） |
| F4 | CEL 规则：`loadBalancerConfigName` 一旦设了不能清空（除非 type=internal），但从 `""` 设为有值**允许** | `databasecluster_types.go` L386 | 可后续 patch DBC 补 loadBalancerConfigName |
| F5 | LBC 的 `spec.annotations` 变化后，OpenEverest 会重新同步到 Service（非一次性拷贝） | `databasecluster_controller.go` L879-L908 | LBC 注解后补也能传播到 Service |
| F6 | LBC 是 **cluster-scoped**（`scope=Cluster`） | `loadbalancerconfig_types.go` L39 | 命名需全局唯一 |
| F7 | LBC 被 DBC 引用时获得 `everest.percona.com/in-use-protection` finalizer | `consts.go` L25, `helper.go` L1111-L1124 | 删除 LBC 前需先解除 DBC 引用 |
| F8 | OpenEverest 不直接创建 Service，而是创建 engine CR（PXC/PSMDB/PG），由 engine operator 建 Service | `databasecluster_controller.go` L735-L737 | Service 命名由 engine operator 决定 |
| F9 | engine CR 与 DBC 同名、同 namespace | `providers/pxc/provider.go` L67-L72 | DBC 名 = engine CR 名 |
| F10 | `kubernetes.io/elb.id` 注解走 LBC `spec.annotations` -> engine CR `ServiceExpose.Annotations` -> Service `metadata.annotations` | `helper.go` L698-L729 | 注解传播路径已验证 |

### 2.1 关键问题确认

> Q1 已通过 CCE 官方文档与开源 CCM 文档确认。Q2-Q5 待实测。

**Q1 结论：CCE 内置 CCM 与开源 CCM 行为不同，本方案对 CCE 场景安全。**

| CCM 类型 | 无 `elb.id` 无 `autocreate` 的 LoadBalancer Service 行为 | 来源 |
|---|---|---|
| **CCE 内置 CCM**（CCE 集群预装，主要场景） | **不创建 ELB，Service 停在 `<pending>`** | [cce_10_0385](https://support.huaweicloud.com/usermanual-cce/cce_10_0385.html)、[cce_10_0681](https://support.huaweicloud.com/usermanual-cce/cce_10_0681.html) |
| **开源 CCM**（kubernetes-sigs/cloud-provider-huaweicloud，ECS 自建集群） | **自动创建 ELB**（`elb.id` 为空即触发） | [usage-guide.md](https://github.com/kubernetes-sigs/cloud-provider-huaweicloud/blob/master/docs/usage-guide.md) |

证据链：

1. CCE 文档只定义两种互斥场景："使用已有ELB"（必填 `elb.id`）与"自动创建ELB"（必填 `elb.autocreate`），无第三种"都不填"场景。
2. CCE 注解全集页明确：`elb.autocreate` 为"仅自动创建ELB的场景：必填"，`elb.id` 为"仅关联已有ELB的场景需填写"，两者"不能同时填写"。
3. 开源 CCM 文档则相反：`elb.id` "If empty, a new ELB service will be created automatically"。
4. **对本控制器主要场景（CCE 集群）**：本方案的 Service 裸奔窗口安全--CCM 不会抢跑建 ELB。
5. **本方案仅支持 CCE 集群**：不考虑 ECS 自建集群（开源 CCM 场景）；裸奔窗口内的 autocreate 风险不在本方案处理范围内。

### 2.2 待实测问题

| # | 问题 | 影响 |
|---|---|---|
| Q2 | OpenEverest UI 创建 DBC 时 `loadBalancerConfigName` 能否留空？ | 决定 UI 路径是否走得通 |
| Q4 | 华为云 ELB 名称长度限制（64 字符？） | 影响 §4.4 命名截断逻辑 |
| Q5 | OpenEverest engine operator 创建的 Service 命名规则（PXC/PSMDB/PG 各自）？ | 影响调试与日志关联 |
> 注：Q3（开源 CCM 行为）已合并入 Q1；本方案仅支持 CCE，不再单独追踪。

### 2.3 待验证事实

| # | 事实 | 确认方式 |
|---|---|---|
| F11 | DBC 删除时，OpenEverest 先清理 engine CR → `in-use-protection` 随之解除 | 需在 CCE 环境实测或再次溯源 OpenEverest 源码 |
---

## 3. 方案设计
经过分析，形成两个候选方案，都对标 EKS/GKE 的零配置体验（创建 DBC 即获得 ELB，无额外注解）。§3.1-3.5 详述方案 1，§3.6 详述方案 2，§3.7 分析精细控制与 LBC UI 可见性，§3.8 给出方案 1 与方案 2 的对比，§3.9 给出当前方案与自动方案的对比。


### 3.1 方案 1：watch DBC -> 自动建 LBC + ELB -> patch DBC

新增 DBC Reconciler，watch DBC 创建事件，自动建 LBC+ELB，再 patch DBC 的 `loadBalancerConfigName`。

**流程时序**：
```
① 用户建 DBC（loadBalancerConfigName 留空）
② OpenEverest 建 Service（空注解）← ⚠️ Service 裸奔窗口开始
③ 控制器检测到 DBC，开始建 LBC + ELB
④ 控制器 patch DBC 的 loadBalancerConfigName = elb-<dbc-name>
⑤ OpenEverest 重新 reconcile，GetAnnotations 读到 elb.id，更新 Service
   ← Service 裸奔窗口结束
⑥ CCM 绑定 ELB
```

> **「Service 裸奔窗口」详解**

步骤 ②-⑤ 之间，Service 已作为 `type: LoadBalancer` 存在，但 `metadata.annotations` 里**没有任何 ELB 相关注解**（无 `elb.id`，无 `elb.autocreate`）。

```
时间线:

  ② OpenEverest 建 Service
  │  此时 Service 长这样:
  │  - type: LoadBalancer
  │  - annotations: {}  ← 空的！没有 elb.id 也没有 autocreate
  │  - status.loadBalancer: {}  ← CCM 还没绑定
  │
  │  ┌──────────── 裸奔窗口（几秒到几十秒）────────────┐
  │  │ CCE CCM 检查 Service -> 没 elb.id 也没 autocreate │
  │  │ -> 什么都不做，Service 停在 <pending>           │
  │  │ （开源 CCM 则相反：会趁这个窗口自动建 ELB）       │
  │  └────────────────────────────────────────────────┘
  │
  ③ 控制器建 LBC -> LBC Reconciler 建 ELB -> 写 elb.id
  ④ 控制器 patch DBC（loadBalancerConfigName = elb-xxx）
  ⑤ OpenEverest Reconcile: GetAnnotations() 读到 LBC 上的 elb.id
     -> 更新 engine CR -> engine operator update Service annotations:
     -> {kubernetes.io/elb.id: xxx, kubernetes.io/elb.class: union}
  │  ← 裸奔窗口结束，Service 终于有了 elb.id
  │
  ⑥ CCM 检测到 Service 有了 elb.id -> 调华为云 API 绑定 -> Service 获得外部 IP
```

> **为什么叫「裸奔」？** 就像一个 LoadBalancer Service 光着身子跑出去--它告诉 CCM"我需要一个 LB"，但没告诉 CCM"用哪一个"或"帮我建一个"。CCM 只能干瞪眼。

**Q1 已确认**：CCE 内置 CCM 在这个窗口里**不会**自动建 ELB（Service 老实停在 `<pending>`），所以方案安全。

**对比 EKS/GKE**：它们没有这个窗口--因为 CCM 从 Service 注解里直接读 LB 配置、自己调云 API 建 LB。Service 出生时注解已经就位，CCM 直接建完绑定，不存在"Service 有了、LB 还没建"的中间状态。这就是 CCE 架构差异的根本来源：CCE CCM 只会绑定不会建，必须由外部（控制器）提前建好 ELB。

### 3.2 触发条件

- `spec.proxy.expose.type == LoadBalancer`
- `spec.proxy.expose.loadBalancerConfigName == ""`
- DBC 有注解 `huawei-elb.io/auto-elb: "true"`（opt-in 开关）
- DBC 创建超过 grace period（5s）--OpenEverest UI 可能先创建 DBC 再立即 patch 设 `loadBalancerConfigName`（两次 API 调用），grace period 避免控制器在 UI 两次调用之间抢跑创建不必要的 LBC。若 UI 不支持两步创建（单次请求即设好值），此条件可移除。
- 不含其他云提供商注解（复用现有 `hasForeignCloudAnnotations` 逻辑）

**不触发的情况**：
- `loadBalancerConfigName` 已设值 -> 走现有 LBC 流程
- `huawei-elb.io/auto-elb` 未设或为 `false` -> 不自动建，Service 保持空注解（等同 EKS/GKE 上不配置 LB 的情况）

> **opt-in 设计的意义**：用户显式声明"我要自动建 ELB"，控制器才行动。存量 DBC 不受影响。OpenEverest UI 如果不支持加这个注解，用户可通过 `kubectl annotate` 或 Helm post-install hook 添加。

### 3.3 LBC 命名与 DBC 删除

**LBC 命名**：`elb-<namespace>-<dbc-name>`（cluster-scoped，需避免跨命名空间撞名）

**DBC 删除处理**：
- 控制器检测到 DBC 被删
- 等 OpenEverest 移除 LBC 的 `in-use-protection` finalizer（DBC 不再引用 LBC）
- 控制器删除自动创建的 LBC
- LBC finalizer 删除 ELB

### 3.4 新增组件

```
internal/controller/databasecluster_controller.go   # 新增 DBC Reconciler
internal/controller/loadbalancerconfig_controller.go # 不变
```

- **代码改动**：+300~400 行（新 Reconciler）
- **用户体验**：UI 点一下创建数据库，ELB 全自动
- **竞态**：⚠️ Service 裸奔窗口（步骤 ②-⑤），Q1 已确认 CCE 场景安全
- **patch DBC**：会修改用户的 DBC 资源（设 loadBalancerConfigName）

### 3.5 方案 1 设计理由

1. **最大化复用现有代码**：LBC Reconciler 全部逻辑（探测、创建、删除、finalizer、状态注解）原样复用，新增的 DBC Reconciler 只是触发器 + patch 逻辑。
2. **状态可见性完整**：LBC 上的 `ready`/`elb-status`/`error`/`public-ip` 注解全保留，运维体验不变。
3. **与现有方案完全兼容**：用户已设 `loadBalancerConfigName` 的 DBC 不受影响，新功能仅对空值触发。

### 3.6 方案 2：watch Service -> 注入 autocreate 注解 -> CCM 建 ELB

**思路**：新增 Service Reconciler，watch LoadBalancer Service，自动探测 VPC/子网/AZ，构造 `elb.autocreate` JSON 注入到 Service，让 CCE CCM 原生建 ELB。架构最接近 EKS/GKE--CCM 直接建 ELB，控制器只负责探测注入。

**流程时序**：
```
① 用户建 DBC（loadBalancerConfigName 留空）
② OpenEverest 建 Service（空注解）← ⚠️ Service 裸奔窗口开始
③ 控制器检测到 Service，自动探测 VPC/子网/AZ
④ 控制器构造 autocreate JSON + elb.class + elb.tags，patch 到 Service
   ← Service 裸奔窗口结束
⑤ CCE CCM 检测到 autocreate 注解，创建 ELB + 绑定
⑥ CCM 写 Service.status.loadBalancer.ingress
```

**Service 裸奔窗口**：与方案 1 相同，步骤 ②-④ 之间 Service 无 ELB 注解。但窗口更短--方案 1 需要建 LBC + 建 ELB + patch DBC + 更新 Service（4 步），方案 2 只需探测 + 注入注解（1 步）。

**触发条件**：
- Service `type == LoadBalancer`
- Service 不含 `kubernetes.io/elb.id` 和 `kubernetes.io/elb.autocreate`（未配置 ELB）
- Service 由 OpenEverest engine operator 创建（识别机制待 Q5 确认后定稿：ownerReference 链跨 DBC->engine CR->Service 两层，或按命名规则）
- 不含其他云提供商注解

**ELB 删除**：CCM 原生处理。Service 被删时，CCM 根据 `kubernetes.io/elb.instance-reclaim-policy` 注解决定保留或删除 ELB（默认删除）。控制器无需 finalizer。

**新增组件**：
```
internal/controller/service_controller.go   # 新增 Service Reconciler
internal/controller/loadbalancerconfig_controller.go # 不变
```

- **代码改动**：+200 行（新 Service Reconciler，无 ELB CRUD 逻辑）
- **用户体验**：与 EKS/GKE 完全一致--创建 DBC 即获得外部 IP，无额外注解
- **竞态**：⚠️ Service 裸奔窗口（步骤 ②-④），比方案 1 短，Q1 已确认 CCE 场景安全
- **patch 用户资源**：patch Service（注入注解），不 patch DBC
- **LBC**：不需要

**`elb.autocreate` JSON schema 摘要**（完整 14 字段见附录 C）：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | String | 否 | ELB 名称，默认 `cce-lb-<service.UID>` |
| `type` | String | 否 | `public`（公网）/ `inner`（内网），默认 `inner` |
| `bandwidth_name` | String | 公网必填 | 带宽名称 |
| `bandwidth_chargemode` | String | 公网必填 | `bandwidth` / `traffic` |
| `bandwidth_size` | Integer | 公网必填 | 1-2000 Mbit/s |
| `bandwidth_sharetype` | String | 公网必填 | `PER`（独享带宽） |
| `eip_type` | String | 公网必填 | `5_bgp` / `5_sbgp` / `5_telcom` / `5_union` |
| `vip_subnet_cidr_id` | String | 否 | IPv4 子网 ID |
| `available_zone` | []String | 独享必填 | 可用区（独享 ELB 专有） |
| `l4_flavor_name` | String | 否 | L4 flavor（独享 ELB 专有） |

> Tags 不在 autocreate JSON 内，是独立注解 `kubernetes.io/elb.tags`（格式 `key1=value1,key2=value2`）。

**方案 2 的限制**：
1. **仅 CCE 可用**：`elb.autocreate` 是 CCE CCM 专有注解（本方案仅支持 CCE，非额外限制）。
2. **无 Region 覆盖**：autocreate JSON 无 region 字段，ELB 永远建在集群所在 Region。（EKS/GKE 也不支持跨 Region，不是 parity gap）
3. **ELB 创建后不可变**：autocreate 注解创建后不可修改。可变的属性（端口、健康检查）通过 Service spec 改即可；不可变的（带宽、EIP 类型、AZ）需重建 ELB。虽然 EKS/GKE 也不支持大部分属性变更，但华为云 ELB 有带宽计费模型，用户大概率需要事后调带宽--这使得不可变性在华为云场景下是真实痛点而非理论问题（详见 §3.7）
4. **状态可见性弱**：无 LBC，ELB 状态只有 Service events 和 `status.loadBalancer.ingress`，缺少 `ready`/`error`/`public-ip` 等结构化状态注解。
5. **ELB 精细控制受限**：autocreate JSON 14 个字段（名称、类型、带宽、EIP、子网、AZ、flavor），控制器直接 API 支持更多参数。

### 3.7 精细控制与 LBC UI 可见性分析

> 本节分析两个方案在 ELB 参数配置入口和创建后可变性上的差异。这是方案选择的关键决策点。

#### 当前精细控制机制

ELB 参数通过 LBC 的 `spec.annotations` 配置。用户在 OpenEverest LBC UI 页面填写注解，控制器的 `parseELBOptions()` 读取后调 ELB v3 API：

| 注解 | 说明 | 默认值 |
|---|---|---|
| `huawei-elb.io/public` | 公网/内网 | `true` |
| `huawei-elb.io/bandwidth-size` | 带宽大小（Mbit/s） | `10` |
| `huawei-elb.io/bandwidth-charge-mode` | 计费模式 | `traffic` |
| `huawei-elb.io/public-ip-network-type` | EIP 类型 | `5_bgp` |
| `huawei-elb.io/region` | Region 覆盖 | 集群 Region |

**关键**：DBC 创建 UI 里没有 ELB 参数配置入口，只有一个 LBC 下拉框。ELB 参数全部在 LBC 配置 UI 里通过注解设置。当前方案支持创建后修改：用户改 LBC 注解 -> 控制器重新 reconcile -> 调 API 更新 ELB 参数（如带宽扩容）。

#### 方案 1：LBC 仍出现在 UI 中，精细控制保留

控制器自动建的 LBC **会出现在 OpenEverest LBC UI 中**，用户可见可编辑：

```
创建 DBC -> 控制器自动建 LBC（默认参数：公网/10Mbit/s/traffic/5_bgp）
  -> ELB 建好
  -> 用户去 LBC UI 找到自动创建的 LBC
  -> 加注解 huawei-elb.io/bandwidth-size: "100"
  -> 控制器检测 LBC 变化 -> 调 ELB API 更新带宽为 100Mbit/s ✓
```

精细控制保留，只是延后到创建之后。默认值开箱即用，需要定制时事后改 LBC 注解。

#### 方案 2：无 LBC，精细控制丢失

方案 2 不建 LBC，控制器直接把 `elb.autocreate` JSON 注入 Service。但 autocreate 注解**创建后不可变**（CCE 官方文档明确：“该参数在创建完成后不可修改”）：

```
创建 DBC -> OpenEverest 建 Service -> 控制器注入 autocreate JSON（默认值）
  -> CCM 建 ELB（10Mbit/s，不可变）
  -> 用户想改带宽？❌ autocreate 不可改，只能删 Service 重建
  -> 用户在哪里配置？❌ DBC UI 无 ELB 配置，LBC UI 无对应 LBC
```

| | 方案 1 | 方案 2 |
|---|---|---|
| LBC 出现在 UI 中？ | ✅ 是（自动创建的 LBC 可见可编辑） | ❌ 无 LBC |
| 创建时用默认值？ | 是 | 是 |
| 创建后可改带宽？ | ✅ 改 LBC 注解 -> 控制器调 API 更新 | ❌ autocreate 不可变 |
| 创建后可改公网/内网？ | ✅ 同上 | ❌ 不可变 |
| 创建后可改 EIP 类型？ | ✅ 同上 | ❌ 不可变 |
| 用户配置入口 | LBC UI（事后编辑） | 无 |

#### 为什么这对华为云场景是硬伤

之前分析说“autocreate 不可变不是 parity gap，因为 EKS/GKE 也不支持大部分属性变更”。这个判断在纯 EKS/GKE parity 视角下成立，但忽略了一个关键差异：

- **EKS/GKE 没有带宽概念**--AWS 和 GCP 的网络自动扩展，用户不设带宽上限
- **华为云 ELB 有带宽计费**--带宽大小直接影响费用和性能，用户大概率需要事后调（如数据库流量增大要扩带宽）

方案 1 保留 LBC 作为配置入口，用户随时可调。方案 2 没有 LBC，autocreate 又不可变，**用户失去了唯一的精细控制途径**。这使得方案 2 的“不可变”限制在华为云场景下是真实痛点，而非理论问题。

#### ELB 能力差异决定了 LBC 的价值

> 前面的分析以 EKS/GKE 为参照系。但华为云 ELB 的可调参数比 AWS/GKE LB 多一个数量级，直接决定了 LBC 在华为云场景下不是“可选定制机制”而是“必需配置入口”。

**三家云 LB 可调参数对比**：

| 云 | LB 可调参数 | 默认零配置够用？ | 事后调参需求 |
|---|---|---|---|
| AWS NLB | 类型、scheme、跨区、idle timeout | ✅ 够（自动扩展，无带宽） | 弱（大多一次定终身） |
| GKE Global LB | 类型、scheme、证书 | ✅ 够（自动扩展，无带宽） | 弱 |
| 华为云 ELB | **带宽大小**、计费模式、EIP 类型、Region、标签... | ⚠️ 默认能用但带宽/计费几乎必调 | **强**（流量大扩带宽、省钱改计费模式） |

华为云 ELB 的可调参数比 AWS/GKE 多一个数量级，特别是**带宽计费模型**--这是 AWS/GKE 完全没有的概念。参数多 = 需要 UI 入口 = LBC 有存在价值。

**EKS/GKE 与华为云的 LBC 模式差异**：

```
EKS/GKE（LB 参数少，默认够用）：
  默认流程：建 DBC -> CCM 用默认值建 LB -> 完成（LBC 列表空，不需要）
  定制流程：需要时才建 LBC -> 填注解 -> CCM 按注解建定制 LB -> 事后可改
  => LBC 是“需要定制时才创建”的可选资源，不是默认生成的

方案 1（华为云 LB 参数多，默认不够用）：
  默认流程：建 DBC -> 控制器自动建 LBC（默认值）-> 控制器建 ELB
  定制流程：用户去 LBC UI 改注解 -> 控制器调 API 更新 ELB
  => 每个 DBC 都生成一个 LBC = 每个DB一个可调参入口

方案 2（对标 EKS/GKE 的“零 LBC”模式，但丢了可配性）：
  默认流程：建 DBC -> 控制器注入 autocreate -> CCM 建 ELB
  定制流程：❌ 无 LBC 可改，autocreate 不可变
  => 失去唯一配置途径
```

**LBC 列表“污染”的重新定性**：

用 EKS/GKE parity 视角看，方案 1“每个 DBC 都生成一个 LBC”似乎是缺陷（EKS/GKE 默认不生成 LBC）。但这个判断用错了参照系：

| 视角 | 判断 |
|---|---|
| EKS/GKE 视角（错误套用） | LBC 列表每DB一个 = 污染 |
| 华为云实际场景（正确） | LBC 列表每DB一个 = 每个DB一个可调参入口，正好需要 |

在华为云场景下，每个 ELB 确实需要独立调参（不同 DB 流量不同，带宽需求不同）。方案 1 的“每个 DBC 都生成一个 LBC”不是污染，而是**特性**--每个 DB 一个配置面板。EKS/GKE 不需要是因为它们的 LB 参数少到几乎不用调。

**结论**：方案 1 的“总是建 LBC”不是对标 EKS/GKE 失败，而是对华为云 ELB 高可配能力的正确响应。EKS/GKE 的“少配置”是云平台能力决定的，不是设计取舍--方案 2 强行套用 EKS/GKE 的“零 LBC”模式，却丢了华为云 ELB 的可配性。用错了参照系。

**EKS/GKE vs 两方案 LBC 行为对比**：

EKS/GKE 能同时实现“默认无 LBC”+“事后可改”，是因为它的 CCM 原生支持从 Service 注解创建和更新 LB。CCE CCM 做不到，所以两个方案各牺牲了一半：

| | EKS/GKE | 方案 1 | 方案 2 |
|---|---|---|---|
| 默认创建 LBC？ | ❌ 不创建 | ✅ 每个 DBC 都建一个 | ❌ 不创建 |
| LBC 列表是否被污染？ | ❌ 干净 | ⚠️ 建几个DB就几个LBC（但这是特性，见上） | ❌ 干净 |
| 需要定制时可建LBC？ | ✅ 可选 | ✅（自动建的那个就能编辑） | ❌ 无 LBC 可建 |
| 定制可变？ | ✅ 改 LBC 注解即可 | ✅ 同左 | ❌ autocreate 不可变 |

方案 1 偏离了“默认无 LBC”（每 DBC 生成一个），但保留了“可变”。方案 2 对标了“默认无 LBC”，但丢了“可变”。在华为云 ELB 高可配场景下，可变比无 LBC 更重要。



### 3.8 方案 1 vs 方案 2 对比

| 维度 | 方案 1（watch DBC -> LBC+ELB） | 方案 2（watch Service -> autocreate） |
|---|---|---|
| **架构接近 EKS/GKE** | 中（控制器建 ELB，CCM 只绑定） | **高**（CCM 原生建 ELB，控制器只探测注入） |
| **Service 裸奔窗口** | 长（建LBC→建ELB→patchDBC→更新Service） | **短**（仅探测→注入注解） |
| **代码量** | +300~400 行 | **+200 行** |
| **patch 用户资源** | 是（patch DBC `loadBalancerConfigName`） | 否（patch Service 注解） |
| **LBC 资源** | 需要（出现在 UI 中，用户可编辑） | **不需要** |
| **状态可见性** | **完整**（LBC: ready/error/elb-status/public-ip） | 弱（Service events + status.ingress） |
| **ELB 精细控制** | **完整**（直接 ELB v3 API，全参数） | 受限（autocreate 14 字段） |
| **Region 覆盖** | **支持**（huawei-elb.io/region） | 不支持（autocreate 无 region；EKS/GKE 也不支持） |
| **ELB 创建后可变** | **支持**（改 LBC 注解 -> 重 reconcile -> 更新 ELB） | ❌ 不可变（带宽/EIP/AZ 不可改；华为云带宽计费场景下是真实痛点，见 §3.7） |
| **ELB 生命周期** | 控制器 + finalizer | **CCM 原生**（含删除，reclaim-policy 注解） |
| **删除安全** | 高（finalizer 保证 ELB 删除） | **高**（CCM 按 reclaim-policy 处理） |
| **平台适用性** | 仅 CCE | 仅 CCE |
| **多 DB 共享 ELB** | 不支持（每 DBC 独立 LBC） | 不支持（每 Service 独立 ELB） |
| **ELB Tags** | 支持（LBC 注解 → API） | 支持（独立 `elb.tags` 注解） |

**关键差异总结**：
- 方案 2 **架构更优雅**、代码更少、裸奔窗口更短、不 patch 用户 DBC
- 方案 1 **状态可见性更好**、ELB 控制更精细
- 方案 1 **精细控制可事后调整**（LBC UI 编辑注解），方案 2 autocreate 不可变，华为云带宽计费场景下失去唯一调参途径（见 §3.7）
- 两者 UX 都对标 EKS/GKE（创建 DBC 即获得 ELB，零额外操作）
- 平台适用性：两方案均仅支持 CCE（本设计不考虑 ECS 自建集群），无差异

### 3.9 当前方案 vs 自动方案对比

| 维度 | 当前方案（手动两步） | 方案 1（DBC 自动建 LBC） | 方案 2（Service 注入 autocreate） |
|---|---|---|---|
| **用户步骤** | 2 步（建 LBC -> 等 ready -> 建 DBC） | 1 步（建 DBC） | 1 步（建 DBC） |
| **用户等待** | 必须等 LBC ready 后才能建 DBC | 只需等 DBC ready | 只需等 DBC ready |
| **前提知识** | 理解 LBC 概念、知道要建 LBC | 无需了解 LBC | 无需了解 LBC |
| **VPC/子网/AZ** | 自动探测 | 自动探测（共享逻辑） | 自动探测（共享逻辑） |
| **ELB 生命周期** | 控制器 finalizer 管理 | 同左（复用） | CCM 原生管理 |
| **状态可见性** | LBC 注解（ready/error/public-ip） | 同左 | Service events + status.ingress |
| **删除流程** | 删 DBC -> 等 in-use-protection 移除 -> 删 ELB | 删 DBC -> Reconciler 等 in-use-protection -> 删 LBC -> 删 ELB | 删 DBC -> CCM 按 reclaim-policy 删 ELB |
| **异常回滚** | 删 LBC 即删 ELB | 删 DBC 时自动清理 LBC；patch 失败有补偿（R2） | CCM 处理，无孤儿风险 |
| **多 DB 共享 ELB** | 支持（多 DBC 引用同一 LBC） | 不支持；如需共享手动建 LBC | 不支持 |
| **ELB 配置精细度** | 完整（LBC 注解） | 完整（自动建 LBC 仍可手动加注解） | 受限（autocreate 14 字段） |
| **与当前方案兼容** | - | 完全兼容 | 完全兼容 |
| **平台适用性** | 仅 CCE | 仅 CCE | 仅 CCE |

```
当前：     LBC -> [等 ready] -> DBC（用户感知两步、等待一次）
           ↑ 用户操作        ↑ 用户操作

方案 1：   DBC -> LBC -> ELB -> patch DBC -> Service -> CCM 绑定
           ↑ 用户操作     └──── 控制器自动完成 ────┘

方案 2：   DBC -> Service -> 控制器注入 autocreate -> CCM 建ELB+绑定
           ↑ 用户操作     └──── 控制器+CCM自动完成 ────┘
```

---

## 4. 详细设计

### 4.0 基础概念：Reconcile 循环

> controller-runtime 框架的核心机制。每个 Reconciler 实现一个 `Reconcile(ctx, req)` 方法，框架在资源变化时自动调用。**不可重入、最终一致性、需幂等**——同一资源可能被多次 Reconcile，每次都必须得到相同结果。

```
Reconcile 三步循环:

① Observe  — Get 资源当前状态（从 API Server / informer cache）
② Diff     — 对比"当前状态"与"期望状态"，找出差异
③ Act      — 执行操作消除差异，完成后 return / requeue

每次 K8s 资源变化（创建、修改、删除），框架自动调一次 Reconcile。
控制器通过 return error 或 requeue 来等待下次触发。
```

**对本方案的影响**:
- DBC 创建后毫秒级触发 DBC Reconciler -> 判断触发条件 -> 建 LBC ->
  **return（不等 ELB ready）**
- LBC 创建后毫秒级触发 LBC Reconciler -> 探测 VPC/子网/AZ -> 调华为云 API 建 ELB ->
  写 elb.id → **return**
- LBC 的 elb.id 被写回后，再次触发 DBC Reconciler（因为 LBC 状态变了）→
  检测到 LBC ready → patch DBC 的 loadBalancerConfigName → **完成**

> 关键理解：**控制器不是"建 LBC → 等 ELB → patch DBC"这条同步线，而是多次 Reconcile 协作的异步过程。** 每次 Reconcile 只做一小步，然后 return。

### 4.1 核心步骤详解

#### patch DBC：连接两个 Reconciler 的关键关节

将 `spec.proxy.expose.loadBalancerConfigName` 从空字符串 `""` 改成 `"elb-<ns>-<dbc-name>"`：

```
patch 前：                              patch 后：
spec:                                   spec:
  proxy:                                  proxy:
    expose:                                 expose:
      type: LoadBalancer                      type: LoadBalancer
      loadBalancerConfigName: ""      →       loadBalancerConfigName: "elb-default-my-db"
```

没有这一步，DBC 的 `loadBalancerConfigName` 永远是空，OpenEverest 永远不会去读 LBC 的注解。LBC 上有 `elb.id` 也没用——Service 永远是空注解，CCM 永远不动。

**(F4) CEL 规则保证可行**：允许 `loadBalancerConfigName` 从 `""` 改成有值。

#### CCM 绑定触发机制

CCM 通过 K8s 原生 watch 机制检测，**没有显式调用**。CCM 内部有一个 controller，一直在 watch 集群里的 `type: LoadBalancer` Service：

```
CCM 的 Reconcile 循环

  ① watch 检测到 Service annotations 里出现 kubernetes.io/elb.id: "xxx"
  ② 调华为云 ELB API：查 ELB → 创建 listener + backend
  ③ 写 Service.status.loadBalancer.ingress = [{ip: "<VIP>"}]
  ④ EXTERNAL-IP 从 <pending> 变成 VIP
```

整条链路每一步都是事件驱动：

```
Service annotations 变 ──→ CCM 自动检测（watch 触发）──→ 调 API ──→ 写 status
       ↑
  OpenEverest update
```

#### 时序：数据库 ready vs ELB 绑定

数据库初始化和 ELB 创建是**并行**的，可能出现数据库先 ready 但外部 IP 还没到位的边界情况。

**正常（数据库慢、ELB 快）**：
```
创建 DBC ──── 数据库 Pod 初始化（~3min）───────────── DBC ready
    │                                                    │
    └─ 建 LBC → 建 ELB(15s) → patch → Service 更新      │
       → CCM 绑定(5s) ──────────────────────────────────┘
                      ↑ ELB 在数据库 ready 前就绑定好了 ✓
```

**边界（数据库快、ELB 慢）**：
```
创建 DBC ── 数据库 Pod 秒起（~1min）── DBC ready
    │                                    │
    └─ 建 LBC → 建 ELB(120s) → patch → Service → CCM 绑定
                                                ↑ 数据库 ready 但外部 IP 还在 pending
```

> **这不是 bug**——EKS/GKE 也一样，LB 创建和数据库初始化并行。`DBC.status.state == Ready` 代表**数据库引擎正常**，不是"外部可达"。外部可达要看 Service 的 `EXTERNAL-IP` 或 LBC 的 `ready` 注解。

#### 端到端完整流程

以上 7 步详细追踪从用户创建 DBC 到 CCM 绑定 ELB 的每一步：谁触发了谁、资源状态如何变化、注解如何传递。

完整流程分为 7 个步骤，每一步都是独立的 Reconcile 循环，靠 K8s watch 机制级联触发。用户只操作第一步，其余全自动。

**步骤 1 — 用户创建 DBC**

用户通过 kubectl 或 UI 创建 DBC，带 `huawei-elb.io/auto-elb: "true"` 注解，`loadBalancerConfigName` 留空。API Server 将 DBC 持久化到 etcd。此时 DBC 的 `loadBalancerConfigName` 为空，`auto-lbc-name` 注解尚未出现。

DBC 写入 etcd 后，所有 watch 该资源的 controller 被触发，包括 OpenEverest 的 DBC Reconciler 和我们的 DBC Reconciler——两者同时启动、各自独立运行。

**步骤 2a — OpenEverest 侧：建 engine CR 和 Service**

OpenEverest DBC Reconciler 检测到新 DBC，根据 engine 类型（PXC/PSMDB/PG）创建对应的 engine CR（与 DBC 同名、同 namespace）。Engine operator watch 到 engine CR 后创建 Service，类型为 LoadBalancer。此时 Service 的 annotations 为空——裸奔窗口开始。CCM 检查这个 Service：没有 `elb.id`，没有 `autocreate`，什么都不做，Service 的 EXTERNAL-IP 保持 `<pending>`。

> 这步与步骤 2b 同时发生，没有先后依赖。

**步骤 2b — 我们的 DBC Reconciler：检测触发条件**

我们的 DBC Reconciler 同步收到 DBC 创建事件。检查触发条件：`expose.type == LoadBalancer`（满足）、`loadBalancerConfigName == ""`（满足）、`auto-elb == "true"`（满足）——触发！调用共享的 NetworkDetector 自动探测 VPC/子网/AZ，然后创建 LBC CR（命名 `elb-<ns>-<dbc-name>`），spec.annotations 填入探测到的 VPC/子网/AZ 参数。接着给 DBC 加上 `auto-lbc-name` 注解（记录对应关系）和 `dbc-finalizer`（删除保护）。最后 return，不等 ELB 创建完成。

> 为什么不等？这是 controller-runtime 的设计原则：每次 Reconcile 只做一小步，做完就 return。等 LBC Reconciler 建好 ELB 后，LBC 状态变化会再次触发 DBC Reconciler。

**步骤 3 — LBC Reconciler：建 ELB**

LBC Reconciler 一直 watch LBC CR。检测到新 LBC 出现，且 `spec.annotations` 里没有 `elb.id`，触发创建流程。NetworkDetector 缓存命中（步骤 2b 已探测过），跳过重复探测。调用华为云 ELB v3 API 创建 ELB，轮询等待 STATUS=ACTIVE（最长 120 秒）。ELB 就绪后，将 `elb.id` 写入 LBC 的 `spec.annotations`，同时在 `metadata.annotations` 写入 `ready=true`、`elb-status=ACTIVE`、`public-ip` 等状态注解。return。

> LBC 的 spec.annotations 变化 → 触发所有 watch LBC 的 controller。

**步骤 4 — DBC Reconciler 再次触发：patch DBC**

因为步骤 3 修改了 LBC（写入了 elb.id），DBC Reconciler 收到 LBC 变化事件，再次 Reconcile。检测到 DBC 有 `auto-lbc-name` 注解 → 说明之前已触发过自动建 LBC。检查对应 LBC：存在且 `ready=true`。DBC 的 `loadBalancerConfigName` 仍为空 → 执行 patch：将 `loadBalancerConfigName` 从 `""` 改为 `"elb-<ns>-<dbc-name>"`。F4（CEL 规则）允许这个操作。

这是连接两个 Reconciler 的关键一步。没有这个 patch，DBC 永远不知道自己关联了哪个 LBC，OpenEverest 永远不会去读 LBC 的注解。

> DBC 的 spec 变化 → 触发 OpenEverest DBC Reconciler。

**步骤 5 — OpenEverest 再次触发：更新 Service 注解**

OpenEverest DBC Reconciler 检测到 DBC 的 `loadBalancerConfigName` 从空变成了有值。调用 `GetAnnotations()` 从对应的 LBC 读取 `spec.annotations`，提取 OpenEverest 认识的 key（包括 `kubernetes.io/elb.id`）。将这些注解写入 engine CR 的 `ServiceExpose.Annotations`。Engine operator watch 到 engine CR 变化，更新 Service：

```
之前: annotations = {}
现在: annotations = {kubernetes.io/elb.id: "xxx", kubernetes.io/elb.class: "union"}
```

裸奔窗口结束——Service 终于有了 ELB 绑定所需的一切。

> Service annotations 变化 → 触发 CCM。

**步骤 6 — CCM 绑定 ELB**

CCE CCM 内部的 Service controller 一直 watch LoadBalancer Service。检测到目标 Service 的 annotations 里出现了 `kubernetes.io/elb.id`。根据 elb.id 调用华为云 ELB API 查询 ELB 详情。在 ELB 上创建 listener（基于 Service 的 Ports）和 backend members（基于 Endpoints）。将 VIP 写入 Service 的 `status.loadBalancer.ingress`。

```
kubectl get svc → EXTERNAL-IP 从 <pending> 变为 1.2.3.4
```

**步骤 7 — 完成**

用户得到可连接的外部 IP，完全在后台自动完成。删除时 DBC Reconciler 的 finalizer 按 §4.5 的流程清理 LBC 和 ELB。

---

**关键理解**：用户只操作了步骤 1（创建 DBC），步骤 2-6 全部自动完成。这不是一条同步调用链——每一步都是独立的 Reconcile 循环，靠 K8s watch + 事件驱动级联触发。DBC Reconciler 说"我建了 LBC，剩下的交给 LBC Reconciler"，LBC Reconciler 说"我建了 ELB，elb.id 写回去了"，DBC Reconciler 说"LBC ready 了，我来 patch DBC"，OpenEverest 说"DBC 变了，我来更新 Service"，CCM 说"Service 有 elb.id 了，我来绑定"——每个参与者只关心自己的事，不调用别人。

### 4.2 架构

```
┌─────────────────────────────────────────────────────────┐
│  huawei-elb-controller (单个 Deployment，两个 Reconciler) │
│                                                         │
│  ┌───────────────────────┐  ┌────────────────────────┐ │
│  │ LoadBalancerConfig    │  │ DatabaseCluster        │ │
│  │ Reconciler (现有不变)  │  │ Reconciler (新增)       │ │
│  │                       │  │                        │ │
│  │ watch LBC CR          │  │ watch DBC CR           │ │
│  │ 探测 VPC/子网/AZ       │  │ 检测 auto-elb 触发条件  │ │
│  │ 创建/删除 ELB          │  │ 创建 LBC + patch DBC    │ │
│  │ 写 elb.id 到 LBC       │  │ DBC 删除时清理 LBC      │ │
│  └──────────┬────────────┘  └───────────┬────────────┘ │
│             │                           │              │
│             └───────── 共享 ────────────┘              │
│                  huaweicloud.ELBClient                 │
│                  huaweicloud.Credentials               │
│                  autoDetectedParams cache              │
└─────────────────────────────────────────────────────────┘
```

两个 Reconciler 在同一个 manager 中运行（同一个 Pod），共享 ELB client 和凭证。

### 4.3 DBC Reconciler 流程

```
Reconcile(ctx, req)
  │
  ├─ Get DBC by name
  │   └─ NotFound -> return (清理逻辑由 finalizer 处理)
  │
  ├─ 检查是否在删除中 (DeletionTimestamp != 0)
  │   ├─ 有 huawei-elb.io/dbc-finalizer -> reconcileDeleteDBC
  │   │   ├─ 检查 LBC (elb-<ns>-<name>) 是否还被任何 DBC 引用
  │   │   ├─ 无引用 -> 删 LBC (LBC finalizer 删 ELB)
  │   │   └─ 移除 dbc-finalizer
  │   └─ 无 finalizer -> return
  │
  ├─ 检查触发条件
  │   ├─ expose.type != LoadBalancer -> return
  │   ├─ loadBalancerConfigName != "" -> return (走现有流程)
  │   ├─ auto-elb != "true" -> return
  │   ├─ hasForeignCloudAnnotations -> return
  │   └─ age < gracePeriod -> requeue
  │
  ├─ 检查是否已自动建过 LBC
  │   ├─ DBC 有注解 huawei-elb.io/auto-lbc-name -> 已建过
  │   │   ├─ LBC 存在 -> 检查 LBC ready
  │   │   │   ├─ ready=true 且 DBC.loadBalancerConfigName 已设 -> 完成，长轮询
  │   │   │   ├─ ready=true 且 DBC.loadBalancerConfigName 未设 -> patch DBC
  │   │   │   └─ ready=false
  │   │   │       ├─ elb-status == PENDING_CREATE -> requeue (正常等待)
  │   │   │       └─ elb-status 含错误 -> mirror 错误信息到 DBC 的 huawei-elb.io/error 注解，requeue（让用户直接在 DBC 上看到故障原因）
  │   │   └─ LBC 不存在 -> 异常，重建（记录错误）
  │   └─ 无 auto-lbc-name 注解 -> 首次处理
  │       ├─ 自动探测 VPC/子网/AZ (复用 autoDetectParams)
  │       ├─ 创建 LBC (elb-<ns>-<dbc-name>)
  │       │   spec.annotations: {huawei-elb.io/vpc-id, subnet-id, azs, public}
  │       ├─ 给 DBC 加 huawei-elb.io/auto-lbc-name 注解 + dbc-finalizer
  │       └─ requeue (等 LBC controller 建好 ELB)
  │
  └─ patch DBC.loadBalancerConfigName = auto-lbc-name
     (CEL 规则允许从 "" 设为有值 ✓ F4)
```

### 4.4 LBC 命名规则

```
elb-<namespace>-<dbc-name>
```

- LBC 是 cluster-scoped (F6)，需跨命名空间唯一
- `<namespace>` 避免 `db1` 在两个命名空间撞名
- ELB 名 = `elb-<namespace>-<dbc-name>`（ELBNamePrefix `elb-` + LBC 名）
- 华为云 ELB 名长度限制 64 字符；`<namespace>-<dbc-name>` 需约束长度

**长度保护**：如果 `len(namespace) + len(dbc-name) > 60`，截断 dbc-name 并加 SHA256 前 8 字符 hex 后缀。碰撞概率 `1/2^32`，冲突时直接报错不重试。

### 4.5 DBC 删除链路

```
用户删 DBC
  ↓
DBC Reconciler 检测到 DeletionTimestamp
  ↓
检查 LBC (elb-<ns>-<name>) 是否还被其他 DBC 引用——通过 `List` 全集群 DBC，检查是否有其他 DBC 的 `loadBalancerConfigName` 或 `auto-lbc-name` 注解指向此 LBC。考虑到每个 auto-elb DBC 都有独立 LBC，此场景罕见，仅在删除时执行一次，无需高频调用。
  ├─ 被引用 -> 直接移除 dbc-finalizer（LBC 保留）
  └─ 无引用 -> 删 LBC
      ↓
      LBC Reconciler 的 finalizer 删 ELB
      ↓
      LBC 被删
      ↓
      DBC Reconciler 移除 dbc-finalizer
      ↓
      DBC 被删
```

**关键**：DBC 删除时，`loadBalancerConfigName` 字段随 DBC 一起被删除，无需也无法单独清空（CEL 禁止）。OpenEverest 需通过清理 engine CR 解除 `in-use-protection`（F11 待验证）。

**解决**：DBC 删除时，DBC Reconciler 轮询 LBC 的 finalizers，确认 `in-use-protection` 已移除后再删 LBC（轮询间隔 5s，超时 10 分钟；超时后在 DBC 的 `huawei-elb.io/error` 注解写错误信息，控制器继续重试）。

> ⚠️ **F11 待验证**："OpenEverest 会先清理 engine CR → `in-use-protection` 随之解除" 是推断，未在源码中确认。若实际行为是先删 DBC 再清理 engine CR（或并发），则 `in-use-protection` 可能在控制器尝试删 LBC 时仍存在。兜底：超时后打印告警日志，人工介入。
### 4.6 新增常量与注解

```go
const (
    // DBC 注解：opt-in 自动建 ELB
    autoELBAnnotation = "huawei-elb.io/auto-elb"

    // DBC 注解：记录自动创建的 LBC 名（用于幂等性 + 删除清理）
    autoLBCNameAnnotation = "huawei-elb.io/auto-lbc-name"

    // DBC finalizer
    dbcFinalizerName = "huawei-elb.io/dbc-finalizer"

    // DBC GVK
    dbcGVK = schema.GroupVersionKind{
        Group:   "everest.percona.com",
        Version: "v1alpha1",
        Kind:    "DatabaseCluster",
    }
)
```

### 4.7 RBAC 新增

DBC Reconciler 需要额外权限：

```yaml
# ClusterRole 新增
- apiGroups: ["everest.percona.com"]
  resources: ["databaseclusters"]
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: ["everest.percona.com"]
  resources: ["loadbalancerconfigs"]
  verbs: ["get", "list", "watch", "create", "delete"]
```

### 4.8 共享探测缓存

两个 Reconciler 共享 `autoDetectedParams` 缓存。DBC Reconciler 调用 LBC Reconciler 的 `autoDetectParams` 方法（需提取为公共方法或共享 detector 结构）。

```go
// 重构：提取共享的 NetworkDetector
type NetworkDetector struct {
    Creds    *huaweicloud.Credentials
    detectMu sync.Mutex
    detected *autoDetectedParams
}

func (d *NetworkDetector) Detect(ctx, logger, client) (vpcID, subnetID, azs, error)
```

LBC Reconciler 和 DBC Reconciler 都持有 `*NetworkDetector` 引用。

**缓存策略**：探测结果缓存 10 分钟（`detected` + `detectedAt`），过期后重新探测。两个 Reconciler 创建时 NetworkDetector 初始为空，首次调用自动触发探测。不缓存主要是避免跨 Reconciler 共享状态的复杂性——探测本身只调用一次 ECS API，开销很小。

---

## 5. 风险点与缓解

| # | 风险 | 影响 | 概率 | 缓解措施 |
|---|---|---|---|---|
| R1 | **Service 裸奔窗口：CCM 对无注解 LoadBalancer Service autocreate 孤儿 ELB** | 孤儿 ELB 持续计费 | **已解除**（Q1 确认 CCE CCM 不 autocreate） | ✅ CCE 场景安全；本方案仅支持 CCE 集群 |
| R2 | **patch DBC 失败导致孤儿 LBC+ELB** | LBC+ELB 存在但无人引用，持续计费 | 中 | ① patch 失败时立即删 LBC（最终一致性）；② 补偿：独立 goroutine 每 5 分钟全集群扫描——有 `auto-lbc-name` 但 DBC 不存在则删 LBC |
| R3 | **DBC 删除时 `in-use-protection` finalizer 阻止 LBC 删除** | DBC 卡在删除中 | 高 | DBC Reconciler 轮询 LBC finalizers（5s 间隔），等 `in-use-protection` 移除后再删 LBC；超时 10 分钟后在 DBC 的 `huawei-elb.io/error` 写错误信息，继续重试 |
| R4 | **LBC 命名冲突（cluster-scoped）** | 创建 LBC 失败 | 低 | 命名加 namespace 前缀 + 长度截断 + SHA256 前 8 位 hex hash；冲突时直接报错不重试 |
| R5 | **OpenEverest UI 不支持给 DBC 加 `auto-elb` 注解** | 用户无法通过 UI 触发自动建 | 高 | 提供 kubectl annotate 命令；或 Helm post-install hook；或文档说明 |
| R6 | **engine operator 重建 Service 覆盖注解** | elb.id 丢失，ELB 解绑 | 低 | 本方案不直接改 Service（走 LBC -> engine CR -> Service 路径），engine operator 重建时会重新 GetAnnotations，不受影响 |
| R7 | **两个 Reconciler 并发写 LBC** | resourceVersion 冲突 | 中 | LBC 写操作已有 `updateWithRetry`（RetryOnConflict），DBC Reconciler 创建 LBC 后不直接改，交给 LBC Reconciler |
| R8 | **用户手动删了自动建的 LBC** | DBC 引用的 LBC 消失 | 低 | DBC Reconciler 检测到 LBC 不存在且 DBC 有 `auto-lbc-name` 注解 -> 重建 LBC |
| R9 | **跨 region DBC（huawei-elb.io/region 注解）** | ELB 建在错误 region | 低 | DBC Reconciler 读取 DBC 的 region 注解，传给 LBC 的 spec.annotations |

---

## 6. 实现计划

### Phase 0：确认关键问题

- [x] **Q1**：CCE CCM 对无注解 LoadBalancer Service 的行为--**已确认**：CCE 内置 CCM 不 autocreate（Service 停 pending），开源 CCM 会 autocreate。本方案对 CCE 场景可行。
- [ ] **Q2**：确认 OpenEverest UI 能否留空 loadBalancerConfigName 提交

### Phase 1：实现

- [ ] 重构 `autoDetectParams` 为共享 `NetworkDetector`
- [ ] 实现 `DatabaseClusterReconciler`（触发、创建 LBC、patch DBC）
- [ ] 实现 DBC 删除链路（等 in-use-protection 移除、删 LBC）
- [ ] 新增 RBAC（DBC + LBC create/delete 权限）
- [ ] 新增常量与注解
- [ ] 更新 Helm chart（新 RBAC）
- [ ] 更新 deploy/ manifests
- [ ] 单元测试
- [ ] 集成测试（kind + mock 华为云 API）
- [ ] 文档更新（README + 配置参考）
- [ ] 实现孤儿 LBC 补偿清理（独立 goroutine，每 5 分钟全集群扫描）
> 注：以上实现计划对应方案 1。方案 2（Service Reconciler）的实现计划待方案确认后补充。

---

## 7. 与当前方案的兼容性

| 场景 | 当前方案 | 本方案上线后 |
|---|---|---|
| 用户手动建 LBC + DBC 引用 | 正常工作 | 不受影响（loadBalancerConfigName 已设值，不触发自动建） |
| 用户建 DBC 不填 loadBalancerConfigName | Service 空注解，无 ELB | 若 DBC 有 `auto-elb: true` 注解 -> 自动建 LBC+ELB；否则不变 |
| 用户用预创建 ELB（spec.annotations 设 elb.id） | 正常工作 | 不受影响 |
| 用户用 CCM autocreate（kubernetes.io/elb.autocreate） | 控制器跳过 | 不受影响 |
| 多数据库共享同一 LBC | 支持 | 仍支持（手动指定 loadBalancerConfigName） |

**结论**：本方案是纯增量 opt-in 功能，不破坏任何现有行为。

---

## 8. 决策记录

| 决策 | 选择 | 理由 |
|---|---|---|
| 方案选择 | 待定（方案 1 vs 方案 2，见 §3.8/§3.9） | 待用户确认后定稿 |
| 触发方式 | opt-in 注解 `huawei-elb.io/auto-elb: "true"` | 不影响存量，用户显式声明 |
| LBC 命名 | `elb-<ns>-<dbc-name>` | cluster-scoped 唯一性 |
| 是否 patch DBC | 是（设 loadBalancerConfigName） | F4 允许从空设值，F5 保证注解传播 |
| 是否保留 LBC | 是 | 复用现有 Reconciler + 状态可见性 |
| 共享探测缓存 | 是（NetworkDetector） | 避免重复 API 调用 |

---

## 9. 待确认问题（状态同步自 §2.1-2.3）

- [x] **Q1**：CCE CCM 对无注解 LoadBalancer Service 的行为（✅ CCE 不 autocreate）
- [ ] **Q2**：OpenEverest UI 创建 DBC 时 loadBalancerConfigName 能否留空？
- [ ] **Q4**：华为云 ELB 名长度限制（64 字符？）
- [ ] **Q5**：OpenEverest engine operator 创建的 Service 命名规则
- [ ] **F11**：DBC 删除时 OpenEverest 的 `in-use-protection` 解除时序
- [ ] **Q6**：是否移除 `auto-elb` opt-in 注解以实现完全 EKS/GKE 对等？权衡：移除后存量空值 DBC 会触发自动建 ELB（破坏性），需评估影响面
- [ ] **Q7**：CCE CCM 对 `spec.loadBalancerSourceRanges` 的实际行为（告警还是阻塞？）。已知华为云访问控制用 `elb.acl-*` 注解而非 `loadBalancerSourceRanges`，OpenEverest 设置后者会触发 `no access-controll (source ranges enabled)` 错误。需实测确认错误级别及 `elb.acl-*` 注解能否正确生效。
---

## 附录 A：研究证据来源

所有 OpenEverest 行为事实（F1-F10）来自 `openeverest/openeverest-operator` 源码研究，commit `a4519bd1e331731cfeb71ff414b29d6d5c6d31e3`。关键文件：

- `api/everest/v1alpha1/databasecluster_types.go` - DBC 类型与 CEL 校验
- `api/everest/v1alpha1/loadbalancerconfig_types.go` - LBC 类型
- `internal/webhook/everest/v1alpha1/databasecluster_webhook.go` - 校验 webhook
- `internal/controller/everest/common/helper.go` - GetAnnotations / GetLoadBalancerConfig
- `internal/controller/everest/databasecluster_controller.go` - DBC controller（含 LBC watch）
- `internal/controller/everest/loadbalancerconfig_controller.go` - LBC controller（inUse + finalizer）
- `internal/controller/everest/providers/{pxc,pg,psmdb}/applier.go` - Service 创建逻辑
- `test/integration/features/lbc_custom_pg/` - 集成测试（证明注解重新同步）

## 附录 B：AWS EKS 注解传播路径源码分析

> 目的：从 OpenEverest 源码确认注解传播是平台无关的纯透传，AWS 和 CCE 走同一条代码路径。差异 100% 在 OpenEverest 之下（CCM 层）。

### B.1 结论

OpenEverest 的注解传播路径**没有任何云平台分支逻辑**。`GetAnnotations()` 读 LBC 的 `spec.annotations`（一个 `map[string]string`），可选地展开 Go 模板，然后原样返回。三个引擎 applier（PXC/PG/PSMDB）将返回值直接赋给 engine CR 的 `ServiceExpose.Annotations`。上游 Percona engine operator 再把注解 stamp 到 Service 上。

AWS EKS 和 CCE 在 OpenEverest 层**完全相同**，唯一区别是用户在 LBC 里放什么注解。

### B.2 LBC 类型定义--纯注解 map

```go
// api/everest/v1alpha1/loadbalancerconfig_types.go L23-L33
type LoadBalancerConfigSpec struct {
    Annotations map[string]string `json:"annotations,omitempty"`
}
```

没有云平台字段，没有 region，没有类型区分。`LoadBalancerConfig` 是 cluster-scoped CRD，只存这一个 map。

### B.3 DBC 如何引用 LBC

```go
// api/everest/v1alpha1/databasecluster_types.go L322-L334
type Expose struct {
    Type                   ExposeType `json:"type,omitempty"`
    LoadBalancerConfigName string     `json:"loadBalancerConfigName,omitempty"`
}
```

DBC 通过 `Spec.Proxy.Expose.LoadBalancerConfigName` **按名字引用** LBC，没有云平台提示。

### B.4 GetAnnotations()--整个传播逻辑

```go
// internal/controller/everest/common/helper.go L699-L729
func GetAnnotations(ctx, c, database) (map[string]string, error) {
    lbc, err := GetLoadBalancerConfig(ctx, c, database)
    if err != nil {
        if errors.Is(err, ErrEmptyLbc) {
            return map[string]string{}, nil  // 空名 -> 返回空 map
        }
        return nil, err
    }

    annotations := lbc.Spec.Annotations  // 直接读 map

    for key, value := range annotations {
        if strings.Contains(value, "{{") && strings.Contains(value, "}}") {
            updatedVal, err := SetTemplateValues(value, database)
            if err != nil {
                return map[string]string{}, err
            }
            annotations[key] = updatedVal
        }
    }

    return annotations, nil  // 原样返回
}
```

**没有 key 过滤，没有云平台判断，没有白名单校验。** `service.beta.kubernetes.io/aws-load-balancer-type` 和 `kubernetes.io/elb.class` 被一视同仁--都是 `map[string]string` 条目。唯一的转换是 Go 模板展开（如 `{{ .ObjectMeta.Name }}`），本身也是平台无关的。

### B.5 三个引擎 applier--同一模式

PXC、PG、PSMDB 三个 provider 的 `ExposeTypeLoadBalancer` 分支都是同一套代码：

```go
// PXC applier.go L613-L624（PG L222、PSMDB L295 同理）
case everestv1alpha1.ExposeTypeLoadBalancer:
    annotations, err := common.GetAnnotations(p.ctx, p.C, p.DB)
    if err != nil {
        return err
    }
    expose = pxcv1.ServiceExpose{
        Enabled:     true,
        Type:        corev1.ServiceTypeLoadBalancer,
        Annotations: annotations,  // 直接赋值，无过滤
    }
```

对 `ClusterIP`/`NodePort` 类型，annotations 设为空 `map[string]string{}`--LBC 注解**只在 `Type == LoadBalancer` 时**生效。

### B.6 唯一的两处 AWS 相关代码--都与注解传播无关

**`GetClusterType`（helper.go L127-L142）**：通过 StorageClass 的 provisioner 检测 EKS，但结果存了不读：

| Provider | 赋值位置 | 读取 `p.clusterType` |
|---|---|---|
| PXC | provider.go:107 | **无** |
| PG | provider.go:87 | **无** |
| PSMDB | provider.go:102 | **无** |

全代码库 grep `p.clusterType` 只返回结构体声明和单次赋值，零读取。对 LB 路径而言是死代码。

**`defaultEKSLoadBalancerConfigName = "eks-default"`（migrator.go L43）**：定义了但全代码库**零引用**。`eks-default` LBC 是预置的 CR 数据（YAML），不是代码按云类型创建的。用户同样可以创建名为 `cce-default` 的 LBC 放华为云注解，operator 一视同仁。

**PSMDB `engine_features_applier.go` L290-L297**：读取 `svc.Status.LoadBalancer.Ingress` 的 `.IP` 和 `.Hostname`（AWS NLB 返回 DNS hostname 而非 IP），这是**读回**方向，用于 split-horizon DNS，不影响**设置**注解的逻辑。

### B.7 AWS EKS 完整流程（源码确认）

```
用户创建 LBC "my-aws-lbc"：
  spec.annotations:
    service.beta.kubernetes.io/aws-load-balancer-type: "nlb"
    service.beta.kubernetes.io/scheme: "internet-facing"
     │
     ▼
用户创建 DBC，loadBalancerConfigName: "my-aws-lbc"
     │
     ▼  DBC Reconciler -> engine applier
common.GetAnnotations()              [helper.go:699]
  └─ GetLoadBalancerConfig()         [helper.go:660]
  └─ annotations = lbc.Spec.Annotations
  └─ (可选 Go 模板展开)
  └─ return annotations               ← 纯 map，不过滤
     │
     ▼
engine CR.Spec.ServiceExpose.Annotations = annotations  [applier.go:618]
     │
     ▼  上游 Percona engine operator（OpenEverest 仓库之外）
Service.metadata.annotations = ServiceExpose.Annotations
     │
     ▼  AWS CCM（Kubernetes 上游，非 OpenEverest）
AWS CCM 读 service.beta.kubernetes.io/aws-load-balancer-* 注解
  -> 调 AWS API 建 NLB
  -> 写 status.loadBalancer.ingress.hostname
```

### B.8 AWS vs CCE 对比

| 步骤 | AWS EKS | CCE |
|---|---|---|
| LBC.spec.annotations | AWS 注解（配置指令） | `elb.id`（已建好的 ELB ID） |
| `GetAnnotations()` | **相同代码** | **相同代码** |
| engine CR 注解赋值 | **相同代码** | **相同代码** |
| Service annotations | **相同传播** | **相同传播** |
| 谁建 LB | AWS CCM（读注解 -> 调 AWS API） | huawei-elb-controller（调华为云 API -> 写 ID 回 LBC） |

**核心结论**：OpenEverest 层完全平台无关。差异 100% 在 OpenEverest 之下--AWS CCM 自己能建 LB，CCE 需要外部控制器先建好再填 ID。这也是为什么这个控制器只存在于华为云场景，AWS 上不需要。

## 附录 C：`elb.autocreate` JSON schema 完整参考

> 来源：华为云 CCE 官方文档 [cce_10_0385](https://support.huaweicloud.com/usermanual-cce/cce_10_0385.html) Table 25（更新于 2026-06-18）。`elb.autocreate` 是 CCE CCM 专有注解，开源 CCM 不支持。

**全部 14 个字段**：

| # | 字段 | 类型 | 必填 | 说明 | 默认值 |
|---|---|---|---|---|---|
| 1 | `name` | String | 否 | ELB 名称，1-64 字符 | `cce-lb-<service.UID>` |
| 2 | `type` | String | 否 | `public`（公网）/ `inner`（内网） | `inner` |
| 3 | `bandwidth_name` | String | 公网必填 | 带宽名称 | - |
| 4 | `bandwidth_chargemode` | String | 公网必填 | `bandwidth` / `traffic` | - |
| 5 | `bandwidth_size` | Integer | 公网必填 | 1-2000 Mbit/s | - |
| 6 | `bandwidth_sharetype` | String | 公网必填 | `PER`（独享带宽） | - |
| 7 | `eip_type` | String | 公网必填 | `5_bgp` / `5_sbgp` / `5_telcom` / `5_union` | - |
| 8 | `vip_subnet_cidr_id` | String | 否 | IPv4 子网 ID | 集群同子网 |
| 9 | `ipv6_vip_virsubnet_id` | String | 否 | IPv6 子网 ID（独享 ELB 专有） | - |
| 10 | `elb_virsubnet_ids` | []String | 否 | 后端子网 ID 列表（独享 ELB 专有） | 同 `vip_subnet_cidr_id` |
| 11 | `vip_address` | String | 否 | ELB 私网 IP | 自动分配 |
| 12 | `available_zone` | []String | 独享必填 | 可用区（独享 ELB 专有） | - |
| 13 | `l4_flavor_name` | String | 否 | L4 flavor 名称（独享 ELB 专有） | - |
| 14 | `l7_flavor_name` | String | 否 | L7 flavor 名称（独享 ELB 专有） | - |

**不在 autocreate JSON 内的独立注解**：

| 注解 | 说明 |
|---|---|
| `kubernetes.io/elb.class` | `union`（共享）/ `performance`（独享），须与 autocreate 同时设置 |
| `kubernetes.io/elb.tags` | ELB 标签，格式 `key1=value1,key2=value2`（v1.23.11-r0+） |
| `kubernetes.io/elb.enterpriseID` | 企业项目 ID |
| `kubernetes.io/elb.instance-reclaim-policy` | 回收策略：`retain` / `alwaysDelete`（v1.28.15-r60+） |

**与当前控制器功能映射**：

| 控制器功能 | autocreate 对应 | 状态 |
|---|---|---|
| ELB 名称 | `name` | ✅ |
| 子网 ID | `vip_subnet_cidr_id` | ✅ |
| 可用区 | `available_zone` | ✅（独享 ELB） |
| 公网/内网 | `type` | ✅ |
| 带宽大小 | `bandwidth_size` | ✅ |
| 带宽计费模式 | `bandwidth_chargemode` | ✅ |
| EIP 类型 | `eip_type` | ✅ |
| ELB 标签 | `elb.tags`（独立注解） | ⚠️ 需两个注解 |
| Region 覆盖 | 无 | ❌ |
| 创建后可变 | 不可变 | ❌（华为云带宽计费场景下是真实痛点，见 §3.7） |

**关键限制**：
1. 无 Region 覆盖（EKS/GKE 也不支持跨 Region，不是 parity gap）
2. 创建后不可变（华为云带宽计费场景下是真实痛点，详见 §3.7）
3. 仅 CCE 可用（开源 CCM 不支持 autocreate）
4. 5/14 字段为独享 ELB 专有
5. Tags 需独立注解，不在 JSON 内

---

## 附录 D：`spec.loadBalancerSourceRanges` 与华为云访问控制机制冲突

> 目的：解释 §9 Q7 中 `no access-controll (source ranges enabled)` 错误的背景。

### D.1 `spec.loadBalancerSourceRanges` 是什么

Kubernetes `Service` 的一个**标准字段**，用来限制**哪些源 IP 可以访问这个 LoadBalancer Service**。

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-db
spec:
  type: LoadBalancer
  loadBalancerSourceRanges:   # ← 这个字段
    - 192.168.1.0/24          # 只允许公司办公网访问
    - 10.0.0.0/8              # 只允许 VPC 内网访问
  ports:
    - port: 5432
```

效果：**只有来自 `192.168.1.0/24` 和 `10.0.0.0/8` 的流量能到达数据库**，其他 IP 在负载均衡器层面被拒绝。

`loadBalancerSourceRanges` 为空 = **0.0.0.0/0** = 全公网可访问（数据库直接暴露在公网，有安全风险）。

### D.2 各云实现差异

| 云 | 实现方式 | 认 `loadBalancerSourceRanges`？ |
|---|---|---|
| **AWS** | CCM 把 CIDR 转成 ELB 的 listener 规则 / 安全组 | ✅ |
| **GCP** | CCM 把 CIDR 转成 VPC 防火墙规则 | ✅ |
| **华为云 CCE** | CCM **不认这个字段**，改用 `elb.acl-*` 注解 + 预创建的 IP 地址组 | ❌ |

### D.3 OpenEverest 如何使用

OpenEverest UI 有「Source range」输入框，用户填了 CIDR 后，OpenEverest 把它写进 `spec.loadBalancerSourceRanges`。

OpenEverest 源码确认（commit `a4519bd`，三个数据库 provider 都有）：

```go
// internal/controller/everest/providers/pg/applier.go L222-L226
case everestv1alpha1.ExposeTypeLoadBalancer:
    pg.Spec.Proxy.PGBouncer.ServiceExpose = &pgv2.ServiceExpose{
        Type:                     string(corev1.ServiceTypeLoadBalancer),
        LoadBalancerSourceRanges: p.DB.Spec.Proxy.Expose.IPSourceRangesStringArray(),
    }
```

CRD 字段定义：`spec.proxy.expose.ipSourceRanges`（`[]IPSourceRange`）

OpenEverest 是云无关的，它只用 K8s 通用标准字段。在 AWS/GCP 上自动生效；在华为云 CCE 上 CCM 不认，就报错。

### D.4 华为云的正确做法：`elb.acl-*` 注解

华为云 ELB 的访问控制（access control）是 listener 级别的 IP 白/黑名单，必须用华为专有注解：

| 注解 | 作用 |
|---|---|
| `kubernetes.io/elb.acl-id` | ELB 控制台**预先创建的 IP 地址组**的 ID（不能写裸 CIDR） |
| `kubernetes.io/elb.acl-status` | `on` 启用 / `off` 关闭 |
| `kubernetes.io/elb.acl-type` | `white`（白名单/trustlist）/ `black`（黑名单/blocklist） |

**关键差异**：`loadBalancerSourceRanges` 直接写 CIDR 即可；`elb.acl-*` 必须先在 ELB 控制台创建一个「IP 地址组」对象（把 CIDR 填进去），再引用它的 ID。不能在 Service 注解里直接写裸 CIDR。

### D.5 冲突发生过程

```
① 用户在 OpenEverest UI 填 Source range（CIDR）
② OpenEverest 把 CIDR 写进 Service.spec.loadBalancerSourceRanges
③ CCE CCM 检测到 loadBalancerSourceRanges 非空 -> "(source ranges enabled)"
④ CCE CCM 发现 Service 没有 elb.acl-* 注解 -> "no access-controll"
⑤ 报错：no access-controll (source ranges enabled)
```

### D.6 对本方案的影响

- **当前控制器**：完全没处理 source ranges 和 access control，是一个已知的 parity gap
- **方案 1**：用户在 LBC 注解里配 `elb.acl-*` 可以生效（LBC 注解会传播到 Service），但仍需 OpenEverest 侧清空 Source range 避免 `loadBalancerSourceRanges` 触发报错
- **方案 2**：同理，需在注入 autocreate 时同时注入 `elb.acl-*` 注解
- **长期方案**：控制器可自动调 ELB API 创建 IP 地址组，把用户在 LBC 里写的 CIDR 转成 IP 地址组 ID 并注入 `elb.acl-id`，实现与 AWS/GCP 对等的体验

### D.7 证据来源

- OpenEverest 源码：`openeverest/openeverest-operator` SHA `a4519bd`，`internal/controller/everest/providers/{pg,pxc,psmdb}/applier.go`
- OpenEverest 文档：[GKE 指南博客](https://openeverest.io/blog/running-openeverest-on-gke/) 确认 Source range -> loadBalancerSourceRanges 映射
- 华为云 CCE 文档：[cce_10_0831](https://support.huaweicloud.com/usermanual-cce/cce_10_0831.html)（访问控制）、[cce_10_0385](https://support.huaweicloud.com/usermanual-cce/cce_10_0385.html)（注解全集，无 loadBalancerSourceRanges）
- 华为云 ELB 文档：[elb_03_0003](https://support.huaweicloud.com/usermanual-elb/elb_03_0003.html)（listener 级访问控制）
- 开源 CCM：`kubernetes-sigs/cloud-provider-huaweicloud` SHA `e4d2b145` 无 `elb.acl` 逻辑（确认该错误来自闭源 CCE CCM）
