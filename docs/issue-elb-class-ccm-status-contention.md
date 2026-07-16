# Bug（已修复）：`kubernetes.io/elb.class` 残留导致 CCM 清空 Service status，集群卡 initializing

## 问题概述

mysql-tpd 集群（PXC）Pod 全部 Ready（PXC 3/3 wsrep Synced，HAProxy 3/3），但 CR `status.status = "initializing"` 持续不转 ready。

## 现象

```
Pod 状态：全部 Running/Ready
  mysql-tpd-pxc-0/1/2     1/1 Running
  mysql-tpd-haproxy-0/1/2  2/2 Running

PXC wsrep 状态：3 节点 Synced / Primary / ON

StatefulSet：3/3，currentRevision == updateRevision

CR status:
  status: "initializing"
  conditions:
    ready=True (10:10:59)
    initializing=True (10:11:00)  ← 1秒后又变回初始化
```

## 根因

### 竞争链路

```
OpenEverest LBC 配置 (spec.annotations):
  "huawei-elb.io/public": "false"
  "kubernetes.io/elb.class": "union"        ← OpenEverest 习惯性添加

      │ OpenEverest 同步到 Service
      ▼

Service annotations:
  kubernetes.io/elb.class: "union"          ← CCM 接管信号
  huawei-elb.io/elb-id: "c7c15654-..."     ← 我们的 controller 写的（CCM 不认）
  (无 kubernetes.io/elb.id)                 ← CCM 找不到

      │ CCM 每 ~3 分钟 EnsureLoadBalancer
      ▼

CCM: 有 elb.class -> 接管
CCM: 无 elb.id -> GetLoadBalancerFailed
CCM: 清空 status.loadBalancer.ingress       ← 竞争源头！

      │ 我们的 controller 每 ~2 秒 reconcile
      ▼

Controller: 检测 status.ingress 为空
Controller: ShowELB 获取 IP -> 写 status.loadBalancer.ingress

      │ 3 分钟后 CCM 再次清空
      ▼

死循环：status 在有 IP 和空之间反复跳变
```

### 实测证据

连续查询 `mysql-tpd-haproxy` Service status：

```
第1次查询: {"ingress":[{"ip":"192.168.0.165","ipMode":"VIP"}]}  ← controller 写的
第2次(3s后): {}                                                  ← CCM 清空了
第3次(8s后): {}                                                  ← 还是空的
```

Events 确认 CCM 在处理：

```
Warning  GetLoadBalancerFailed  service/mysql-tpd-haproxy
  Details: service annotation(kubernetes.io/elb.id) or service.spec.loadBalancerIP is not defined, skip.
```

### 为什么 operator 卡 initializing

OpenEverest operator 等待 `mysql-tpd-haproxy`（主 Service，5 端口）的 `status.loadBalancer.ingress` 稳定有 IP。由于 CCM 不断清空，operator 无法确认 ELB 就绪，集群状态卡在 `initializing`。

### 与方案B 原始问题的关系

**同一个根因，不同症状**：

| | 原始问题（方案B 立项原因） | 现在的问题 |
|---|---|---|
| **根因** | `kubernetes.io/elb.class` 唤醒 CCM | `kubernetes.io/elb.class` 唤醒 CCM |
| **触发方** | OpenEverest 在 LBC 加 `elb.class` | OpenEverest 在 LBC 加 `elb.class` |
| **CCM 行为** | 创建 ELB + 写回 `elb.id` | 找不到 `elb.id` + **清空 status** |
| **冲突方** | OpenEverest 同步改 `elb.id` -> forbidden | controller 写 status 被清空 |
| **结果** | 集群 forbidden 报错 | 集群卡 initializing |

方案B 解决了 autocreate 路径的 `elb.id` 冲突，但没处理 `elb.class` 残留的情况。OpenEverest 的 LBC 模板习惯性带 `elb.class: union`，只要这个注解在，CCM 就会介入。

## 实测验证（2026-07-15）

### 测试1：能否写 `kubernetes.io/elb.id`
- ✅ **可以写**（webhook 只校验 UUID 格式，不校验存在性）
- 假 UUID 格式字符串 `aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee` 写入成功
- 真实 ELB ID 写入成功

### 测试2：能否删 `kubernetes.io/elb.class`
- ❌ **不可以删**
```
Error from server (Forbidden): services "webhook-test" is forbidden: 
can't modify service elb [kubernetes.io/elb.class] annotation
```
- CCE webhook 保护 `kubernetes.io/elb.*` 注解不可变

### 测试3：写 `kubernetes.io/elb.id` 后 CCM 行为

用真实 ELB（controller 创建内网 ELB `f612880b-...`，IP `192.168.0.181`）测试：

| 维度 | 结果 |
|---|---|
| CCM 清空 status | ✅ **不再清空**（60秒内 status 稳定） |
| CCM 管理 listener | ❌ 尝试创建已存在的 listener，报 409 |
| CCM 管理 pool/member | ✅ **不修改**（卡在 listener 创建阶段） |
| CCM 管理 healthcheck | ✅ **不修改**（同上） |
| CCM finalizer | ✅ 自动添加 `service.kubernetes.io/load-balancer-cleanup` |

CCM 持续报错（每 1-2 分钟一次）：
```
CreatingLoadBalancerFailed: Create listener(k8s_TCP_3306) error:
  EnsureCreateListener: Failed to CreateListener:
  status_code: 409, error_msg: Load Balancer f612880b-... 
  already has a listener with protocol_port of 3306.
```

### 测试4：删除时 CCM finalizer
- ✅ CCM finalizer `service.kubernetes.io/load-balancer-cleanup` **自动移除**（10秒内）
- CCM 发现 listener 已存在/不存在就放弃，移除自己的 finalizer
- **不卡删除**

### 测试5：删除时 controller 行为 -- 发现新 bug
- ❌ **Controller 没收到删除事件！**
- 原因：Service 有 `kubernetes.io/elb.id`，被 `shouldReconcileService` 的 `hasLegacyELBID` 跳过
- `shouldReconcileService` 检查 `hasLegacyELBID` -> true -> return false（跳过）
- Service 卡在 Terminating（只剩 `huawei-elb.io/elb-cleanup` finalizer）

```go
// service_utils.go 当前逻辑：有 kubernetes.io/elb.id 就跳过
if hasLegacyELBID(svc) {
    return false  // ❌ 我们管理的 Service 也会被跳过！
}
```

## 最终方案：方案1修订版

基于实测结果，方案1（写 `kubernetes.io/elb.id`）可行，但需要同时修改 predicate：

### 1. 写 `kubernetes.io/elb.id`（reconcileCreate）
```go
// 创建 ELB 后，同时写两个注解
latest.Annotations[huaweicloud.AnnotationELBID] = info.ID       // huawei-elb.io/elb-id
latest.Annotations["kubernetes.io/elb.id"] = info.ID         // CCM 认得这个，不再清空 status
```

### 2. 修改 predicate（shouldReconcileService）
```go
// 旧逻辑：有 kubernetes.io/elb.id 就跳过
if hasLegacyELBID(svc) {
    return false
}

// 新逻辑：有 kubernetes.io/elb.id 但没有 huawei-elb.io/elb-id 才跳过
// （有 huawei-elb.io/elb-id 说明是我们管理的，kubernetes.io/elb.id 只是为了防 CCM 清空 status）
if hasLegacyELBID(svc) && !hasManagedELBID(svc) {
    return false
}
```

### 3. reconcileUpdate 确保注解存在
OpenEverest 可能覆盖 `kubernetes.io/elb.id`（webhook 应该阻止），但保险起见每次 reconcile 确保存在。

### 4. reconcileDelete 正常删除
- Controller 能收到删除事件（predicate 不再跳过）
- CCM finalizer 自动移除（实测验证）
- Controller 删除 ELB stack -> 移除自己的 finalizer -> Service 删除完成

### 5. CCM 409 噪音（预期行为）
- CCM 持续尝试创建已存在的 listener，报 409
- 不影响功能：listener 已由 controller 创建，CCM 无法覆盖
- 不修改 pool/member/healthcheck：CCM 卡在 listener 创建阶段
- 文档说明这是预期噪音

## 实现清单

- [x] `reconcileCreate`：创建 ELB 后写 `kubernetes.io/elb.id`
- [x] `shouldReconcileService`：修改 `hasLegacyELBID` 检查逻辑
- [x] `Reconcile` 入口 guard：同样修改（关键修复，否则 update 路径被阻断）
- [x] `reconcileUpdate`：确保 `kubernetes.io/elb.id` 存在
- [x] 单元测试：predicate 逻辑修改（`withBothELBIDs` 测试用例）
- [ ] 单元测试：reconcileCreate 写双注解（已有集成测试覆盖，未单独写单元测试）
- [ ] CCE 验证：创建集群（带 elb.class LBC）不再卡 initializing
- [ ] CCE 验证：删除集群 ELB 正确清理
- [x] 文档：Troubleshooting 加 CCM 409 噪音说明

## 实现完成（2026-07-15）

方案1修订版已实现，代码已通过 `go build` / `go vet` / `go test` 全部验证。

### 实际代码变更

**`internal/controller/service_utils.go`**：
- 新增常量 `ccmELBIDAnnotation = "kubernetes.io/elb.id"`
- `shouldReconcileService`：`hasLegacyELBID(svc) && !hasManagedELBID(svc)` 才跳过
  - 有 `huawei-elb.io/elb-id` 说明是我们管理的 Service，不跳过
- `hasLegacyELBID`：用常量替换硬编码字符串

**`internal/controller/service_controller.go`**（4 处修改）：
- `Reconcile` 入口 guard：`(hasLegacyELBID(svc) && !hasManagedELBID(svc)) || hasLegacyAutocreate(svc)` 才跳过
  - **关键修复**：不加这个的话，写了 `elb.id` 后下一次 reconcile 会被跳过，永远进不了 update 路径
- `reconcileCreate` 反查恢复路径：同时写 `huawei-elb.io/elb-id` + `kubernetes.io/elb.id`
- `reconcileCreate` 创建后 patch：同时写两个注解
- `reconcileUpdate`：确保 `kubernetes.io/elb.id` 存在（防 OpenEverest 覆盖后丢失）

**`internal/controller/service_utils_test.go`**：
- 新增 `withBothELBIDs` 测试用例（两个注解都在时必须 reconcile）

### CCE 部署状态

- [ ] 构建新镜像（含 ACL finalizer 修复 + ELB 删除日志 + CCM status 修复）推送 SWR
- [ ] 部署到 CCE 替换当前 `:planb`
- [ ] 验证 mysql-tpd 集群不再卡 initializing
- [ ] 验证删除集群 ELB 正确清理
- [x] 文档：Troubleshooting 加 CCM 409 噪音说明
