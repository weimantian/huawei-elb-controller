# Issue: mongodb-7rx 全生命周期日志审计发现

**审计时间**: 2026-07-15
**审计范围**: mongodb-7rx（3 节点 PSMDB）创建+删除全流程
**ELB controller 版本**: `:planb-v2`（commit `596d8af`）

## 审计摘要

| 维度 | 结论 |
|---|---|
| ELB 创建 | ✅ 正常（3 个公网 ELB + listener + pool + member + healthcheck + ACL） |
| ELB 删除 | ✅ 正常（healthcheck -> member -> pool -> listener -> ELB 全部删除） |
| EIP 删除 | ✅ 正常（3 个 EIP 全部删除，无 error 日志） |
| ACL IP group 删除 | ❌ **泄漏**（3 个 IP group 全部未删除）-> 详见 `bug-acl-ipgroup-leak-on-delete.md` |
| CCM 行为 | ⚠️ 噪音（`UpdateLoadBalancerFailed: listener is empty`），不影响功能 |
| OpenEverest operator | ✅ 正常（reconcile + PSMDB 初始化 + replset 选举） |
| PSMDB operator | ✅ 正常（key 创建 + pod 等待 + replset 初始化 + user 创建） |
| Pod 生命周期 | ✅ 正常（3 个 pod 依次创建、就绪、删除） |
| PVC | ✅ 正常（3 个 PVC 创建 + 挂载 + 随 pod 删除） |

## 时间线

```
13:05:01  OpenEverest 创建 DBC mongodb-7rx
13:05:01  PSMDB operator 创建 key + secret
13:05:15  ELB controller 创建 ELB #1 (rs0-0, 公网, 1de647b1)
13:05:25  ELB #1 fully provisioned, ingress IP 写入 status
13:05:32  ELB controller 创建 ELB #2 (rs0-1, 公网, d2ab0f7f)
13:05:41  ELB #2 fully provisioned
13:06:00  ELB controller 创建 ELB #3 (rs0-2, 公网, bf11b0ba)
13:06:08  ELB #3 fully provisioned
13:06:10  PSMDB replset 初始化, rs0-0 成为 primary
13:06:12  user admin 创建完成
13:07:43  DBC 删除触发, ELB #3 开始删除
13:07:47  ELB #3 删除完成, EIP 删除
13:07:49  ELB #2 开始删除
13:07:52  ELB #2 删除完成, EIP 删除
13:08:03  ELB #1 开始删除
13:08:08  ELB #1 删除完成, EIP 删除
13:08:08+  Service 全部消失, DBC 全部消失
```

## 发现的问题

### 问题1: ACL IP group 泄漏（严重）

详见 `bug-acl-ipgroup-leak-on-delete.md`。

3 个 ACL IP group 创建后未删除，成为孤儿资源。根因是 OpenEverest 覆盖注解导致 `reconcileDelete` 丢失 `acl-id`。

### 问题2: EIP 删除无成功日志（轻微）

`deleteELBStack` 中 EIP 删除只在**失败**时打 error 日志：

```go
if err := huaweicloud.DeleteEIPByID(r.Creds, eipID); err != nil {
    logger.Error(err, "deleting EIP, manual cleanup needed", "eipID", eipID)
}
```

成功时无日志，导致无法从日志确认 EIP 是否删除成功。需排查时只能看是否有 error 日志（无 error = 成功），不够明确。

**建议**: 补一行成功日志。

### 问题3: CCM `UpdateLoadBalancerFailed: listener is empty`（已知噪音）

事件日志显示 CCM 对 Service 报：
```
UpdateLoadBalancerFailed: Update loadbalancer of service(mongodb-cvf-rs0-X/everest) error: listener is empty
GetLoadBalancerFailed: service annotation(kubernetes.io/elb.id) or service.spec.loadBalancerIP is not defined, skip.
```

这是 CCM 尝试管理我们的 ELB 但找不到它创建的 listener（我们的 listener 命名规则 `mongodb-cvf-rs0-0-27017` 和 CCM 的不同）。CCM 在无 `kubernetes.io/elb.id` 时 skip，在有 `elb.id` 时尝试更新但找不到 listener。

**不影响功能**，是 CCM 噪音。这是方案B 回退（不写 `kubernetes.io/elb.id`）的预期行为。

### 问题4: 创建后反复 reconcile（性能浪费，非 bug）

每个 Service 创建后有 4-5 次 reconcile（Pool members synced + ACL bound to listeners 反复执行）。这是 OpenEverest 不断更新 Service spec（`percona.com/last-config-hash`）触发 controller 重新 reconcile。

每次 reconcile 都会调华为云 API（ListPools + ListListeners + ListMembers + 绑定 ACL），造成不必要的 API 调用。

**不是 bug**，但可优化（加 hash 对比跳过无变化的 reconcile）。

## 资源清理确认

| 资源类型 | 创建数 | 删除数 | 泄漏数 |
|---|---|---|---|
| ELB | 3 | 3 | 0 |
| EIP | 3 | 3 | 0 |
| Listener | 3 | 3 | 0 |
| Pool | 3 | 3 | 0 |
| Member | 15 (5 nodes × 3) | 15 | 0 |
| HealthCheck | 3 | 3 | 0 |
| ACL IP Group | 3 | 0 | **3** |

## 待手动清理

3 个泄漏的 ACL IP group（华为云控制台）：
- `4d10a9a7-e6c3-441e-8796-3385d4d140be`
- `e9cd4530-f293-48d9-bf55-610f169c887a`
- `c50b158f-b154-4410-a763-8c48089b6304`
