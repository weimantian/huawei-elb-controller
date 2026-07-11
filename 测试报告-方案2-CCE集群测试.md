# 方案 2 CCE 集群测试报告

> **测试日期**: 2026-07-11  
> **集群**: CCE 1.35.3, 3 节点 (cn-north-4)  
> **控制器版本**: Plan 2 (Service Reconciler + legacy LBC Reconciler)  
> **测试方法**: 命令行 (kubectl) + 浏览器 UI

---

## 一、部署验证

### 1.1 部署状态
| 组件 | 状态 | 说明 |
|------|------|------|
| CCE 集群 | ✅ 3 节点 Ready | 192.168.0.139, .153, .17 |
| everest-operator | ✅ Running | OpenEverest v1 |
| everest-server | ✅ Running | UI 服务 |
| huawei-elb-controller | ✅ Running | Plan 2 代码，含 Service Reconciler |

### 1.2 控制器日志验证
```
Starting Controller "service"     ← Service Reconciler 已注册
Starting Controller "loadbalancerconfig"  ← LBC Reconciler (legacy) 已注册
Starting workers "service" (worker count: 1)
Starting workers "loadbalancerconfig" (worker count: 1)
starting huawei-elb-controller (Plan 2: Service Reconciler + legacy LBC Reconciler)
```

### 1.3 RBAC 验证
- ✅ ClusterRole 已添加 `services: get/list/watch/update/patch` 权限
- ✅ 存量 `nodes` 和 `events` 权限不受影响
- ✅ ServiceAccount `huawei-elb-controller` 权限正确

---

## 二、功能测试

### 测试 1：自动模式（不使用 LBC，默认参数）

**操作**: 创建 LoadBalancer Service（模拟 OpenEverest 创建 DBC 且不配置 LBC）

**Service 规格**:
```yaml
type: LoadBalancer
labels:
  app.kubernetes.io/managed-by: percona-xtradb-cluster-operator  # OpenEverest 创建标记
annotations: {}  # 无 elb.id，无 autocreate，无 LBC 参数
```

**结果** ✅:

| 检查项 | 预期 | 实际 | 结果 |
|--------|------|------|------|
| autocreate 注入 | `elb.autocreate` JSON 出现 | 已注入，含默认参数 (10M/traffic/5_bgp) | ✅ |
| elb.class | "union" | "union" | ✅ |
| reclaim-policy | "alwaysDelete" | "alwaysDelete" | ✅ |
| acl-status (防御性) | "off" | "off" | ✅ |
| VPC/子网探测 | 自动探测 | subnet: e8e541e1-... | ✅ |
| AZ 探测 | 自动探测 | cn-north-4a | ✅ |
| ELB 创建 | CCM 创建成功 | elb.id: 8470729e-... | ✅ |
| 外部 IP | 公网 IP 分配 | 120.46.30.94 | ✅ |
| ELB 名称 | cce-lb-everest-{svc-name} | cce-lb-everest-test-plan2-haproxy | ✅ |

**autocreate JSON**:
```json
{
  "name": "cce-lb-everest-test-plan2-haproxy",
  "type": "public",
  "bandwidth_name": "cce-lb-everest-test-plan2-haproxy-bw",
  "bandwidth_chargemode": "traffic",
  "bandwidth_size": 10,
  "bandwidth_sharetype": "PER",
  "eip_type": "5_bgp",
  "vip_subnet_cidr_id": "e8e541e1-814b-4856-8aa3-a8f1e111af4a",
  "available_zone": ["cn-north-4a"]
}
```

**裸奔窗口**: Service 创建到 autocreate 注入间隔 < 1s，符合设计预期。

---

### 测试 2：手动模式（使用 LBC 参数模板）

**操作**: 创建带 `huawei-elb.io/*` 参数的 Service（模拟 OpenEverest 从 LBC 同步参数到 Service）

**Service 注解**:
```yaml
huawei-elb.io/public: "true"
huawei-elb.io/bandwidth-size: "20"
huawei-elb.io/bandwidth-charge-mode: "traffic"
huawei-elb.io/eip-type: "5_bgp"
huawei-elb.io/name: "my-custom-elb-name"
```

**结果** ✅:

| 检查项 | 预期 | 实际 | 结果 |
|--------|------|------|------|
| ELB 名称 | "my-custom-elb-name" (用户指定) | "my-custom-elb-name" | ✅ |
| 带宽 | 20 Mbit/s | bandwidth_size: 20 | ✅ |
| autocreate 注入 | JSON 含用户参数 | 参数正确映射 | ✅ |
| ELB 创建 | CCM 创建成功 | elb.id: d903bbdc-... | ✅ |
| 外部 IP | 公网 IP 分配 | 113.44.161.138 | ✅ |

---

### 测试 3：ACL 访问控制（sourceRanges）

**操作**: 创建带 `loadBalancerSourceRanges` 的 Service

**sourceRanges**:
```yaml
loadBalancerSourceRanges:
  - "10.0.0.0/8"
  - "172.16.0.0/12"
```

**结果** ✅:

| 检查项 | 预期 | 实际 | 结果 |
|--------|------|------|------|
| IP 组创建 | 调用 ELB API 创建 IP 地址组 | acl-id: 21e08ad8-... | ✅ |
| acl-status | "on" | "on" | ✅ |
| acl-type | "white" | "white" | ✅ |
| autocreate 注入 | 正常 | JSON 含默认参数 | ✅ |
| last-known-params | 包含 sourceRanges 状态 | {"source-ranges":"[\"10.0.0.0/8\",\"172.16.0.0/12\"]"} | ✅ |

---

### 测试 4：存量兼容性

| 检查项 | 结果 |
|--------|------|
| 存量 Service (mysql-65w-haproxy) 有 elb.id | ✅ Service Reconciler 跳过，不受影响 |
| 存量 LBC Reconciler 继续工作 | ✅ cce-test 和 test LBC 正常 reconcile |
| 控制器重启后无异常 | ✅ |

---

### 测试 5：删除流程

| 检查项 | 结果 |
|--------|------|
| 删除 Service → CCM 原生删 ELB | ✅ reclaim-policy: alwaysDelete |
| 删除后无孤儿资源 | ✅ |

---

## 三、Bug 修复记录

| Bug | 现象 | 修复 |
|-----|------|------|
| getLBCParams 递归膨胀 | `last-known-params` 被包含在 `getLBCParams` 结果中，导致 annotation 增长超 262KB | 在 `getLBCParams` 中排除 `huawei-elb.io/last-known-params` |
| RBAC 缺少 Service 权限 | `services is forbidden` 错误 | 在 deploy/ 和 Helm chart ClusterRole 中添加 services 权限 |

---

## 四、UI 界面测试

### 4.1 登录界面
![OpenEverest 登录页](test-report-ui-01-login.png)
*用户: admin / Admin@123456*

### 4.2 数据库列表（已有 mysql-65w）
![数据库列表](test-report-ui-02-databases-list.png)

### 4.3 创建数据库向导 — 基本信息
![Step 1](test-report-ui-04-step1-basic-info.png)

### 4.4 Advanced Configurations — External Access
Exposure Method 选择 'Load balancer' 后，LoadBalancer configuration 下拉默认显示 '- No configuration -'
![External Access](test-report-ui-12-external-access.png)

### 4.5 LoadBalancer 配置选项
选项包括: '- No configuration -' (默认), cce-test, eks-default, test
选择 '- No configuration -' 即走方案 2 自动创建 ELB 路径
![LBC Options](test-report-ui-15-lbc-options.png)

### 4.6 数据库创建成功 — 自动获得两个独立 ELB
创建后，OpenEverest 自动创建两个 LoadBalancer Service (primary + replicas)：
- **mysql-sz3-haproxy** (primary): EXTERNAL-IP **113.44.161.138**
- **mysql-sz3-haproxy-replicas** (replicas): EXTERNAL-IP **120.46.30.94**
每个 Service 独立 ELB，端口 3306 不冲突 ✅（对齐 EKS/GKE 行为）
![DB Creating](test-report-ui-16-db-creating.png)

### 4.7 Service Reconciler 日志验证
```
Reconciling Service mysql-sz3-haproxy
Injected autocreate annotation on Service mysql-sz3-haproxy
Reconciling Service mysql-sz3-haproxy-replicas
Injected autocreate annotation on Service mysql-sz3-haproxy-replicas
```
两个 Service 均成功注入 autocreate → CCM 创建 ELB → 零报错

### 4.8 验证注解
Primary Service: `elb.id: ac35eeec-...`, `elb.class: union`, `acl-status: off`
### 4.9 Source Range（访问控制）配置验证
Source Range 字段在 External Access 区域的 LoadBalancer 配置下方：
- 描述："Specify trusted IP addresses to restrict access. Leaving this blank will expose the database to all IP addresses."
- 输入格式：IP with netmask (e.g. 192.168.1.1/24)
- 支持多个 CIDR 条目
![Source Range](test-report-ui-17-source-range-config.png)

通过命令行验证已确认：当 Service 的 `loadBalancerSourceRanges` 被设置时，Service Reconciler 自动调 ELB API 创建 IP 地址组并注入 `elb.acl-id/status=on/type=white` 注解。

---

## 五、单元测试结果

```
$ go test ./... -count=1
ok  internal/controller  1.464s  (30 tests)
ok  internal/huaweicloud  2.002s  (13 tests)
```

| 测试文件 | 测试数 | 覆盖内容 |
|----------|--------|----------|
| `service_controller_test.go` | 22 | 创建路径、更新路径、skip 逻辑、ACL 逻辑、参数比较 |
| `params_test.go` | 5 | autocreate JSON 映射（public/inner/默认/default） |
| `service_utils_test.go` | 8 | Service 工具函数、OpenEverest 识别 |
| `service_controller_test.go` 补充 | 8 | buildUpdateOption (7)、sourceRangesEqual (7)、paramsEqual (7) |

---

## 六、测试结论

### ✅ 通过项
- 自动模式（无 LBC）ELB 创建 ✅
- 手动模式（有 LBC 参数）ELB 创建 ✅
- ACL 自动处理（sourceRanges → acl-*） ✅
- 存量兼容性（存量 DBC/Service 不受影响） ✅
- ELB 删除（CCM 原生 reclaim-policy） ✅
- VPC/子网/AZ 自动探测 ✅
- NetworkDetector 重构（LBC Reconciler + Service Reconciler 共享） ✅
- 所有单元测试通过 ✅
- go build / go vet 无警告 ✅

| 浏览器 UI 端到端测试 | ✅ admin/Admin@123456 登录，创建 mysql-sz3，选择 No configuration，primary + replicas 均获外部 IP |
| ELB 带宽更新 | ⏳ 代码已实现（EIP v2 API），CCE 集群冻结后补测 |

### 总结
方案 2（Service Reconciler）在 CCE 1.35.3 集群上功能正确，实现了与 EKS/GKE 对等的"创建数据库即获得 ELB"体验。存量系统不受影响。
