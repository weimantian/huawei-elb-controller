# huawei-elb-controller

[中文](README-zh.md) | **English**

---

## 概述

`huawei-elb-controller` 是一个 Kubernetes 控制器，为 [OpenEverest](https://openeverest.io/documentation/current/)（原 Percona Everest）数据库集群自动创建和管理**华为云 ELB**（弹性负载均衡）实例。

**解决的问题**：OpenEverest 的 `LoadBalancerConfig` CR 可以向 Kubernetes Service 注入 annotation，但不会创建华为云 ELB 本身。没有这个控制器，你每次都需要在华为云控制台手动创建 ELB、复制其 ID、再粘贴到 CR 中。

**工作原理**：监听 `LoadBalancerConfig` CR，调用华为云 ELB v3 API 自动创建/删除 ELB，并将 ELB ID 写回 CR。OpenEverest 的 operator 随后读取 ELB ID，将其添加到 Service，华为云 CCM 绑定 ELB —— 为你的数据库集群提供外部负载均衡访问入口。

---

## 功能特性

- **零配置自动探测** —— 从集群节点自动探测 VPC、子网和可用区（与 EKS/GKE 体验一致）
- **默认公网 ELB** —— 默认创建带 EIP 的公网 ELB；设 `huawei-elb.io/public: "false"` 创建内网 ELB
- **完整生命周期管理** —— 通过华为云 ELB v3 API 创建、监控、删除 ELB，带 finalizer 安全保障
- **状态可见** —— 在 CR 上暴露 `ready`、`elb-status`、`error`、`public-ip` 注解
- **UI 友好** —— 与 OpenEverest Web UI 无缝协作；端到端配置无需 `kubectl`
- **多区域支持** —— 通过 `huawei-elb.io/region` 注解按 CR 覆盖区域

---

## 工作流程

```
你创建 LoadBalancerConfig（包含 ELB 参数）
    ↓
huawei-elb-controller 通过华为云 API 创建 ELB
    ↓
控制器将 ELB ID 写回 LoadBalancerConfig
    ↓
你创建 DatabaseCluster，引用该 LoadBalancerConfig
    ↓
OpenEverest operator 创建 LoadBalancer 类型 Service
    ↓
华为云 CCM 绑定 ELB → Service 获得外部 IP
    ↓
你通过 ELB 的 IP 连接数据库
```

控制器在 `everest-system` 命名空间创建一个类型为 `LoadBalancer` 的 Kubernetes `Service`。这个 Service 携带华为云 ELB 注解，告诉 CCE 云控制器管理器（CCM）如何将 Service 绑定到华为云 ELB 实例：

- `kubernetes.io/elb.id: <elbID>` — 将 Service 绑定到预创建的 ELB 实例（即本控制器创建的 ELB）
- `kubernetes.io/elb.class: union` — 使用华为云 ELB 负载均衡模式

当 CCE CCM 检测到带有这些注解的 `LoadBalancer` Service 时，会根据 Service 的端口和 Pod 端点配置 ELB 监听器和后端服务器组。控制器先通过华为云 ELB v3 API 创建 ELB，然后创建带有 `kubernetes.io/elb.id` 注解的 Service 指向新创建的 ELB，CCE CCM 随后完成绑定。

这是 CCE 集成外部负载均衡器的标准机制 — 详见 [CCE 文档](https://support.huaweicloud.com/usermanual-cce/cce_10_0385.html)。

---

## 使用前提

### 1. Kubernetes 集群

一个运行中的 Kubernetes 集群，需要：
- **华为云 CCM**（Cloud Controller Manager）已安装 —— 用于将 ELB 绑定到 Service
- **StorageClass** 已配置（用于数据库持久化存储）

OpenEverest 已认证的平台：

| 平台 | Kubernetes 版本 |
|---|---|
| Google GKE | 1.31 – 1.33 |
| Amazon EKS | 1.31 – 1.33 |
| OpenShift | 4.16 – 4.18 |

> 其他平台（AKS、DigitalOcean、原生 kubeadm）可以运行但未完全认证。本地集群（minikube、kind、k3d）因网络限制不推荐使用。
>
> **华为云 CCE 集群**：CCM 已预装。如果是华为云 ECS 自建集群，需要单独安装 CCM。

### 2. OpenEverest（原 Percona Everest）

集群中必须已安装并运行 OpenEverest。使用 `everest.percona.com/v1alpha1` API group（名称沿用原 "Percona Everest" 项目）。

安装方法和管理员密码获取请参考 [OpenEverest 文档](https://openeverest.io/documentation/current/)。快速检查：

```bash
kubectl get pods -n everest-system
# 预期：everest-operator 和 everest-server pod 处于 Running 状态

kubectl get dbengine -n everest
# 预期：percona-postgresql-operator、percona-psmdb-operator、percona-pxc-operator
```

### 3. 华为云账号

- 已开通 ELB 服务的华为云账号
- **AK**（Access Key）和 **SK**（Secret Key）-- 在 IAM -> 我的凭证 -> 访问密钥 中创建。支持永久密钥（主账号或 IAM 子账号）或临时密钥（STS）。临时密钥需额外设置 `securityToken` 字段。
- **Project ID** -- 在 IAM -> 我的凭证 -> 项目 中查看。必须与集群所在 region 一致（每个 region 有独立的 Project ID）。

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
  -f my-values.yaml
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

2. 构建并推送容器镜像到 SWR：

```bash
# Docker:
docker buildx build --platform linux/amd64 --provenance=false -t huawei-elb-controller:latest .
# nerdctl（仅有 containerd 的环境，无 Docker）:
# nerdctl build --platform linux/amd64 -t huawei-elb-controller:latest .

# 登录 SWR 并推送镜像
# 在 SWR 控制台总览页面获取登录指令
# Docker:
docker login -u <你的命名空间> -p <登录令牌> <swr-registry>
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest
# nerdctl:
# nerdctl login -u <你的命名空间> -p <登录令牌> <swr-registry>
# nerdctl tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
# nerdctl push <swr-registry>/huawei-elb-controller:latest
```

然后修改 `deploy/deployment.yaml`，将容器镜像改为 `<swr-registry>/huawei-elb-controller:latest`。

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
INFO    Starting Controller               {"controller": "loadbalancerconfig"}
INFO    Starting workers                  {"controller": "loadbalancerconfig", "worker count": 1}
```

### 步骤 3：创建 LoadBalancerConfig

在 CCE 上，控制器自动从集群节点探测 VPC、子网和可用区 —— 无需手动配置。默认 ELB 类型为**公网**（带 EIP）。设 `huawei-elb.io/public: "false"` 创建内网 ELB。

#### 方式 A（推荐）：零配置 via OpenEverest UI

通过 OpenEverest Web UI 创建 LoadBalancerConfig —— 无需 `kubectl`：

1. 在浏览器中打开 OpenEverest UI（例如通过端口转发访问 `http://localhost:8080`）。
2. 进入 **Settings → Policies & Configurations → Load Balancer Configuration**。
3. 点击 **Create configuration**。
4. 填写配置**名称**（例如 `huawei-elb`）。
5. 如果是**内网 ELB**，添加一个注解：
   - Key: `huawei-elb.io/public`，Value: `false`
   - 如果是**公网 ELB**（默认），跳过此步 —— 注解留空即可。
6. 点击 **Save** 保存。

控制器会自动检测到新的 CR，从节点自动探测 VPC/子网/可用区，并在几秒内创建 ELB。可通过 `kubectl get loadbalancerconfig` 验证。

#### 方式 B：零配置 via kubectl

**公网 ELB**（默认，带弹性公网 IP，可从互联网访问）：

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: huawei-elb
spec:
  annotations: {}
EOF
```

**内网 ELB**（仅 VPC 内访问 —— 只需填一个注解）：

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: huawei-internal-elb
spec:
  annotations:
    huawei-elb.io/public: "false"
EOF
```

控制器会：

1. 列出所有节点，收集内网 IP 和可用区标签（`topology.kubernetes.io/zone`）
2. 调用华为云 VPC API，根据节点 IP 匹配所在的 VPC 和子网
3. 将探测到的值写入 `metadata.annotations`（标记 `huawei-elb.io/auto-detected: "true"`）
4. 使用探测到的参数创建 ELB

这提供了与 EKS/GKE 类似的零配置体验 —— 只需创建配置，控制器自动完成其余工作。

> **注意**：自动探测适用于所有节点在同一 VPC 的 CCE 集群。如果节点跨多个 VPC，控制器会在 `huawei-elb.io/error` 注解中报错。

##### 公网 vs 内网 ELB

自动探测覆盖 VPC、子网和可用区 —— 但**公网还是内网是用户的选择**，无法自动探测：

| 注解 | 不填时（自动探测） | 用户手动设置 |
|---|---|---|
| `huawei-elb.io/vpc-id` | ✅ 从节点 IP 自动探测 | 需要时覆盖 |
| `huawei-elb.io/subnet-id` | ✅ 从节点 IP 自动探测 | 需要时覆盖 |
| `huawei-elb.io/availability-zones` | ✅ 从节点标签自动探测 | 需要时覆盖 |
| `huawei-elb.io/public` | 默认 `true`（公网） | 设为 `"false"` 创建内网 ELB |
### 步骤 4：等待 ELB 就绪

```bash
# 等待 ELB 创建完成并激活（最多 120 秒）
kubectl wait loadbalancerconfig huawei-elb \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# 验证 ELB 状态
kubectl get loadbalancerconfig huawei-elb -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-status}'
# 预期：ACTIVE

# 验证 ELB ID 已写入
kubectl get loadbalancerconfig huawei-elb -o jsonpath='{.spec.annotations}'
# 预期：{"kubernetes.io/elb.id":"xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"}
```

> **重要**：在创建 DatabaseCluster 之前，务必等待 `ready=true`。这确保 ELB ID 已写入 LoadBalancerConfig，OpenEverest operator 读取时能获取到。

### 步骤 5：创建数据库集群

创建数据库集群时引用步骤 4 创建的 LoadBalancerConfig。完整的 UI/kubectl 流程请参考 [OpenEverest 文档](https://openeverest.io/documentation/current/)，关键字段是 `spec.proxy.expose.loadBalancerConfigName`：

```yaml
spec:
  proxy:
    expose:
      type: LoadBalancer
      loadBalancerConfigName: huawei-elb  # 步骤 4 创建的 LBC
```

> **注意**：如果 UI 中 LoadBalancer config 下拉菜单显示 "- No configuration -"，说明 ELB 还未就绪。请回到步骤 5 等待 `ready=true`。

### 步骤 6：验证连接

数据库运行后，验证 ELB 已正确绑定并获取连接 IP。

#### 1. 验证 ELB 已绑定到数据库 Service

```bash
# 从 Service 获取 ELB ID（应与 LBC 的 elb.id 一致）
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.id}'

# 验证与 LBC 一致
kubectl get loadbalancerconfig <lbc-name> -o jsonpath='{.spec.annotations.kubernetes\.io/elb\.id}'
```

#### 2. 获取连接 IP

**公网 ELB (EIP)** — 从集群外部连接：
```bash
kubectl get loadbalancerconfig <lbc-name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/public-ip}'
# 输出：<EIP-address>
```

**内网 ELB (VIP)** — 从 VPC 内部连接：
```bash
kubectl get svc <service-name> -n everest -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
# 输出：<ELB-VIP>
```

#### 3. 连接数据库

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

> **注意**：根据 ELB 类型选择正确的连接路径：
> - **公网 ELB (EIP)**：从集群**外部**（本地电脑）测试。从 Pod 内部连接公网 EIP 可能因 CCE 网络限制超时。
> - **内网 ELB (VIP)**：只能在 VPC **内部**访问。从集群内的 Pod 测试：
>   ```bash
>   kubectl exec -it <pod-name> -n everest -- mysql -h <ELB-VIP> -u root -p
>   ```

---

## 配置参考

### 自动检测注解

以下注解在 CCE 上会从集群节点自动检测。均为可选 — 如未设置，控制器会自动填充。如需覆盖，在 `metadata.annotations` 中手动设置。

| Annotation | 自动检测来源 |
|---|---|
| `huawei-elb.io/vpc-id` | 通过节点 `machineID` 查询 ECS 服务器元数据 |
| `huawei-elb.io/subnet-id` | Neutron 子网 ID（通过 VPC API 从 Virsubnet ID 转换） |
| `huawei-elb.io/availability-zones` | 节点 label `topology.kubernetes.io/zone` |

### 可选 Annotation

| Annotation | 默认值 | 说明 |
|---|---|---|
| `huawei-elb.io/public` | `true` | `false` = 内网 ELB；默认 `true` = 公网 ELB（带 EIP） |
| `huawei-elb.io/bandwidth-size` | `10` | EIP 带宽（Mbit/s）—— 仅公网 ELB |
| `huawei-elb.io/bandwidth-charge-mode` | `traffic` | `traffic`（按流量计费）或 `bandwidth`（按带宽计费） |
| `huawei-elb.io/public-ip-network-type` | `5_bgp` | EIP 网络类型；`5_bgp` 为 BGP |
| `huawei-elb.io/region` | 全局 REGION | 为特定 CR 覆盖华为云区域 |

### 控制器写入的 Annotation

| 位置 | Annotation | 说明 |
|---|---|---|
| `spec.annotations` | `kubernetes.io/elb.id` | ELB ID —— OpenEverest 复制到 Service；CCM 用它绑定 ELB |
| `metadata.annotations` | `huawei-elb.io/ready` | `true` 表示 ELB 就绪；`false` 表示创建中或出错 |
| `metadata.annotations` | `huawei-elb.io/elb-status` | ELB 状态：`ACTIVE`、`PENDING_CREATE` 等 |
| `metadata.annotations` | `huawei-elb.io/public-ip` | EIP 地址（仅公网 ELB） |
| `metadata.annotations` | `huawei-elb.io/error` | 最近一次错误信息（正常时为空） |

### 手动覆盖

如果自动检测失败或需要覆盖，给 `LoadBalancerConfig` CR 加注解：

```yaml
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: my-elb-config
  annotations:
    huawei-elb.io/vpc-id: "<your-vpc-id>"
    huawei-elb.io/subnet-id: "<your-subnet-id>"
    huawei-elb.io/availability-zones: "cn-north-4a"
spec:
  # ... 其余 spec
```

只要设置了这些注解，控制器就会使用提供的值，跳过对应字段的自动检测。

### Helm Values

| 参数 | 默认值 | 说明 |
|---|---|---|
| `image.repository` | `huawei-elb-controller` | 镜像仓库 |
| `image.tag` | `latest` | 镜像标签 |
| `image.pullPolicy` | `IfNotPresent` | 镜像拉取策略 |
| `credentials.ak` | `""` | 华为云 AK |
| `credentials.sk` | `""` | 华为云 SK |
| `credentials.projectId` | `""` | 华为云 Project ID |
| `credentials.region` | `cn-north-4` | 华为云区域 |
| `credentials.securityToken` | `""` | STS 安全令牌（仅临时 AK/SK 需要） |
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
- **镜像未找到** → 确保镜像已导入集群
- **Secret 缺失** → 检查 `everest-system` 命名空间中是否存在 `huawei-cloud-credentials` Secret
- **RBAC 权限不足** → 检查 ClusterRole 和 ClusterRoleBinding

### 401 IAM 认证失败 (apigw.0301)

```
error_code: apigw.0301
error_message: incorrect IAM authentication, information: unauthorized
```

这是华为云 API 网关拒绝了凭据。原因：

1. **Project ID 与 region 不匹配** -- 每个 region 有独立的 Project ID。在 IAM -> 我的凭证 -> 项目 中核对，确保 Project ID 与 `credentials.region` 一致。
2. **临时 AK/SK 缺少 Security Token** -- 临时凭据（STS）需要三件套：AK + SK + Security Token。在 values.yaml 中设置 `credentials.securityToken`。
3. **AK/SK 填错或已禁用** -- 在 IAM -> 我的凭证 -> 访问密钥 中检查。

```bash
# 查看 LBC 的错误注解
kubectl get lbc <name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/error}'

# 查看控制器日志详情
kubectl logs -n everest-system deployment/huawei-elb-controller --tail=20
```


### ELB 创建失败

```bash
# 查看错误 annotation
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/error}'

# 查看控制器日志获取 API 错误详情
kubectl logs -n everest-system deployment/huawei-elb-controller
```

常见错误：
- `auto-detection failed: ...` → 检查所有节点是否在同一 VPC 内；查看控制器日志了解详情
- `creating ELB: ...` → 查看控制器日志了解华为云 API 错误详情

### Service 没有外部 IP

```bash
# 1. 检查 LoadBalancerConfig 是否就绪
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'
# 应为 "true"

# 2. 检查 Service 是否有 elb.id annotation
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.id}'
# 应显示 ELB ID

# 3. 检查 CCM 是否运行
kubectl get pods -A | grep cloud-controller
```

### LoadBalancerConfig 删除卡住

```bash
# 检查 finalizer 是否存在
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.finalizers}'
# 应包含 "huawei-elb.io/finalizer"

# 查看控制器日志
kubectl logs -n everest-system deployment/huawei-elb-controller --tail=20

# 如果 ELB 已在华为云控制台手动删除，
# 控制器会检测到 404 并自动移除 finalizer。
```

## 卸载

### 1. 先删除所有 LoadBalancerConfig（重要）

**顺序很重要。** 删除 `LoadBalancerConfig` 会通过 finalizer 触发控制器删除对应的华为云 ELB。如果先卸载控制器，ELB 会成为孤儿资源并**持续计费**（公网 ELB 的 EIP 按小时收费）。

```bash
# 列出所有 LoadBalancerConfig
kubectl get loadbalancerconfig -A

# 逐个删除（控制器会删除对应的华为云 ELB）
kubectl delete loadbalancerconfig <name>
```

在继续之前，查看控制器日志确认 ELB 已删除：

```bash
kubectl logs -n everest-system deployment/huawei-elb-controller --tail=5
# 等待看到每个 LBC 对应的 "deleted ELB" 日志
```

> 如果 `LoadBalancerConfig` 带有 `everest.percona.com/in-use-protection` finalizer，说明它仍被某个数据库集群引用。请先删除该数据库（或切换到其他 LBC）。

### 2. 卸载控制器

#### 方式 A：Helm

```bash
helm uninstall huawei-elb-controller

# 删除凭据 Secret（Helm 可能会保留它）
kubectl delete secret -n everest-system huawei-elb-controller-credentials 2>/dev/null
```

#### 方式 B：原始 Manifests

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

在 Amazon EKS 和 Google GKE 上，创建 `type: LoadBalancer` 的 Service 会自动创建云负载均衡器 —— 不需要部署额外控制器，不需要手动配置 VPC/子网。云平台的 CCM 直接从节点元数据读取 VPC/子网信息。

华为云 CCE 的 CCM 缺少这种自动探测能力 —— 用户必须手动查询并填写 VPC/子网/可用区参数。本控制器补上了这一层：

| 特性 | EKS / GKE | CCE + 本控制器 |
|---|---|---|
| 额外部署控制器 | 不需要 | 需要 |
| 用户填 VPC/子网/可用区 | 不用 | **不用（自动探测）** |
| 配置复杂度 | 零 | **零** |
| ELB 生命周期管理 | CCM | 控制器 + finalizer |
| 状态可见性 | Service 事件 | LBC 注解（`ready`、`elb-status`、`error`） |
| 删除安全性 | CCM 处理 | finalizer 确保 ELB 先于 CR 删除 |
| ELB 精细控制 | 有限 | 完整（标签、命名、参数） |
| 错误反馈 | Service 事件 | LBC 上的 `huawei-elb.io/error` 注解 |

**架构差异**：

```
EKS/GKE:    Service → CCM 创建 LB（从节点元数据读 VPC）

CCE + 本控制器：
            LBC → 控制器从节点探测 VPC/子网/可用区
                → 控制器调 API 创建 ELB
                → 将 elb.id 写回 LBC
                → Everest 复制 elb.id 到 Service
                → CCM 绑定 ELB 到 Service
```

用户体验是一样的：创建配置 → 获得负载均衡器 → 连接。内部流程多了一跳（控制器单独创建 ELB，再由 CCM 绑定），但换来了更好的可控性、状态报告和删除安全性。


## 开源许可证

本项目基于 Apache License 2.0 开源。详见 [LICENSE](LICENSE)。
