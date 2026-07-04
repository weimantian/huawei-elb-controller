# huawei-elb-controller

[中文](README-zh.md) | **English**

---

## 概述

`huawei-elb-controller` 是一个 Kubernetes 控制器，为 [OpenEverest](https://openeverest.io/documentation/current/)（原 Percona Everest）数据库集群自动创建和管理**华为云 ELB**（弹性负载均衡）实例。

**解决的问题**：OpenEverest 的 `LoadBalancerConfig` CR 可以向 Kubernetes Service 注入 annotation，但不会创建华为云 ELB 本身。没有这个控制器，你每次都需要在华为云控制台手动创建 ELB、复制其 ID、再粘贴到 CR 中。

**工作原理**：监听 `LoadBalancerConfig` CR，调用华为云 ELB v3 API 自动创建/删除 ELB，并将 ELB ID 写回 CR。OpenEverest 的 operator 随后读取 ELB ID，将其添加到 Service，华为云 CCM 绑定 ELB —— 为你的数据库集群提供外部负载均衡访问入口。

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
Percona Everest operator 创建 LoadBalancer 类型 Service
    ↓
华为云 CCM 绑定 ELB → Service 获得外部 IP
    ↓
你通过 ELB 的 IP 连接数据库
```

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

> **注意**：原 "Percona Everest" 项目已改名为 **OpenEverest**。`everest.percona.com/v1alpha1` API group 保持不变。旧的 Percona Helm 仓库仍然可用，但推荐使用新的 OpenEverest 仓库。

如果尚未安装 OpenEverest：

**前提条件**：工作站需安装 Helm v3 和 [yq](https://github.com/mikefarah/yq)。不支持离线环境。

#### 方式 A：Helm（推荐）

```bash
# 添加 OpenEverest Helm 仓库
helm repo add openeverest https://openeverest.github.io/helm-charts/
helm repo update

# 安装 OpenEverest
helm install everest-core openeverest/openeverest \
    --namespace everest-system \
    --create-namespace
```

这会安装：
- Everest operator 和 server（`everest-system` 命名空间）
- 数据库引擎 operator（PostgreSQL、MongoDB、PXC）（`everest` 命名空间）

**可选参数**：

| 参数 | 用途 |
|---|---|
| `--set dbNamespace.enabled=false` | 不自动创建 `everest` 数据库命名空间 |
| `--set dbNamespace.namespaceOverride=<name>` | 使用自定义数据库命名空间名 |
| `--set dbNamespace.pxc=false` | 跳过 PXC operator 安装 |
| `--set dbNamespace.postgresql=false` | 跳过 PostgreSQL operator 安装 |
| `--set dbNamespace.psmdb=false` | 跳过 MongoDB operator 安装 |
| `--set server.tls.enabled=true` | 为 Everest 组件通信启用 TLS |

> ⚠️ 不要使用 `--no-hooks` —— 不支持无 hook 安装。

#### 方式 B：everestctl 命令行工具

```bash
# 下载 everestctl（macOS Apple Silicon）
curl -sSL -o everestctl-darwin-arm64 \
  https://github.com/openeverest/openeverest/releases/latest/download/everestctl-darwin-arm64
sudo install -m 555 everestctl-darwin-arm64 /usr/local/bin/everestctl
rm everestctl-darwin-arm64

# 交互式安装
everestctl install

# 或无头安装
everestctl install \
  --namespaces everest \
  --operator.postgresql=true \
  --operator.mysql=true \
  --operator.mongodb=true \
  --skip-wizard
```

#### 验证安装

```bash
# 检查 Everest pod 运行状态
kubectl get pods -n everest-system

# 检查数据库引擎 operator 已注册
kubectl get dbengine -n everest
# 预期：percona-postgresql-operator、percona-psmdb-operator、percona-pxc-operator

# 获取管理员密码
kubectl get secret everest-accounts -n everest-system \
  -o jsonpath='{.data.users\.yaml}' | base64 --decode | yq '.admin.passwordHash'
```

> 更多详情请参考 [OpenEverest 快速安装指南](https://docs.percona.com/everest/quick-install.html) 或 [OpenEverest 文档](https://openeverest.io/documentation/current/)。

### 3. 华为云账号

- 已开通 ELB 服务的华为云账号
- **AK**（Access Key）和 **SK**（Secret Key）—— 在 IAM → 我的凭证 → 访问密钥 中创建
- **Project ID** —— 在控制台右上角用户名下拉菜单中找到
- 已知你的 **VPC ID** 和 **Neutron 子网 ID**（见下方步骤 2）

---

## 快速开始

### 步骤 1：验证前提条件

```bash
# 检查 OpenEverest 运行状态
kubectl get pods -n everest-system
# 预期：everest-operator 和 everest-server pod 处于 Running 状态

# 检查数据库引擎 operator 已注册
kubectl get dbengine -n everest
# 预期：percona-postgresql-operator、percona-psmdb-operator、percona-pxc-operator

# 检查 CCM 运行状态（华为云）
kubectl get pods -A | grep cloud-controller
# 预期：cloud-controller-manager pod 处于 Running 状态
```

### 步骤 2：获取 VPC 和子网信息

控制器需要 **VPC ID** 和 **Neutron 子网 ID** 来创建 ELB。

> **用哪个子网？** 使用**节点子网** —— 即 Kubernetes 工作节点 IP 所在的子网。不要使用 CCE 管理节点子网或容器/Pod 子网。

**方式 A：通过华为云控制台**

1. 进入"虚拟私有云"服务
2. 找到集群所在的 VPC —— 记录 **VPC ID**
3. 点击节点 IP 所属的子网 —— 记录 **Neutron ID**（不是子网资源 ID）

**方式 B：通过 `list-vpcs` 命令行工具**

```bash
# 克隆仓库并运行 VPC 查询工具
git clone https://github.com/weimantian/huawei-elb-controller.git
cd huawei-elb-controller

export HUAWEI_CLOUD_AK=<你的-AK>
export HUAWEI_CLOUD_SK=<你的-SK>
export HUAWEI_CLOUD_PROJECT_ID=<你的-ProjectID>
export HUAWEI_CLOUD_REGION=cn-north-4

go run ./cmd/list-vpcs/
```

输出示例：
```
VPC: vpc-a489 (0d60646b-e3b7-4ad9-b422-015ee7da9a48) CIDR: 192.168.0.0/16
  Subnet: subnet-a489
    Resource ID:  566342ef-...  ← 不是这个
    Neutron ID:   c265b187-...  ← 用这个！
    CIDR:         192.168.0.0/24
```

> **重要**：`huawei-elb.io/subnet-id` 需要的是 **Neutron 子网 ID**，不是 VPC 子网资源 ID。用错 ID 会导致 ELB 创建失败。

### 步骤 3：部署控制器

#### 方式 A：使用 Helm（推荐）

```bash
git clone https://github.com/weimantian/huawei-elb-controller.git
cd huawei-elb-controller

cat > my-values.yaml << 'EOF'
image:
  repository: huawei-elb-controller
  tag: latest
  pullPolicy: IfNotPresent

credentials:
  ak: "<你的-AK>"
  sk: "<你的-SK>"
  projectId: "<你的-ProjectID>"
  region: "cn-north-4"

namespace: everest-system
EOF

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

2. 构建并导入容器镜像：

```bash
GOOS=linux GOARCH=amd64 go build -o huawei-elb-controller ./cmd/
docker buildx build --platform linux/amd64 -t huawei-elb-controller:latest .
# CCE 集群推送到 SWR；自建集群用 docker save + ctr import 导入
```

3. 应用清单：

```bash
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml
kubectl apply -f deploy/deployment.yaml
```

### 步骤 4：验证控制器运行状态

```bash
kubectl get pods -n everest-system -l app=huawei-elb-controller
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

### 步骤 5：创建 LoadBalancerConfig

创建**内部 ELB**（仅 VPC 内访问）：

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: huawei-internal-elb
spec:
  annotations:
    huawei-elb.io/vpc-id: "0d60646b-e3b7-4ad9-b422-015ee7da9a48"
    huawei-elb.io/subnet-id: "c265b187-a0a8-45cf-9cb3-7c3b757f8ff8"
    huawei-elb.io/availability-zones: "cn-north-4a,cn-north-4b"
    huawei-elb.io/public: "false"
EOF
```

或创建**公网 ELB**（带弹性公网 IP，可从互联网访问）：

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: huawei-public-elb
spec:
  annotations:
    huawei-elb.io/vpc-id: "0d60646b-e3b7-4ad9-b422-015ee7da9a48"
    huawei-elb.io/subnet-id: "c265b187-a0a8-45cf-9cb3-7c3b757f8ff8"
    huawei-elb.io/availability-zones: "cn-north-4a,cn-north-4b"
    huawei-elb.io/public: "true"
    huawei-elb.io/bandwidth-size: "20"
    huawei-elb.io/bandwidth-charge-mode: "traffic"
    huawei-elb.io/public-ip-network-type: "5_bgp"
EOF
```

### 步骤 6：等待 ELB 就绪

```bash
# 等待 ELB 创建完成并激活（最多 120 秒）
kubectl wait loadbalancerconfig huawei-internal-elb \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# 验证 ELB 状态
kubectl get loadbalancerconfig huawei-internal-elb -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-status}'
# 预期：ACTIVE

# 验证 ELB ID 已写入
kubectl get loadbalancerconfig huawei-internal-elb -o jsonpath='{.spec.annotations}'
# 预期：{"kubernetes.io/elb.id":"xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"}
```

> **重要**：在创建 DatabaseCluster 之前，务必等待 `ready=true`。这确保 ELB ID 已写入 LoadBalancerConfig，Percona Everest operator 读取时能获取到。

### 步骤 7：创建数据库集群

创建一个使用该 LoadBalancerConfig 的 PostgreSQL 数据库集群：

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: DatabaseCluster
metadata:
  name: my-pg
  namespace: everest
spec:
  engine:
    type: postgresql
    version: "17.4"
    replicas: 1
    resources:
      cpu: "1"
      memory: 2G
    storage:
      size: 10Gi
      class: csi-disk
  proxy:
    type: pgbouncer
    replicas: 1
    resources:
      cpu: "1"
      memory: 30M
    storage:
      size: 1Gi
    expose:
      type: LoadBalancer
      loadBalancerConfigName: huawei-internal-elb
EOF
```

> **支持的引擎类型**：`postgresql`、`pxc`（MySQL）、`psmdb`（MongoDB）。支持的代理类型：`pgbouncer`（PostgreSQL）、`haproxy`（MySQL）、`mongos`（MongoDB）。
>
> **可选**：在 `expose` 下添加 `ipSourceRanges` 限制仅受信任 IP 访问（CIDR 格式）：
> ```yaml
>     expose:
>       type: LoadBalancer
>       loadBalancerConfigName: huawei-internal-elb
>       ipSourceRanges:
>         - "10.0.0.0/24"
> ```

### 步骤 8：验证数据库访问

```bash
# 1. 检查数据库集群运行状态
kubectl get databasecluster -n everest
# 预期：my-pg 处于 ready 状态

# 2. 查找 Percona Everest 创建的 Service
kubectl get svc -n everest -l app.kubernetes.io/instance=my-pg
# 预期：一个 LoadBalancer 类型的 Service，带有 EXTERNAL-IP

# 3. 验证 Service 包含 ELB ID annotation
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.id}'
# 预期：与 LoadBalancerConfig 中相同的 ELB ID

# 4. 通过 ELB IP 连接数据库
# 内部 ELB：
psql -h <ELB-VIP> -U postgres -d mydb

# 公网 ELB：
psql -h <EIP-地址> -U postgres -d mydb
```

输出示例：
```
NAME                TYPE           CLUSTER-IP      EXTERNAL-IP      PORT(S)          AGE
my-pg-pgbouncer     LoadBalancer   10.96.145.200   192.168.0.235    5432:31234/TCP   5m
```

`EXTERNAL-IP` 就是 ELB 的 VIP 地址 —— 客户端通过这个地址连接数据库。

---

## 配置参考

### LoadBalancerConfig Annotation

#### 必需 Annotation

| Annotation | 说明 | 示例 |
|---|---|---|
| `huawei-elb.io/vpc-id` | 创建 ELB 所在的 VPC ID | `0d60646b-...` |
| `huawei-elb.io/subnet-id` | Neutron 子网 ID（不是 VPC 子网资源 ID） | `c265b187-...` |
| `huawei-elb.io/availability-zones` | 可用区列表（逗号分隔） | `cn-north-4a,cn-north-4b` |

#### 可选 Annotation

| Annotation | 默认值 | 说明 |
|---|---|---|
| `huawei-elb.io/public` | `false` | `true` = 公网 ELB（带 EIP）；`false` = 内部 ELB |
| `huawei-elb.io/bandwidth-size` | `10` | EIP 带宽（Mbit/s）—— 仅公网 ELB |
| `huawei-elb.io/bandwidth-charge-mode` | `traffic` | `traffic`（按流量计费）或 `bandwidth`（按带宽计费） |
| `huawei-elb.io/public-ip-network-type` | `5_bgp` | EIP 网络类型；`5_bgp` 为 BGP |
| `huawei-elb.io/region` | 全局 REGION | 为特定 CR 覆盖华为云区域 |

#### 控制器写入的 Annotation

| 位置 | Annotation | 说明 |
|---|---|---|
| `spec.annotations` | `kubernetes.io/elb.id` | ELB ID —— Percona Everest 复制到 Service；CCM 用它绑定 ELB |
| `metadata.annotations` | `huawei-elb.io/ready` | `true` 表示 ELB 就绪；`false` 表示创建中或出错 |
| `metadata.annotations` | `huawei-elb.io/elb-status` | ELB 状态：`ACTIVE`、`PENDING_CREATE` 等 |
| `metadata.annotations` | `huawei-elb.io/public-ip` | EIP 地址（仅公网 ELB） |
| `metadata.annotations` | `huawei-elb.io/error` | 最近一次错误信息（正常时为空） |

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
| `existingSecret` | `""` | 使用已有 Secret（覆盖 credentials） |
| `namespace` | `everest-system` | 部署命名空间 |
| `resources.requests.cpu` | `100m` | CPU 请求 |
| `resources.requests.memory` | `128Mi` | 内存请求 |
| `resources.limits.cpu` | `500m` | CPU 限制 |
| `resources.limits.memory` | `256Mi` | 内存限制 |

---

## 故障排查

### 控制器 Pod 无法启动

```bash
kubectl describe pod -n everest-system -l app=huawei-elb-controller
```

常见原因：
- **镜像未找到** → 确保镜像已导入集群
- **Secret 缺失** → 检查 `everest-system` 命名空间中是否存在 `huawei-cloud-credentials` Secret
- **RBAC 权限不足** → 检查 ClusterRole 和 ClusterRoleBinding

### ELB 创建失败

```bash
# 查看错误 annotation
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/error}'

# 查看控制器日志获取 API 错误详情
kubectl logs -n everest-system deployment/huawei-elb-controller
```

常见错误：
- `missing required annotations` → 检查 `vpc-id`、`subnet-id`、`availability-zones`
- `vip_subnet_cidr_id not found` → 用了 VPC 子网资源 ID 而非 Neutron ID
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

---

## 开发

构建说明、架构详情和贡献指南请参见 [DEVELOPMENT.md](DEVELOPMENT.md)。
