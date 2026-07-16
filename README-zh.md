# huawei-elb-controller

[中文](README-zh.md) | **English**

---

## 概述

`huawei-elb-controller` 是一个 Kubernetes 控制器，为 [OpenEverest](https://openeverest.io/documentation/current/) 数据库集群自动创建和管理**华为云 ELB**（弹性负载均衡）实例。

控制器采用 **ELBBinding 架构**，由三个核心组件协同工作：

1. **ELBBinding CRD** -- 每个 LoadBalancer Service 对应一个 `ELBBinding` 自定义资源，作为 ELB 状态的**唯一权威存储**（ELB ID、Ingress IP、ACL、参数快照）。CRD 通过 OwnerReference 关联 Service，独立于 Service metadata 存在。
2. **Mutating Webhook** -- 拦截 `everest` 命名空间的 Service CREATE 请求，注入 `spec.loadBalancerClass: huawei-elb.io/direct-api`。CCE CCM 看到不匹配的 class 后**从源头完全跳过**该 Service（0 events、不清空 status、不写 elb.id），彻底消除 controller 与 CCM 的 Service status 竞争。
3. **Finalizer 隔离** -- cleanup finalizer 写在 `ELBBinding` 上而非 Service 上。即使 CCM 覆盖整个 Service metadata，finalizer 也不会丢失，ELB 资源不会泄漏。

控制器直接调用华为云 ELB v3 API 创建完整的 ELB 资源栈（ELB + Listener + Pool + Members + HealthCheck + ACL），不依赖 CCM autocreate，不写任何 `kubernetes.io/elb.*` 注解。

**两种使用模式**：

- **自动模式**（推荐）：不创建 LBC -> ELB 全自动创建，对齐 EKS/GKE 体验
- **手动模式**：创建 LBC 作为参数模板 -> 自定义 ELB 带宽、EIP 类型等参数。LBC 存的是**参数**而非 ELB ID，多个 Service 引用同一 LBC 各自独立 ELB，零端口冲突

---

## 功能特性

- **ELBBinding CRD 状态隔离** -- ELB 状态存储在独立 CRD 中，不受 Service metadata 覆盖影响
- **Mutating Webhook 源头阻断 CCM** -- 自动注入 `loadBalancerClass`，CCE CCM 从创建之初就完全跳过，0 干预 0 噪音
- **Finalizer 隔离** -- cleanup finalizer 在 ELBBinding 上，CCM 覆盖 Service 也丢不了
- **直接 API 管理** -- 控制器直接调华为云 ELB v3 API 创建/更新/删除 ELB 及子资源，不依赖 CCM
- **零配置自动模式** -- 不创建 LBC，直接用默认参数创建 ELB，与 EKS/GKE 体验一致
- **LBC 参数模板** -- 手动模式下 LBC 存储带宽/EIP/公网等配置参数，多 Service 独立 ELB
- **VPC 自动探测** -- 从集群节点自动探测 VPC、子网和可用区
- **节点感知** -- watch nodes/endpoints 变化，自动同步 ELB 后端成员
- **ACL 自动处理** -- 自动将 Service 的 `loadBalancerSourceRanges` 转换为 ELB IP 地址组并绑定到所有监听器
- **参数热更新** -- 修改 LBC 或 Service 注解后，控制器自动调 API 更新 ELB 带宽
- **孤儿资源兜底** -- Service 删除后 ELBBinding 级联删除被 finalizer 阻止，controller 检测孤儿 binding 后清理 ELB + EIP + IP 组，再移除 finalizer
- **防重复创建 ELB** -- 当 OpenEverest 覆盖 `elb-id` 注解时，控制器通过 ELBBinding Status 或名称反查恢复关联，避免重复创建孤儿 ELB

---

## 工作流程

### Webhook 注入（Service 创建时）

```
OpenEverest 创建 LoadBalancer Service（everest 命名空间）
        |
        v
  K8s API Server 收到 CREATE 请求
        |
        v
  API Server 调用我们的 webhook（https://huawei-elb-controller-webhook:443/mutate）
        |
        v
  Webhook 检查：
    1. 是 CREATE 操作吗？        不是 -> 放行（不修改）
    2. 是 LoadBalancer 类型吗？  不是 -> 放行（不修改）
    3. 已有 loadBalancerClass？  有   -> 放行（不修改）
    4. 有 CCM 注解               有   -> 放行（CCM 管的，别碰）
       (elb.autocreate 或 elb.id)？
        | 全部通过
        v
  注入 spec.loadBalancerClass = "huawei-elb.io/direct-api"
        |
        v
  返回 patch 给 API Server
        |
        v
  API Server 把修改后的 Service 写入 etcd
        |
        v
  CCM 看到不匹配的 loadBalancerClass -> 完全跳过   OK
  Controller 创建 ELB + 写入 status                  OK
```

> Webhook 跳过已有 `kubernetes.io/elb.autocreate` 或 `kubernetes.io/elb.id` 注解的 Service（即 CCM 管理的 Service 不受影响）。

### Webhook 证书机制

Webhook 通过 HTTPS 调用，API Server 必须验证 webhook server 的证书。`gen-webhook-cert.sh` 脚本一步完成整个证书链配置：

```
gen-webhook-cert.sh 做三件事：
  1. 生成自签 CA + server 证书
     （CN = huawei-elb-controller-webhook）
  2. 在 everest-system 创建 Secret huawei-elb-controller-webhook-tls
     -> 挂载到 controller pod 的 /tmp/k8s-webhook-server/serving-certs
     -> 在 :9443 提供 HTTPS 服务
  3. 把 CA 证书 patch 进 MutatingWebhookConfiguration.caBundle
     -> API Server 用此 CA 验证 webhook server 的证书
```

> **部署顺序很重要**：必须先 apply `webhook.yaml`（创建 MutatingWebhookConfiguration 对象），再运行 `gen-webhook-cert.sh`（patch caBundle）。如果证书 Secret 缺失，controller pod 无法启动（volume 挂载失败）；如果 caBundle 为空，API Server 拒绝 webhook 调用（证书验证失败），Service 创建会卡住。

### ELB 创建（自动模式）

```
创建 DBC（LBC 选 "No configuration")
  -> OpenEverest 创建 LoadBalancer Service（webhook 注入 loadBalancerClass）
  -> Controller 创建 ELBBinding CRD（OwnerReference 指向 Service + cleanup finalizer）
  -> Controller 探测 VPC/子网/AZ
  -> 直接调 ELB API 创建 ELB + Listener + Pool + Members + HealthCheck + ACL
  -> 写 ELBBinding Status（elbID, ingressIP, phase=Ready）+ Service 注解 + 更新 Service status
  -> Service 获得 EXTERNAL-IP ✅
```

### 手动模式（LBC 参数模板）

```
创建 LBC（参数模板：带宽/EIP/公网等）
  -> 创建 DBC，引用该 LBC
  -> OpenEverest 同步 LBC 参数到 Service
  -> Controller 读取参数 + 探测 VPC/子网/AZ
  -> 创建 ELBBinding + 直接调 ELB API 创建独立 ELB ✅
```

### Service 删除

```
删除 DBC -> OpenEverest 删除 Service
  -> ELBBinding 级联删除被 finalizer 阻止（Service 没了，binding 还在）
  -> Controller 检测孤儿 ELBBinding
  -> 清理 ELB -> EIP -> ACL IP 组
  -> 移除 ELBBinding finalizer -> ELBBinding 删除 ✅
```

### 参数变更

```
手动模式：改 LBC annotation -> OpenEverest 同步到 Service -> 控制器调 API 更新带宽
自动模式：直接改 Service annotation -> 控制器调 API 更新带宽 ✅
```

---

## 使用前提

### 1. Kubernetes 集群

一个运行中的 Kubernetes 集群，需要：
- **华为云 CCE 集群**（或华为云 ECS 自建集群）
- **StorageClass** 已配置（用于数据库持久化存储）

> 控制器**不依赖 CCM**（Cloud Controller Manager）创建 ELB，直接调华为云 API。Mutating Webhook 会让 CCM 从源头跳过本控制器管理的 Service，两者零冲突。

OpenEverest 已认证的平台：

| 平台 | Kubernetes 版本 |
|---|---|
| Google GKE | 1.31 – 1.33 |
| Amazon EKS | 1.31 – 1.33 |
| OpenShift | 4.16 – 4.18 |

### 2. OpenEverest

集群中必须已安装并运行 OpenEverest。使用 `everest.percona.com/v1alpha1` API group。

安装方法和管理员密码获取请参考 [OpenEverest 文档](https://openeverest.io/documentation/current/)。快速检查：

```bash
kubectl get pods -n everest-system
# 预期：everest-operator 和 everest-server pod 处于 Running 状态

kubectl get dbengine -n everest
# 预期：percona-postgresql-operator、percona-psmdb-operator、percona-pxc-operator
```

### 3. 华为云账号

- 已开通 ELB 服务的华为云账号
- **AK**（Access Key）和 **SK**（Secret Key）-- 在 IAM -> 我的凭证 -> 访问密钥 中创建
- **Project ID** -- 在控制台右上角用户名下拉菜单中找到

> ⚠️ **重要**：必须使用**永久** AK/SK（主账号或已授权的 IAM 子用户均可）。**临时 AK/SK**（STS Token）不被支持。所需权限：ELB Administrator、EIP Administrator、VPC ReadOnly、ECS ReadOnly。

---

## 快速开始

### 步骤 1：部署控制器

```bash
# 1. 构建镜像
#    加 --provenance=false 避免 SWR 报 "Invalid image, fail to parse 'manifest.json'" 错误
git clone https://github.com/weimantian/huawei-elb-controller.git
#    如 GitHub 无法访问（如中国大陆），可使用镜像加速：
#    git clone https://ghfast.top/https://github.com/weimantian/huawei-elb-controller.git
cd huawei-elb-controller

# Docker:
docker buildx build --platform linux/amd64 --provenance=false -t huawei-elb-controller:latest .
# nerdctl（仅有 containerd 的环境，无 Docker）:
# nerdctl build --platform linux/amd64 -t huawei-elb-controller:latest .

# 2. 登录 SWR 并推送镜像
#    在 SWR 控制台总览页面获取登录指令
# Docker:
docker login -u <你的命名空间> -p <登录令牌> <swr-registry>
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest
# nerdctl:
# nerdctl login -u <你的命名空间> -p <登录令牌> <swr-registry>
# nerdctl tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
# nerdctl push <swr-registry>/huawei-elb-controller:latest

# 3. 创建凭据 Secret
kubectl create secret generic huawei-cloud-credentials \
  --namespace everest-system \
  --from-literal=ak=<你的-AK> \
  --from-literal=sk=<你的-SK> \
  --from-literal=project-id=<你的-ProjectID> \
  --from-literal=region=cn-north-4

# 4. 修改 deploy/deployment.yaml 第 24 行镜像地址为：
#    image: <swr-registry>/huawei-elb-controller:latest

# 5. 安装 ELBBinding CRD
kubectl apply -f deploy/crd.yaml

# 6. 安装 RBAC
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml

# 7. 安装 Mutating Webhook
kubectl apply -f deploy/webhook.yaml
bash deploy/gen-webhook-cert.sh    # 生成自签证书 + Secret + patch caBundle

# 8. 部署 Controller
kubectl apply -f deploy/deployment.yaml
```

> **部署顺序**：凭据 Secret -> 镜像 -> CRD -> RBAC -> Webhook（含证书）-> Deployment。Webhook 证书脚本 `gen-webhook-cert.sh` 需在 `webhook.yaml` apply 之后运行，它会创建 TLS Secret 并把 CA bundle patch 进 MutatingWebhookConfiguration。
### 步骤 2：验证控制器运行状态

```bash
kubectl get pods -n everest-system -l app.kubernetes.io/name=huawei-elb-controller
```

预期输出：
```
NAME                                     READY   STATUS    RESTARTS   AGE
huawei-elb-controller-xxxxxxxxxx-xxxxx   1/1     Running   0          1m
```

验证 Webhook 生效：
```bash
# Webhook 注入的 loadBalancerClass 应为 huawei-elb.io/direct-api
kubectl get svc <service-name> -n everest -o jsonpath='{.spec.loadBalancerClass}'
# 预期输出：huawei-elb.io/direct-api
```

查看日志：
```bash
kubectl logs -n everest-system deployment/huawei-elb-controller
```

### 步骤 3：更新已运行的控制器

新版本发布后，如集群中已有控制器在运行，可直接原地更新，无需卸载。

```bash
# 1. 拉取最新代码
cd huawei-elb-controller
git pull
#    注意：git pull 会把 deploy/deployment.yaml 的镜像地址重置为占位符。
#    必须在下面的步骤 3 中恢复你的真实 SWR 地址。

# 2. 构建并推送新镜像到 SWR
#    重要：必须 push 到 SWR -- 集群从 SWR 拉镜像，不是你本地 Docker。
#    使用相同 tag (:latest) 没问题，因为 imagePullPolicy=Always 会强制重新拉取。
docker buildx build --platform linux/amd64 --provenance=false -t huawei-elb-controller:latest .
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest

# 3. 恢复 deployment.yaml 中的 SWR 镜像地址，然后 apply
#    git pull 会把第 24 行重置为：  image: <swr-registry>/huawei-elb-controller:latest
#    改回你的真实地址，例如：  image: swr.cn-north-4.myhuaweicloud.com/<你的命名空间>/huawei-elb-controller:latest
sed -i 's|<swr-registry>|swr.cn-north-4.myhuaweicloud.com/<你的命名空间>|' deploy/deployment.yaml
kubectl apply -f deploy/deployment.yaml
kubectl apply -f deploy/crd.yaml
kubectl apply -f deploy/webhook.yaml

# 4. 重启滚动更新以拉取新镜像
kubectl rollout restart deploy huawei-elb-controller -n everest-system

# 5. 确认新 pod 已运行
kubectl rollout status deploy huawei-elb-controller -n everest-system
```

验证新镜像确实在运行（启动日志必须输出 **Plan B**，且 webhook server 必须在 :9443 监听）：

```bash
kubectl logs -n everest-system deployment/huawei-elb-controller | head -5
# 预期输出包含：
#   "Registering webhook","path":"/mutate-v1-service"
#   "starting huawei-elb-controller (Plan B: ...)"
#   "Serving webhook server",...,"port":9443
```

> 更新过程无中断：已有的 ELBBinding 会保留，控制器会从中断处继续调谐。如 webhook 证书 Secret 被删除，重新运行 `bash deploy/gen-webhook-cert.sh` 即可。

### 步骤 4：创建数据库集群（自动模式，推荐）

自动模式无需创建 LoadBalancerConfig。直接在 OpenEverest 中创建数据库集群：

1. 打开 OpenEverest UI（例如通过端口转发 `http://localhost:8080`）
2. 创建数据库集群时，**Load Balancer Config 下拉选择 "No configuration"**
3. 完成集群创建

OpenEverest 创建 LoadBalancer Service 后，Controller 自动：
1. Webhook 注入 `loadBalancerClass`（CCM 跳过）
2. 创建 ELBBinding CRD（OwnerReference + finalizer）
3. 探测 VPC/子网/可用区
4. 调华为云 ELB API 创建 ELB + Listener + Pool + Members + HealthCheck + ACL
5. 写 ELBBinding Status（权威状态）+ Service 注解（兼容可见性）+ 更新 Service status
6. Service 获得 EXTERNAL-IP ✅

> **默认参数**：公网 ELB、10Mbit/s 带宽、按流量计费、5_bgp EIP 类型、TCP 健康检查（10s/10s/3次重试）。

### 步骤 5：获取连接 IP

```bash
# 从 Service status 获取外部 IP
kubectl get svc <service-name> -n everest -o jsonpath='{.status.loadBalancer.ingress[0].ip}'

# 或从 ELBBinding Status 获取（权威状态）
kubectl get elbbinding <service-name> -n everest -o jsonpath='{.status.ingressIP}'
```

完整连接指南（密码获取、客户端安装、各引擎连接命令）请参考 [OpenEverest 文档](https://openeverest.io/documentation/current/)。速查表：

| 引擎 | 端口 | 密码键 | 连接命令 |
|---|---|---|---|
| PostgreSQL | 5432 | `.data.postgres` | `psql -h <IP> -U postgres -d <db-name>` |
| MySQL / PXC | 3306 | `.data.root` | `mysql -h <IP> -u root -p` |
| MongoDB / PSMDB | 27017 | `.data.clusterAdmin` | `mongosh "mongodb://clusterAdmin:<password>@<IP>:27017/?replicaSet=rs0"` |

获取密码：
```bash
kubectl get secret everest-secrets-<db-name> -n everest -o jsonpath='{.data.<password-key>}' | base64 -d
```

### （可选）手动模式：使用 LBC 参数模板

如需自定义 ELB 参数（带宽、计费模式等），创建 LBC 作为参数模板：

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: my-elb-config
spec:
  annotations:
    huawei-elb.io/public: "true"
    huawei-elb.io/bandwidth-size: "20"
    huawei-elb.io/bandwidth-charge-mode: "traffic"
    huawei-elb.io/eip-type: "5_bgp"
EOF
```

然后在创建 DBC 时引用该 LBC（`loadBalancerConfigName: my-elb-config`）。Controller 读取 LBC 参数后直接调 API 创建独立 ELB。

> **重要**：LBC 是参数模板，不是实例引用。多个 DBC 引用同一个 LBC，各自获得独立 ELB -- 零端口冲突，完全对齐 EKS/GKE 行为。

---

## 配置参考

### 自动模式默认参数

当不创建 LBC（自动模式）时，Controller 使用以下默认值：

| 参数 | 默认值 | 说明 |
|---|---|---|
| ELB 类型 | 公网（带 EIP） | 与 GKE 一致，对外暴露 |
| 带宽 | `10` Mbit/s | 华为云最小值 |
| 计费模式 | `traffic`（按流量） | 按实际流量付费 |
| EIP 类型 | `5_bgp` | BGP 多线 |
| ELB 名称 | `k8s-{ns_8}-{name_8}-{uid_10}` | 对齐 EKS/GKE 命名，64 字符内 |
| 健康检查 | TCP, 10s/10s/3次重试 | 对齐 EKS NLB 默认 |
| 后端模式 | NodePort（node IP + NodePort） | 对齐 GKE 默认 |
| VPC ID | 自动探测 | 从节点 ECS 元数据获取 |
| 子网 ID | 自动探测 | 从节点标签获取 |
| 可用区 | 自动探测 | 从节点 zone 标签获取 |

### 手动模式注解（LBC 参数模板）

LBC 的 `spec.annotations` 中可配置以下参数。OpenEverest 会自动同步到 Service，Controller 读取后调 API 创建 ELB：

| 注解 | 类型 | 可选值 | 默认值 | 说明 |
|---|---|---|---|---|
| `huawei-elb.io/public` | string | `"true"` / `"false"` | `"true"` | 公网/内网 ELB |
| `huawei-elb.io/bandwidth-size` | int | `1` – `2000` | `10` | EIP 带宽（Mbit/s） |
| `huawei-elb.io/bandwidth-charge-mode` | string | `"traffic"` / `"bandwidth"` | `"traffic"` | 按流量/按带宽计费 |
| `huawei-elb.io/eip-type` | string | `"5_bgp"` / `"5_sbgp"` / `"5_telcom"` / `"5_union"` | `"5_bgp"` | EIP 线路类型（创建后不可变） |
| `huawei-elb.io/name` | string | 自定义（≤64 字符） | `k8s-{ns_8}-{name_8}-{uid_10}` | ELB 实例名称 |

> **注意**：`eip-type` 创建后不可变更（华为云 API 限制），修改此参数需删除重建 ELB。

### ELBBinding CRD

每个 LoadBalancer Service 对应一个 `ELBBinding` 资源（同名同命名空间），是 ELB 状态的**唯一权威存储**：

```bash
kubectl get elbbinding -n everest
# NAME                    SERVICE                 ELB-ID                                PHASE    AGE
# mongodb-rs0-0           mongodb-rs0-0           b331e0b2-eae6-4ebe-81b0-9e0e8010f8e3  Ready    5m
```

**Spec 字段**：

| 字段 | 说明 |
|---|---|
| `serviceName` | 关联的 Service 名称（同名同命名空间，不可变） |
| `serviceUID` | Service UID，防止 Service 名称复用（不可变） |

**Status 字段**（权威状态）：

| 字段 | 说明 |
|---|---|
| `elbID` | 华为云 ELB 实例 ID |
| `ingressIP` | ELB 对外 IP（公网 EIP 或内网 VIP） |
| `phase` | `Provisioning` / `Ready` / `Deleting` |
| `aclID` | ACL IP 地址组 ID（如有 source ranges） |
| `aclStatus` | `"on"` / `"off"` |
| `aclType` | `"white"`（白名单模式） |
| `lastKnownParams` | 上次同步的参数快照（JSON），用于检测参数变更 |
| `observedGeneration` | 已观察到的 generation |

### 控制器写入的注解

> 以下注解写入 Service（兼容性/可见性保留），但**权威状态存储在 ELBBinding Status** 中。即使 Service 注解被 OpenEverest 覆盖，Controller 会从 ELBBinding Status 自愈。

| 注解 | 说明 |
|---|---|
| `huawei-elb.io/elb-id` | ELB 实例 ID |
| `huawei-elb.io/last-known-params` | 上次同步的参数快照（JSON） |
| `huawei-elb.io/acl-id` | ACL IP 地址组 ID（如有 source ranges） |
| `huawei-elb.io/acl-status` | `"on"` / `"off"` |
| `huawei-elb.io/acl-type` | `"white"`（白名单模式） |

> **Finalizer**：`huawei-elb.io/elb-cleanup` 写在 **ELBBinding** 上（不在 Service 上），确保 CCM 覆盖 Service metadata 时 finalizer 不丢失。

### Webhook 注入

Mutating Webhook 拦截 `everest` 命名空间的 Service CREATE 请求，注入：

| 字段 | 值 | 说明 |
|---|---|---|
| `spec.loadBalancerClass` | `huawei-elb.io/direct-api` | CCM 看到不匹配的 class 完全跳过该 Service |

**跳过条件**（不注入）：Service 已有 `kubernetes.io/elb.autocreate` 或 `kubernetes.io/elb.id` 注解（CCM 管理的 Service 不受影响）。

> ⚠️ **重要**：`region` 必须与你的 CCE 集群所在 region 一致（如 `cn-north-4`、`sa-brazil-1`）。凭据 Secret 中的 `region` key 不设置会导致控制器启动失败。

> **凭据 Secret 字段**：`ak`、`sk`、`project-id`、`region`（见步骤 3 的 `kubectl create secret` 命令）。

---

## 故障排查

### 控制器 Pod 无法启动

```bash
kubectl describe pod -n everest-system -l app.kubernetes.io/name=huawei-elb-controller
```

常见原因：
- **镜像未找到** -> 确保镜像已导入集群
- **Secret 缺失** -> 检查 `everest-system` 命名空间中是否存在凭据 Secret
- **Webhook 证书 Secret 缺失** -> 检查 `huawei-elb-controller-webhook-tls` Secret 是否存在，不存在则重新运行 `bash deploy/gen-webhook-cert.sh`
- **RBAC 权限不足** -> 检查 ClusterRole 和 ClusterRoleBinding

### ELB 创建失败

```bash
# 查看控制器日志
kubectl logs -n everest-system deployment/huawei-elb-controller

# 查看 ELBBinding 状态
kubectl get elbbinding <service-name> -n everest -o yaml
```

常见错误：
- `network detection failed: ...` -> 检查所有节点是否在同一 VPC 内；查看控制器日志了解详情
- `creating ELB: ...` -> 查看控制器日志了解华为云 API 错误详情（如配额不足、权限不够）
- `waiting for ELB active` -> ELB 创建中，控制器会自动重试

### Service 没有外部 IP

```bash
# 1. 检查 Webhook 是否注入了 loadBalancerClass（应返回 huawei-elb.io/direct-api）
kubectl get svc <service-name> -n everest -o jsonpath='{.spec.loadBalancerClass}'

# 2. 检查 ELBBinding 是否创建且 phase=Ready
kubectl get elbbinding <service-name> -n everest

# 3. 查看控制器日志确认 ELB 创建进度
kubectl logs -n everest-system deployment/huawei-elb-controller

# 4. 查看 Service 事件
kubectl describe svc <service-name> -n everest
```

> 如果 `loadBalancerClass` 为空，说明 Webhook 未生效。检查 MutatingWebhookConfiguration 是否存在、CA bundle 是否已 patch、webhook pod 是否 Running。

### CCM 干预（Webhook 未生效的征兆）

正常情况下，Webhook 注入 `loadBalancerClass` 后 CCM **完全跳过**，不会产生任何 `GetLoadBalancerFailed` 事件。如果看到以下现象，说明 **Webhook 未正确配置**：

```bash
# 检查是否有 CCM 干预事件（正常应为 0）
kubectl get events -n everest --field-selector reason=GetLoadBalancerFailed
```

排查步骤：
1. `kubectl get mutatingwebhookconfiguration huawei-elb-controller-webhook` -- 确认 Webhook 配置存在
2. `kubectl get secret huawei-elb-controller-webhook-tls -n everest-system` -- 确认证书 Secret 存在
3. `kubectl get pod -n everest-system -l app.kubernetes.io/name=huawei-elb-controller` -- 确认 pod Running 且 9443 端口就绪
4. 重新运行 `bash deploy/gen-webhook-cert.sh` 修复证书

### ELBBinding 卡在删除状态

Controller 通过 ELBBinding 上的 finalizer 确保 ELB 清理。如果 ELBBinding 卡在 `Terminating`：

```bash
# 检查 ELBBinding 是否有 cleanup finalizer
kubectl get elbbinding <name> -n everest -o jsonpath='{.metadata.finalizers}'

# 查看控制器日志确认删除进度
kubectl logs -n everest-system deployment/huawei-elb-controller
```

如果 Controller 已卸载导致 finalizer 无法清理，需手动删除 ELB 后移除 finalizer：
```bash
# 1. 在华为云控制台手动删除对应 ELB（及关联的 EIP、IP 组）
# 2. 移除 ELBBinding finalizer
kubectl patch elbbinding <name> -n everest --type=merge -p '{"metadata":{"finalizers":[]}}'
```


### EIP 配额不足

华为云账号的 EIP 配额有限（默认约 5-6 个）。创建公网 ELB 时如果配额已满，ELB 创建会失败：

```
creating ELB: ... Quota exceeded for resources: publicip ...
```

**解决方法**：
1. 在华为云控制台释放不再使用的 EIP（网络 -> 虚拟私有云 VPC -> 弹性公网 IP）
2. 或使用内网 ELB（设置 `huawei-elb.io/public: "false"`），不消耗 EIP 配额
3. 释放配额后控制器会自动重试创建

> **提示**：删除 DBC 时控制器会自动删除 ELB 和关联的 EIP，释放配额。但如果之前有手动创建的 ELB/EIP，需在控制台手动清理。

---

## 卸载

### 1. 先删除数据库集群

**顺序很重要。** 删除数据库集群（DBC）后，对应的 Service 会被删除，Controller 通过 ELBBinding finalizer 自动删除华为云 ELB。如果先卸载 Controller，ELB 会成为孤儿资源并**持续计费**。

```bash
# 列出所有数据库集群
kubectl get dbc -A

# 逐个删除（删除 Service 后 Controller 自动清理 ELB）
kubectl delete dbc <name> -n <namespace>

# 确认 ELBBinding 全部清理
kubectl get elbbinding -A
# 预期：无残留

# 再删除剩余的 LoadBalancerConfig
kubectl get loadbalancerconfig -A
kubectl delete loadbalancerconfig <name>
```

```bash
kubectl delete -f deploy/deployment.yaml
kubectl delete -f deploy/webhook.yaml
kubectl delete -f deploy/clusterrolebinding.yaml
kubectl delete -f deploy/clusterrole.yaml
kubectl delete -f deploy/serviceaccount.yaml
kubectl delete secret -n everest-system huawei-cloud-credentials
kubectl delete secret -n everest-system huawei-elb-controller-webhook-tls
```

### 3.（可选）删除 CRD

此操作会永久删除 `ELBBinding` 和 `LoadBalancerConfig` 自定义资源定义。如果计划重新安装，请跳过此步骤。

```bash
kubectl delete crd elbbindings.huawei-elb.io
kubectl delete crd loadbalancerconfigs.everest.percona.com
```

---

## 与 EKS/GKE 对比

在 Amazon EKS 和 Google GKE 上，创建 `type: LoadBalancer` 的 Service 会自动创建云负载均衡器 -- 不需要部署额外控制器。本控制器为华为云 CCE 补上了这一层：

| 特性 | EKS / GKE | CCE + 本控制器 |
|---|---|---|
| 额外部署控制器 | 不需要 | 需要 |
| 用户填 VPC/子网/可用区 | 不用 | **不用（自动探测）** |
| 配置复杂度 | 零 | **零** |
| LBC 角色 | 参数模板 | **参数模板** ✅ |
| 多 Service 引用同一 LBC | 各自独立 LB | **各自独立 ELB** ✅ |
| ELB 创建方式 | 云平台 CCM | **控制器直接调 API** |
| CCM 冲突隔离 | N/A | **Webhook 源头阻断** ✅ |
| 状态存储 | Service status | **ELBBinding CRD（隔离）** ✅ |
| ELB 参数事后可调 | ✅ | ✅（控制器调 API） |
| ELB 生命周期管理 | CCM | **Controller（ELBBinding finalizer）** |
| 后端成员同步 | watch nodes/endpoints | **watch nodes/endpoints** |
| ELB 命名 | `k8s-<ns>-<svc>-<uid>` | **`k8s-{ns_8}-{name_8}-{uid_10}`** ✅ |

---

## 开源许可证

本项目基于 Apache License 2.0 开源。详见 [LICENSE](LICENSE)。
