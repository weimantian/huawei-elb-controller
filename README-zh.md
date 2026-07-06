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

### 步骤 2：部署控制器

#### 方式 A：使用 Helm（推荐）

```bash
# 1. 构建镜像
git clone https://github.com/weimantian/huawei-elb-controller.git
cd huawei-elb-controller
docker buildx build --platform linux/amd64 -t huawei-elb-controller:latest .

# 2. 登录 SWR 并推送镜像
#    在 SWR 控制台总览页面获取登录指令
docker login -u <你的命名空间> -p <登录令牌> <swr-registry>
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest

# 3. 创建包含华为云凭据的 values 文件
cat > my-values.yaml << 'EOF'
image:
  repository: <swr-registry>/huawei-elb-controller
  tag: latest
  pullPolicy: Always

credentials:
  ak: "<你的-AK>"
  sk: "<你的-SK>"
  projectId: "<你的-ProjectID>"
  region: "cn-north-4"

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
docker buildx build --platform linux/amd64 -t huawei-elb-controller:latest .

# 登录 SWR 并推送镜像
# 在 SWR 控制台总览页面获取登录指令
docker login -u <你的命名空间> -p <登录令牌> <swr-registry>
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest
```

然后修改 `deploy/deployment.yaml`，将容器镜像改为 `<swr-registry>/huawei-elb-controller:latest`。

3. 应用清单：

```bash
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml
kubectl apply -f deploy/deployment.yaml
```

### 步骤 3：验证控制器运行状态

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

### 步骤 4：创建 LoadBalancerConfig

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

公网 ELB 可选参数（仅 `public: "true"` 时生效）：

| 注解 | 默认值 | 说明 |
|---|---|---|
| `huawei-elb.io/bandwidth-size` | `10` | EIP 带宽（Mbit/s） |
| `huawei-elb.io/bandwidth-charge-mode` | `traffic` | `traffic`（按流量计费）或 `bandwidth`（按带宽计费） |
| `huawei-elb.io/public-ip-network-type` | `5_bgp` | EIP 网络类型 |

### 步骤 5：等待 ELB 就绪

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

### 步骤 6：创建数据库集群

创建一个使用步骤 4 中创建的 LoadBalancerConfig 的数据库集群。

#### 方式 A（推荐）：via OpenEverest UI

1. 在 OpenEverest UI 中进入 **Databases**。
2. 点击 **Create database**。
3. **Step 1 — Basic Information**：选择引擎（例如 PostgreSQL），填写名称（例如 `my-pg`），选择版本。
4. **Step 2 — Resources**：设置 CPU、内存、磁盘大小和节点数。
5. **Step 3 — Backups**：配置备份存储（或跳过）。
6. **Step 4 — Advanced Configurations**：
   - 设置 **Storage class**（例如 `csi-disk`）。
   - 启用 **External access**（LoadBalancer）。
   - 选择步骤 4 中创建的 **Load Balancer config**（例如 `huawei-elb`）。
7. **Step 5 — Monitoring**：配置监控（或跳过）。
8. 点击 **Create database**。

> **注意**：如果 LoadBalancer config 下拉菜单显示 “- No configuration -”，可能是 ELB 尚未就绪。请返回步骤 5 等待 `ready=true`。

#### 方式 B：via kubectl

创建一个 PostgreSQL 数据库集群：

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
    version: "17.9"
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
      loadBalancerConfigName: huawei-elb
EOF
```

> **支持的引擎类型**：`postgresql`、`pxc`（MySQL）、`psmdb`（MongoDB）。支持的代理类型：`pgbouncer`（PostgreSQL）、`haproxy`（MySQL）、`mongos`（MongoDB）。
>
> **可选**：在 `expose` 下添加 `ipSourceRanges` 限制仅受信任 IP 访问（CIDR 格式）：
> ```yaml
>     expose:
>       type: LoadBalancer
>       loadBalancerConfigName: huawei-elb
>       ipSourceRanges:
>         - "10.0.0.0/24"
> ```

### 步骤 7：验证数据库访问

#### 1. 检查数据库集群运行状态

```bash
# 列出所有数据库集群及其状态
kubectl get databasecluster -n everest
```

预期输出：
```
NAME        SIZE   READY   STATUS   HOSTNAME        AGE
my-pg       1      1       ready    <ELB-VIP>   5m
```

- `READY`：就绪副本数 / 总副本数（应与 `SIZE` 一致）
- `STATUS`：应为 `ready`
- `HOSTNAME`：ELB 的 VIP 地址（内网 IP）

#### 2. 查找 LoadBalancer Service

```bash
# 列出 OpenEverest 为数据库创建的 Service
# 将 <db-name> 替换为你的数据库名称（如 my-pg）
kubectl get svc -n everest -l app.kubernetes.io/instance=<db-name>
```

预期输出：
```
NAME                TYPE           CLUSTER-IP      EXTERNAL-IP      PORT(S)          AGE
my-pg-pgbouncer     LoadBalancer   <CLUSTER-IP>   <ELB-VIP>    5432:31234/TCP   5m
```

- `TYPE`：应为 `LoadBalancer`
- `EXTERNAL-IP`：ELB 的 VIP 地址（内网和公网 ELB 都显示内网 VIP）
- `PORT(S)`：数据库端口 —— PostgreSQL 5432、MySQL 3306、MongoDB 27017

#### 3. 获取连接 IP

**内网 ELB**（仅 VPC 内访问）：

步骤 2 中的 `EXTERNAL-IP` 就是连接地址：
```bash
# 从 Service 状态提取内网 VIP
kubectl get svc <service-name> -n everest -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
# 输出：<ELB-VIP>
```

**公网 ELB**（互联网访问）：

从 LoadBalancerConfig 获取公网 IP（EIP）：
```bash
# 读取控制器写入的公网 IP 注解
kubectl get loadbalancerconfig <lbc-name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/public-ip}'
# 输出：<EIP-address>
```

#### 4. 验证 ELB 已绑定到 Service

```bash
# 检查 Service 是否携带 ELB ID 注解
# 这是 ELB 的 UUID（不是 IP）—— CCM 用它来将预创建的 ELB 绑定到 Service
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.id}'
# 输出：<ELB-UUID>（ELB UUID，不是 IP）
```

该值应与 LoadBalancerConfig 中的 ELB ID 一致：
```bash
# 验证 LBC CR 中存储了相同的 ELB ID
kubectl get loadbalancerconfig <lbc-name> -o jsonpath='{.spec.annotations.kubernetes\.io/elb\.id}'
```

> **注意**：ELB ID 是 CCM 内部使用的 UUID。连接数据库请使用步骤 3 中的 IP，不是这个 UUID。

#### 5. 安装数据库客户端（如未安装）

**PostgreSQL (`psql`)**：

| 操作系统 | 命令 |
|---|---|
| macOS | `brew install postgresql` |
| Ubuntu/Debian | `sudo apt install postgresql-client` |
| CentOS/RHEL | `sudo yum install postgresql` |

**MySQL (`mysql`)**：

| 操作系统 | 命令 |
|---|---|
| macOS | `brew install mysql-client` |
| Ubuntu/Debian | `sudo apt install mysql-client` |
| CentOS/RHEL | `sudo yum install mysql` |

**MongoDB (`mongosh`)**：

| 操作系统 | 命令 |
|---|---|
| macOS | `brew install mongosh` |
| Ubuntu/Debian | 参考[官方安装指南](https://www.mongodb.com/docs/mongodb-shell/install/) |
| CentOS/RHEL | 参考[官方安装指南](https://www.mongodb.com/docs/mongodb-shell/install/) |

#### 6. 连接数据库

将 `<IP>` 替换为步骤 3 中的 IP，`<db-name>` 替换为你的数据库名称。

**PostgreSQL**（端口 5432）：

```bash
# 获取数据库密码
kubectl get secret everest-secrets-<db-name> -n everest -o jsonpath='{.data.postgres}' | base64 -d

# 通过 psql 连接
psql -h <IP> -U postgres -d <db-name>
# 公网 ELB 示例：  psql -h <EIP-address> -U postgres -d my-pg
# 内网 ELB 示例：  psql -h <ELB-VIP> -U postgres -d my-pg
```

**MySQL / PXC**（端口 3306）：

```bash
# 获取数据库密码
kubectl get secret everest-secrets-<db-name> -n everest -o jsonpath='{.data.root}' | base64 -d

# 通过 mysql 客户端连接
mysql -h <IP> -u root -p -e "SELECT VERSION();"
# 公网 ELB 示例：  mysql -h <EIP-address> -u root -p
# 内网 ELB 示例：  mysql -h <ELB-VIP> -u root -p
```

**MongoDB / PSMDB**（端口 27017）：

```bash
# 获取数据库密码
kubectl get secret everest-secrets-<db-name> -n everest -o jsonpath='{.data.clusterAdmin}' | base64 -d

# 通过 mongosh 连接
mongosh "mongodb://clusterAdmin:<password>@<IP>:27017/?replicaSet=rs0"
# 公网 ELB 示例：  mongosh "mongodb://clusterAdmin:<password>@<EIP-address>:27017/?replicaSet=rs0"
```

> **注意**：内网 ELB 的 VIP 只能在 VPC 内部访问。如果从本地电脑（VPC 外）测试，请使用公网 ELB，或者在 Pod 内部连接：
> ```bash
> kubectl exec -it <pod-name> -n everest -- psql -h <IP> -U postgres -d <db-name>
> ```

---

## 配置参考

### LoadBalancerConfig Annotation

#### 可选 Annotation

| Annotation | 默认值 | 说明 |
|---|---|---|
| `huawei-elb.io/public` | `true` | `false` = 内网 ELB；默认 `true` = 公网 ELB（带 EIP） |
| `huawei-elb.io/bandwidth-size` | `10` | EIP 带宽（Mbit/s）—— 仅公网 ELB |
| `huawei-elb.io/bandwidth-charge-mode` | `traffic` | `traffic`（按流量计费）或 `bandwidth`（按带宽计费） |
| `huawei-elb.io/public-ip-network-type` | `5_bgp` | EIP 网络类型；`5_bgp` 为 BGP |
| `huawei-elb.io/region` | 全局 REGION | 为特定 CR 覆盖华为云区域 |

#### 控制器写入的 Annotation

| 位置 | Annotation | 说明 |
|---|---|---|
| `spec.annotations` | `kubernetes.io/elb.id` | ELB ID —— OpenEverest 复制到 Service；CCM 用它绑定 ELB |
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
