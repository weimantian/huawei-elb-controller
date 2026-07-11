# 方案 1、方案 2 边界值分析

> **日期**: 2026-07-10

---

## 1. 方案 1 边界值（DBC Reconciler）

### 创建阶段

| # | 场景 | 影响 | 建议处理 |
|---|---|---|---|
| E1-1 | DBC 创建时 auto-lbc-name 注解对应的 LBC 已存在（命名冲突） | Create LBC 失败 | 截断 + SHA256 hash 后缀（已设计），冲突时报错不重试 |
| E1-2 | Primary LBC 创建成功但 replicas LBC 创建失败 | 半完成状态，只建了一个 LBC | DBC Reconciler 检测到 auto-lbc-name 有值但 auto-lbc-replicas-name 无 LBC → 删除已创建的 primary LBC，重建或报错 |
| E1-3 | 两个 LBC 都 ready 后 patch PXC CR 时 PXC CR 尚不存在 | patch 失败（404），DBC 卡在 creating | DBC Reconciler retry（requeue 5s），等 OpenEverest 建完 PXC CR |
| E1-4 | Patch PXC CR 被 Webhook 拒绝 | patch 失败，DBC 卡住 | 记录错误到 DBC `huawei-elb.io/error`，requeue。如果 Webhook 持续拒绝（如 PXC CR 状态不允许修改）→ 需分析 Webhook 逻辑 |
| E1-5 | Patch DBC `loadBalancerConfigName` 失败 | DBC 不出错，LBC 建了但 DBC 没引用 → 孤儿 LBC | Compensation Goroutine 5min 扫描：LBC 有 auto-lbc-name 注解但对应 DBC 不存在或 `loadBalancerConfigName` 仍为空 → 删 LBC |
| E1-6 | `loadBalancerConfigName` 为空但 DBC 没有 engine type（用户配置错误） | OpenEverest 不会建 engine CR/Service → PXC CR 永远不会出现 | DBC Reconciler 等 PXC CR 出现 → 超时？还是忽略（等用户修正 DBC）→ 建议：设置超时（如 5min），超时后 requeue 更长间隔（1h） |
| E1-7 | DBC 名称导致 LBC 名超 K8s 253 字符限制 | Create LBC 失败 | `namespace + dbc-name + "-replicas"` 可能超限。当前设计设为 60 字符截断（华为云 ELB 64 字符限制），K8s 资源名最大 253 字符，先满足华为云。冲突用 hash |

### 删除阶段

| # | 场景 | 影响 | 建议处理 |
|---|---|---|---|
| E1-8 | 删除 DBC 时 OpenEverest 不移除 `in-use-protection`（F11 被某些条件阻断） | DBC Reconciler 一直轮询，DBC 卡在删除 | 10min 超时后写 error 注解继续重试。**需要加硬超时**（如 30min），超时后删 LBC 的 `in-use-protection` finalizer 强行清理（高权限操作，仅告警后执行） |
| E1-9 | 删除 DBC 时 primary LBC 删成功但 replicas LBC 删失败 | 孤儿 replicas LBC + ELB 持续扣费 | DBC Reconciler 检查两个 LBC 都删完才移除 dbc-finalizer。任一失败 → 记录 error，重组重试 |
| E1-10 | 用户删除 DBC 后又改名重建（同名 DBC） | Stage LBC 仍有 auto-lbc-name 注解指向已删除的 DBC；新 DBC 同名触发 LBC 命名冲突 | 检测阶段：DBC Reconciler 在 Create LBC 前先 Get，若 LBC 已存在且 owner（auto-lbc-name 注解）指向已删 DBC → 先删旧 LBC 再继续 |

### 运行阶段

| # | 场景 | 影响 | 建议处理 |
|---|---|---|---|
| E1-11 | DBC 创建后 OpenEverest 修改了 engine type 或 expose type | 参数变化可能导致 LBC 不适用 | DBC Reconciler 监测 DBC 变更。如果 `loadBalancerConfigName` 从空变到非空（人工修改）→ 自动流程已触发过 → 取消 auto 注解？→ 建议：设置 DBC 注解 `huawei-elb.io/auto: "true"` 后由人工决定是否继续自动管理 |
| E1-12 | 用户手动删掉了 auto-lbc-name 注解 | DBC Reconciler 下次调和 → 当作首次处理 → 会创建新 LBC | 风险：用户删注解 -》新 LBC 被建，旧 LBC 仍在但无人引用 -》孤儿。用 auto-lbc-name 注解的修改时间检测，若删除时间 < 调和间隔 → 忽略；若已超过 2 次调和 → 恢复 auto-lbc-name（防抖） |
| E1-13 | 用户手动修改 PXC CR 的 expose 注解（绕过 DBC Reconciler） | DBC Reconciler 会认为 PXC CR 仍是旧的 ELB ID | DBC Reconciler 周期（5min 长轮询）检查 PXC CR 和 LBC 的 ELB ID 一致性。不一致 → 重新 patch PXC CR（覆盖用户手动修改） |
| E1-14 | 并发创建两个 DBC，auto-lbc-name 指向不同但 LBC 名称潜在冲突 | Create LBC 冲突 → 一次成功、一次失败 | 失败方用 hash 后缀重试。并发安全由 K8s Create（原子操作）保证 |

---

## 2. 方案 2 边界值（Service Reconciler）

### 创建阶段

| # | 场景 | 影响 | 建议处理 |
|---|---|---|---|
| E2-1 | Service 上同时有 `elb.id`（存量 LBC）和 `huawei-elb.io/*`（新 LBC 模板） | 参数冲突 | `elb.id` 优先：Service Reconciler 跳过这个 Service（走现有 CCM 绑定流程）。`huawei-elb.io/*` 参数被忽略，仅在 Service 无 `elb.id` 时才使用 |
| E2-2 | Service Reconciler patch Service 注入 autocreate 后，CCM 创建 ELB 失败（API 错误） | Service 卡在注入状态下，无 ELB | 检测 Service status.ingress 无 IP → requeue 并重试（一定次数后停止，避免无限重试）。或 CCM 本身有重试逻辑（由 CCM 管理） |
| E2-3 | autocreate JSON 中的 VPC/子网 ID 无效 | CCM 创建 ELB 失败 | NetworkDetector 探测即验证。不确定时，可加华为云 API 验证（用 VPC/子网 API 查询是否存在） |
| E2-4 | OpenEverest 创建的 Service-A（primary）和 Service-B（replicas）同时触发 | 两个 Service Reconciler reconcile 并发 → patching 相同 LBC 参数 → 互不冲突（每个 Service 独立的 autocreate） | ✅ 无问题（每 Service 独立 ELB） |

### 更新阶段（LBC 参数变更）

| # | 场景 | 影响 | 建议处理 |
|---|---|---|---|
| E2-5 | 用户连续快速修改 LBC 参数（如带宽 10→20→30→10 在 1 分钟内） | 每次变更都调华为云 API → API 限频？ | 加 throttle：同一 Service 的参数变更至少间隔 30s 才调 API，期间只记录最新期望值 |
| E2-6 | 参数更新 API 失败（华为云返回错误） | 参数未更新，Service 注解和 ELB 状态不一致 | 写 error 到 Service event 或 annotation。requeue 重试（退避策略）。连续失败超过阈值 → 告警 |
| E2-7 | LBC 参数变更后 OpenEverest 同步到 Service 有延迟 | Service Reconciler 读到的仍是旧参数 | 正常——等 OpenEverest 同步完成后下次 reconcile 自动检测。最多延迟一个调和周期（<1s） + OpenEverest 同步延迟（几秒） |
| E2-8 | 多个 DBC 引用同一 LBC 模板 → 参数变更需更新多个 ELB | 需要找到所有关联的 Service 并逐一调 API 更新 | Service Reconciler 根据 Service label（OpenEverest 创建的 Service 都有 DBC 标识）查找同 LBC 的所有 Service → 批量更新 |
| E2-9 | 用户修改 LBC 添加了 `kubernetes.io/elb.id` | 从参数模板切换到实例引用模式 | Service Reconciler 检测到 Service 上同时有 `elb.id` → 跳过（E2-1 处理）。但已通过 autocreate 创建的 ELB 仍存在 → 需要手动清理 |

### 删除阶段

| # | 场景 | 影响 | 建议处理 |
|---|---|---|---|
| E2-10 | 删 DBC → 删 Service → CCM reclaim-policy `alwaysDelete` → ELB 被删 | 正常流程 ✅ | CCM 原生处理 |
| E2-11 | 删 Service 时 CCM 未删 ELB（CCM 异常） | ELB 孤儿，持续扣费 | 加补偿扫描：Service Reconciler 每 10min 扫描有 `huawei-elb.io/auto: "true"` 但 Service 已不存在的 ELB → 调 API 删除 |
| E2-12 | 删除时 reclaim-policy 为 `retain`（用户手动改过） | ELB 保留 | 默认注入 `alwaysDelete`。如果用户改了注解 → 尊重用户选择 |

### 运行阶段

| # | 场景 | 影响 | 建议处理 |
|---|---|---|---|
| E2-13 | Service 注解被用户手动修改（覆盖 autocreate 或 huawei-elb.io/* 参数） | Service Reconciler 读到不一致的参数 | 调和原则：Service 上的 `huawei-elb.io/*` 参数是真实来源（来自 LBC），如果被覆盖 → Service Reconciler 重新注入 autocreate 或更新 API |
| E2-14 | Service 上 ELB ID 未知但 CCM 已创建 ELB（CCM 创建了 ELB 但未回复 autocreate JSON） | ELB 已存在但没有记录 | 检测 Service.status.ingress 有 IP 但无 ELB ID 注解 → 调华为云 API 反查 ELB（按名称或 IP） |
| E2-15 | DBC 暂停（pause annotation） | Service Reconciler 不应处理该 Service 的参数变更 | 检测 DBC 的 pause 注解 → 只读状态，不处理参数更新 |
| E2-16 | CCE 升级导致 CCM 行为变化 | autocreate 的参数格式可能变化 | 设计和华为云 CCE 文档绑定，升级前需验证 |

---

## 3. 两个方案共有的边界值

| # | 场景 | 两个方案的影响 |
|---|---|---|
| EC-1 | 集群节点异构（不同 AZ、不同 VPC） | NetworkDetector 探测可能失败或多值。哪个方案都需要处理多 VPC 场景 |
| EC-2 | StorageClass `csi-disk` 不可用 | DBC 创建成功但 PXC 卡在 PVC pending。两个方案都不负责存储，但需要检测并通知用户 |
| EC-3 | 华为云 ELB API 不可达或限频 | 创建/更新 ELB 失败。两个方案都需要退避 + 重试 + 限频 |
| EC-4 | OpenEverest 升级导致 CRD/Webhook 变化 | 两个方案的核心逻辑依赖 OpenEverest 不变的部分（注解传播、PXC CR 结构） |
| EC-5 | `loadBalancerSourceRanges` 配了不合法 CIDR | CCM 仍忽略（两个方案同处理：ACL 自动处理时识别非法 CIDR → 跳过或报错） |
