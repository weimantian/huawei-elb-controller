# huawei-elb-controller

[中文](README-zh.md) | **English**

---

## 概述

`huawei-elb-controller` 是一个 Kubernetes 控制器，为 [OpenEverest](https://openeverest.io/documentation/current/)（原 Percona Everest）数据库集群自动创建和管理**华为云 ELB**（弹性负载均衡）实例。

**解决的问题**：OpenEverest 的 `LoadBalancerConfig` CR 可以向 Kubernetes Service 注入 annotation，但不会创建华为云 ELB 本身。没有这个控制器，你每次都需要在华为云控制台手动创建 ELB、复制其 ID、再粘贴到 CR 中。

**架构**：

**Service Reconciler** watch `LoadBalancer` 类型的 Service，注入 `elb.autocreate`，处理参数更新

**两种使用模式**：

- **自动模式**（推荐）：不创建 LBC，在 OpenEverest UI 中选择 "No configuration" → ELB 全自动创建，对齐 EKS/GKE 体验
- **手动模式**：创建 LBC 作为参数模板 → 自定义 ELB 带宽、EIP 类型等参数。LBC 存的是**参数**而非 ELB ID，多个 Service 引用同一 LBC 各自独立 ELB，零端口冲突

---

## 功能特性

- **零配置自动模式** —— 不创建 LBC，直接用默认参数创建 ELB，与 EKS/GKE 体验完全一致
- **LBC 参数模板** —— 手动模式下 LBC 存储带宽/EIP/公网等配置参数，多 Service 独立 ELB
- **VPC 自动探测** —— 从集群节点自动探测 VPC、子网和可用区
- **ACL 自动处理** —— 自动将 Service 的 `loadBalancerSourceRanges` 转换为 ELB ACL 规则
- **kubectl 修改带宽** —— 修改 LBC annotation 后控制器自动调用华为云 API 更新 ELB 带宽
- **完整生命周期管理** —— ELB 创建通过 CCM autocreate 机制，删除随 Service 自动清理
- **多区域支持** —— 通过 `huawei-elb.io/region` 注解按集群覆盖区域

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
注入 elb.autocreate + elb.class + reclaim-policy
    ↓
CCM 创建 ELB → Service 获得 EXTERNAL-IP ✅
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
注入 elb.autocreate + elb.class + reclaim-policy
    ↓
CCM 创建独立 ELB → Service 获得 EXTERNAL-IP ✅

参数变更（如修改带宽）：
改 LBC annotation
    ↓
OpenEverest 同步到 Service
    ↓
Service Reconciler 调华为云 API 更新 ELB ✅
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
- **AK**（Access Key）和 **SK**（Secret Key）—— 在 IAM → 我的凭证 → 访问密钥 中创建
- **Project ID** —— 在控制台右上角用户名下拉菜单中找到

> ⚠️ **重要**：必须使用**主账号**的 AK/SK（非 IAM 子用户或临时凭证）。临时 AK/SK Token 调用 ELB/EIP/VPC API 时会鉴权失败。

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
INFO    Starting Controller               {"controller": "service"}
INFO    Starting workers                  {"controller": "service", "worker count": 1}
```

### 步骤 3：创建数据库集群（自动模式，推荐）

自动模式无需创建 LoadBalancerConfig。直接在 OpenEverest 中创建数据库集群：

1. 打开 OpenEverest UI（例如通过端口转发 `http://localhost:8080`）
2. 创建数据库集群时，**Load Balancer Config 下拉选择 "No configuration"**
3. 完成集群创建

OpenEverest 创建 LoadBalancer Service 后，Service Reconciler 自动：
1. 探测 VPC/子网/可用区
2. 使用默认参数构造 `elb.autocreate` 注解
3. 注入 `elb.class` 和 `reclaim-policy`
4. CCM 读取 autocreate 后自动创建 ELB 并绑定

> **默认参数**：公网 ELB、10Mbit/s 带宽、按流量计费、5_bgp EIP 类型。

### 步骤 4：等待 ELB 就绪

```bash
# 等待 Service 获得外部 IP（CCM 创建 ELB 约 15-30 秒）
kubectl get svc -n everest -w

# 查看 ELB 详情
kubectl describe svc <service-name> -n everest
```

ELB 就绪后 Service 的 `EXTERNAL-IP` 字段会显示 IP 地址。

### 步骤 5：验证连接

数据库运行后，验证 ELB 已正确绑定并获取连接 IP。

#### 1. 获取连接 IP

```bash
# 公网 ELB（默认）— 获取 EIP
kubectl get svc <service-name> -n everest -o jsonpath='{.status.loadBalancer.ingress[0].ip}'

# 内网 ELB — 获取 VIP
kubectl get svc <service-name> -n everest -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
```

#### 2. 连接数据库

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

然后在创建 DBC 时引用该 LBC（`loadBalancerConfigName: my-elb-config`）。Service Reconciler 读取 LBC 参数后注入 autocreate，CCM 创建独立 ELB。

> **重要**：LBC 是参数模板，不是实例引用。多个 DBC 引用同一个 LBC，各自获得独立 ELB —— 零端口冲突，完全对齐 EKS/GKE 行为。

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
| ELB 名称 | `cce-lb-<ns>-<svc>` | 自动生成，64 字符内截断 |
| VPC ID | 自动探测 | 从节点 ECS 元数据获取 |
| 子网 ID | 自动探测 | 从节点标签获取 |
| 可用区 | 自动探测 | 从节点 zone 标签获取 |

### 手动模式注解（LBC 参数模板）

LBC 的 `spec.annotations` 中可配置以下参数。OpenEverest 会自动同步到 Service，Service Reconciler 读取后构造 autocreate JSON 或调 API 更新：

| 注解 | 类型 | 可选值 | 默认值 | 说明 |
|---|---|---|---|---|
| `huawei-elb.io/public` | string | `"true"` / `"false"` | `"true"` | 公网/内网 ELB |
| `huawei-elb.io/bandwidth-size` | int | `1` – `2000` | `10` | EIP 带宽（Mbit/s） |
| `huawei-elb.io/bandwidth-charge-mode` | string | `"traffic"` / `"bandwidth"` | `"traffic"` | 按流量/按带宽计费 |
| `huawei-elb.io/eip-type` | string | `"5_bgp"` / `"5_sbgp"` / `"5_telcom"` / `"5_union"` | `"5_bgp"` | EIP 线路类型（创建后不可变） |
| `huawei-elb.io/name` | string | 自定义（≤64 字符） | `cce-lb-<ns>-<svc>` | ELB 实例名称 |
| `huawei-elb.io/region` | string | 如 `cn-north-4` | 全局配置 | 覆盖集群级区域设置 |

> **注意**：`eip-type` 创建后不可变更（华为云 API 限制），修改此参数需删除重建 ELB。这与 EKS/GKE 的 NLB/LB 类型不可切换一致。

### ACL 注解

控制器自动将 Service 的 `loadBalancerSourceRanges`（OpenEverest 的 `ipSourceRanges`）转换为华为云 ELB ACL：

| 注解 | 说明 |
|---|---|
| `kubernetes.io/elb.acl-status` | `"on"` — 开启 ACL 白名单 |
| `kubernetes.io/elb.acl-type` | `"white"` — 白名单模式 |
| `kubernetes.io/elb.acl-id` | ACL ID（控制器自动创建/复用） |

### 自动探测注解

以下注解在 CCE 上会从集群节点自动检测。均为可选 — 如未设置，控制器会自动填充。如需覆盖，在 LBC 的 `metadata.annotations` 中手动设置。

| 注解 | 自动检测来源 |
|---|---|
| `huawei-elb.io/vpc-id` | 通过节点 `machineID` 查询 ECS 服务器元数据 |
| `huawei-elb.io/subnet-id` | Neutron 子网 ID（通过 VPC API 从 Virsubnet ID 转换） |
| `huawei-elb.io/availability-zones` | 节点 label `topology.kubernetes.io/zone` |

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
- **镜像未找到** → 确保镜像已导入集群
- **Secret 缺失** → 检查 `everest-system` 命名空间中是否存在 `huawei-cloud-credentials` Secret
- **RBAC 权限不足** → 检查 ClusterRole 和 ClusterRoleBinding

### ELB 创建失败

```bash
# 查看控制器日志
kubectl logs -n everest-system deployment/huawei-elb-controller

# 检查 Service 事件
kubectl describe svc <service-name> -n everest
```

常见错误：
- `auto-detection failed: ...` → 检查所有节点是否在同一 VPC 内；查看控制器日志了解详情
- `CCM not processing autocreate` → 检查 CCM 是否运行：`kubectl get pods -A | grep cloud-controller`
- `creating ELB: ...` → 查看控制器日志了解华为云 API 错误详情

### Service 没有外部 IP

```bash
# 1. 检查 Service 是否有 autocreate 注解
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.autocreate}'

# 2. 检查 CCM 是否运行
kubectl get pods -A | grep cloud-controller

# 3. 查看 Service 事件
kubectl describe svc <service-name> -n everest
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

在 Amazon EKS 和 Google GKE 上，创建 `type: LoadBalancer` 的 Service 会自动创建云负载均衡器 —— 不需要部署额外控制器，不需要手动配置 VPC/子网。云平台的 CCM 直接从节点元数据读取 VPC/子网信息。

华为云 CCE 的 CCM 缺少这种自动探测能力 —— 用户必须手动查询并填写 VPC/子网/可用区参数。本控制器补上了这一层：

| 特性 | EKS / GKE | CCE + 本控制器 |
|---|---|---|
| 额外部署控制器 | 不需要 | 需要 |
| 用户填 VPC/子网/可用区 | 不用 | **不用（自动探测）** |
| 配置复杂度 | 零 | **零** |
| LBC 角色 | 参数模板 | **参数模板** ✅ |
| 多 Service 引用同一 LBC | 各自独立 LB | **各自独立 ELB** ✅ |
| ELB 参数事后可调 | ✅（CCM 调 API） | ✅（Service Reconciler 调 API） |
| ELB 生命周期管理 | CCM | CCM（autocreate + reclaim-policy） |
| 错误反馈 | Service 事件 | 控制器日志 + Service 事件 |

**架构差异**：

```
EKS/GKE:    Service → CCM 创建 LB（从节点元数据读 VPC）

CCE + 本控制器：
            Service → Service Reconciler 探测 VPC/子网/AZ
                   → 注入 elb.autocreate
                   → CCM 创建 ELB ✅
```

用户体验是一样的：创建集群 → 获得负载均衡器 → 连接。内部流程多了一步（控制器探测 VPC 并注入 autocreate），这是为了填补 CCE CCM 的能力缺口（不会自动探测 VPC、不认自定义 LBC 参数），但换来的是与 EKS/GKE 完全一致的使用体验。

---

## 开源许可证

本项目基于 Apache License 2.0 开源。详见 [LICENSE](LICENSE)。
