# Bug: ACL IP Group 泄漏 - OpenEverest 覆盖注解导致删除时丢失 ipGroupID

**状态**: 待修复
**发现时间**: 2026-07-15
**发现场景**: mongodb-7rx 集群创建+删除全生命周期日志审计

## 现象

mongodb-7rx（3 节点 PSMDB）删除后，3 个 ACL IP group 未被清理，成为孤儿资源：

| Service | ELB ID | IP Group ID | ELB | EIP | IP Group |
|---|---|---|---|---|---|
| mongodb-7rx-rs0-0 | `1de647b1-6047-4d77-bd07-7f852d061c99` | `4d10a9a7-e6c3-441e-8796-3385d4d140be` | ✅ 已删 | ✅ 已删 | ❌ **泄漏** |
| mongodb-7rx-rs0-1 | `d2ab0f7f-4436-48a0-ba74-5690e917365c` | `e9cd4530-f293-48d9-bf55-610f169c887a` | ✅ 已删 | ✅ 已删 | ❌ **泄漏** |
| mongodb-7rx-rs0-2 | `bf11b0ba-9883-47f3-b832-107243aa458e` | `c50b158f-b154-4410-a763-8c48089b6304` | ✅ 已删 | ✅ 已删 | ❌ **泄漏** |

ELB controller 日志中**完全没有 ACL IP group 删除日志**，但 ELB/EIP 删除日志完整。

## 根因

`reconcileDelete`（`internal/controller/service_controller.go:506`）依赖 `huawei-elb.io/acl-id` 注解获取 ipGroupID：

```go
// reconcileDelete line 530-539
if controllerutil.ContainsFinalizer(svc, aclCleanupFinalizer) {
    if ipGroupID := svc.Annotations[aclIDAnnotation]; ipGroupID != "" {
        if err := huaweicloud.DeleteIPGroup(r.ELBClient, ipGroupID); err != nil {
            ...
        }
    }
    // 即使 ipGroupID 为空，也会移除 finalizer
}
```

**问题链**：
1. OpenEverest 在 Service 生命周期中会反复 reconcile，用 LBC 模板的 `spec.annotations` 覆盖 Service 的 `huawei-elb.io/*` 注解
2. LBC 模板（如 `test-11`）只有 `huawei-elb.io/public: false`，**没有** `huawei-elb.io/acl-id`
3. OpenEverest 覆盖时把 controller 写入的 `huawei-elb.io/acl-id` 注解清空
4. Service 删除时，`reconcileDelete` 发现 `aclIDAnnotation` 为空 -> 跳过 `DeleteIPGroup` -> IP group 泄漏
5. 但 `aclCleanupFinalizer` 仍然被移除（因为代码在 finalizer 检查通过后无条件移除）

**证据**：对比当前存活的 mongodb-cvf-rs0-0 Service，其 annotations 中 `huawei-elb.io/acl-id` 存在，但 `last-known-params` 里的 key 集合和 LBC 模板不一致 -- 说明 OpenEverest 的覆盖是部分覆盖（只覆盖 LBC 模板里有的 key），但某些时机会全量覆盖导致 controller 写的 key 丢失。

## 修复方案

`reconcileDelete` 中，当 `aclCleanupFinalizer` 存在但 `aclIDAnnotation` 为空时，通过 IP group 名称反查并删除：

```go
if controllerutil.ContainsFinalizer(svc, aclCleanupFinalizer) {
    ipGroupID := svc.Annotations[aclIDAnnotation]
    if ipGroupID == "" {
        // Annotation lost due to OpenEverest overwrite -- recover by name.
        ipGroupName := "acl-" + svc.Namespace + "-" + svc.Name
        recoveredID, findErr := huaweicloud.FindIPGroupByName(r.ELBClient, ipGroupName)
        if findErr != nil {
            logger.Error(findErr, "finding ACL IP group by name for deletion", "name", ipGroupName)
            return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
        }
        ipGroupID = recoveredID
    }
    if ipGroupID != "" {
        if err := huaweicloud.DeleteIPGroup(r.ELBClient, ipGroupID); err != nil {
            if !huaweicloud.IsNotFoundError(err) {
                logger.Error(err, "deleting ACL IP group, will retry", "ipGroupID", ipGroupID)
                return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
            }
        }
        logger.Info("ACL IP group deleted", "ipGroupID", ipGroupID)
    }
    // remove finalizer
}
```

## 关联文件

- `internal/controller/service_controller.go:530-546`（`reconcileDelete` ACL 删除分支）
- `internal/huaweicloud/elb.go:444`（`FindIPGroupByName`，已存在，可直接复用）

## 影响

- **资源泄漏**：每次删除有 source ranges 的 OpenEverest Service 都会泄漏一个 ACL IP group
- **配额消耗**：华为云 ACL IP group 有配额限制，长期泄漏会耗尽配额导致新 Service 创建失败
- **费用**：ACL IP group 可能产生保管费用（需确认）

## 待清理的泄漏 IP Group

以下 3 个 IP group 需在华为云控制台手动清理：
- `4d10a9a7-e6c3-441e-8796-3385d4d140be`（mongodb-7rx-rs0-0）
- `e9cd4530-f293-48d9-bf55-610f169c887a`（mongodb-7rx-rs0-1）
- `c50b158f-b154-4410-a763-8c48089b6304`（mongodb-7rx-rs0-2）
