# PSMDB 内网 ELB `elb.class` 注解冲突问题分析与方案B设计

## 问题概述

PSMDB（Percona Server for MongoDB）operator 在 OpenEverest 环境下创建内网 LoadBalancer Service 时，CCE validating webhook 拒绝其 `client.Update()` 操作，报错：

```
services "mongodb-ctn-rs0-0" is forbidden: can't modify service elb [kubernetes.io/elb.class] annotation
```

后续测试发现 `elb.class` 解决后，`elb.id` 注解冲突紧随其后：

```
services "mongodb-ctn-rs0-0" is forbidden: can't modify service elb [kubernetes.io/elb.id] annotation
```

根因是 CCE webhook 保护**所有** `kubernetes.io/elb.*` 注解不可变，而 PSMDB operator 的 `Update()` 会移除不在其模板里的注解。

## 完整根因链路

### 1. 配置流（LBC 参数模板）

```
LoadBalancerConfig CR (spec.annotations: huawei-elb.io/*)
  -> everest-operator (command=/manager, manager name="manager")
    -> 填到 PSMDB CR spec.replsets[0].expose.annotations
      -> PSMDB operator 填到 Service metadata.annotations
        -> huawei-elb-controller 读取 huawei-elb.io/* 参数
```

### 2. huawei-elb-controller 注入 CCE CCM autocreate 注解

`reconcileCreate`（service_controller.go:148-150）注入：
- `kubernetes.io/elb.autocreate`：JSON 配置（ELB 名称、子网、AZ、EIP 等）
- `kubernetes.io/elb.class: union`
- `kubernetes.io/elb.instance-reclaim-policy: alwaysDelete`

### 3. CCE CCM 创建 ELB 并写入动态注解

CCM autocreate 完成后，CCE CCM 在 Service 上写入：
- `kubernetes.io/elb.id`：ELB 实例 ID（动态，创建后才有）
- `kubernetes.io/elb.pass-through: onlyLocal`
- `kubernetes.io/elb.acl-status`（如果有 ACL）

### 4. CCE validating webhook 保护注解不可变

```
service-webhook/validate.crd.service:
  groups=['*'] resources=['services'] operations=['CREATE', 'UPDATE']
```

规则：`kubernetes.io/elb.*` 注解一旦写入，**不能移除、不能改值**（保持不变时允许 Update）。

### 5. PSMDB operator 用 `client.Update()` 触发冲突

PSMDB operator `createOrUpdateSvc`（psmdb_controller.go:1799）的关键逻辑：

```go
// 快速路径：当 !saveOldMeta && len(IgnoreAnnotations)==0
// 直接用模板的新对象做 Update，不合并旧注解
if !saveOldMeta && len(IgnoreAnnotations) == 0 {
    return r.client.Update(ctx, svc)
}
```

触发条件：用户在 PSMDB CR 设了 `expose.annotations: {"huawei-elb.io/public":"false"}`，导致 `SaveOldMeta()` 返回 false。

PSMDB 模板只有 `huawei-elb.io/*`，没有 `kubernetes.io/elb.*` -> `client.Update()` 等同于移除 `elb.class`、`elb.id`、`elb.autocreate` 等 -> **webhook 拒绝**。

### 6. 为什么 PXC/PG 不受影响

PXC operator 的 `SaveOldMeta` 逻辑不同，或 `expose.annotations` 为空（走合并路径保留旧注解）。PSMDB 是唯一触发快速路径的引擎。

## 排查过程与关键证据

### 证据1：PSMDB CR expose.annotations 无法持久化 `elb.class`

```bash
# JSON patch 返回成功
kubectl patch psmdb mongodb-ctn -n everest --type=json -p='[
  {"op":"add","path":"/spec/replsets/0/expose/annotations/kubernetes.io~1elb.class","value":"union"}
]'
# perconaservermongodb.psmdb.percona.com/mongodb-ctn patched

# 但立即查询，注解不在
kubectl get psmdb mongodb-ctn -n everest -o jsonpath='{.spec.replsets[0].expose.annotations}'
# {"huawei-elb.io/public":"false"}  -- elb.class 被清除
```

**根因**：everest-operator（manager name="manager"）管理 PSMDB CR 的 `f:replsets` 字段，reconcile 时用模板覆盖 spec。

```
Manager: manager (op=Update) time=2026-07-15T01:40:48Z
  spec keys: ['.', 'f:backup', 'f:enableVolumeExpansion', 'f:image', ..., 'f:replsets', ...]
```

### 证据2：通过 LBC 注入 `elb.class` 成功但 `elb.id` 冲突紧随其后

```bash
# 给 LBC 加 elb.class
kubectl patch loadbalancerconfig test -n everest --type=merge -p '{"spec":{"annotations":{"huawei-elb.io/public":"false","kubernetes.io/elb.class":"union"}}}'

# everest-operator 透传到 PSMDB CR expose.annotations 成功
kubectl get psmdb mongodb-ctn -n everest -o jsonpath='{.spec.replsets[0].expose.annotations}'
# {"huawei-elb.io/public":"false","kubernetes.io/elb.class":"union"}  -- 持久化了

# 但 PSMDB operator Update 仍被拒绝，换成了 elb.id
# Error: ... can't modify service elb [kubernetes.io/elb.id] annotation
```

**结论**：`elb.id` 是 CCE CCM 动态写入的，无法在 LBC/PSMDB CR 模板中预设。**方案C（预设 elb.class）不可行**。

### 证据3：`ignoreAnnotations` 也无法持久化

```bash
kubectl patch psmdb mongodb-8az -n everest --type=merge -p '{"spec":{"ignoreAnnotations":["kubernetes.io/elb.class"]}}'
# The PerconaServerMongoDB "mongodb-8az" is invalid: ... (被 everest-operator 清除)
```

### 证据4：CCE webhook 规则

```
ingress-webhook/validate.crd.ingress:
  groups=['*'] resources=['ingresses'] ops=['CREATE', 'UPDATE']

service-webhook/validate.crd.service:
  groups=['*'] resources=['services'] ops=['CREATE', 'UPDATE']
```

无 mutating webhook 针对 PSMDB CR。PSMDB CRD schema 允许任意 annotation key（`additionalProperties: {type: string}`）。

## 方案对比

| | 方案A（空 annotations + merge 路径） | 方案B（直接调 API） | 方案C（LBC 预设 elb.class） |
|---|---|---|---|
| **解决方式** | 绕过：依赖 PSMDB operator merge 路径不删注解 | 消除：根本不产生 `kubernetes.io/elb.*` 注解 | 绕过：预设 elb.class |
| **冲突源** | 仍存在（CCM 写 elb.id，PSMDB 可能删） | **不存在** | 部分解决（elb.id 仍冲突） |
| **依赖第三方行为** | 依赖 PSMDB operator SaveOldMeta 实现 | 不依赖任何第三方 | 依赖 webhook 只保护已存在注解 |
| **升级风险** | 每次升级重新验证 | 无 | 已证明不可行 |
| **交付质量** | 夹缝求生，脆弱 | 永久消除冲突 | ❌ 已否决 |

## 方案B 设计：直接调华为云 ELB API

### 核心思路

huawei-elb-controller 的 Service Reconciler **不再注入 `kubernetes.io/elb.*` 注解**，改为直接调华为云 ELB v3 API 管理 ELB 全生命周期。`kubernetes.io/elb.*` 注解完全不产生，冲突源消失。

### 不变的部分

```
LBC 参数模板角色不变：
  LoadBalancerConfig CR (spec.annotations: huawei-elb.io/*)
    -> everest-operator 填到 PSMDB CR expose.annotations
      -> PSMDB operator 填到 Service annotations
        -> huawei-elb-controller 读取 huawei-elb.io/* 参数

LBC Reconciler 行为不变：
  hasPlan2Annotations(lbc) -> 跳过（仍是参数模板，不管理 ELB 生命周期）
```

### 改变的部分

```
Service Reconciler 的 ELB 创建方式：
  旧：注入 kubernetes.io/elb.autocreate -> CCE CCM 创建+管理 ELB
  新：直接调华为云 API 创建 ELB + Listener + Pool + Member
```

### 关键决策

#### 1. Member 后端类型：NodePort 模式

ELB -> Node IP : NodePort -> kube-proxy -> pod

理由：
- 与 CCE CCM autocreate 行为一致
- 不依赖 pod 网络可达性（CCE 容器网络与 ELB 后端网络可能不同）
- `NetworkDetector.Detect()` 能拿到 node IP
- 健康检查走 NodePort，可靠

#### 2. 迁移策略：不做迁移

旧 autocreate 创建的 Service 需删除重建。文档说明，不做自动迁移（避免复杂性和风险）。

#### 3. 多端口支持

支持多端口 Service（每个 port 建独立 Listener+Pool+HealthCheck）。虽然 PSMDB/PG/PXC 都是单端口，但架构上应支持。

### 实现范围

#### 新增 ELB API 封装（internal/huaweicloud/）

| 文件 | 函数 | 说明 |
|------|------|------|
| `listener.go` | `CreateListener` | 创建监听器（协议、端口） |
| | `DeleteListener` | 删除监听器 |
| | `ListListeners` | 列出 ELB 下所有监听器 |
| `pool.go` | `CreatePool` | 创建后端服务器组（LB 算法） |
| | `DeletePool` | 删除后端服务器组 |
| `member.go` | `BatchSyncMembers` | 同步成员（diff 增删） |
| `healthcheck.go` | `CreateHealthCheck` | 创建 TCP 健康检查 |
| | `DeleteHealthCheck` | 删除健康检查 |

已有基础（elb.go）：`CreateELB`、`DeleteELB`、`ShowELB`、`FindELBByName`、`UpdateELB`、IP group 管理。

#### Service Reconciler 重写（internal/controller/service_controller.go）

**reconcileCreate**（新写）：
1. `NetworkDetector.Detect()` -> VPC ID, subnet ID, AZs
2. 读取 `huawei-elb.io/*` 参数
3. `CreateELB()` 创建 ELB（已有）
4. 等待 ELB ACTIVE
5. 为每个 Service port 创建 Listener + Pool + HealthCheck
6. 创建 ACL IP group（如有 source ranges，已有逻辑）
7. 写 `huawei-elb.io/elb-id` 注解（不是 `kubernetes.io/elb.id`）
8. 写 `service.status.loadBalancer.ingress`
9. 加 `huawei-elb.io/elb-cleanup` finalizer

**reconcileUpdate**（重写）：
- 端口变化：增删 Listener + Pool + HealthCheck
- Endpoints 变化：同步 Pool Member（NodePort + Node IP）
- 带宽/公网类型变化：`UpdateELB`（已有）
- source ranges 变化：更新 ACL IP group（已有）

**reconcileDelete**（新增）：
- `DeleteELB()`（级联删除 listener/pool/member）
- 删除 ACL IP group
- 移除 finalizer

**新增 Endpoints watch**：
- `SetupWithManager` 增加 Endpoints watcher
- Endpoints 变化触发对应 Service reconcile
- 同步 Pool Member（node IP + NodePort）

#### 注解命名空间迁移

| 旧（kubernetes.io/elb.*） | 新（huawei-elb.io/*） | 说明 |
|---|---|---|
| `kubernetes.io/elb.id` | `huawei-elb.io/elb-id` | ELB 实例 ID |
| `kubernetes.io/elb.acl-id` | `huawei-elb.io/acl-id` | ACL IP 组 ID |
| `kubernetes.io/elb.acl-status` | `huawei-elb.io/acl-status` | ACL 状态 |
| `kubernetes.io/elb.acl-type` | `huawei-elb.io/acl-type` | ACL 类型 |
| `kubernetes.io/elb.autocreate` | （删除） | 不再使用 |
| `kubernetes.io/elb.class` | （删除） | 不再使用 |
| `kubernetes.io/elb.instance-reclaim-policy` | （删除） | 不再使用，用 finalizer |
| （无） | `huawei-elb.io/listener-map` | JSON: {"port/protocol": "listener-uuid"} |
| （无） | `huawei-elb.io/pool-map` | JSON: {"port/protocol": "pool-uuid"} |
| （无） | `huawei-elb.io/healthcheck-map` | JSON: {"port/protocol": "hc-uuid"} |
| （无） | `huawei-elb.io/elb-cleanup` | finalizer |

#### RBAC 新增

```yaml
- apiGroups: [""]
  resources: ["services/status"]
  verbs: ["update", "patch"]
- apiGroups: [""]
  resources: ["endpoints"]
  verbs: ["get", "list", "watch"]
```

### 测试计划

| 测试 | 验证内容 |
|------|---------|
| `Test_CreatePath_DirectCreateELB` | CreateELB 调用，注解+status 写入，finalizer 添加 |
| `Test_CreatePath_MultiPortListeners` | N 端口 -> N listener+pool+healthcheck |
| `Test_CreatePath_WaitsForELBActive` | 等待 ELB ACTIVE 再创建子资源 |
| `Test_UpdatePath_AddPort` | 新端口 -> listener+pool+hc 创建 |
| `Test_UpdatePath_RemovePort` | 删端口 -> 级联删除 |
| `Test_UpdatePath_BandwidthChange` | UpdateELB 调用 |
| `Test_DeletePath_CleansUpELB` | DeleteELB 调用，finalizer 移除 |
| `Test_EndpointsSync_MembersSynced` | Endpoints 变化 -> Member 同步 |
| `Test_Status_LoadBalancerIngress` | status.loadBalancer.ingress 写入 |

### 提交策略

| # | Commit | 说明 |
|---|--------|------|
| 1 | `huaweicloud: add listener/pool/member/healthcheck API wrappers` | 新增 API 封装 + 测试 |
| 2 | `params: add new annotations, remove CCE autocreate code` | 注解迁移，删死代码 |
| 3 | `controller: rewrite reconcileCreate with direct ELB API` | 创建路径重写 |
| 4 | `controller: rewrite reconcileUpdate with port diffing` | 更新路径重写 |
| 5 | `controller: add reconcileDelete with ELB cleanup` | 删除路径新增 |
| 6 | `controller: add Endpoints watch + member sync` | Endpoints 监听 |
| 7 | `rbac: add services/status + endpoints permissions` | RBAC 更新 |
| 8 | `tests: comprehensive test coverage` | 测试补全 |

### 风险

| 风险 | 缓解 |
|------|------|
| ELB 创建异步（PENDING_CREATE） | `WaitForELBActive()` 等待再创建子资源 |
| 级联删除非即时 | 检查 ELB 状态，必要时重试 |
| Service update + Endpoints update 并发 | Reconcile 循环内同时读 Service + Endpoints |
| 旧 autocreate Service | 需删除重建，文档说明 |

## 环境信息

- CCE 集群：4 节点 s6.xlarge.2（cn-north-4a），K8s v1.35.3-r0-35.0.8
- PSMDB operator 镜像：`docker.io/percona/percona-server-mongodb-operator:1.22.0`
- OpenEverest 1.16.1
- VPC 子网 ID：`e8e541e1-814b-4856-8aa3-a8f1e111af4a`，AZ：`cn-north-4a`
- LBC `test` 配置：`spec.annotations: {huawei-elb.io/public: "false", kubernetes.io/elb.class: "union"}`

## PSMDB operator 关键源码位置

- `createOrUpdateSvc`：`psmdb_controller.go:1799` -- 快速路径判断
- `setIgnoredAnnotations`：`psmdb_controller.go:1829` -- 从旧对象保留被忽略的注解
- `ensureExternalServices`：`service.go:94` -- 报错点

## 方案B 与 EKS/GKE 实现对比与对齐

目标：体验和实现尽量与 EKS/GKE 一致。基于 AWS Load Balancer Controller 和 GKE L4 NetLB CCM 的源码研究，对比方案B 当前设计并调整。

### 逐项对比

| 维度 | EKS (aws-load-balancer-controller) | GKE (L4 NetLB CCM) | 方案B 当前设计 | 对齐建议 |
|------|-----------------------------------|---------------------|---------------|---------|
| **配置读取** | `service.beta.kubernetes.io/aws-load-balancer-*` 注解 | `networking.gke.io/*` + `cloud.google.com/*` 注解 + `loadBalancerClass` | `huawei-elb.io/*` 注解 | ✅ 已对齐（自命名空间注解） |
| **注解写回** | 不写回 config 注解，只写 `elbv2.k8s.aws/checkpoint`（不透明状态） | 写回 `networking.gke.io/backend-service` + `networking.gke.io/target-pool`（顶层资源名） | 写回 `elb-id` + `listener-map` + `pool-map` + `healthcheck-map` | ⚠️ 调整：只写 `huawei-elb.io/elb-id`，删除子资源 map 注解 |
| **状态追踪** | AWS 云标签 `service.k8s.aws/stack` + `service.k8s.aws/resource` | K8s 注解（顶层资源名） | K8s 注解（子资源 ID 列表） | ⚠️ 调整：只存顶层 ELB ID，子资源运行时 List 查询 |
| **Member 后端** | instance（node IP + NodePort）或 IP（pod IP），注解选择 | 仅 instance（node IP + NodePort） | NodePort（instance 模式） | ✅ 已对齐 GKE 默认 |
| **Member watch** | IP 模式 watch EndpointSlice；instance 模式不需 | watch **nodes**（不是 endpoints） | watch Endpoints | ⚠️ 调整：改为 watch nodes（NodePort 模式下 pod 变化由 kube-proxy 处理，ELB 无需感知） |
| **健康检查** | TCP traffic-port，10s/10s/3/3，可注解配置 | HTTP /healthz:10256，8s/1s/1/3，不可配置 | TCP backend port | ✅ TCP 模式对齐 EKS |
| **Service status** | `Hostname` + `Ports[]`（每端口一个） | 仅 `IP` | `IP` | ✅ 对齐 GKE（华为云 ELB 返回 IP，无 DNS 名） |
| **finalizer** | `service.k8s.aws/resources` | `service.kubernetes.io/load-balancer-cleanup` + `gke.networking.io/l4-netlb-v1` | `huawei-elb.io/elb-cleanup` | ✅ 模式对齐（域名 + 功能） |
| **ELB 命名** | `k8s-{ns_8}-{name_8}-{uid_10}`（hash 避免重名） | `"a" + service.UID`（去连字符，截断 32） | `cce-lb-<namespace>-<name>` | ⚠️ 调整：改为 `k8s-{ns_8}-{name_8}-{uid_10}` 避免重名 |
| **多端口** | 每端口一个 target group | 所有端口共享一个 forwarding rule | 每端口一个 Listener + Pool | ✅ 对齐 EKS（华为云 Listener 概念支持） |
| **删除清理** | finalizer，部署空模型清理云资源 | finalizer，直接删除云资源 | finalizer，DeleteELB 级联 | ✅ 对齐 |

### 方案B 调整项（对齐 EKS/GKE）

#### 调整1：状态追踪简化 -- 删除子资源 map 注解

**EKS 模式**：不写子资源 ID 到 Service，用 AWS 云标签追踪。
**GKE 模式**：只写顶层资源名（backend-service/target-pool），不写子资源。

**方案B 调整**：
- Service 上只存 `huawei-elb.io/elb-id`（顶层 ELB ID）
- 删除 `huawei-elb.io/listener-map`、`huawei-elb.io/pool-map`、`huawei-elb.io/healthcheck-map`
- 子资源（Listener/Pool/HealthCheck）通过 ELB ID + List API 运行时查询
- reconcile 周期是分钟级，List API 开销可接受

**理由**：减少 Service 注解写入（降低与 PSMDB operator Update 的冲突面），对齐 EKS 最干净的模式。

#### 调整2：Member watch -- 从 Endpoints 改为 nodes

**EKS instance 模式**：不 watch endpoints（NodePort 由 kube-proxy 管理）。
**GKE**：watch nodes，node 增删时重建 target pool。

**方案B 调整**：
- `SetupWithManager` watch nodes 而非 Endpoints
- node 增删时触发 Service reconcile，更新 ELB Pool Member
- pod 增删不触发 reconcile（kube-proxy 自动处理 NodePort 路由）

**理由**：NodePort 模式下 ELB 只需感知 node 变化，不需感知 pod 变化。简化实现，对齐 GKE。

#### 调整3：ELB 命名 -- 加 UID 后缀避免重名

**EKS**：`k8s-{ns_8}-{name_8}-{sha256_10}`（clusterName + UID + scheme 的 hash）
**GKE**：`"a" + service.UID`（去掉连字符）

**方案B 调整**：
- 从 `cce-lb-<namespace>-<name>` 改为 `k8s-{ns_8}-{name_8}-{uid_10}`
- ns/name 截断到 8 字符，UID 取前 10 字符
- 总长度约 30 字符，远低于华为云 ELB 64 字符限制
- 用户仍可通过 `huawei-elb.io/name` 注解自定义

**理由**：避免 namespace+name 相同的 Service 重名冲突（不同集群或重建场景），对齐 EKS/GKE 用 UID 保证唯一性的模式。

#### 调整4：RBAC -- 从 endpoints 改为 nodes

```yaml
# 删除
# - apiGroups: [""]
#   resources: ["endpoints"]
#   verbs: ["get", "list", "watch"]

# 新增
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["services/status"]
  verbs: ["update", "patch"]
```

### 调整后的注解表

| 注解 | 方向 | 说明 |
|------|------|------|
| `huawei-elb.io/*`（public/bandwidth-size 等） | 读 | 从 LBC -> PSMDB CR -> Service 传入的配置参数 |
| `huawei-elb.io/elb-id` | 写 | ELB 实例 ID（顶层资源标识，唯一写回的注解） |
| `huawei-elb.io/last-known-params` | 写 | 上次同步的参数快照（用于变更检测） |
| `huawei-elb.io/acl-id` | 写 | ACL IP 组 ID（如有 source ranges） |
| `huawei-elb.io/acl-status` | 写 | ACL 状态 on/off |
| `huawei-elb.io/acl-type` | 写 | ACL 类型 white |
| `huawei-elb.io/elb-cleanup` | 写 | finalizer（删除时清理 ELB） |
| `huawei-elb.io/acl-cleanup` | 写 | finalizer（删除时清理 ACL IP 组） |

**注意**：不再写任何 `kubernetes.io/elb.*` 注解。不再写 `listener-map`/`pool-map`/`healthcheck-map`。

### 调整后的 reconcileCreate 流程

```
1. NetworkDetector.Detect() -> VPC ID, subnet ID, AZs, node IPs
2. 读取 huawei-elb.io/* 参数
3. ELB 名称 = k8s-{ns_8}-{name_8}-{uid_10}
4. CreateELB(name, vpcID, subnetID, AZs, isPublic, bandwidth) -> elbID, publicIP
5. 等待 ELB ACTIVE
6. 为每个 Service port:
   a. CreateListener(elbID, port, TCP)
   b. CreatePool(listenerID, ROUND_ROBIN, TCP) -> poolID
   c. CreateHealthCheck(poolID, TCP, backendPort)
   d. 为每个 node 添加 Member(poolID, nodeIP, nodePort)
7. 创建 ACL IP group（如有 source ranges）
8. 写 huawei-elb.io/elb-id 注解
9. 写 service.status.loadBalancer.ingress = [{IP: vip/eip}]
10. 加 huawei-elb.io/elb-cleanup finalizer
```

### 调整后的 reconcileUpdate 流程

```
1. 读取 ELB ID（从 huawei-elb.io/elb-id 注解）
2. ListListeners(elbID) 获取当前 listener 列表
3. diff Service ports vs listeners:
   - 新增 port: CreateListener + CreatePool + CreateHealthCheck + AddMembers
   - 删除 port: DeleteListener（级联删 pool/hc/members）
4. node 列表变化: ListMembers(poolID) -> diff -> Add/Remove Members
5. 带宽/公网类型变化: UpdateELB
6. source ranges 变化: 更新 ACL IP group
7. 更新 service.status.loadBalancer.ingress（如 VIP/EIP 变化）
```

### 调整后的 reconcileDelete 流程

```
1. 读取 ELB ID（从 huawei-elb.io/elb-id 注解）
2. DeleteELB(elbID)（级联删除 listener/pool/member/hc）
3. 删除 ACL IP group（如有）
4. 移除 finalizer
5. 清除 service.status.loadBalancer.ingress
```

### 调整后的测试计划

| 测试 | 验证内容 |
|------|---------|
| `Test_CreatePath_DirectCreateELB` | CreateELB 调用，elb-id 注解写入，status 写入，finalizer 添加 |
| `Test_CreatePath_MultiPortListeners` | N 端口 -> N listener+pool+healthcheck |
| `Test_CreatePath_WaitsForELBActive` | 等待 ELB ACTIVE 再创建子资源 |
| `Test_CreatePath_MembersFromNodes` | 每个 node 添加为 pool member（nodeIP + NodePort） |
| `Test_UpdatePath_AddPort` | 新端口 -> listener+pool+hc 创建（ListListeners 检测） |
| `Test_UpdatePath_RemovePort` | 删端口 -> DeleteListener 级联 |
| `Test_UpdatePath_NodeAdded` | node 新增 -> 新 member 添加 |
| `Test_UpdatePath_NodeRemoved` | node 删除 -> member 移除 |
| `Test_UpdatePath_BandwidthChange` | UpdateELB 调用 |
| `Test_DeletePath_CleansUpELB` | DeleteELB 调用，finalizer 移除，status 清除 |
| `Test_Status_LoadBalancerIngress` | status.loadBalancer.ingress 写入 |
| `Test_Naming_UIDSuffix` | ELB 名称含 UID 后缀，避免重名 |
