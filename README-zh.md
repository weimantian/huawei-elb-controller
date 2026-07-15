# huawei-elb-controller

[中文](README-zh.md) | **English**

---

## 概述

`huawei-elb-controller` 是一个 Kubernetes 控制器，为 [OpenEverest](https://openeverest.io/documentation/current/) 数据库集群自动创建和管理**华为云 ELB**（弹性负载均衡）实例。

**核心机制**：控制器 watch LoadBalancer 类型的 Service，自动探测 VPC/子网/可用区，**直接调用华为云 ELB API** 创建完整的 ELB 资源栈（ELB + Listener + Pool + Members + HealthCheck），不依赖 CCM autocreate。这样永久避免了 PSMDB operator 与 CCE webhook 之间的 `kubernetes.io/elb.*` 注解冲突。

**两种使用模式**：

- **自动模式**（推荐）：不创建 LBC -> ELB 全自动创建，对齐 EKS/GKE 体验
- **手动模式**：创建 LBC 作为参数模板 -> 自定义 ELB 带宽、EIP 类型等参数。LBC 存的是**参数**而非 ELB ID，多个 Service 引用同一 LBC 各自独立 ELB，零端口冲突

---

## 功能特性

- **直接 API 管理** -- 控制器直接调华为云 ELB v3 API 创建/更新/删除 ELB 及子资源，不依赖 CCM
- **零配置自动模式** -- 不创建 LBC，直接用默认参数创建 ELB，与 EKS/GKE 体验一致
- **LBC 参数模板** -- 手动模式下 LBC 存储带宽/EIP/公网等配置参数，多 Service 独立 ELB
- **VPC 自动探测** -- 从集群节点自动探测 VPC、子网和可用区
- **节点感知** -- watch nodes 变化，自动同步 ELB 后端成员（NodePort 模式）
- **ACL 自动处理** -- 自动将 Service 的 `loadBalancerSourceRanges` 转换为 ELB IP 地址组并绑定到所有监听器
- **参数热更新** -- 修改 LBC 或 Service 注解后，控制器自动调 API 更新 ELB 带宽
- **Finalizer 清理** -- Service 删除时控制器自动清理 ELB、IP 地址组和 EIP，防止孤儿资源
- **永久消除注解冲突** -- 不写任何 `kubernetes.io/elb.*` 注解，彻底解决 PSMDB operator 冲突
- **防重复创建 ELB** -- 当 OpenEverest 覆盖 `elb-id` 注解时，控制器通过名称反查恢复关联，避免重复创建孤儿 ELB
---

## 工作流程

### 自动模式（不创建 LBC）

```
创建 DBC（LBC 选 "No configuration"）
    ↓
OpenEverest 创建 LoadBalancer Service
    ↓
Service Reconciler 探测 VPC/子网/AZ
    ↓
直接调 ELB API 创建 ELB + Listener + Pool + Members + HealthCheck
    ↓
写 huawei-elb.io/elb-id 注解 + finalizer + 更新 status
    ↓
Service 获得 EXTERNAL-IP ✅
```

### 手动模式（LBC 参数模板）

```
创建 LBC（参数模板：带宽/EIP/公网等）
    ↓
创建 DBC，引用该 LBC
    ↓
OpenEverest 同步 LBC 参数到 Service
    ↓
Service Reconciler 读取参数 + 探测 VPC/子网/AZ
    ↓
直接调 ELB API 创建独立 ELB ✅
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

> 控制器**不依赖 CCM**（Cloud Controller Manager）创建 ELB，直接调华为云 API。但 CCE 集群自带的 CCM 不影响控制器运行。

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

#### 方式 A：使用 Helm（推荐）

```bash
# 1. 构建镜像
#    加 --provenance=false 避免 SWR 报 "Invalid image, fail to parse 'manifest.json'" 错误
git clone https://github.com/weimantian/huawei-elb-controller.git
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

# 3. 创建包含华为云凭据的 values 文件
cat > my-values.yaml << 'EOF'
image:
  repository: <swr-registry>/huawei-elb-controller
  tag: latest
  pullPolicy: Always

# CCE 必需：节点默认没有 SWR 认证。
# 使用 CCE 内置的 `default-secret`（每个命名空间都有）
# 或创建自己的 image pull secret。
imagePullSecrets:
  - name: default-secret

credentials:
  ak: "<你的-AK>"
  sk: "<你的-SK>"
  projectId: "<你的-ProjectID>"
  region: "<your-region>"  # 如 cn-north-4、sa-brazil-1

namespace: everest-system
EOF

# 4. 通过 Helm 安装
helm install huawei-elb-controller \
  ./charts/huawei-elb-controller \
  -f my-values.yaml \
  -n everest-system
```

#### 方式 B：使用原生清单

1. 创建凭证 Secret：

```bash
kubectl create secret generic huawei-cloud-credentials \
  --namespace everest-system \
  --from-literal=ak=<你的-AK> \
  --from-literal=sk=<你的-SK> \
  --from-literal=project-id=<你的-ProjectID> \
  --from-literal=region=cn-north-4
```

2. 构建并推送容器镜像到 SWR（同方式 A）。

然后修改 `deploy/deployment.yaml` 第 21 行镜像：`image: <swr-registry>/huawei-elb-controller:latest`

3. 应用清单：

```bash
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml
kubectl apply -f deploy/deployment.yaml
```

### 步骤 2：验证控制器运行状态

```bash
kubectl get pods -n everest-system -l app.kubernetes.io/name=huawei-elb-controller
```

预期输出：
```
NAME                                     READY   STATUS    RESTARTS   AGE
huawei-elb-controller-xxxxxxxxxx-xxxxx   1/1     Running   0          1m
```

查看日志：
```bash
kubectl logs -n everest-system deployment/huawei-elb-controller
```

预期输出：
```
INFO    starting huawei-elb-controller    {"region": "cn-north-4"}
INFO    Starting Controller               {"controller": "service"}
INFO    Starting workers                  {"controller": "service", "worker count": 1}
```

### 代码更新后重新部署

**Helm**：
```bash
git pull
docker buildx build --platform linux/amd64 --provenance=false -t huawei-elb-controller:latest .
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest
helm upgrade huawei-elb-controller ./charts/huawei-elb-controller -f my-values.yaml -n everest-system
```

**原生清单**：
```bash
git pull
docker buildx build --platform linux/amd64 --provenance=false -t huawei-elb-controller:latest .
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest
kubectl rollout restart deploy huawei-elb-controller -n everest-system
```

### 步骤 3：创建数据库集群（自动模式，推荐）

自动模式无需创建 LoadBalancerConfig。直接在 OpenEverest 中创建数据库集群：

1. 打开 OpenEverest UI（例如通过端口转发 `http://localhost:8080`）
2. 创建数据库集群时，**Load Balancer Config 下拉选择 "No configuration"**
3. 完成集群创建

OpenEverest 创建 LoadBalancer Service 后，Service Reconciler 自动：
1. 探测 VPC/子网/可用区
2. 调华为云 ELB API 创建 ELB + Listener + Pool + Members + HealthCheck
3. 写 `huawei-elb.io/elb-id` 注解和 cleanup finalizer
4. 更新 Service status，填充 EXTERNAL-IP

> **默认参数**：公网 ELB、10Mbit/s 带宽、按流量计费、5_bgp EIP 类型、TCP 健康检查（10s/10s/3次重试）。

### 步骤 4：获取连接 IP

```bash
# 获取外部 IP
kubectl get svc <service-name> -n everest -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
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

然后在创建 DBC 时引用该 LBC（`loadBalancerConfigName: my-elb-config`）。Service Reconciler 读取 LBC 参数后直接调 API 创建独立 ELB。

> **重要**：LBC 是参数模板，不是实例引用。多个 DBC 引用同一个 LBC，各自获得独立 ELB -- 零端口冲突，完全对齐 EKS/GKE 行为。

---

## 配置参考

### 自动模式默认参数

当不创建 LBC（自动模式）时，Service Reconciler 使用以下默认值：

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

LBC 的 `spec.annotations` 中可配置以下参数。OpenEverest 会自动同步到 Service，Service Reconciler 读取后调 API 创建 ELB：

| 注解 | 类型 | 可选值 | 默认值 | 说明 |
|---|---|---|---|---|
| `huawei-elb.io/public` | string | `"true"` / `"false"` | `"true"` | 公网/内网 ELB |
| `huawei-elb.io/bandwidth-size` | int | `1` – `2000` | `10` | EIP 带宽（Mbit/s） |
| `huawei-elb.io/bandwidth-charge-mode` | string | `"traffic"` / `"bandwidth"` | `"traffic"` | 按流量/按带宽计费 |
| `huawei-elb.io/eip-type` | string | `"5_bgp"` / `"5_sbgp"` / `"5_telcom"` / `"5_union"` | `"5_bgp"` | EIP 线路类型（创建后不可变） |
| `huawei-elb.io/name` | string | 自定义（≤64 字符） | `k8s-{ns_8}-{name_8}-{uid_10}` | ELB 实例名称 |

> **注意**：`eip-type` 创建后不可变更（华为云 API 限制），修改此参数需删除重建 ELB。

> **自定义名称限制**：使用 `huawei-elb.io/name` 自定义 ELB 名称时，控制器跳过防重复创建反查逻辑（因为自定义名称不含 UID，无法保证唯一性）。如果 OpenEverest 覆盖了 `elb-id` 注解，控制器可能重复创建 ELB。建议生产环境使用默认名称（含 UID，唯一且支持反查恢复）。

### 控制器写入的注解

| 注解 | 说明 |
|---|---|
| `huawei-elb.io/elb-id` | ELB 实例 ID -- 控制器创建 ELB 后写入 |
| `huawei-elb.io/elb-cleanup` | Cleanup finalizer -- Service 删除时触发 ELB 清理 |
| `huawei-elb.io/last-known-params` | 上次同步的参数快照（JSON）-- 用于检测参数变更 |
| `huawei-elb.io/acl-id` | ACL IP 地址组 ID（如有 source ranges） |
| `huawei-elb.io/acl-status` | `"on"` / `"off"` |
| `huawei-elb.io/acl-type` | `"white"`（白名单模式） |
| `huawei-elb.io/acl-cleanup` | ACL cleanup finalizer |

### Helm Values

| 参数 | 默认值 | 说明 |
|---|---|---|
| `image.repository` | `huawei-elb-controller` | 镜像仓库 |
| `image.tag` | `latest` | 镜像标签 |
| `image.pullPolicy` | `IfNotPresent` | 镜像拉取策略 |
| `credentials.ak` | `""` | 华为云 AK |
| `credentials.sk` | `""` | 华为云 SK |
| `credentials.projectId` | `""` | 华为云 Project ID |
| `credentials.region` | `""` | 华为云区域（必填，如 `cn-north-4`） |
| `existingSecret` | `""` | 使用已有 Secret（覆盖 credentials） |
| `namespace` | `everest-system` | 部署命名空间 |
| `resources.requests.cpu` | `100m` | CPU 请求 |
| `resources.requests.memory` | `128Mi` | 内存请求 |
| `resources.limits.cpu` | `500m` | CPU 限制 |
| `resources.limits.memory` | `256Mi` | 内存限制 |

> ⚠️ **重要**：`credentials.region` 必须与你的 CCE 集群所在 region 一致（如 `cn-north-4`、`sa-brazil-1`）。默认值为空，不设置会导致部署失败。

---

## 故障排查

### 控制器 Pod 无法启动

```bash
kubectl describe pod -n everest-system -l app.kubernetes.io/name=huawei-elb-controller
```

常见原因：
- **镜像未找到** -> 确保镜像已导入集群
- **Secret 缺失** -> 检查 `everest-system` 命名空间中是否存在凭据 Secret
- **RBAC 权限不足** -> 检查 ClusterRole 和 ClusterRoleBinding

### ELB 创建失败

```bash
# 查看控制器日志
kubectl logs -n everest-system deployment/huawei-elb-controller
```

常见错误：
- `network detection failed: ...` -> 检查所有节点是否在同一 VPC 内；查看控制器日志了解详情
- `creating ELB: ...` -> 查看控制器日志了解华为云 API 错误详情（如配额不足、权限不够）
- `waiting for ELB active` -> ELB 创建中，控制器会自动重试

### Service 没有外部 IP

```bash
# 1. 检查 Service 是否有 ELB ID 注解
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-id}'

# 2. 查看控制器日志确认 ELB 创建进度
kubectl logs -n everest-system deployment/huawei-elb-controller

# 3. 查看 Service 事件
kubectl describe svc <service-name> -n everest
```

### ELB 未随 Service 删除而删除

控制器使用 finalizer 机制确保 ELB 清理。如果 Service 卡在删除状态：

```bash
# 检查 Service 是否有 cleanup finalizer
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.finalizers}'

# 查看控制器日志确认删除进度
kubectl logs -n everest-system deployment/huawei-elb-controller
```

如果控制器已卸载导致 finalizer 无法清理，需手动删除 ELB 后移除 finalizer：
```bash
# 1. 在华为云控制台手动删除对应 ELB
# 2. 移除 finalizer
kubectl patch svc <service-name> -n everest --type=merge -p '{"metadata":{"finalizers":[]}}'
```

### Helm 重新安装时提示 release 名称冲突

如果执行 `helm uninstall` 后再 `helm install` 仍报错 `cannot reuse a name that is still in use`：

```bash
# 检查是否有残留的 Helm secret
kubectl get secret -n everest-system | grep sh.helm.release

# 手动删除残留 secret
kubectl delete secret -n everest-system sh.helm.release.v1.huawei-elb-controller.v1
```

### CCM 警告日志（预期噪音，无需处理）

CCE 集群自带的 CCM（Cloud Controller Manager）会 watch 所有 LoadBalancer Service。本控制器创建的 Service 不带 `kubernetes.io/elb.*` 注解，CCM 会报以下三类警告 -- **这些都是预期行为，不影响功能**：

| 警告 | 原因 | 影响 |
|---|---|---|
| `GetLoadBalancerFailed` | Service 没有 `kubernetes.io/elb.id` 注解，CCM 跳过 | 无 -- 控制器独立管理 ELB |
| `UpdateLoadBalancerFailed: listener is empty` | CCM 不认识控制器创建的 ELB，尝试更新失败 | 无 -- 控制器自行维护 listener |
| `FailedScheduling: unbound immediate PVC` | PVC 刚创建还未绑定，Pod 调度延迟 | 无 -- PVC 绑定后自动调度 |

> 如果日志中只有这三类警告，说明系统正常运行。

### Service 卡在删除状态（K8s finalizer）

如果删除 DBC 后 Service 一直卡在 `Terminating` 状态，通常是因为 LBC 中包含了 CCM 的 `kubernetes.io/elb.class` 注解，导致 CCM 也给 Service 加了 K8s 原生 finalizer（`service.kubernetes.io/load-balancer-cleanup`），而 CCM 无法删除它不认识的 ELB。

**解决方法**：手动移除 K8s finalizer（控制器的 `huawei-elb.io/elb-cleanup` finalizer 会正常清理 ELB 资源）：

```bash
# 查看卡住的 finalizer
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.finalizers}'

# 移除 K8s 原生 finalizer（保留 huawei-elb.io/elb-cleanup）
kubectl patch svc <service-name> -n everest --type=json -p='[{"op":"remove","path":"/metadata/finalizers/service.kubernetes.io~1load-balancer-cleanup"}]'
```

> **预防**：LBC 中不要添加 `kubernetes.io/elb.class` 等 CCM 注解。本控制器不需要任何 CCM 注解。

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

**顺序很重要。** 删除数据库集群（DBC）后，对应的 Service 会被删除，控制器通过 finalizer 自动删除华为云 ELB。如果先卸载控制器，Service 删除后 ELB 会成为孤儿资源并**持续计费**。

```bash
# 列出所有数据库集群
kubectl get dbc -A

# 逐个删除（删除 Service 后控制器自动清理 ELB）
kubectl delete dbc <name> -n <namespace>

# 再删除剩余的 LoadBalancerConfig
kubectl get loadbalancerconfig -A
kubectl delete loadbalancerconfig <name>
```

### 2. 卸载控制器

**先确认安装方式：**

```bash
# 如果有输出，用 Helm（方式 A）
helm list -n everest-system | grep huawei-elb
# 否则用原生清单（方式 B）
```

#### 方式 A：Helm

```bash
helm uninstall huawei-elb-controller -n everest-system

# 删除凭据 Secret（Helm 可能会保留它）
kubectl delete secret -n everest-system huawei-elb-controller-credentials 2>/dev/null
```

#### 方式 B：原生清单

```bash
kubectl delete -f deploy/deployment.yaml
kubectl delete -f deploy/clusterrolebinding.yaml
kubectl delete -f deploy/clusterrole.yaml
kubectl delete -f deploy/serviceaccount.yaml
kubectl delete secret -n everest-system huawei-cloud-credentials
```

### 3.（可选）删除 CRD

此操作会永久删除 `LoadBalancerConfig` 自定义资源定义。如果计划重新安装，请跳过此步骤。

```bash
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
| ELB 参数事后可调 | ✅ | ✅（控制器调 API） |
| ELB 生命周期管理 | CCM | **控制器（finalizer）** |
| 后端成员同步 | watch nodes/endpoints | **watch nodes（NodePort 模式）** |
| ELB 命名 | `k8s-<ns>-<svc>-<uid>` | **`k8s-{ns_8}-{name_8}-{uid_10}`** ✅ |

---

## 开源许可证

本项目基于 Apache License 2.0 开源。详见 [LICENSE](LICENSE)。
