# huawei-elb-controller

[English](README.md) | **中文**

---

## 目录

- [概述](#概述)
- [工作原理](#工作原理)
- [功能特性](#功能特性)
- [快速开始](#快速开始)
- [配置参考](#配置参考)
- [故障排查](#故障排查)
- [开发指南](#开发指南)

---

## 概述

`huawei-elb-controller` 是一个 Kubernetes 控制器，用于为 [OpenEverest V1](https://github.com/openeverest/openeverest)（Percona Everest）管理**华为云 ELB**（弹性负载均衡）实例。

它解决了以下问题：OpenEverest V1 的 `LoadBalancerConfig` CR 可以向 K8s Service 注入注解，但**本身不会创建华为云 ELB**。这个控制器填补了这一缺口——监听 `LoadBalancerConfig` CR，自动创建/删除华为云 ELB，并将 ELB ID 写回 CR，使 V1 Operator 创建的 Service 能够绑定预创建的 ELB。

---

## 工作原理

### 背景

OpenEverest V1 是 Percona 提供的数据库集群管理平台。当用户创建 `DatabaseCluster` CR 时，V1 Operator 会为数据库集群创建一个 Kubernetes `LoadBalancer` 类型的 Service，用于外部访问。

V1 的 `LoadBalancerConfig` CR 机制允许用户自定义 Service 的注解：

```
DatabaseCluster.spec.proxy.expose.loadBalancerConfigName → 指向一个 LoadBalancerConfig CR
LoadBalancerConfig.spec.annotations → V1 Operator 将这些注解复制到 K8s Service
```

在华为云 CCE（云容器引擎）环境中，Cloud Controller Manager（CCM）通过 `kubernetes.io/elb.id` 注解绑定预创建的 ELB。但 V1 不会创建 ELB——它只负责传递注解。

### 问题

```
用户创建 LoadBalancerConfig (spec.annotations 为空)
    ↓
V1 Operator 创建 Service (没有 elb.id 注解)
    ↓
CCM 找不到 ELB → Service 永远拿不到外部 IP ❌
```

### 解决方案

`huawei-elb-controller` 自动完成 ELB 的创建和写回：

```
用户创建 LoadBalancerConfig (带标签 + ELB 参数注解)
    ↓
huawei-elb-controller 监听到 CR，调用华为云 ELB v3 API 创建 ELB
    ↓
控制器将 ELB ID 写回 LoadBalancerConfig.spec.annotations["kubernetes.io/elb.id"]
    ↓
用户创建 DatabaseCluster，引用该 LoadBalancerConfig
    ↓
V1 Operator 创建 Service，复制 spec.annotations (包含 elb.id)
    ↓
CCM 读取 elb.id，绑定预创建的 ELB → Service 获得外部 IP ✅
```

### 端到端数据流

```
┌─────────────────────────────────────────────────────────────────────┐
│                        用户操作                                       │
└──────────────┬──────────────────────────────────┬───────────────────┘
               │                                  │
    ① 创建 LoadBalancerConfig           ④ 创建 DatabaseCluster
    (带标签 + ELB 参数)                  (引用 LoadBalancerConfig)
               │                                  │
               ▼                                  ▼
┌──────────────────────┐              ┌──────────────────────────┐
│ huawei-elb-controller │              │   V1 Operator             │
│                      │              │                          │
│ ② 监听 CR            │              │ ⑤ 创建 K8s LoadBalancer    │
│   调用 ELB v3 API    │              │   Service                │
│   创建华为云 ELB      │              │   复制 spec.annotations   │
│                      │              │   (包含 elb.id)           │
│ ③ 写回 elb.id 到     │              │                          │
│   spec.annotations   │              │ ⑥ CCM 读取 elb.id        │
│   设置 ready=true    │              │   绑定预创建的 ELB        │
└──────────────────────┘              │   Service 获得 EXTERNAL-IP│
                                      └──────────────────────────┘
```

### 时序保护

为了确保 V1 Operator 在读取 `spec.annotations` 时 ELB ID 已经写回，控制器提供了 `huawei-elb.io/ready` 注解：

- ELB 创建中：`ready=false`
- ELB ACTIVE 且 ONLINE：`ready=true`
- ELB 删除中：`ready=false`

用户应在创建 `DatabaseCluster` 之前等待 `ready=true`：

```bash
kubectl wait loadbalancerconfig <name> \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s
```

---

## 功能特性

### 核心功能

| 功能 | 说明 |
|---|---|
| **ELB 创建** | 根据 `LoadBalancerConfig` 的注解参数，调用华为云 ELB v3 API 创建 ELB |
| **ELB 删除** | 删除 CR 时，通过 finalizer 机制自动删除对应的华为云 ELB，避免残留费用 |
| **状态调和** | 持续轮询 ELB 状态，更新 `metadata.annotations` 中的状态信息 |
| **幂等性** | 控制器重启后，通过 ELB 名称查找已有实例，不会重复创建 |
| **时序保护** | `huawei-elb.io/ready` 注解标记 ELB 是否就绪，用户可等待后再创建数据库集群 |

### 生产级特性

| 特性 | 说明 |
|---|---|
| **标签过滤** | 只处理带 `huawei-elb.io/controlled=true` 标签的 CR，不影响其他 LoadBalancerConfig |
| **错误分类** | 永久错误（参数缺失）5 分钟重试，临时错误（网络/限流）10 秒重试 |
| **错误记录** | `huawei-elb.io/error` 注解记录最近的调和错误，方便排查 |
| **更新冲突处理** | 使用 `retry.RetryOnConflict` 处理与 V1 Operator 的并发更新冲突 |
| **多区域支持** | 支持通过 `huawei-elb.io/region` 注解为每个 CR 指定不同的华为云区域 |
| **健康检查** | 内置 `/readyz` 和 `/healthz` 端点，支持 Kubernetes readiness/liveness probe |
| **凭证安全** | AK/SK 通过 Kubernetes Secret 注入，不硬编码在镜像中 |
| **Helm Chart** | 提供完整的 Helm Chart，支持参数化部署 |

### 支持的 ELB 类型

| 类型 | `huawei-elb.io/public` | 说明 |
|---|---|---|
| **内部 ELB** | `false`（默认） | 只有 VPC 内网 IP，适合 VPC 内部访问 |
| **公网 ELB** | `true` | 带有浮动 IP（EIP），可从公网访问 |

---

## 快速开始

### 前置条件

在开始之前，请确保您具备以下条件：

1. **华为云账号**：已开通 ELB 服务，拥有 AK（Access Key）和 SK（Secret Key）
2. **Kubernetes 集群**：已安装 OpenEverest V1 和华为云 CCM
3. **kubectl**：已配置好集群访问凭证
4. **Helm 3**（可选）：如果使用 Helm Chart 部署
5. **Go 1.26+**（可选）：如果从源码编译

### 步骤 1：获取华为云凭证

1. 登录 [华为云控制台](https://console.huaweicloud.com/)
2. 进入「统一身份认证服务」→「我的凭证」→「访问密钥」
3. 创建访问密钥，记录 AK 和 SK
4. 获取 Project ID：在控制台右上角用户名下拉菜单中查看

### 步骤 2：获取 VPC 和子网信息

控制器需要 VPC ID 和 Neutron 子网 ID 来创建 ELB。

**方法 A：通过控制台获取**

1. 进入「虚拟私有云」服务
2. 找到您的集群所在的 VPC，记录 VPC ID（如 `0d60646b-xxxx-xxxx-xxxx-xxxxxxxxxxxx`）
3. 点击子网，记录子网的 **Neutron 网络 ID**（不是子网资源 ID）

**方法 B：通过命令行工具获取**

如果您已部署控制器源码，可以使用 `list-vpcs` 工具：

```bash
# 设置凭证
export HUAWEI_CLOUD_AK=<您的AK>
export HUAWEI_CLOUD_SK=<您的SK>
export HUAWEI_CLOUD_PROJECT_ID=<您的ProjectID>
export HUAWEI_CLOUD_REGION=cn-north-4

# 列出所有 VPC 和子网
go run ./cmd/list-vpcs/
```

输出示例：
```
VPC: vpc-a489 (0d60646b-e3b7-4ad9-b422-015ee7da9a48) CIDR: 192.168.0.0/16
  Subnet: subnet-a489
    Resource ID:  566342ef-db1b-4ffa-a5ec-4185f5d61d40  ← 不是这个
    Neutron ID:   c265b187-a0a8-45cf-9cb3-7c3b757f8ff8  ← 用这个！
    CIDR:         192.168.0.0/24
```

> **⚠️ 注意**：`huawei-elb.io/subnet-id` 需要的是 **Neutron 子网 ID**（SubnetCidr.Id），不是 VPC 子网资源 ID（Virsubnet.Id）。使用错误的 ID 会导致 ELB 创建失败。

### 步骤 3：部署控制器

#### 方式 A：使用 Helm Chart（推荐）

```bash
# 添加 chart（如果已发布到 registry）
# helm repo add huawei-elb https://your-org.github.io/charts

# 使用 values.yaml 部署
cat > my-values.yaml << 'EOF'
image:
  repository: huawei-elb-controller
  tag: latest
  pullPolicy: IfNotPresent

credentials:
  ak: "<您的AK>"
  sk: "<您的SK>"
  projectId: "<您的ProjectID>"
  region: "cn-north-4"

namespace: everest-system
EOF

helm install huawei-elb-controller \
  ./charts/huawei-elb-controller \
  -f my-values.yaml
```

#### 方式 B：手动部署

1. 创建凭证 Secret：

```bash
kubectl create secret generic huawei-cloud-credentials \
  --namespace everest-system \
  --from-literal=ak=<您的AK> \
  --from-literal=sk=<您的SK> \
  --from-literal=project-id=<您的ProjectID> \
  --from-literal=region=cn-north-4
```

2. 构建镜像并导入集群：

```bash
# 编译 linux/amd64 二进制
GOOS=linux GOARCH=amd64 go build -o huawei-elb-controller ./cmd/

# 构建 Docker 镜像
docker buildx build --platform linux/amd64 -t huawei-elb-controller:latest .

# 导入到集群（CCE 集群可直接推送 SWR，自建集群可用 docker save + ctr import）
```

3. 部署 RBAC 和 Deployment：

```bash
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml
kubectl apply -f deploy/deployment.yaml
```

### 步骤 4：验证控制器运行

```bash
# 检查 Pod 状态
kubectl get pods -n everest-system -l app=huawei-elb-controller

# 查看控制器日志
kubectl logs -n everest-system deployment/huawei-elb-controller
```

预期输出：
```
NAME                                     READY   STATUS    RESTARTS   AGE
huawei-elb-controller-xxxxxxxxxx-xxxxx   1/1     Running   0          1m
```

日志中应出现：
```
INFO    starting huawei-elb-controller    {"region": "cn-north-4", "metrics": ":8081"}
INFO    Starting Controller               {"controller": "loadbalancerconfig", ...}
INFO    Starting workers                  {"controller": "loadbalancerconfig", ..., "worker count": 1}
```

### 步骤 5：创建 LoadBalancerConfig

创建一个内部 ELB（VPC 内访问）：

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: huawei-internal-elb
  labels:
    huawei-elb.io/controlled: "true"    # 必须有此标签，控制器才会处理
  annotations:
    huawei-elb.io/vpc-id: "0d60646b-e3b7-4ad9-b422-015ee7da9a48"
    huawei-elb.io/subnet-id: "c265b187-a0a8-45cf-9cb3-7c3b757f8ff8"
    huawei-elb.io/availability-zones: "cn-north-4a,cn-north-4b"
    huawei-elb.io/public: "false"
spec:
  annotations: {}   # 控制器会自动填充 kubernetes.io/elb.id
EOF
```

创建一个公网 ELB（带浮动 IP，可从公网访问）：

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: huawei-public-elb
  labels:
    huawei-elb.io/controlled: "true"
  annotations:
    huawei-elb.io/vpc-id: "0d60646b-e3b7-4ad9-b422-015ee7da9a48"
    huawei-elb.io/subnet-id: "c265b187-a0a8-45cf-9cb3-7c3b757f8ff8"
    huawei-elb.io/availability-zones: "cn-north-4a,cn-north-4b"
    huawei-elb.io/public: "true"
    huawei-elb.io/bandwidth-size: "20"               # 带宽 20Mbit/s
    huawei-elb.io/bandwidth-charge-mode: "traffic"    # 按流量计费
    huawei-elb.io/public-ip-network-type: "5_bgp"     # BGP 公网 IP
spec:
  annotations: {}
EOF
```

### 步骤 6：等待 ELB 就绪

```bash
# 等待 ready 注解变为 true（最多 120 秒）
kubectl wait loadbalancerconfig huawei-internal-elb \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# 查看状态
kubectl get loadbalancerconfig huawei-internal-elb -o jsonpath='{.spec.annotations}'
# 预期输出: {"kubernetes.io/elb.id":"xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"}

kubectl get loadbalancerconfig huawei-internal-elb -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-status}'
# 预期输出: ACTIVE
```

### 步骤 7：创建 DatabaseCluster

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: DatabaseCluster
metadata:
  name: my-database
  namespace: everest
spec:
  engine:
    type: postgresql
    replicas: 1
    storage:
      size: 10Gi
      class: csi-disk
  proxy:
    replicas: 1
    storage:
      size: 1Gi
    expose:
      type: LoadBalancer
      loadBalancerConfigName: huawei-internal-elb   # 引用步骤 5 创建的 LBC
EOF
```

### 步骤 8：验证 Service 绑定 ELB

```bash
# 查看 V1 创建的 Service
kubectl get svc -n everest -l app.kubernetes.io/instance=my-database

# 检查 Service 注解是否包含 elb.id
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.id}'
# 预期输出: 与 LoadBalancerConfig 中的 elb.id 相同

# 检查 Service 是否获得外部 IP
kubectl get svc <service-name> -n everest
# 预期 EXTERNAL-IP 列显示 ELB 的 VIP 地址
```

---

## 配置参考

### LoadBalancerConfig 注解

#### 必填注解（`metadata.annotations`）

| 注解 | 说明 | 示例 |
|---|---|---|
| `huawei-elb.io/vpc-id` | ELB 所在的 VPC ID | `0d60646b-e3b7-4ad9-b422-015ee7da9a48` |
| `huawei-elb.io/subnet-id` | Neutron 子网 ID（不是 VPC 子网资源 ID） | `c265b187-a0a8-45cf-9cb3-7c3b757f8ff8` |
| `huawei-elb.io/availability-zones` | 可用区列表（逗号分隔） | `cn-north-4a,cn-north-4b` |

#### 可选注解（`metadata.annotations`）

| 注解 | 默认值 | 说明 |
|---|---|---|
| `huawei-elb.io/public` | `false` | `true` 创建公网 ELB（带 EIP），`false` 创建内部 ELB |
| `huawei-elb.io/bandwidth-size` | `10` | EIP 带宽大小（Mbit/s），仅公网 ELB 有效 |
| `huawei-elb.io/bandwidth-charge-mode` | `traffic` | 计费模式：`traffic`（按流量）或 `bandwidth`（按带宽） |
| `huawei-elb.io/public-ip-network-type` | `5_bgp` | EIP 网络类型，`5_bgp` 为 BGP 公网 IP |
| `huawei-elb.io/region` | 全局 REGION | 为单个 CR 指定不同的华为云区域 |

#### 控制器自动写入的注解

| 位置 | 注解 | 说明 |
|---|---|---|
| `spec.annotations` | `kubernetes.io/elb.id` | 华为云 ELB ID，V1 Operator 复制到 Service，CCM 用于绑定 ELB |
| `metadata.annotations` | `huawei-elb.io/ready` | `true` 表示 ELB 已就绪，`false` 表示创建中或异常 |
| `metadata.annotations` | `huawei-elb.io/elb-status` | ELB 的 provisioning 状态（`ACTIVE`、`PENDING_CREATE` 等） |
| `metadata.annotations` | `huawei-elb.io/public-ip` | 公网 ELB 的 EIP 地址（内部 ELB 为空） |
| `metadata.annotations` | `huawei-elb.io/error` | 最近的调和错误信息（正常时为空） |

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
| `existingSecret` | `""` | 使用已有的 Secret（优先于 credentials） |
| `namespace` | `everest-system` | 部署命名空间 |
| `resources.requests.cpu` | `100m` | CPU 请求 |
| `resources.requests.memory` | `128Mi` | 内存请求 |
| `resources.limits.cpu` | `500m` | CPU 限制 |
| `resources.limits.memory` | `256Mi` | 内存限制 |
| `healthProbe.readinessProbe.initialDelaySeconds` | `5` | 就绪探针初始延迟 |
| `healthProbe.livenessProbe.initialDelaySeconds` | `15` | 存活探针初始延迟 |

---

## 故障排查

### 控制器 Pod 无法启动

```bash
# 查看 Pod 事件
kubectl describe pod -n everest-system -l app=huawei-elb-controller

# 常见原因：
# 1. 镜像不存在 → 检查镜像是否已导入集群
# 2. Secret 不存在 → 检查 huawei-cloud-credentials Secret
# 3. RBAC 权限不足 → 检查 ClusterRole 和 ClusterRoleBinding
```

### ELB 创建失败

```bash
# 查看错误注解
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/error}'

# 常见错误：
# "missing required annotations" → 检查 vpc-id、subnet-id、availability-zones 是否填写
# "creating ELB: ..." → 查看控制器日志获取华为云 API 错误详情
```

### subnet-id 错误

> **最常见的错误**：使用了 VPC 子网资源 ID 而非 Neutron 子网 ID。

```
错误信息: "creating ELB: ... vip_subnet_cidr_id ... not found"
原因: subnet-id 填了 Virsubnet.Id 而非 SubnetCidr.Id (Neutron ID)
解决: 运行 go run ./cmd/list-vpcs/ 获取正确的 Neutron 子网 ID
```

### Service 没有获得外部 IP

```bash
# 1. 检查 LoadBalancerConfig 是否 ready
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'
# 应为 "true"

# 2. 检查 Service 注解
kubectl get svc <service-name> -o jsonpath='{.metadata.annotations}'
# 应包含 kubernetes.io/elb.id

# 3. 如果注解正确但没有 IP，检查 CCM 是否运行
kubectl get pods -A | grep cloud-controller
```

### 删除 LoadBalancerConfig 卡住

```bash
# 检查 finalizer 是否存在
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.finalizers}'
# 应包含 "huawei-elb.io/finalizer"

# 查看控制器日志，确认删除操作是否执行
kubectl logs -n everest-system deployment/huawei-elb-controller --tail=10

# 如果 ELB 已被手动删除，控制器会跳过并移除 finalizer
```

---

## 开发指南

### 从源码构建

```bash
# 依赖
go mod tidy

# 编译
go build ./...

# 代码检查
go vet ./...

# 本地运行（需要 kubeconfig 和凭证）
export HUAWEI_CLOUD_AK=...
export HUAWEI_CLOUD_SK=...
export HUAWEI_CLOUD_PROJECT_ID=...
export HUAWEI_CLOUD_REGION=cn-north-4
go run ./cmd/
```

### 项目结构

```
huawei-elb-controller/
├── cmd/
│   ├── main.go              # 控制器入口
│   └── list-vpcs/           # VPC/子网查询工具
├── internal/
│   ├── controller/
│   │   └── loadbalancerconfig_controller.go  # 核心调和逻辑
│   └── huaweicloud/
│       ├── client.go         # 华为云客户端构建
│       └── elb.go            # ELB CRUD 操作
├── deploy/                   # Kubernetes 部署清单
│   ├── serviceaccount.yaml
│   ├── clusterrole.yaml
│   ├── clusterrolebinding.yaml
│   └── deployment.yaml
├── charts/                   # Helm Chart
│   └── huawei-elb-controller/
├── examples/                 # 示例 YAML
│   ├── internal-elb.yaml
│   └── public-elb.yaml
├── Dockerfile
├── Makefile
└── go.mod
```

### 调和循环说明

控制器的调和循环遵循以下逻辑：

1. **获取 CR**：从集群获取 LoadBalancerConfig
2. **标签检查**：跳过没有 `huawei-elb.io/controlled=true` 标签的 CR
3. **删除处理**：如果有 deletion timestamp，删除 ELB 并移除 finalizer
4. **Finalizer 确保**：如果没有 finalizer，添加并重新排队
5. **ELB 创建**：如果 `spec.annotations` 中没有 `elb.id`，创建 ELB
6. **ELB 状态检查**：如果已有 `elb.id`，查询 ELB 状态，更新 `ready` 注解
7. **重新排队**：根据状态决定下次调和时间（30s/5min/10s/5min）
