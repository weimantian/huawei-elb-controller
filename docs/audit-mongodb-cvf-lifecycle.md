# Audit: mongodb-cvf 全生命周期日志审计

**审计时间**: 2026-07-15
**审计范围**: mongodb-cvf（3 节点 PSMDB，内网 ELB）创建+删除全流程
**ELB controller 版本**: `:planb-v2`（commit `596d8af`）
**集群状态**: 已删除

## 审计摘要

| 维度 | 结论 |
|---|---|
| ELB 创建 | ✅ 正常（3 个内网 ELB + listener + pool + member + healthcheck + ACL） |
| ELB 删除 | ❌ **rs0-0 的 ELB 未删除（泄漏）**，rs0-1/rs0-2 正常 |
| ACL IP group 删除 | ❌ **3 个全部泄漏**（同 `bug-acl-ipgroup-leak-on-delete.md`） |
| CCM 行为 | ⚠️ 噪音（`listener is empty` / `GetLoadBalancerFailed`），不影响功能 |
| OpenEverest operator | ✅ 正常（reconcile 循环 + DBC 删除） |
| PSMDB operator | ⚠️ 删除时有竞争 error（`Operation cannot be fulfilled: the object has been modified`），但不影响 DBC 删除 |
| Pod 生命周期 | ✅ 正常（3 个 pod 创建 -> 就绪 -> kill -> 删除） |
| PVC | ✅ 正常（3 个 PVC 随 StatefulSet 删除） |

## 时间线

```
13:08:56  ELB controller 创建 ELB #1 (rs0-0, 内网, f9cf08e9)
13:09:02  ELB #1 created, 开始等 ACTIVE
13:09:06  ELB #1 listener stack + ACL 绑定完成
13:09:21  ELB controller 创建 ELB #2 (rs0-1, 内网, edaa77e7)
13:09:25  ELB #2 created
13:09:27  ELB #2 fully provisioned
13:09:43  ELB controller 创建 ELB #3 (rs0-2, 内网, 479729f4)
13:09:47  ELB #3 created
13:09:49  ELB #3 fully provisioned
13:09:50  PSMDB replset 初始化, rs0-0 primary
13:10:27  OpenEverest 最后一次正常 reconcile
          --- 集群稳定运行 ~5 分钟 ---
13:15:33  DBC 删除触发, OpenEverest + PSMDB operator 开始清理
13:15:34  PSMDB: deleting rs pods -> StatefulSet -> PVC -> secret
13:15:34  PSMDB ERROR: ensure external service for rs0-1: object has been modified (竞争)
13:15:34  rs0-2: ELB controller 走 reconcileDelete -> 删除 healthcheck/member/pool/listener/ELB ✅
13:15:34  rs0-0: ELB controller 走 reconcileCreate 反查（注解被覆盖）-> patch 失败 Service not found
13:15:37  rs0-1: ELB controller 走 reconcileDelete -> 删除 healthcheck/member/pool/listener/ELB ✅
13:15:40  rs0-1 ELB deleted ✅
13:17:43  PSMDB: cluster not found, deleting telemetry job (清理完成)
```

## 发现的问题

### 问题1: rs0-0 ELB 泄漏（严重 - 新 bug）

**现象**: 3 个 ELB 只删除了 2 个，rs0-0 的 ELB `f9cf08e9` 及其子资源（listener/pool/member/healthcheck）全部泄漏。

**根因**: PSMDB operator 在 DBC 删除时与 ELB controller 产生竞争：

1. PSMDB operator 的 `ensureExternalServices` 用 `Update`（不是 `Patch`）更新 Service，其本地副本没有 controller 写的 `huawei-elb.io/elb-id` 注解 -> **覆盖清空注解**
2. ELB controller reconcile 时发现没有 `elb-id` -> 走 `reconcileCreate` 反查恢复路径
3. 反查到 ELB -> 尝试 `patchWithRetry` 恢复注解 + finalizer
4. 但此时 Service 已被 PSMDB operator 删除（StatefulSet 级联删除）-> patch 失败 `Service not found`
5. 后续 reconcile `r.Get` 返回 NotFound -> `IgnoreNotFound` -> 返回 OK
6. **ELB 永远不会被删除**（`reconcileDelete` 从未被触发）

**关键日志证据**:
```
[13:15:34] rs0-0 Reconciling Service
[13:15:34] rs0-0 Found existing ELB by name, restoring annotation (走 create 反查，不是 delete!)
[13:15:34] rs0-0 ERROR: restoring ELB ID annotation - Service "mongodb-cvf-rs0-0" not found
```

对比 rs0-1/rs0-2 正常删除:
```
[13:15:34] rs0-2 Deleting health check -> member -> pool -> listener -> ELB deleted ✅
[13:15:37] rs0-1 Deleting health check -> member -> pool -> listener -> ELB deleted ✅
```

**为什么 rs0-0 不同**: rs0-0 恰好在 Service 被删除的瞬间走了反查恢复路径（注解被 PSMDB Update 覆盖），而 rs0-1/rs0-2 的注解在删除时还在，走了正常的 `reconcileDelete`。

### 问题2: 3 个 ACL IP group 泄漏（严重 - 同 `bug-acl-ipgroup-leak-on-delete.md`）

| Service | IP Group ID | ELB 状态 | IP Group 状态 |
|---|---|---|---|
| rs0-0 | `e0b6ff68-9b3c-467e-a8fe-959f1418b32f` | ❌ 泄漏 | ❌ 泄漏 |
| rs0-1 | `1283483f-3f7f-4a42-9ad6-a2c625083d8d` | ✅ 已删 | ❌ 泄漏 |
| rs0-2 | `55827a3d-b956-479e-ad6d-82d05f03caa2` | ✅ 已删 | ❌ 泄漏 |

rs0-0 的 IP group 泄漏是因为整个 ELB 都泄漏了（reconcileDelete 未触发）。
rs0-1/rs0-2 的 IP group 泄漏是 `bug-acl-ipgroup-leak-on-delete.md` 描述的 bug（注解被覆盖导致 `acl-id` 丢失）。

### 问题3: PSMDB operator 竞争 error（轻微）

PSMDB operator 在删除时报：
```
ERROR: ensure external service for replset rs0: Operation cannot be fulfilled on services "mongodb-cvf-rs0-1": the object has been modified
```

这是 PSMDB operator 用 `Update` 更新 Service 时和 ELB controller 的 `Patch` 产生竞争。PSMDB operator 重试后成功，不影响 DBC 删除流程。但这个竞争是问题1的根因。

### 问题4: CCM 噪音（已知）

事件日志显示 CCM 报：
- `GetLoadBalancerFailed: service annotation(kubernetes.io/elb.id) or service.spec.loadBalancerIP is not defined, skip.`
- `UpdateLoadBalancerFailed: listener is empty`

这是方案B 回退（不写 `kubernetes.io/elb.id`）的预期行为，不影响功能。

## 泄漏资源汇总

| 资源类型 | 创建数 | 删除数 | 泄漏数 | 泄漏 ID |
|---|---|---|---|---|
| ELB | 3 | 2 | **1** | `f9cf08e9-c328-42d6-b3f7-a5c03a0a3072` |
| Listener | 3 | 2 | **1** | (附属于泄漏 ELB) |
| Pool | 3 | 2 | **1** | `2d35cb16-8f24-44d6-907b-eba2ed4ea1be` |
| Member | 15 | 10 | **5** | (附属于泄漏 pool) |
| HealthCheck | 3 | 2 | **1** | (附属于泄漏 pool) |
| ACL IP Group | 3 | 0 | **3** | `e0b6ff68...`, `1283483f...`, `55827a3d...` |

## 待手动清理

华为云控制台手动清理以下资源：

**rs0-0 泄漏的 ELB 及子资源**（删除 ELB 会级联删除子资源）：
- ELB: `f9cf08e9-c328-42d6-b3f7-a5c03a0a3072`

**3 个泄漏的 ACL IP group**：
- `e0b6ff68-9b3c-467e-a8fe-959f1418b32f`（rs0-0）
- `1283483f-3f7f-4a42-9ad6-a2c625083d8d`（rs0-1）
- `55827a3d-b956-479e-ad6d-82d05f03caa2`（rs0-2）

## 与 mongodb-7rx 审计对比

| 维度 | mongodb-7rx | mongodb-cvf |
|---|---|---|
| ELB 类型 | 公网 | 内网 |
| ELB 创建 | 3/3 ✅ | 3/3 ✅ |
| ELB 删除 | 3/3 ✅ | 2/3 ❌ (rs0-0 泄漏) |
| EIP 删除 | 3/3 ✅ | N/A (内网无 EIP) |
| ACL IP group 删除 | 0/3 ❌ | 0/3 ❌ |
| ELB 泄漏根因 | 无 | PSMDB Update 竞争 |
| IP group 泄漏根因 | 注解被覆盖 | 注解被覆盖（同 bug） |
