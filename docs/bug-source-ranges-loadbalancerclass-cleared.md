# BUG 分析：编辑 Source Range 后数据库集群变 Down

## 问题概述

在 everest 面板编辑数据库集群的 Source Range（`loadBalancerSourceRanges`）并保存后，MySQL/MongoDB 数据库集群状态变为 Down，外部连接中断。PostgreSQL 不受影响。

## 现象

| 现象 | MySQL (PXC) / MongoDB (PSMDB) | PostgreSQL |
|---|---|---|
| 面板编辑 Source Range 保存后 | **集群变 Down** | 正常 |
| Service `loadBalancerClass` | **被清空（null）** | 保留 |
| DB operator 日志 | Update Service 被 K8s 拒绝 | 正常 |
| DBC status | error / unavailable | ready |
| 外部连接 | 超时（ELB 流量中断） | 正常 |

## 根因分析

### 1. Webhook 仅拦截 CREATE

修复前，MutatingWebhookConfiguration 的 `operations` 只配置了 `["CREATE"]`：

```yaml
webhooks:
- operations: ["CREATE"]   # ← 只在创建时注入 class
  ...
```

Service 创建时 webhook 注入 `spec.loadBalancerClass: huawei-elb.io/direct-api`，CCM 看到非匹配 class 会跳过该 Service。但 **UPDATE 操作不经过 webhook**，class 没有保护。

### 2. DB operator 覆盖式 Update 清空 loadBalancerClass

不同 DB operator 更新 Service 的行为不同：

| DB operator | Update 行为 | loadBalancerClass |
|---|---|---|
| PXC (MySQL) | **覆盖式**：用 new Service object 调用 Update，字段没保留 class | **被清空（nil）** |
| PSMDB (MongoDB) | **覆盖式**：同 PXC | **被清空（nil）** |
| PG (PostgreSQL) | **保留式**：Update 时保留已有 class | **保留** |

PXC/PSMDB operator 在重建 Service 对象时没有拷贝 `loadBalancerClass` 字段，导致提交给 API Server 的 Update 请求中该字段为 nil。

### 3. K8s 拒绝 loadBalancerClass 从 non-null 改为 null

Kubernetes API 规定：`spec.loadBalancerClass` 一旦设置，**不可改为 null**。当 PXC/PSMDB operator 提交清空 class 的 Update 请求时：

```
Service "mysql-hbp-haproxy" is invalid: spec.loadBalancerClass: Invalid value: "null":
  field is immutable once set
```

这个 Update 被 K8s 拒绝。但 DB operator 的状态机已经认为 Update 提交了，导致 DBC status 进入 error，集群标记为 Down。

### 4. 链式影响

```
用户编辑 Source Range 保存
  ↓
DB operator (PXC/PSMDB) 覆盖式 Update Service（清空 loadBalancerClass）
  ↓
K8s API 拒绝（class 不可从 non-null 改 null）
  ↓
DB operator reconcile 失败，DBC status = error
  ↓
集群标记 Down，但 ELB 资源仍在（流量实际不中断，但面板显示 Down）
```

## 修复方案

**思路**：webhook 拦截 UPDATE 操作，在 DB operator 清空 class 时恢复它。不修改 DB operator，不修改 OpenEverest，只改 huawei-elb-controller。

### 改动 1：webhook operations 增加 UPDATE

`deploy/webhook.yaml` + `charts/.../webhook.yaml`：

```yaml
# 修复前
operations: ["CREATE"]

# 修复后
operations: ["CREATE", "UPDATE"]
```

### 改动 2：mutator Handle 增加 UPDATE 分支

`internal/webhook/service_mutator.go`：

```go
// UPDATE 分支：旧 Service 有我们 class + 新被清空时恢复
if oldSvc.Spec.LoadBalancerClass != nil &&
   *oldSvc.Spec.LoadBalancerClass == huaweicloud.LoadBalancerClass &&
   newSvc.Spec.LoadBalancerClass == nil {
    // 恢复 class
    newSvc.Spec.LoadBalancerClass = ptr.To(huaweicloud.LoadBalancerClass)
}
```

**恢复条件**（精确匹配，避免误伤）：
- 旧 Service 有我们的 `loadBalancerClass`（`huawei-elb.io/direct-api`）
- 新 Service 的 `loadBalancerClass` 被清空（nil）

**放行情况**（不恢复）：
- operator 自己保留 class（PG 行为）-> 新旧都有，不触发
- 其他 class（非我们的）-> 旧的不是我们的，不触发
- 新建的 Service -> 走 CREATE 分支

### 改动 3：单元测试

`internal/webhook/service_mutator_test.go` 新增 8 个测试（CREATE 4 + UPDATE 4）：

| 测试 | 场景 | 预期 |
|---|---|---|
| CREATE 无 class | 新 Service 无 class | 注入 class |
| CREATE 有其他 class | 已有别的 class | 不注入 |
| CREATE 有 CCM annotation | 有 elb.id | 不注入 |
| CREATE 非 LoadBalancer | type=ClusterIP | 不注入 |
| UPDATE class 被清空 | 旧有 class，新 nil | **恢复 class** |
| UPDATE class 保留 | 新旧都有 class | 不改 |
| UPDATE 改成其他 class | 旧我们，新别的 | 不改 |
| UPDATE 从无到有 | 旧 nil | 不改 |

## 验证

### 验证矩阵

三种 DB operator 全生命周期验证（CCE 集群，K8s v1.35.3）：

| 场景 | MongoDB (PSMDB) | MySQL (PXC) | PostgreSQL (PG) |
|---|---|---|---|
| 创建 | ✅ | ✅ | ✅ |
| Source Range 加网段 | ✅ class 保留 + ACL 更新 | ✅ class 保留 + ACL 更新 | ✅ class 保留 + ACL 更新 |
| Source Range 删网段 | — | — | ✅ class 保留 + ACL 更新 |
| 删除 | ✅ 无泄漏 | ✅ 无泄漏（含 EIP 回收） | — |

### 加网段验证（核心场景）

用户在面板把 Source Range 从 `10.0.0.0/8` 改为 `10.0.0.0/8,192.168.0.0/16`：

| 验证项 | 修复前 | 修复后 |
|---|---|---|
| Service `loadBalancerClass` | ❌ 被清空 | ✅ 保留 |
| DBC status | ❌ error / Down | ✅ ready |
| DB pod | ❌ 异常 | ✅ Running |
| ELB ACL IP group | 不更新 | ✅ 更新为新网段列表 |

ACL IP group 通过华为云 API 确认实际更新（非日志推断）：

```python
# 修复后：rs0-0 ACL IP group
['10.0.0.0/8', '192.168.0.0/16']  # ✅ 新网段已加入
```

### 删除流程验证

删除集群时 ELB 子资源按正确依赖顺序清理，无泄漏：

```
orphaned binding 检测
  -> healthcheck（健康检查）
  -> member（后端服务器）
  -> pool（后端服务器组）
  -> listener（监听器）
  -> ELB 实例
  -> EIP（仅 public ELB）
  -> ACL IP group（访问控制 IP 组）
  -> finalizer 移除 -> ELBBinding 被 GC
```

华为云 API 确认 ELB / IP group / EIP 全部删除，无残留。

## 三种 DB operator 对比

| 维度 | PSMDB (MongoDB) | PXC (MySQL) | PG (PostgreSQL) |
|---|---|---|---|
| Update 行为 | 覆盖式（清空 class） | 覆盖式（清空 class） | **保留式**（保留 class） |
| 修复前是否受影响 | ❌ 变 Down | ❌ 变 Down | ✅ 不受影响 |
| 修复后 webhook 作用 | 恢复 class | 恢复 class | 双保险（实际不触发） |
| LoadBalancer Service 数 | 3（每 member 1 个） | 2（haproxy + replicas） | 1（pgbouncer） |
| externalTrafficPolicy | Local（1 member/ELB） | Cluster（5 member/ELB） | Cluster（5 member/ELB） |
| ELB 类型 | internal | public | public |

PG operator 保留式 Update 是 PG 不受影响的根本原因。修复后 webhook 对 PG 是"双保险"--class 没被清空，webhook 检测到 class 仍在，不触发恢复逻辑。

## 涉及文件

| 文件 | 改动 |
|---|---|
| `deploy/webhook.yaml` | `operations: ["CREATE"]` -> `["CREATE", "UPDATE"]` |
| `charts/huawei-elb-controller/templates/webhook.yaml` | 同上（Helm chart 同步） |
| `internal/webhook/service_mutator.go` | Handle 增加 UPDATE 分支恢复 class + `hasCCMAnnotations` 辅助函数 |
| `internal/webhook/service_mutator_test.go` | 新增 8 个单元测试 |
| `internal/controller/service_controller.go` | UpdateFunc 改用 `reflect.DeepEqual(Spec)` 替代失效的 generation 比较 |
| `internal/webhook/service_mutator_test.go` | 新增 8 个单元测试 |

## 已修复问题：ACL 更新有 5 分钟延迟

### 现象

用户编辑 Source Range 保存后，Service spec 立即更新，但 ELB ACL IP group 最长等 5 分钟才同步。

### 根因

Service 是 K8s 少数**没有 `metadata.generation` 字段**的核心资源（其 REST strategy 没有递增 generation 的逻辑），所以 Service 的 generation 始终为 0。

UpdateFunc 用 generation 判断 spec 变更：

```go
if svcNew.Generation == svcOld.Generation {  // 0 == 0 永远 true
    return false  // 所有 spec 变更都被过滤
}
```

导致 sourceRanges 变更被过滤，不触发即时 reconcile，只能等 5 分钟周期兜底 requeue。

### 修复

改用 `reflect.DeepEqual(svcOld.Spec, svcNew.Spec)` 直接比较 spec 内容：

```go
if reflect.DeepEqual(svcOld.Spec, svcNew.Spec) {
    return false
}
```

DeepEqual 只比较 spec，不比较 metadata（annotation/label），所以：
- ✅ spec 变更（sourceRanges 等）立即触发 reconcile
- ✅ controller 自己写 annotation 不触发（避免死循环）
- ✅ 外部 annotation/label 变更不触发（减少噪声）

### 验证

手动 patch sourceRanges，观察 controller 日志：

```bash
kubectl patch svc postgresql-8ax-pgbouncer -n everest --type=merge \
  -p '{"spec":{"loadBalancerSourceRanges":["192.168.0.0/16","10.0.0.0/8"]}}'
```

修复前：patch 后 30 秒内无 reconcile 日志（等 5 分钟周期）。
修复后：patch 后几秒内触发 reconcile，ACL 即时更新。

### 现象

用户编辑 Source Range 保存后，Service spec 立即更新，但 ELB ACL IP group 最长等 5 分钟才同步。

### 原因

controller 的 `serviceRequeue = 5 * time.Minute`（周期性 reconcile 间隔）。Source Range 变更虽然 bump 了 Service generation，但 `UpdateFunc` 没有触发即时 reconcile（待排查 generation 检测逻辑）。

### 影响

- 修改后最长 5 分钟 ACL 才生效
- 这期间新网段客户端被旧 ACL 拒绝（连接超时）
- 但 Service 不变 Down，DBC 不变 Down -- 核心修复目标已达成

### 处理

作为单独问题定位修复，不在本次 Source Range 变 Down 的修复范围内。
