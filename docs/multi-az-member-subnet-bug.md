# 多 AZ 场景 Member 子网 Bug

## 背景

方案B（直接调华为云 ELB API）实现完成后，在审查多 AZ 部署场景时发现一个 bug：**跨子网节点的 ELB member 创建会失败**。

## 触发条件

**只有一种情况会触发**：集群节点跨多个子网（多 AZ 部署，且不同 AZ 用不同子网）。

华为云 CCE 集群创建时有两种节点网络模式：

- **单子网**：所有节点在同一 VPC 子网 → 不受影响
- **多子网/多 AZ**：节点分布在多个 AZ，每个 AZ 一个子网 → 触发 bug

单 AZ 单子网集群（如当前 `cn-north-4a` + 子网 `e8e541e1-...`）不会触发。

## Bug 根因

### ELB 资源层级

华为云 ELB 的资源结构：

```
ELB（负载均衡器实例）
 └── Listener（监听器，对应一个端口，如 27017）
      └── Pool（后端服务器组）
           ├── Member 1  (192.168.1.10:30001)  ← node-1
           ├── Member 2  (192.168.1.11:30001)  ← node-2
           ├── Member 3  (192.168.2.10:30001)  ← node-3
           └── Member 4  (192.168.2.11:30001)  ← node-4
```

**Member = 后端服务器**，是真正接收流量的那台机器。本项目用 NodePort 模式，每个 member 的字段：

| 字段 | 值 | 来源 |
|---|---|---|
| `address` | 节点内网 IP（如 `192.168.1.10`） | `node.status.addresses[NodeInternalIP]` |
| `protocolPort` | NodePort 端口（如 `30001`） | `service.spec.ports[].nodePort` |
| `subnet_cidr_id` | 节点所在子网的 neutron ID | 从节点标签 `node.kubernetes.io/subnetid` 查出 |

华为云 ELB API 要求：**每个 member 的 `subnet_cidr_id` 必须和 `address` 在同一网段**。ELB 用这个信息建路由表，知道怎么把流量送到这台机器。

### 修复前的代码逻辑

假设集群 4 个节点分布在 2 个 AZ：

| 节点 | AZ | 子网 (virsubnet) | 节点 IP |
|---|---|---|---|
| node-1 | cn-north-4a | subnet-A (192.168.1.0/24) | 192.168.1.10 |
| node-2 | cn-north-4a | subnet-A | 192.168.1.11 |
| node-3 | cn-north-4b | subnet-B (192.168.2.0/24) | 192.168.2.10 |
| node-4 | cn-north-4b | subnet-B | 192.168.2.11 |

修复前代码的问题：

1. **`detector.go` 的 `Detect()`**：只取**第一个节点**的 virsubnet，查询它的 neutron subnet ID，所有后续逻辑都用这一个 `subnetID`。
2. **`createListenerStack` / `syncAllPoolMembers`**：所有 member 都用同一个 `subnetID` 创建。
3. **`AddMember` / `SyncMembers`**：只接受一个 `subnetCID` 参数，无法 per-member 指定。

结果：调用 `AddMember(pool, 192.168.2.10, nodePort, subnet-A)` 时，`192.168.2.10` 属于 subnet-B，但代码告诉 ELB 它在 subnet-A（192.168.1.0/24）—— API 校验失败：

```
CreatingElbMemberFailed: SubnetCidrId does not match the address's subnet
```

## 连锁后果

### 1. 创建阶段（`reconcileCreate` → `createListenerStack`）

- listener、pool、healthcheck 都创建成功
- 到第 3 个 member（node-3）时 API 报错
- 函数返回 error → reconcile 失败 → **Service 永远拿不到 EXTERNAL-IP**
- ELB 已创建但只有 2/4 member，处于半成品状态
- 每次 requeue 重试都失败在同一个地方

### 2. 更新阶段（`syncAllPoolMembers`）

- 节点扩容加入新 AZ 时，新 member 创建失败
- 旧 member 仍正常，但新节点永远加不进后端池
- 流量只打到旧 AZ 的节点，**负载不均**

### 3. 删除阶段

不受影响（删 ELB 不涉及 member 子网）。

## 修复方案

核心思路：**每个 member 用各自节点所在的子网**。

### 改动 3 个文件

#### 1. `internal/huaweicloud/detector.go`

- `DetectedParams` 增加 `SubnetMap map[string]string`（virsubnetID → neutron subnetID，所有节点的子网映射）
- `Detect()` 改为收集所有节点的 virsubnet，逐一查询 neutron subnet ID，构建完整 `SubnetMap`
- VIP 子网仍用第一个节点的子网（ELB VIP 只需一个子网）
- 新增 `GetNeutronSubnet(virsubnetID)` 方法：带缓存查询，缓存命中直接返回，未命中查 API 并写入缓存

#### 2. `internal/huaweicloud/member.go`

- `MemberTarget` 增加 `SubnetID string` 字段
- `SyncMembers` 签名移除 `subnetCID` 参数，改用 per-member `d.SubnetID`

#### 3. `internal/controller/service_controller.go`

- 新增 `NodeBackend{IP, VirsubnetID}` 结构体（带节点子网信息）
- `getNodeIPs()` → `getNodeBackends()`：返回 `[]NodeBackend`，从节点标签 `node.kubernetes.io/subnetid` 取 virsubnet ID
- `createListenerStack` 和 `syncAllPoolMembers`：每个 member 调 `r.NetworkDetector.GetNeutronSubnet(be.VirsubnetID)` 取各自子网，再传给 `AddMember` / `MemberTarget`

### 修复后效果

每个 member 创建时用各自节点的子网：

- node-1/2 → 查 subnet-A → 用 subnet-A 的 neutron ID ✅
- node-3/4 → 查 subnet-B → 用 subnet-B 的 neutron ID ✅

API 校验通过，4 个 member 全部创建成功。

## 验证

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./...` ✅（controller 2.668s, huaweicloud 1.804s）

## 何时需要关注

- **短期不用**：当前集群单 AZ 单子网，bug 不会触发
- **需要关注的场景**：
  - 集群扩容到第二个 AZ
  - 新建多 AZ 集群（CCE 控制台创建时勾选多个 AZ）
  - 跨子网迁移节点

修复已合入代码，上述场景均不会出问题。
