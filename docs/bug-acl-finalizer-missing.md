# Bug：ensureACL Update 分支缺失 finalizer 导致 ACL IP Group 残留

## 问题概述

删除数据库集群时，华为云 ACL IP Group（白名单 IP 组）残留，未被清理。

实测确认：
- mongodb-4e5：3 个 ACL IP group 残留（`acl-everest-mongodb-4e5-rs0-0/1/2`）
- mysql-yl1：删除阶段无任何 ACL 解绑/删除日志，确认未清理

## 根因

`ensureACL`（`internal/controller/service_controller.go:636`）有两个分支：

| 分支 | 触发条件 | 设 aclIDAnnotation | 加 aclCleanupFinalizer |
|---|---|---|---|
| **Create** | IP group 不存在（`FindIPGroupByName` 返回空） | ✅ | ✅ |
| **Update** | IP group 已存在（`FindIPGroupByName` 返回 ID） | ❌ **缺失** | ❌ **缺失** |

`reconcileDelete`（`service_controller.go:531`）只在 `aclCleanupFinalizer` 存在时清理 ACL：

```go
if controllerutil.ContainsFinalizer(svc, aclCleanupFinalizer) {
    // 删除 IP group...
}
```

当 IP group 已存在（之前测试残留），`ensureACL` 走 Update 分支，不加 finalizer。删除 Service 时 `reconcileDelete` 检查 finalizer 为 false，**跳过 ACL 清理**，IP group 残留。

### 自我持续的 bug 循环

```
1. 最初残留（之前测试产生 IP group 未删）
2. 重建集群 -> ensureACL -> FindIPGroupByName 找到残留 -> Update 分支 -> 不加 finalizer
3. 删除集群 -> reconcileDelete -> finalizer 不存在 -> 跳过 ACL 清理 -> IP group 再次残留
4. 回到步骤 2
```

### 为什么 mongodb-4e5 和 mysql-yl1 都触发

| 维度 | mongodb-4e5 | mysql-yl1 |
|---|---|---|
| ELB 类型 | 内网 | 公网 |
| 端口数 | 1 | 5+1 |
| 反查恢复风暴 | 有（400+次） | 无 |
| ACL 未清理 | ✅ 是 | ✅ 是 |
| **根因** | 走 Update 分支 | 走 Update 分支 |

**反查恢复风暴不是 ACL 未清理的原因**：两个集群都有 ACL 未清理，但只有一个有反查恢复风暴。根因完全相同 -- 首次创建时 `FindIPGroupByName` 找到了同名残留 IP group -> 走 Update 分支 -> 修复前不加 finalizer。

## 修复

**commit `766f34c`**：Update 分支补全 `aclIDAnnotation` / `aclTypeAnnotation` / `aclCleanupFinalizer`，与 Create 分支对齐。

```diff
 if ipGroupID != "" {
     if err := huaweicloud.UpdateIPGroup(...); err != nil {
         return err
     }
+    svc.Annotations[aclIDAnnotation] = ipGroupID
     svc.Annotations[aclStatusAnnotation] = "on"
+    svc.Annotations[aclTypeAnnotation] = "white"
+    controllerutil.AddFinalizer(svc, aclCleanupFinalizer)
 } else {
```

修复后即使找到残留 IP group 也会加 finalizer，删除时清理，打破循环。

## 验证状态

- ✅ 代码修复已 commit（`766f34c`）
- ✅ `go build` / `go vet` / `go test` 通过
- ❌ **尚未部署到 CCE 验证**
- 待办：构建新镜像部署后，重建集群验证 ACL 是否正确清理

## 清理遗留

mongodb-4e5 残留的 3 个 ACL IP group 需手动在华为云控制台清理，或部署修复后重建集群验证自动清理。
