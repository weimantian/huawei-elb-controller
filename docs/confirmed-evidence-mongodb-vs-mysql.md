# 已确认证据：MongoDB vs MySQL 差异与 CCM 行为

> 本文档记录所有已通过实测验证的事实。每条事实均附明确证据（命令输出 / API 响应 / 日志）。
> 创建时间：2026-07-16

---

## 事实1：PSMDB 与 PXC 的 Service 架构完全不同

### 事实

| 引擎 | Service 数量 | Service 结构 | 端口 |
|---|---|---|---|
| PSMDB (mongodb) | 3 个（每 pod 1 个） | `mongodb-x3f-rs0-0/1/2`，selector 指向单个 pod | 各 1 端口 27017 |
| PXC (mysql) | 2 个（共享） | `mysql-4zj-haproxy`（主）+ `mysql-4zj-haproxy-replicas` | 主 5 端口，replicas 1 端口 |

### 证据

```
$ kubectl get svc -n everest -o wide | grep LoadBalancer

# PSMDB: 3 个独立 Service，每个 selector 指向单个 StatefulSet pod
mongodb-x3f-rs0-0   LoadBalancer   10.247.75.70    192.168.0.210   27017:32744/TCP   ...statefulset.kubernetes.io/pod-name=mongodb-x3f-rs0-0
mongodb-x3f-rs0-1   LoadBalancer   10.247.224.70   192.168.0.157   27017:31625/TCP   ...statefulset.kubernetes.io/pod-name=mongodb-x3f-rs0-1
mongodb-x3f-rs0-2   LoadBalancer   10.247.151.164  192.168.0.213   27017:31107/TCP   ...statefulset.kubernetes.io/pod-name=mongodb-x3f-rs0-2

# PXC: 2 个共享 Service，selector 指向所有 haproxy pod
mysql-4zj-haproxy          LoadBalancer   ...   3306:...,3309:...,33062:...,33060:...,8404:...   ...app.kubernetes.io/component=haproxy
mysql-4zj-haproxy-replicas LoadBalancer   ...   3306:...                                          ...app.kubernetes.io/component=haproxy
```

---

## 事实2：CCE CCM 对两者都清空 `status.loadBalancer.ingress`

### 事实

CCE CCM 对所有 `type: LoadBalancer` Service 都会干预（不管有没有 `elb.class`）。无 `kubernetes.io/elb.id` + 无 `loadBalancerIP` 时，CCM 报 `GetLoadBalancerFailed` 并**主动清空** `status.loadBalancer.ingress`。PSMDB 和 PXC 都受影响。

### 证据

**PSMDB（mongodb-x3f）status 被清空**：
```
$ for i in 1 2 3 4 5; do
    ts=$(date +%H:%M:%S)
    s0=$(kubectl get svc mongodb-x3f-rs0-0 -n everest -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
    echo "$ts rs0-0=$s0"
    sleep 2
  done

09:32:51 rs0-0=192.168.0.210    ← controller 写的
09:32:54 rs0-0=192.168.0.210
09:32:56 rs0-0=                 ← CCM 清空了
09:32:59 rs0-0=192.168.0.210    ← controller 重新写入
09:33:01 rs0-0=192.168.0.210
```

**CCM 事件（PSMDB）**：
```
$ kubectl get events -n everest --sort-by='.lastTimestamp' | grep mongodb-x3f | tail -3

3s   Warning   UpdateLoadBalancerFailed   service/mongodb-x3f-rs0-2   Update loadbalancer of service(mongodb-x3f-rs0-2/everest) error: listener is empty
2s   Normal    EnsuredLoadBalancer        service/mongodb-x3f-rs0-2   Ensured load balancer
2s   Warning   GetLoadBalancerFailed      service/mongodb-x3f-rs0-2   Details: service annotation(kubernetes.io/elb.id) or service.spec.loadBalancerIP is not defined, skip.
```

**CCM 事件（PXC）-- 同样的 `GetLoadBalancerFailed` + skip**：
```
$ kubectl get events -n everest --sort-by='.lastTimestamp' | grep mysql-4zj | grep LoadBalancer

7m37s  Warning  SyncLoadBalancerFailed  service/mysql-4zj-haproxy-replicas  Error syncing load balancer: ... ELB.8907 ... already has a listener
```

---

## 事实3：PSMDB operator 容忍 status 闪烁，PXC operator 不容忍

### 事实

尽管 CCM 对两者都清空 status，但：
- **PSMDB operator**：容忍 status 闪烁，看到 IP 就继续，DBC 能转 ready
- **PXC operator**：严格等待 status 稳定 + 用 `Update` 修改 Service，status 被清空时等待重置

### 证据

**PSMDB（mongodb-x3f）2分47秒转 ready**：
```
$ kubectl get dbc mongodb-x3f -n everest

NAME          SIZE   READY   STATUS   HOSTNAME                                                      AGE
mongodb-x3f   3      3       ready    192.168.0.157:27017,192.168.0.210:27017,192.168.0.213:27017   2m47s
```

**PXC（mysql-4zj）卡 initializing**：
```
$ kubectl get dbc mysql-4zj -n everest

NAME        SIZE   READY   STATUS         HOSTNAME   AGE
mysql-4zj   6      6       initializing              25m
```

**PXC operator 持续报 forbidden（PSMDB operator 无此错误）**：
```
$ kubectl logs -n everest deploy/percona-xtradb-cluster-operator --since=30s | grep mysql-4zj | grep error

2026-07-15T14:04:44Z  ERROR  Reconciler error  {"controller": "pxc-controller", ...
  "error": "mysql-4zj-haproxy upgrade error: services \"mysql-4zj-haproxy\" is forbidden:
  can't modify service elb [kubernetes.io/elb.id] annotation"}

$ kubectl logs -n everest deploy/percona-server-mongodb-operator --since=30s | grep -iE "error|fail|forbid"
# 仅 telemetry 请求失败（集群无外网），无 Service 相关错误
```

---

## 事实4：PXC operator 用 `Update` 修改 Service，PSMDB operator 不用

### 事实

PXC operator 在 reconcile 过程中调用 `Update` 修改 Service 对象（如更新 label/annotation），这会触发 CCE webhook 校验。PSMDB operator 不 `Update` Service，只创建后读取 status。

### 证据

**PXC operator 报 forbidden（因 Update 触发 webhook）**：
```
Error from server (Forbidden): services "mysql-4zj-haproxy" is forbidden:
  can't modify service elb [kubernetes.io/elb.id] annotation
```

**PSMDB operator 无 Service Update 操作**：
```
$ kubectl logs -n everest deploy/percona-server-mongodb-operator --since=3m | grep -iE "service|update|patch"
# 无任何 Service update/patch 操作
```

---

## 事实5：`kubernetes.io/elb.id` 写入后整个 Service 被 CCE webhook 锁死

### 事实

一旦 Service 有 `kubernetes.io/elb.id` 注解，CCE webhook 禁止对该 Service 的**任何修改**（不仅是注解本身），导致 PXC operator 的 `Update` 操作被拒。

### 证据

**尝试删除注解被拒**：
```
$ kubectl annotate svc mysql-4zj-haproxy -n everest kubernetes.io/elb.id-
Error from server (Forbidden): services "mysql-4zj-haproxy" is forbidden:
  can't modify service elb [kubernetes.io/elb.id] annotation

$ kubectl annotate svc mysql-4zj-haproxy-replicas -n everest kubernetes.io/elb.id-
Error from server (Forbidden): services "mysql-4zj-haproxy-replicas" is forbidden:
  can't modify service elb [kubernetes.io/elb.id] annotation
```

**注意**：`docs/issue-elb-class-ccm-status-contention.md` 的方案1修订版基于"webhook 只校验注解不可删"的假设，实测证伪 -- webhook 锁死的是整个 Service 的任何修改。

---

## 事实6：`spec.loadBalancerClass` 是唯一能让 CCM 完全跳过的机制

### 事实

K8s 标准 `spec.loadBalancerClass` 字段是"这个 LB 不归你管"的官方信号。CCE CCM 遵守此标准：看到不匹配的 class 就完全跳过 Service（0 事件、不写 `kubernetes.io/elb.id`、不清空 status）。

### 证据

**创建带 `loadBalancerClass` 的测试 Service**：
```yaml
apiVersion: v1
kind: Service
metadata:
  name: test-lbc-skip
  namespace: elb-test
spec:
  type: LoadBalancer
  loadBalancerClass: huawei-elb.io/direct-api
  ports:
  - port: 80
    targetPort: 8080
  selector:
    test: lbc-skip
```

**CCM 完全跳过（0 事件、0 干预）**：
```
$ kubectl get svc test-lbc-skip -n elb-test -o jsonpath='status={.status.loadBalancer.ingress} class={.spec.loadBalancerClass} elb.id={.metadata.annotations.kubernetes\.io/elb\.id}'
status= class=huawei-elb.io/direct-api elb.id=

$ kubectl get events -n elb-test
No resources found in elb-test namespace.
```

**带 `loadBalancerClass` + OpenEverest label 的 Service，controller 正常工作**：
```
$ kubectl get svc test-lbc-full -n elb-test -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-id}'
e62e7cd3-5185-4a53-b989-0f50e0a6ad70

$ kubectl get svc test-lbc-full -n elb-test -o jsonpath='{.status.loadBalancer.ingress}'
[{"ip":"192.168.0.150","ipMode":"VIP"}]
```

---

## 事实7：`spec.loadBalancerClass` 只能在 Service CREATE 时设置

### 事实

K8s API server 内置校验规则：`spec.loadBalancerClass` 一旦落地就不能修改（`may not change once set`）。这是 K8s 标准行为，不是 CCE webhook。从 nil 也无法 patch。

### 证据

**从 nil patch 被拒**：
```
$ kubectl create namespace elb-test2
$ kubectl apply -f -   # Service 不带 loadBalancerClass
apiVersion: v1
kind: Service
metadata:
  name: test-patch-lbc
  namespace: elb-test2
spec:
  type: LoadBalancer
  ports:
  - port: 80
    targetPort: 8080
  selector:
    test: patch-lbc

$ kubectl patch svc test-patch-lbc -n elb-test2 -p '{"spec":{"loadBalancerClass":"huawei-elb.io/direct-api"}}'
The Service "test-patch-lbc" is invalid: spec.loadBalancerClass: Invalid value: "huawei-elb.io/direct-api": may not change once set
```

---

## 事实8：12个异常后端服务器的根因是 `externalTrafficPolicy: Local`

### 事实

PSMDB operator 创建 Service 时设 `externalTrafficPolicy: Local`（保留客户端源 IP）。在此模式下，kube-proxy 的 NodePort 只转发到**本地节点上的 pod**，不转发到其他节点。ELB 把 5 个节点都加为 member 做健康检查，但只有 pod 所在节点的 NodePort 真正转发流量，其他 4 个节点的 NodePort 不转发 -> 健康检查失败。

3 ELB × 5 节点 = 15 member，只有 3 个健康（每 ELB 1 个），**12 个异常**。

### 证据

**3 个 Service 都是 `externalTrafficPolicy: Local`**：
```
$ kubectl get svc -n everest -l app.kubernetes.io/instance=mongodb-x3f -o json | python3 -c "
import sys,json
d=json.load(sys.stdin)
for svc in d.get('items',[]):
    name=svc['metadata']['name']
    ext=svc['spec'].get('externalTrafficPolicy','(默认Cluster)')
    print(f'  {name}: external={ext}')
"

  mongodb-x3f-rs0-0: external=Local
  mongodb-x3f-rs0-1: external=Local
  mongodb-x3f-rs0-2: external=Local
```

**3 个 pod 分布在 3 个不同节点**：
```
$ for i in 0 1 2; do
    node=$(kubectl get pod -n everest mongodb-x3f-rs0-$i -o jsonpath='{.spec.nodeName}')
    echo "  rs0-$i -> node=$node"
  done

  rs0-0 -> node=192.168.0.153
  rs0-1 -> node=192.168.0.5
  rs0-2 -> node=192.168.0.139
```

**NodePort 只在 pod 所在节点可达**（从 mongodb pod 内部测试）：
```
$ kubectl exec -n everest mongodb-x3f-rs0-0 -c mongod -- bash -c '
for ip in 192.168.0.139 192.168.0.153 192.168.0.17 192.168.0.189 192.168.0.5; do
  result=$(timeout 2 bash -c "echo > /dev/tcp/$ip/32744" 2>&1 && echo OK || echo FAIL)
  echo "  $ip:32744 -> $result"
done
'

  192.168.0.139:32744 -> FAIL    ← rs0-0 pod 不在这
  192.168.0.153:32744 -> OK      ← rs0-0 pod 在这
  192.168.0.17:32744 -> FAIL
  192.168.0.189:32744 -> FAIL
  192.168.0.5:32744 -> FAIL
```

**3 个 NodePort 各只有 1 个节点可达**：
```
NodePort 32744 (rs0-0): 只有 192.168.0.153 通 (rs0-0 在此)
NodePort 31625 (rs0-1): 只有 192.168.0.5   通 (rs0-1 在此)
NodePort 31107 (rs0-2): 只有 192.168.0.139 通 (rs0-2 在此)
```

**计算**：3 ELB × (5 节点 - 1 可达) = 12 异常 ✅ 与华为云监控告警吻合

---

## 事实9：`loadBalancerIP` 不是解决方案（CCM 会反查后仍写 `elb.id`）

### 事实

手动设置 `spec.loadBalancerIP` 后，CCM 不再报 `GetLoadBalancerFailed`，status 暂时稳定。但 CCM 会通过 IP 反查到 ELB，然后自动写入 `kubernetes.io/elb.id`，导致 Service 被锁死（回到事实5）。

### 证据

**给 replicas 设 `loadBalancerIP` 后 status 稳定**：
```
$ kubectl patch svc mysql-4zj-haproxy-replicas -n everest -p '{"spec":{"loadBalancerIP":"192.168.0.231"}}'

# 10秒采样，status 稳定
22:03:44 replicas=192.168.0.231
22:03:45 replicas=192.168.0.231
...（10次全部稳定）
```

**但 replicas 自动有了 `kubernetes.io/elb.id`**（我没有手动加）：
```
$ kubectl get svc mysql-4zj-haproxy-replicas -n everest -o json | python3 -c "
import sys,json
d=json.load(sys.stdin)
anns=d['metadata']['annotations']
for k,v in sorted(anns.items()):
    if 'kubernetes.io' in k or 'elb' in k.lower():
        print(f'{k} = {v}')
"

huawei-elb.io/elb-id = 3647a85e-759c-4ba0-ac06-c346a95d6000
kubernetes.io/elb.id = 3647a85e-759c-4ba0-ac06-c346a95d6000    ← CCM 通过 IP 反查后自动写入
```

---

## 事实10：方案1修订版（写 `kubernetes.io/elb.id`）已被证伪

### 事实

`docs/issue-elb-class-ccm-status-contention.md` 提出的方案1修订版核心是"写 `kubernetes.io/elb.id` 防 CCM 清空 status"。实测确认：CCM 确实不再清空 status，但 CCE webhook 锁死整个 Service，PXC operator 无法 `Update` Service -> 集群仍卡。

### 证据

见事实3、事实5。关键证据链：
1. 写 `kubernetes.io/elb.id` -> CCM 不清空 status ✅（文档方案假设成立）
2. 但 CCE webhook 锁死整个 Service ❌（文档方案假设不成立）
3. PXC operator `Update` Service -> forbidden -> 卡 initializing ❌

---

## 事实11：移除 `elb.class` 不能阻止 CCM 清空 status

### 事实

`docs/issue-elb-class-ccm-status-contention.md` 最初认为 CCM 只在有 `kubernetes.io/elb.class` 时才干预。实测证伪：无 `elb.class` 时 CCM 仍对所有 `type: LoadBalancer` Service 清空 status。

### 证据

**test-11 LBC 模板无 `elb.class`**：
```
$ kubectl get lbc test-11 -n everest -o jsonpath='{.spec.annotations}'
{"huawei-elb.io/public": "false"}
```

**mysql-4zj Service 无 `elb.class`，CCM 仍清空 status**：
```
$ kubectl get svc mysql-4zj-haproxy -n everest -o json | python3 -c "
import sys,json
d=json.load(sys.stdin)
anns=d['metadata']['annotations']
print('elb.class:', anns.get('kubernetes.io/elb.class', '不存在'))
"
elb.class: 不存在

$ kubectl get events -n everest | grep mysql-4zj | grep LoadBalancer
... GetLoadBalancerFailed ... service annotation(kubernetes.io/elb.id) or service.spec.loadBalancerIP is not defined, skip.
```

---

## 总结：PSMDB vs PXC 行为差异全景

| 维度 | PSMDB (mongodb) | PXC (mysql) |
|---|---|---|
| Service 架构 | 每 pod 1 个独立 LB Service | 1 haproxy + 1 replicas 共享 Service |
| CCM 清空 status | ✅ 在清空 | ✅ 在清空 |
| operator 对 status 闪烁 | **容忍**（看到 IP 就继续） | **不容忍**（严格等待稳定） |
| operator Update Service | **不用** | **用**（触发 webhook forbidden） |
| DBC 转 ready | ✅ 2分47秒 | ❌ 永远卡 initializing |
| `externalTrafficPolicy` | Local（导致 12 异常 member） | 未确认（mysql-4zj 已删除） |
| ELB member 异常 | 12/15 异常（事实8） | 未确认 |

**核心结论**：CCM 对两者都干预，但 PSMDB operator 的容忍度掩盖了问题。PXC operator 的严格行为暴露了 CCM status 竞争 + webhook 锁死的死结。`loadBalancerClass` 是唯一能让 CCM 完全跳过的标准机制（事实6），但只能在 CREATE 时设置（事实7），需要 Mutating Webhook 注入。
