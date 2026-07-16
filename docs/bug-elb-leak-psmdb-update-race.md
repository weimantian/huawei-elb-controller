# Bug: ELB 泄漏 - PSMDB operator Update 竞争导致 reconcileDelete 未触发

**状态**: 待修复
**发现时间**: 2026-07-15
**发现场景**: mongodb-cvf 集群删除全生命周期日志审计

## 现象

mongodb-cvf（3 节点 PSMDB，内网 ELB）删除后，rs0-0 的 ELB 及其全部子资源未被删除：

| Service | ELB ID | ELB | Listener | Pool | Member | HealthCheck |
|---|---|---|---|---|---|---|
| rs0-0 | `f9cf08e9...` | ❌ 泄漏 | ❌ 泄漏 | ❌ 泄漏 | ❌ 泄漏(5) | ❌ 泄漏 |
| rs0-1 | `edaa77e7...` | ✅ | ✅ | ✅ | ✅ | ✅ |
| rs0-2 | `479729f4...` | ✅ | ✅ | ✅ | ✅ | ✅ |

## 根因

PSMDB operator 在 DBC 删除时用 `Update`（不是 `Patch`）更新 Service，与 ELB controller 产生竞争：

```
时间线 (13:15:33-13:15:34):

1. PSMDB operator: ensureExternalServices -> Update Service (本地副本无 huawei-elb.io/elb-id 注解)
   -> 覆盖清空 huawei-elb.io/elb-id 注解

2. ELB controller: Reconcile -> r.Get 拿到 Service (无 DeletionTimestamp, 无 elb-id)
   -> hasManagedELBID=false -> 走 reconcileCreate
   -> FindELBByName 找到 ELB -> patchWithRetry 恢复注解 + finalizer

3. PSMDB operator: deleting StatefulSet -> 级联删除 Service

4. ELB controller: patchWithRetry 失败 -> "Service not found"
   -> 返回 RequeueAfter

5. ELB controller: 下次 Reconcile -> r.Get 返回 NotFound -> IgnoreNotFound -> 返回 OK
   -> reconcileDelete 从未触发 -> ELB 永久泄漏
```

**关键点**: 如果 Service 有 `huawei-elb.io/elb-cleanup` finalizer，它不会被直接删除，会先进入 DeletionTimestamp 状态，ELB controller 会走 `reconcileDelete`。但 PSMDB operator 的 `Update` 用本地副本覆盖了整个 Service 对象，**可能把 finalizer 也覆盖掉了**。

**PSMDB operator 竞争证据**:
```
[13:15:34.172] ERROR: ensure external service for replset rs0:
  Operation cannot be fulfilled on services "mongodb-cvf-rs0-1": the object has been modified
```
这说明 PSMDB operator 的 Update 和 ELB controller 的 Patch 在同一秒产生冲突。

**ELB controller 竞争证据**:
```
[13:15:34] rs0-0 Reconciling Service
[13:15:34] rs0-0 Found existing ELB by name, restoring annotation (走 create 反查!)
[13:15:34] rs0-0 ERROR: restoring ELB ID annotation - Service "mongodb-cvf-rs0-0" not found
```

## 为什么 rs0-1 和 rs0-2 没有泄漏

时序差异：rs0-1 和 rs0-2 在 Service 被删除前，`huawei-elb.io/elb-id` 注解还在（未被 PSMDB Update 覆盖），所以 ELB controller 走了正常的 `reconcileDelete` 路径。

rs0-0 恰好在 Service 被删除的瞬间走了反查恢复路径（注解已被 PSMDB Update 覆盖），导致 patch 和 Service 删除产生竞争。

## 修复方案

### 方案A: reconcileCreate 反查恢复时处理 Service 已删除（最小修复）

在 `reconcileCreate` 的反查恢复 patch 失败时，检查是否是 NotFound 错误。如果是，说明 Service 已被删除但 ELB 还在 -> **主动删除 ELB**：

```go
if err := r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
    // ...
}); err != nil {
    if apierrors.IsNotFound(err) {
        // Service was deleted while we were trying to restore the annotation.
        // The ELB is orphaned -- delete it now to prevent a leak.
        logger.Info("Service gone during annotation restore, deleting orphaned ELB", "elbID", existing.ID)
        if delErr := r.deleteELBStack(logger, existing.ID); delErr != nil && !huaweicloud.IsNotFoundError(delErr) {
            logger.Error(delErr, "deleting orphaned ELB after Service disappeared", "elbID", existing.ID)
        }
        return ctrl.Result{}, nil
    }
    logger.Error(err, "restoring ELB ID annotation")
    return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
}
```

### 方案B: 用 owner reference 替代 annotation 关联（根治但改动大）

给 ELB 打上 Service 的 owner reference，Service 删除时 ELB 自动级联删除。但华为云 ELB 不支持 owner reference（不是 K8s 资源），需要用 finalizer + 外部垃圾回收机制模拟。

### 方案C: 定期扫描孤儿 ELB（兜底）

启动一个后台 goroutine 定期扫描所有 ELB，检查名字匹配 `k8s-{ns}-{name}-{uid}` 格式的 ELB 是否有对应的 Service，如果没有则删除。这是对方案A的兜底。

## 推荐

**方案A（立即修复）+ 方案C（兜底）**。

方案A 解决最常见的竞争场景（反查恢复时 Service 被删）。方案C 兜底处理其他可能导致 ELB 泄漏的边缘情况（如 controller pod 重启期间 Service 被删）。

## 关联文件

- `internal/controller/service_controller.go:147-169`（`reconcileCreate` 反查恢复分支）
- `internal/controller/service_controller.go:506-549`（`reconcileDelete`）
- `internal/controller/service_controller.go:554-633`（`deleteELBStack`）

## 影响

- **资源泄漏**：每次 DBC 删除时，如果 PSMDB operator 的 Update 恰好覆盖了 `elb-id` 注解，对应的 ELB 及全部子资源都会泄漏
- **费用**：泄漏的 ELB + listener + pool + member + healthcheck 持续计费
- **配额**：泄漏的 ELB 占用配额，长期累积会导致新 Service 创建失败
- **概率**：3 个 Service 中有 1 个泄漏（33%），说明概率不低

## 待手动清理

- ELB: `f9cf08e9-c328-42d6-b3f7-a5c03a0a3072`（删除 ELB 会级联删除 listener/pool/member/healthcheck）
