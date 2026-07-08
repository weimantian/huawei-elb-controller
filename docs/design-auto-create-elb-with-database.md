# 设计：创建数据库时自动创建 ELB（一步到位方案）

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

**Q1 结论：CCE 内置 CCM 与开源 CCM 行为不同，方案 1 对 CCE 场景安全。**

| CCM 类型 | 无 `elb.id` 无 `autocreate` 的 LoadBalancer Service 行为 | 来源 |
|---|---|---|
| **CCE 内置 CCM**（CCE 集群预装，主要场景） | **不创建 ELB，Service 停在 `<pending>`** | [cce_10_0385](https://support.huaweicloud.com/usermanual-cce/cce_10_0385.html)、[cce_10_0681](https://support.huaweicloud.com/usermanual-cce/cce_10_0681.html) |
| **开源 CCM**（kubernetes-sigs/cloud-provider-huaweicloud，ECS 自建集群） | **自动创建 ELB**（`elb.id` 为空即触发） | [usage-guide.md](https://github.com/kubernetes-sigs/cloud-provider-huaweicloud/blob/master/docs/usage-guide.md) |

证据链：

1. CCE 文档只定义两种互斥场景："使用已有ELB"（必填 `elb.id`）与"自动创建ELB"（必填 `elb.autocreate`），无第三种"都不填"场景。
2. CCE 注解全集页明确：`elb.autocreate` 为"仅自动创建ELB的场景：必填"，`elb.id` 为"仅关联已有ELB的场景需填写"，两者"不能同时填写"。
3. 开源 CCM 文档则相反：`elb.id` "If empty, a new ELB service will be created automatically"。
4. **对本控制器主要场景（CCE 集群）**：方案 1 的 Service 裸奔窗口安全——CCM 不会抢跑建 ELB。
5. **对 ECS 自建集群（开源 CCM）**：裸奔窗口内 CCM 会自动建 ELB，需额外处理。当前作为已知限制记录。

### 2.2 待实测问题

| # | 问题 | 影响 |
|---|---|---|
| Q2 | OpenEverest UI 创建 DBC 时 `loadBalancerConfigName` 能否留空？ | 决定 UI 路径是否走得通 |
| Q3 | K8s Mutating Webhook timeout 可配置上限？CCE 默认值？ | 决定 webhook 备选方案可行性 |
| Q4 | 华为云 ELB 名称长度限制（64 字符？） | 影响 §6.3 命名截断逻辑 |
| Q5 | OpenEverest engine operator 创建的 Service 命名规则（PXC/PSMDB/PG 各自）？ | 影响调试与日志关联 |

### 2.3 待验证事实

| # | 事实 | 确认方式 |
|---|---|---|
| F11 | DBC 删除时，OpenEverest 先清理 engine CR → `in-use-protection` 随之解除 | 需在 CCE 环境实测或再次溯源 OpenEverest 源码 |
---

## 3. 候选方案

经过讨论，形成 5 个候选方案。下文逐一详述，第 4 节给出对比矩阵，第 5 节给出推荐。

### 方案 0：脚本封装（不改代码）

**思路**：把当前两步封装成一个脚本，用户跑一次命令完成全部。

```bash
#!/bin/bash
# create-db-with-elb.sh <dbc-name> <engine>
DBC_NAME=$1
LBC_NAME="elb-${DBC_NAME}"

# ① 建 LBC
cat <<EOF | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: ${LBC_NAME}
spec:
  annotations: {}
EOF

# ② 等 ready
kubectl wait loadbalancerconfig ${LBC_NAME} \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# ③ 建 DBC（此时 LBC 已有 elb.id）
cat <<EOF | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: DatabaseCluster
metadata:
  name: ${DBC_NAME}
spec:
  proxy:
    expose:
      type: LoadBalancer
      loadBalancerConfigName: ${LBC_NAME}
  ...
EOF
```

**流程时序**：
```
脚本建 LBC -> 控制器建 ELB -> LBC ready -> 脚本建 DBC -> Service 出生即带 elb.id ✓
```

- **代码改动**：无
- **用户体验**：跑一条命令，但仍是"先 LBC 后 DBC"的顺序，只是脚本封装了
- **竞态**：无（脚本同步等待 ready）
- **Service 裸奔**：无（Service 出生时 LBC 已 ready）

---

### 方案 1：watch DatabaseCluster -> 自动建 LBC + ELB -> patch DBC

**思路**：新增 DBC Reconciler，watch DBC 创建事件，自动建 LBC+ELB，再 patch DBC 的 `loadBalancerConfigName`。

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

**触发条件**：
- `spec.proxy.expose.type == LoadBalancer`
- `spec.proxy.expose.loadBalancerConfigName == ""`
- DBC 创建超过 grace period（避免与 OpenEverest UI 冲突）

**LBC 命名**：`elb-<namespace>-<dbc-name>`（cluster-scoped，需避免跨命名空间撞名）

**DBC 删除处理**：
- 控制器检测到 DBC 被删
- 等 OpenEverest 移除 LBC 的 `in-use-protection` finalizer（DBC 不再引用 LBC）
- 控制器删除自动创建的 LBC
- LBC finalizer 删除 ELB

**新增组件**：
```
internal/controller/databasecluster_controller.go   # 新增 DBC Reconciler
internal/controller/loadbalancerconfig_controller.go # 不变
```

- **代码改动**：+300~400 行（新 Reconciler）
- **用户体验**：UI 点一下创建数据库，ELB 全自动
- **竞态**：⚠️ Service 裸奔窗口（步骤 ②-⑤），依赖 Q1 答案
- **patch DBC**：会修改用户的 DBC 资源（设 loadBalancerConfigName）

---

### 方案 2：watch Service -> 注入 autocreate 注解 -> CCM 建 ELB

**思路**：新增 Service Reconciler，watch LoadBalancer Service，注入 `kubernetes.io/elb.autocreate` 注解，让 CCE CCM 原生建 ELB。

**流程时序**：
```
① 用户建 DBC（无 LBC）
② OpenEverest 建 Service（空注解）
③ 控制器检测到 Service，自动探测 VPC/子网/AZ
④ 控制器构造 autocreate JSON，patch 到 Service
⑤ CCE CCM 检测到 autocreate 注解，创建 ELB + 绑定
⑥ CCM 写 Service.status.loadBalancer.ingress
```

**autocreate JSON 示例**：
```json
{
  "name": "elb-<dbc-name>",
  "vpc_id": "<auto-detected>",
  "vip_subnet_cidr_id": "<auto-detected>",
  "availability_zone_list": ["<auto-detected>"],
  "type": "union"
}
```

- **代码改动**：+200~300 行（新 Service Reconciler）
- **用户体验**：UI 点一下，ELB 全自动，最接近 EKS/GKE
- **竞态**：Service 出生到控制器注入注解之间有窗口，但 CCM 对无注解 Service 的行为仍是 Q1
- **LBC**：完全省掉，但失去 LBC 上的状态注解（ready/error/public-ip）
- **ELB 生命周期**：由 CCM 管理（含删除），控制器不再需要 finalizer

**风险**：
- autocreate 的 JSON schema 支持的参数有限（公网/内网、带宽等需确认）
- 失去对 ELB 的精细控制
- 状态可见性降低

---

### 方案 3：watch Service -> 控制器建 ELB -> 注入 elb.id

**思路**：与方案 2 类似，但不依赖 CCM autocreate。控制器自己建 ELB，把 elb.id 注入 Service。

**流程时序**：
```
① 用户建 DBC（无 LBC）
② OpenEverest 建 Service（空注解）
③ 控制器检测到 Service，自动探测 VPC/子网/AZ，建 ELB
④ 控制器 patch Service 加 kubernetes.io/elb.id 注解
⑤ CCM 检测到 elb.id，绑定 ELB
⑥ CCM 写 Service.status.loadBalancer.ingress
```

- **代码改动**：+300 行
- **用户体验**：一步到位
- **竞态**：Service 裸奔窗口（同方案 1）
- **LBC**：省掉，但需在 Service 上重建状态可见性（用 Service annotation 或 label）
- **ELB 生命周期**：控制器管（需 Service finalizer 或 ownerReference 链）

**风险**：
- Service 由 engine operator 创建，ownerReference 指向 engine CR，不指向我们的控制器--删除链路需精心设计
- engine operator 可能重建 Service（覆盖我们的注解），需确认

---

### 方案 4：Mutating Admission Webhook

**思路**：拦截 DBC 创建请求，同步建 LBC+ELB，等 ready 后改写 DBC 的 `loadBalancerConfigName`，再放行。

**流程时序**：
```
① 用户建 DBC（loadBalancerConfigName 填约定值 "auto"）
② K8s API Server 收到请求
③ 【新增】Mutating Webhook 拦截：
   - 检测 loadBalancerConfigName == "auto"
   - 同步建 LBC
   - 同步等 ELB ready（最多 120s）
   - 改写 loadBalancerConfigName = elb-<dbc-name>
④ Validating Webhook 校验（LBC 已存在且 ready）-> 通过 ✓
⑤ DBC 持久化
⑥ OpenEverest 建 Service -> 出生即带 elb.id ✓
```

- **代码改动**：+400~500 行（webhook server + LBC 创建逻辑）
- **用户体验**：真正一步到位，Service 出生即带注解，无裸奔窗口
- **竞态**：无（同步等待）
- **Service 裸奔**：无

**风险**：
- webhook 必须高可用，否则 DBC 创建被阻塞
- 同步等 ELB 创建（最多 120s）占用 webhook 请求--需确认 Q3（webhook timeout 上限）
- webhook 需要单独 RBAC + Service + Deployment

| 方案 | 拦截点 | 设计意图 |
|---|---|---|
| 0 脚本 | 无（不改代码） | 过渡方案，立即可用 |
| 1 watch DBC | DBC 创建事件 | **首选**——复用 LBC Reconciler，改动最小 |
| 2 watch Service+autocreate | Service 创建事件 | CCM 原生建 ELB，但 VPC/子网需手动填 |
| 3 watch Service+elb.id | Service 创建事件 | 控制器建 ELB，但不保留 LBC，状态可见性差 |
| 4 Webhook | API 请求到达 API Server 之前 | 无裸奔窗口，但高可用要求高 |

> **为何枚举 5 个方案？** DBC → Service → ELB 链路上有多个可拦截的点（DBC CR 创建、Service 创建、API 请求准入阶段），每个点都有不同的安全窗口、代码复用、依赖关系权衡。穷举全部候选再收敛是标准系统设计方法。


## 4. 方案对比矩阵

| 维度 | 方案 0 脚本 | 方案 1 watch DBC | 方案 2 watch Service+autocreate | 方案 3 watch Service+elb.id | 方案 4 Webhook |
|---|---|---|---|---|---|
| **代码改动** | 无 | 中（+300行） | 中（+250行） | 中（+300行） | 大（+450行） |
| **用户步骤** | 跑脚本（1条命令） | 点1下 | 点1下 | 点1下 | 点1下 |
| **Service 裸奔窗口** | 无 | 有（依赖Q1） | 有（依赖Q1） | 有（依赖Q1） | **无** |
| **LBC 是否保留** | 是 | 是（自动建） | 否 | 否 | 是（自动建） |
| **状态可见性** | 完整（LBC注解） | 完整 | 弱（Service事件） | 中（Service注解） | 完整（LBC注解） |
| **ELB 生命周期管理** | 控制器+finalizer | 控制器+finalizer | CCM原生 | 控制器+finalizer | 控制器+finalizer |
| **ELB 精细控制** | 完整 | 完整 | 受限（autocreate schema） | 完整 | 完整 |
| **多DB共享LBC** | 支持（手动） | 不支持（每DBC独立） | 不支持 | 不支持 | 不支持（每DBC独立） |
| **删除安全** | 高（现有finalizer） | 中（DBC删除竞态，见风险R3） | 高（CCM处理） | 中（Service ownerRef问题） | 高（LBC finalizer） |
| **patch 用户资源** | 否 | 是（patch DBC） | 否（patch Service） | 否（patch Service） | 是（mutate DBC） |
| **与现有方案兼容** | 完全兼容 | 兼容（opt-in） | 兼容（opt-in） | 兼容（opt-in） | 兼容（opt-in，约定值触发） |
| **依赖 OpenEverest 配合** | 无 | 无 | 无 | 无 | 无 |
| **高可用要求** | 无 | 低 | 低 | 低 | **高**（webhook 挂了 DBC 建不了） |

---

## 5. 推荐方案

### 5.1 推荐结论

**首选：方案 1（watch DBC -> 自动建 LBC）作为 opt-in 增量功能**。Q1 已确认：CCE 内置 CCM 对无注解 LoadBalancer Service 不 autocreate，Service 停在 `<pending>`，裸奔窗口安全无孤儿 ELB 风险。


**退选：方案 0（脚本封装）作为立即可用的过渡方案**，无需等 Q1 确认。

**备选：方案 4（Webhook）**，仅当未来需要严格无裸奔窗口或支持 ECS 自建集群（开源 CCM）场景时启用。

### 5.2 推荐理由

1. **方案 1 复用现有代码最多**：LBC Reconciler 全部逻辑（探测、创建、删除、finalizer、状态注解）原样复用，新增的 DBC Reconciler 只是触发器 + patch 逻辑。
2. **方案 1 状态可见性完整**：LBC 上的 `ready`/`elb-status`/`error`/`public-ip` 注解全保留，运维体验不变。
3. **方案 1 与现有方案完全兼容**：用户已设 `loadBalancerConfigName` 的 DBC 不受影响，新功能仅对空值触发。
4. **方案 4 虽无裸奔窗口，但复杂度最高**（webhook server + HA + timeout），且阻塞 DBC 创建。

### 5.3 方案 1 的 opt-in 触发设计

为降低风险，方案 1 采用 opt-in 触发，避免影响存量用户：

**触发条件**（全部满足才自动建 LBC）：
1. `spec.proxy.expose.type == LoadBalancer`
2. `spec.proxy.expose.loadBalancerConfigName == ""`
3. DBC 有注解 `huawei-elb.io/auto-elb: "true"`（opt-in 开关）
4. DBC 创建超过 grace period（5s）——OpenEverest UI 可能先创建 DBC 再立即 patch 设 `loadBalancerConfigName`（两次 API 调用），grace period 避免控制器在 UI 两次调用之间抢跑创建不必要的 LBC。若 UI 不支持两步创建（单次请求即设好值），此条件可移除。
5. 不含其他云提供商注解（复用现有 `hasForeignCloudAnnotations` 逻辑）

**不触发的情况**：
- `loadBalancerConfigName` 已设值 -> 走现有 LBC 流程
- `huawei-elb.io/auto-elb` 未设或为 `false` -> 不自动建，Service 保持空注解（等同 EKS/GKE 上不配置 LB 的情况）

> **opt-in 设计的意义**：用户显式声明"我要自动建 ELB"，控制器才行动。存量 DBC 不受影响。OpenEverest UI 如果不支持加这个注解，用户可通过 `kubectl annotate` 或 Helm post-install hook 添加。

### 5.4 当前方案 vs 推荐方案（方案 1）对比

| 维度 | 当前方案（手动两步） | 推荐方案（方案 1：DBC 自动建 LBC） |
|---|---|---|
| **用户步骤** | 2 步（建 LBC → 等 ready → 建 DBC） | 1 步（建 DBC，加 `auto-elb` 注解） |
| **操作界面** | kubectl 或 UI | kubectl annotate（当前）；未来 UI 支持后一键 |
| **用户等待** | 必须等 LBC ready 后才能建 DBC | 只需等 DBC ready（LBC+ELB 在后台自动完成） |
| **前提知识** | 理解 LBC 概念、知道要建 LBC | 只需知道加 `auto-elb: "true"` 注解 |
| **VPC/子网/AZ** | 自动探测（无需手动） | 自动探测（共享同一探测逻辑） |
| **ELB 生命周期** | 控制器 finalizer 管理 | 同左（复用） |
| **状态可见性** | LBC 上的 `ready`/`elb-status`/`error`/`public-ip` | 同左（LBC 仍然存在，用户可查看） |
| **删除流程** | 删 DBC → 控制器等 in-use-protection 移除 → 删 ELB | 删 DBC → DBC Reconciler 等 in-use-protection 移除 → 删 LBC → LBC Reconciler 删 ELB |
| **异常回滚** | 删除 LBC 即删 ELB | 删除 DBC 时自动清理 LBC；DBC patch 失败时有补偿清理逻辑（R2） |
| **多 DB 共享 ELB** | 支持（多 DBC 引用同一 LBC） | 不支持（每个 DBC 独立 LBC）；如需共享，手动建 LBC 即可（不设 auto-elb） |
| **ELB 配置精细度** | 完整（LBC 注解） | 完整（自动建的 LBC 仍可手动加注解） |
| **与当前方案兼容** | — | 完全兼容——已设 `loadBalancerConfigName` 的 DBC 不触发 |
| **代码复用** | — | ~80% 复用（LBC Reconciler 完全不动，只新增 DBC Reconciler） |

```
当前：LBC → [等 ready] → DBC（用户感知两步、等待一次）
  ↑ 用户操作        ↑ 用户操作

方案1：DBC + auto-elb → LBC → ELB → patch DBC → Service → CCM 绑定
       ↑ 用户操作（一步）       └──── 控制器自动完成 ────┘
```
---

## 6. 方案 1 详细设计

### 6.0 基础概念：Reconcile 循环

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
- DBC 创建后 17ms 内触发 DBC Reconciler → 判断触发条件 → 建 LBC →
  **return（不等 ELB ready）**
- LBC 创建后 17ms 内触发 LBC Reconciler → 探测 VPC/子网/AZ → 调华为云 API 建 ELB →
  写 elb.id → **return**
- LBC 的 elb.id 被写回后，再次触发 DBC Reconciler（因为 LBC 状态变了）→
  检测到 LBC ready → patch DBC 的 loadBalancerConfigName → **完成**

> 关键理解：**控制器不是"建 LBC → 等 ELB → patch DBC"这条同步线，而是多次 Reconcile 协作的异步过程。** 每次 Reconcile 只做一小步，然后 return。

### 6.1 架构

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

### 6.2 DBC Reconciler 流程

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

### 6.3 LBC 命名规则

```
elb-<namespace>-<dbc-name>
```

- LBC 是 cluster-scoped (F6)，需跨命名空间唯一
- `<namespace>` 避免 `db1` 在两个命名空间撞名
- ELB 名 = `elb-<namespace>-<dbc-name>`（ELBNamePrefix `elb-` + LBC 名）
- 华为云 ELB 名长度限制 64 字符；`<namespace>-<dbc-name>` 需约束长度

**长度保护**：如果 `len(namespace) + len(dbc-name) > 60`，截断 dbc-name 并加 SHA256 前 8 字符 hex 后缀。碰撞概率 `1/2^32`，冲突时直接报错不重试。

### 6.4 DBC 删除链路

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

**关键**：必须等 DBC 的 `loadBalancerConfigName` 不再指向 LBC 后，OpenEverest 才会移除 LBC 的 `in-use-protection` finalizer。但 DBC 正在删除，CEL 规则禁止清空 `loadBalancerConfigName`（F4）。

**解决**：DBC 删除时，DBC Reconciler 轮询 LBC 的 finalizers，确认 `in-use-protection` 已移除后再删 LBC（轮询间隔 5s，超时 10 分钟；超时后在 DBC 的 `huawei-elb.io/error` 注解写错误信息，控制器继续重试）。

> ⚠️ **F11 待验证**："OpenEverest 会先清理 engine CR → `in-use-protection` 随之解除" 是推断，未在源码中确认。若实际行为是先删 DBC 再清理 engine CR（或并发），则 `in-use-protection` 可能在控制器尝试删 LBC 时仍存在。兜底：超时后打印告警日志，人工介入。
### 6.5 新增常量与注解

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

### 6.6 RBAC 新增

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

### 6.7 共享探测缓存

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

## 7. 风险点与缓解

| # | 风险 | 影响 | 概率 | 缓解措施 |
|---|---|---|---|---|
| R1 | **Service 裸奔窗口：CCM 对无注解 LoadBalancer Service autocreate 孤儿 ELB** | 孤儿 ELB 持续计费 | **已解除**（Q1 确认 CCE CCM 不 autocreate） | ✅ CCE 场景安全；⚠️ ECS 自建集群（开源 CCM）会 autocreate，当前作为已知限制，文档注明仅支持 CCE 场景 |
| R2 | **patch DBC 失败导致孤儿 LBC+ELB** | LBC+ELB 存在但无人引用，持续计费 | 中 | ① patch 失败时立即删 LBC（最终一致性）；② 补偿：独立 goroutine 每 5 分钟全集群扫描——有 `auto-lbc-name` 但 DBC 不存在则删 LBC |
| R3 | **DBC 删除时 `in-use-protection` finalizer 阻止 LBC 删除** | DBC 卡在删除中 | 高 | DBC Reconciler 轮询 LBC finalizers（5s 间隔），等 `in-use-protection` 移除后再删 LBC；超时 10 分钟后在 DBC 的 `huawei-elb.io/error` 写错误信息，继续重试 |
| R4 | **LBC 命名冲突（cluster-scoped）** | 创建 LBC 失败 | 低 | 命名加 namespace 前缀 + 长度截断 + SHA256 前 8 位 hex hash；冲突时直接报错不重试 |
| R5 | **OpenEverest UI 不支持给 DBC 加 `auto-elb` 注解** | 用户无法通过 UI 触发自动建 | 高 | 提供 kubectl annotate 命令；或 Helm post-install hook；或文档说明 |
| R6 | **engine operator 重建 Service 覆盖注解** | elb.id 丢失，ELB 解绑 | 低 | 方案 1 不直接改 Service（走 LBC -> engine CR -> Service 路径），engine operator 重建时会重新 GetAnnotations，不受影响 |
| R7 | **两个 Reconciler 并发写 LBC** | resourceVersion 冲突 | 中 | LBC 写操作已有 `updateWithRetry`（RetryOnConflict），DBC Reconciler 创建 LBC 后不直接改，交给 LBC Reconciler |
| R8 | **用户手动删了自动建的 LBC** | DBC 引用的 LBC 消失 | 低 | DBC Reconciler 检测到 LBC 不存在且 DBC 有 `auto-lbc-name` 注解 -> 重建 LBC |
| R9 | **跨 region DBC（huawei-elb.io/region 注解）** | ELB 建在错误 region | 低 | DBC Reconciler 读取 DBC 的 region 注解，传给 LBC 的 spec.annotations |

---

## 8. 实现计划

### Phase 0：确认关键问题

- [x] **Q1**：CCE CCM 对无注解 LoadBalancer Service 的行为——**已确认**：CCE 内置 CCM 不 autocreate（Service 停 pending），开源 CCM 会 autocreate。方案 1 对 CCE 场景可行。
- [ ] **Q2**：确认 OpenEverest UI 能否留空 loadBalancerConfigName 提交
- [ ] **Q3**：确认 K8s webhook timeout 上限（备选方案 4 用）

### Phase 1：方案 0 脚本封装（立即可用，不阻塞）

- [ ] 编写 `examples/create-db-with-elb.sh`
- [ ] 文档说明用法
- [ ] 测试端到端流程

### Phase 2：方案 1 实现（Q1 确认后）

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

### Phase 3：方案 4 Webhook（备选，暂不实施）

> Q1 已通过，方案 1 为首选。Phase 3 仅在需要支持 ECS 自建集群（开源 CCM 场景）或严格无裸奔窗口时启动。

- [ ] webhook server 搭建（controller-runtime）
- [ ] 同步建 LBC+ELB 逻辑
- [ ] webhook HA 部署
- [ ] timeout 配置

---

## 9. 与当前方案的兼容性

| 场景 | 当前方案 | 方案 1 上线后 |
|---|---|---|
| 用户手动建 LBC + DBC 引用 | 正常工作 | 不受影响（loadBalancerConfigName 已设值，不触发自动建） |
| 用户建 DBC 不填 loadBalancerConfigName | Service 空注解，无 ELB | 若 DBC 有 `auto-elb: true` 注解 -> 自动建 LBC+ELB；否则不变 |
| 用户用预创建 ELB（spec.annotations 设 elb.id） | 正常工作 | 不受影响 |
| 用户用 CCM autocreate（kubernetes.io/elb.autocreate） | 控制器跳过 | 不受影响 |
| 多数据库共享同一 LBC | 支持 | 仍支持（手动指定 loadBalancerConfigName） |

**结论**：方案 1 是纯增量 opt-in 功能，不破坏任何现有行为。

---

## 10. 决策记录

| 决策 | 选择 | 理由 |
|---|---|---|
| 触发方式 | opt-in 注解 `huawei-elb.io/auto-elb: "true"` | 不影响存量，用户显式声明 |
| LBC 命名 | `elb-<ns>-<dbc-name>` | cluster-scoped 唯一性 |
| 是否 patch DBC | 是（设 loadBalancerConfigName） | F4 允许从空设值，F5 保证注解传播 |
| 是否保留 LBC | 是 | 复用现有 Reconciler + 状态可见性 |
| 是否用 Webhook | 否（首选方案 1） | 复杂度高、HA 要求高、阻塞 DBC 创建 |
| 共享探测缓存 | 是（NetworkDetector） | 避免重复 API 调用 |

---

## 11. 待确认问题（状态同步自 §2.1-2.3）

- [x] **Q1**：CCE CCM 对无注解 LoadBalancer Service 的行为（✅ CCE 不 autocreate）
- [ ] **Q2**：OpenEverest UI 创建 DBC 时 loadBalancerConfigName 能否留空？
- [ ] **Q3**：K8s Mutating Webhook timeout 上限（CCE 环境）？
- [ ] **Q4**：华为云 ELB 名长度限制（64 字符？）
- [ ] **Q5**：OpenEverest engine operator 创建的 Service 命名规则
- [ ] **F11**：DBC 删除时 OpenEverest 的 `in-use-protection` 解除时序
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
