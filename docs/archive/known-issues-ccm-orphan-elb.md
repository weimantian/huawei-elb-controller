# CCM EIP 配额满时 ELB 创建失败循环问题记录

## 问题概述

华为云 CCM (Cloud Controller Manager) 在创建 LoadBalancer Service 时，如果账号 EIP 配额已满，会陷入"创建失败循环"--每次 reconcile 都尝试创建 ELB，但因为 EIP 配额满（`EIP.7905`）而失败，Service 的 EXTERNAL-IP 一直 pending。

## 发现时间

2026-07-14，在 CCE 集群测试 PSMDB (MongoDB) 端到端流程时发现。

## 触发条件

同时满足以下两个条件：

1. **EIP 配额已满**：账号下 EIP 数量达到配额上限
2. **正在创建新的 LoadBalancer Service**：CCM 调用华为云 API 创建带 EIP 的 ELB

## 实际现象

### Service 状态

```bash
kubectl get svc mongodb-e2e-test-rs0-2 -n everest
# EXTERNAL-IP 一直 <pending>
```

### Service events

```
Warning  CreatingLoadBalancerFailed  14m  hws-cloudprovider
  Details: Create EIP for loadbalancer(af719812-...) error:
  Failed to Create Loadalancer :
  request failed: {"code":"EIP.7905","message":"Quota exceeded for resources: publicip"}, status code: 409

Warning  CreatingLoadBalancerFailed  14m  hws-cloudprovider
  Details: Create EIP for loadbalancer(e5cec26d-...) error: ...

# 多次重试，每次 loadbalancer ID 不同
Warning  UpdateLoadBalancerFailed  28s (x15 over 14m)  hws-cloudprovider
  Update loadbalancer of service(mongodb-e2e-test-rs0-2/everest) error: listener is empty
```

### 关键观察：华为云 ELB 控制台无残留资源

- events 中出现的 8 个 loadbalancer ID（`af719812`, `e5cec26d`, `e4b0e75a` 等）在华为云 ELB 控制台**均不可见**
- 说明这些 ELB **从未真正创建成功**

## 根因分析

### 华为云 ELB API 行为

华为云 `CreateLoadBalancer` API 是**原子性事务**，包含以下子步骤：

1. 创建 ELB 实例
2. 创建 EIP
3. 绑定 EIP 到 ELB

**任一步失败 -> 整个事务回滚**，ELB 不会保留。本次场景中：

- 步骤 1：ELB 实例创建（可能成功）
- 步骤 2：EIP 创建 -> ❌ 失败（`EIP.7905: Quota exceeded`）
- 事务回滚 -> ELB 不存在，UI 看不到

events 中的 `loadbalancer(xxx)` 里的 ID 是 API 调用过程中的临时标识，**不代表 ELB 资源已持久化**。

### CCM 的行为

CCM 收到 `EIP.7905` 错误后：

1. 不写回 `kubernetes.io/elb.id` annotation（因为创建确实失败了）
2. 下次 reconcile 时，检查 annotation 为空 -> 认为"还没有 ELB" -> 再次尝试创建
3. 结果：反复重试，每次都因 EIP 配额满而失败

### `listener is empty` 报错的来源

这个报错是 CCM 内部状态不一致导致的：

- CCM 在某次重试中可能部分记忆了 ELB 信息（但 annotation 没写回）
- 尝试 `UpdateLoadBalancer` 时，对不存在的 ELB 操作 -> 报 `listener is empty`
- 这是 CCM 在失败重试过程中的内部状态混乱，不是独立的资源泄漏问题

## 实际影响

### 触发条件很苛刻

这个问题只在 EIP 配额满时触发，正常生产环境（配额充足）永远不会遇到。

### 影响评估

| 影响项 | 程度 | 说明 |
|---|---|---|
| Service 不可用 | 有影响 | EXTERNAL-IP 一直 pending，外部无法访问 |
| 资源泄漏 | 无影响 | ELB 创建是原子事务，失败自动回滚，无孤儿资源 |
| 费用 | 无影响 | 没有残留 ELB，不产生费用 |
| 配额恶化 | 无影响 | 没有残留 ELB 或 EIP |
| 集群稳定性 | 无影响 | 不影响其他 service、pod、已就绪的 ELB |

### 本次实际损失

- 无资源泄漏，无费用损失
- Service 不可用直到 EIP 配额释放
- 释放配额后重建一次成功，服务恢复

## 复现方法

1. 创建 N 个 LoadBalancer Service 消耗 EIP 配额到上限
2. 再创建一个 LoadBalancer Service（带 EIP）
3. 观察 events，会看到反复的 `CreatingLoadBalancerFailed`（`EIP.7905`）
4. 华为云 ELB 控制台不会有残留资源（原子事务回滚）

## 临时解决方案

### 释放 EIP 配额

1. 删除不再需要的 LoadBalancer Service（CCM 会自动释放 ELB 和 EIP）
2. 或在华为云控制台手动释放未使用的 EIP

### 触发 Service 重建

释放配额后，删除并重建 service 让 CCM 重试：

```bash
kubectl delete svc <service-name> -n <namespace>
# 等 operator 重建 service，或手动重建
```

### 预防措施

- 监控 EIP 配额使用率，提前扩容
- 批量创建 LoadBalancer Service 前确认 EIP 配额充足
- 华为云控制台 -> 网络 -> 虚拟私有云 VPC -> 弹性公网 IP，可查看当前 EIP 用量和配额

## 为什么不在 huawei-elb-controller 中修复

### 无法修复

这是华为云 CCM + ELB API 的行为，我们的 controller 只负责注入 `autocreate` 注解，ELB 的实际创建由 CCM 调用华为云 API 完成。我们无法干预 CCM 的重试逻辑或华为云 API 的事务行为。

### 职责越界

即使技术上能做（如检测 Service 长时间 pending 后清理），也属于 CCM 的职责范围。在我们的 controller 中加重试控制逻辑会引入与 CCM 的竞争条件。

## 建议跟进

向华为云反馈 CCM 行为优化建议：

**问题描述**：EIP 配额满时，CCM 创建 LoadBalancer Service 会陷入无限重试循环，每次重试都调用华为云 API（虽然原子事务回滚不产生残留资源，但无意义的重试增加了 API 调用负担）。

**优化建议**：

1. CCM 检测到 `EIP.7905`（配额满）错误时，应采用指数退避重试，而非固定间隔重试
2. 可在 Service events 中给出更明确的提示："EIP 配额已满，请释放 EIP 或申请扩容"
3. 可考虑在 CCM 层面缓存"配额满"状态，短时间内不再重试，避免无效 API 调用

## 相关资源

- 发现问题时的 Service: `mongodb-e2e-test-rs0-2` (namespace: `everest`)
- 集群: CCE cn-north-4a, K8s v1.35.3-r0-35.0.8
- CCM 版本: 随 CCE 集群提供（具体版本未查）
- EIP 配额限制: 账号默认配额（本次约 5-6 个 EIP 上限）

## 修正记录

- **2026-07-14 初版**：基于 events 中多个不同 loadbalancer ID，误判为"ELB 创建成功但 EIP 绑定失败，产生孤儿 ELB"
- **2026-07-14 修正**：经华为云 ELB 控制台验证，无任何残留 ELB 资源。确认华为云 `CreateLoadBalancer` API 是原子事务，失败自动回滚。events 中的 loadbalancer ID 是 API 调用过程中的临时标识，不代表 ELB 已持久化。问题本质是 CCM 在 EIP 配额满时的无效重试循环，无资源泄漏。
