# 开发指南

本文档面向希望构建、修改或为 `huawei-elb-controller` 贡献代码的开发者。

面向用户的安装和使用说明见 [README.md](README.md)。

> **注意**：OpenEverest（原 Percona Everest）是本控制器集成的数据库平台。`everest.percona.com/v1alpha1` API group 保持不变。源代码：[openeverest/everest-operator](https://github.com/openeverest/everest-operator)。

---

## 目录

- [从源码构建](#从源码构建)
- [项目结构](#项目结构)
- [架构](#架构)
- [协调循环](#协调循环)
- [CRD 参考](#crd-参考)
- [端到端数据流](#端到端数据流)
- [时序保护](#时序保护)
- [错误处理策略](#错误处理策略)
- [多区域支持](#多区域支持)
- [测试](#测试)
- [贡献](#贡献)

---

## 从源码构建

### 前提条件

- **Go 1.26+**
- **Docker**（用于容器构建）
- **kubectl** + Kubernetes 集群（用于部署）
- **Helm 3**（用于 Chart 部署）

### 构建

```bash
# 下载依赖
go mod tidy

# 构建所有包（类型检查 + 编译）
go build ./...

# 代码检查
go vet ./...

# 构建当前平台的二进制文件
go build -o huawei-elb-controller ./cmd/

# 交叉编译 Linux/amd64（用于容器镜像）
GOOS=linux GOARCH=amd64 go build -o huawei-elb-controller ./cmd/
```

### 本地运行

控制器可以在集群外运行（需要 kubeconfig 和华为云凭证）：

```bash
export HUAWEI_CLOUD_AK=<your-AK>
export HUAWEI_CLOUD_SK=<your-SK>
export HUAWEI_CLOUD_PROJECT_ID=<your-ProjectID>
export HUAWEI_CLOUD_REGION=cn-north-4

go run ./cmd/
```

### 构建容器镜像

```bash
# 构建 linux/amd64
docker buildx build --platform linux/amd64 -t huawei-elb-controller:latest .

# CCE 集群：推送到 SWR（华为云容器镜像仓库）
docker tag huawei-elb-controller:latest <swr-endpoint>/<namespace>/huawei-elb-controller:latest
docker push <swr-endpoint>/<namespace>/huawei-elb-controller:latest

# 自建集群：导出后通过 containerd 导入
docker save huawei-elb-controller:latest | gzip > huawei-elb-controller.tar.gz
# 在节点上执行：ctr -n k8s.io images import huawei-elb-controller.tar.gz
```

### VPC/子网查询工具

用于查找正确的 VPC 和 Neutron 子网 ID 的实用工具：

```bash
export HUAWEI_CLOUD_AK=<your-AK>
export HUAWEI_CLOUD_SK=<your-SK>
export HUAWEI_CLOUD_PROJECT_ID=<your-ProjectID>
export HUAWEI_CLOUD_REGION=cn-north-4

go run ./cmd/list-vpcs/
```

---

## 项目结构

```
huawei-elb-controller/
├── cmd/
│   ├── main.go                          # 控制器入口
│   └── list-vpcs/
│       └── main.go                      # VPC/子网查询工具
├── internal/
│   ├── controller/
│   │   └── loadbalancerconfig_controller.go  # 核心 reconcile 逻辑
│   └── huaweicloud/
│       ├── client.go                    # 华为云客户端构建器
│       └── elb.go                       # ELB CRUD 操作
├── deploy/                              # 原始 Kubernetes manifests
│   ├── serviceaccount.yaml
│   ├── clusterrole.yaml
│   ├── clusterrolebinding.yaml
│   └── deployment.yaml
├── charts/
│   └── huawei-elb-controller/           # Helm Chart
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
│           ├── _helpers.tpl
│           ├── serviceaccount.yaml
│           ├── clusterrole.yaml
│           ├── clusterrolebinding.yaml
│           ├── secret.yaml
│           └── deployment.yaml
├── examples/                            # 示例 LoadBalancerConfig YAML
│   ├── internal-elb.yaml
│   └── public-elb.yaml
├── Dockerfile
├── Makefile
├── go.mod
└── go.sum
```

---

## 架构

### 组件概览

```
                    ┌──────────────────────────────────────────┐
                    │           Kubernetes Cluster              │
                    │                                          │
  ┌──────────┐     │  ┌──────────────┐    ┌───────────────┐   │
  │  OpenEverest  │   │
  │  operator     │   │
  └──────────┘     │  │     (CR)     │    └───────┬───────┘   │
                    │  └──────┬───────┘            │           │
                    │         │ watches            │ creates    │
                    │         ▼                    ▼           │
                    │  ┌──────────────┐    ┌───────────────┐   │
                    │  │   huawei-elb  │    │  K8s Service  │   │
                    │  │  controller   │    │ (LoadBalancer) │   │
                    │  └──────┬───────┘    └───────┬───────┘   │
                    │         │                    │           │
                    └─────────┼────────────────────┼──────────┘
                              │                    │
                    ┌─────────┼────────────────────┼──────────┐
                    │         ▼                    ▼          │
                    │  ┌──────────────┐    ┌───────────────┐   │
                    │  │ Huawei Cloud │    │  Huawei Cloud │   │
                    │  │   ELB v3     │◀───│     CCM      │   │
                    │  │    API       │    │ (binds ELB)  │   │
                    │  └──────────────┘    └───────────────┘   │
                    │         Huawei Cloud                      │
                    └──────────────────────────────────────────┘
```

### 关键设计决策

1. **使用 `unstructured.Unstructured` 访问 CR** —— 控制器通过 `unstructured.Unstructured` 与 `LoadBalancerConfig` CR 交互，而非使用生成的类型化客户端。这样可以避免导入 OpenEverest 的 Go 类型，让控制器与 OpenEverest 的 API 演进保持解耦。

2. **以 Annotation 作为配置通道** —— ELB 创建参数通过 `spec.annotations`（`huawei-elb.io/*`）传递，ELB ID 也会被回写到 `spec.annotations["kubernetes.io/elb.id"]`。这种设计的含义：
   - OpenEverest operator 读取 `spec.annotations` 并复制到 Service（OpenEverest 既有行为）
   - CCM 从 Service 读取 `kubernetes.io/elb.id` 并绑定 ELB（CCM 既有行为）
   - 控制器无需直接创建或管理 Service

3. **基于 finalizer 的清理** —— finalizer（`huawei-elb.io/finalizer`）确保在 CR 从集群中移除前删除华为云 ELB，防止云资源遗留。

4. **基于 annotation 的过滤** —— 在 `spec.annotations`（用户指定）或 `metadata.annotations`（自动探测）中包含 `huawei-elb.io/vpc-id` 的 CR 会被处理。没有此 annotation 的 CR 会触发从集群节点的自动探测。使用 `kubernetes.io/elb.autocreate` 的 CR 会被跳过（由 CCE CCM 直接管理）。这样既能与其他 ELB 管理方案共存，也允许用户通过 OpenEverest UI 创建 LoadBalancerConfig，因为 UI 把 `spec.annotations` 暴露为可编辑字段。

---

## CRD 参考

控制器与两个 OpenEverest CRD 交互。下方的字段引用来自 [everest-operator 源码](https://github.com/openeverest/everest-operator/blob/main/api/everest/v1alpha1/databasecluster_types.go)。

### LoadBalancerConfig

```yaml
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: <config-name>
  annotations:
    # 控制器在此写入状态（metadata.annotations）：
    huawei-elb.io/ready: "true"
    huawei-elb.io/elb-status: "ACTIVE"
    huawei-elb.io/error: ""
    spec:
  annotations:
    # 用户设置这些字段（huawei-elb.io/*）：
    huawei-elb.io/vpc-id: "..."
    huawei-elb.io/subnet-id: "..."
    huawei-elb.io/availability-zones: "..."
    huawei-elb.io/public: "false"
    # 控制器在此写入 ELB ID：
    kubernetes.io/elb.id: "<elb-uuid>"
```

### DatabaseCluster —— `spec.proxy.expose`

来自 [`Expose` 结构体](https://github.com/openeverest/everest-operator/blob/b296204ed61cbf540d3984c4b62451a1c572878a/api/everest/v1alpha1/databasecluster_types.go#L225-L242)：

```go
type Expose struct {
    // Type: ClusterIP | LoadBalancer | NodePort
    // （旧值 "internal" 和 "external" 已弃用）
    Type ExposeType `json:"type,omitempty"`

    // IPSourceRanges：可选的 IP 白名单（CIDR 格式）
    IPSourceRanges []IPSourceRange `json:"ipSourceRanges,omitempty"`

    // LoadBalancerConfigName：引用 LoadBalancerConfig CR
    // ⚠️ 一旦设置，无法清除（XValidation 规则）
    LoadBalancerConfigName string `json:"loadBalancerConfigName,omitempty"`
}
```

### 支持的引擎与代理类型

| `spec.engine.type` | 引擎 | `spec.proxy.type` |
|---|---|---|
| `postgresql` | PostgreSQL | `pgbouncer` |
| `pxc` | MySQL（Percona XtraDB Cluster） | `haproxy` |
| `psmdb` | MongoDB | `mongos` |

> `spec.engine.type` 在创建后不可变。`spec.proxy.expose.loadBalancerConfigName` 一旦设置也无法清除。

---

## 协调循环

控制器的 reconcile 循环遵循以下逻辑：

```
┌─────────────────────────────────────────────────────────────┐
│                    Reconcile(LBC)                            │
└──────────────────────────┬──────────────────────────────────┘
                           │
                   ┌───────▼────────┐
                   │ Fetch CR from  │
                   │   cluster      │
                   └───────┬────────┘
                           │
                   ┌───────▼────────┐       ┌──────────────┐
                   │ Has vpc-id     │──No──▶│ Auto-detect  │
                   │ annotation?    │       │ VPC/subnet/AZ│
                   └───────┬────────┘       └──────────────┘
                           │ Yes
                   ┌───────▼────────┐
                   │ Deletion       │──Yes──┐
                   │ timestamp set? │       │
                   └───────┬────────┘       ▼
                           │ No      ┌──────────────┐
                           │         │ Delete ELB   │
                           │         │ via API      │
                           │         │ Remove       │
                           │         │ finalizer    │
                           │         └──────────────┘
                   ┌───────▼────────┐
                   │ Has finalizer? │──No──▶ Add finalizer, requeue
                   └───────┬────────┘
                           │ Yes
                   ┌───────▼────────┐
                   │ elb.id exists? │──No──┐
                   │ in spec.annots │      │
                   └───────┬────────┘      ▼
                           │ Yes    ┌──────────────┐
                           │        │ Create ELB   │
                           │        │ via API      │
                           │        │ Write elb.id │
                           │        │ to spec.annots│
                           │        └──────┬───────┘
                           │               │
                   ┌───────▼────────┐◀─────┘
                   │ Query ELB      │
                   │ status from API│
                   └───────┬────────┘
                           │
                   ┌───────▼────────┐
                   │ Update ready   │
                   │ annotation     │
                   │ (true/false)   │
                   └───────┬────────┘
                           │
                   ┌───────▼────────┐
                   │ Requeue based  │
                   │ on state:      │
                   │ - ACTIVE: 5min │
                   │ - creating: 30s│
                   │ - perm error:  │
                   │   5min         │
                   │ - trans error: │
                   │   10s          │
                   └────────────────┘
```

### 重新排队间隔

| 状态 | 重新排队 | 原因 |
|---|---|---|
| ELB ACTIVE 且健康 | 5min | 周期性状态同步 |
| ELB 创建/更新中 | 30s | 配置过程中快速反馈 |
| 永久性错误（参数错误、未找到） | 5min | 不要对无法修复的错误反复请求 API |
| 临时性错误（网络、限流） | 10s | 对可恢复错误快速重试 |

---

## 端到端数据流

```
┌─────────────────────────────────────────────────────────────────────┐
│                         用户操作                                    │
└──────────────┬──────────────────────────────────┬───────────────────┘
               │                                  │
    ① 创建 LoadBalancerConfig         ④ 创建 DatabaseCluster
    （包含 label + ELB 参数）          （引用 LoadBalancerConfig）
               │                                  │
               ▼                                  ▼
┌──────────────────────┐              ┌──────────────────────────┐
│ OpenEverest operator │              │                          │
│                      │              │ ⑤ 创建 K8s LoadBalancer  │
│ ② 监听 CR           │              │   Service                │
│   调用 ELB v3 API    │              │   复制 spec.annotations  │
│   创建华为云 ELB     │              │   （包含 elb.id）        │
│                      │              │                          │
│ ③ 将 elb.id 写入     │              │ ⑥ CCM 读取 elb.id        │
│   spec.annotations   │              │   绑定预创建的 ELB       │
│   设置 ready=true    │              │   Service 获得 EXTERNAL-IP│
└──────────────────────┘              └──────────────────────────┘
```

**分步骤说明：**

1. 用户创建 `LoadBalancerConfig` CR，在 `spec.annotations` 中填入 ELB 参数（`huawei-elb.io/*`）
2. `huawei-elb-controller` 检测到 CR，调用华为云 ELB v3 API 创建 ELB
3. 控制器将 ELB ID 写回 `spec.annotations["kubernetes.io/elb.id"]`，并设置 `ready=true`
4. 用户创建 `DatabaseCluster` CR，引用该 `LoadBalancerConfig`
5. OpenEverest operator 创建 K8s `LoadBalancer` 类型 Service，复制 `spec.annotations`（包含 `elb.id`）
6. CCM 从 Service 读取 `kubernetes.io/elb.id`，绑定预创建的 ELB → Service 获得外部 IP

---

## 时序保护

控制器和 OpenEverest operator 都会修改 `LoadBalancerConfig` CR。为保证正确的顺序：

### 问题

```
Time →  T1                    T2                    T3
        Controller creates    OpenEverest op. reads CCM binds ELB
        ELB, writes elb.id    spec.annotations      to Service
```

如果 OpenEverest operator 在控制器写入 `elb.id` **之前**读取了 `spec.annotations`，Service 将不包含该 annotation，CCM 也就无法绑定 ELB。

### 解决方案：`huawei-elb.io/ready` Annotation

| 状态 | `ready` 值 | 含义 |
|---|---|---|
| ELB 创建中 | `false` | 未就绪 —— 暂不要创建 DatabaseCluster |
| ELB ACTIVE + ONLINE | `true` | 已就绪 —— 可以安全创建 DatabaseCluster |
| ELB 删除中 | `false` | 未就绪 —— 清理进行中 |

**推荐工作流：**

```bash
# 创建 LoadBalancerConfig
kubectl apply -f examples/internal-elb.yaml

# 等待 ready=true 后再创建 DatabaseCluster
kubectl wait loadbalancerconfig <name> \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# 此时再创建 DatabaseCluster
kubectl apply -f database-cluster.yaml
```

### 并发更新保护

控制器和 OpenEverest operator 都可能同时更新 CR。控制器采用：

- **`retry.RetryOnConflict`**：在 409 Conflict 错误时自动重新获取并重试
- **`updateWithRetry` 辅助函数**：所有 CR 更新都通过回调进行，在应用更改前重新获取最新版本

---

## 错误处理策略

### 错误分类

| 类型 | 示例 | 重新排队 | Annotation |
|---|---|---|---|
| **永久性** | 缺少必需 annotation、VPC ID 无效、ELB 未找到 | 5 分钟 | 设置 `huawei-elb.io/error` |
| **临时性** | 网络超时、API 限流、5xx 服务器错误 | 10 秒 | 设置 `huawei-elb.io/error` |
| **成功** | ELB ACTIVE，状态已同步 | 5 分钟 | 清除 `huawei-elb.io/error` |

### `errorAnnotation` 机制

控制器将最近的错误信息记录在 `metadata.annotations["huawei-elb.io/error"]`。该值：

- 在协调失败时设置
- 在协调成功时清除
- 仅在值发生变化时更新（避免不必要的写入和冲突）

---

## 多区域支持

控制器支持在不同华为云区域部署 ELB：

1. **全局区域**（默认）：通过 Secret 中的 `HUAWEI_CLOUD_REGION` 环境变量设置
2. **按 CR 覆盖**：在特定 `LoadBalancerConfig` 上设置 `huawei-elb.io/region` annotation

当 CR 指定的区域与全局区域不同时，控制器会为该 CR 创建一个专用的 ELB 客户端（复用全局的 AK/SK/ProjectID）。

```go
func (r *LoadBalancerConfigReconciler) getELBClient(lbc *unstructured.Unstructured) (*elb.ElbClient, error) {
    // 检查按 CR 覆盖的区域
    region := getSpecAnnotation(lbc, "huawei-elb.io/region")
    if region == "" || region == r.Creds.Region {
        return r.ELBClient, nil // 使用全局客户端
    }
    // 创建区域特定的客户端
    return huaweicloud.NewELBClient(&huaweicloud.Credentials{
        AK:        r.Creds.AK,
        SK:        r.Creds.SK,
        Region:    region,
        ProjectID: r.Creds.ProjectID,
    })
}
```

---

## 测试

### 手动测试流程

```bash
# 1. 部署控制器
kubectl apply -f deploy/

# 2. 创建 LoadBalancerConfig
kubectl apply -f examples/internal-elb.yaml

# 3. 等待 ELB 创建
kubectl wait loadbalancerconfig internal-elb \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# 4. 验证 ELB 在华为云中存在（应显示 ACTIVE）
kubectl get loadbalancerconfig internal-elb -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-status}'

# 5. 删除 CR 并验证 ELB 已清理
kubectl delete loadbalancerconfig internal-elb
# 控制器日志应显示 "deleting ELB" → "ELB deleted successfully"
```

### 验证 ELB 清理

```bash
# 删除 CR 后，验证 ELB 已从华为云消失
# 控制器应记录日志："ELB deleted successfully"

# 如果在华为云控制台手动删除了 ELB，
# 控制器会检测到 404 并优雅地移除 finalizer。
```

---

## 贡献

### 提交规范

本项目遵循 **DCO（Developer Certificate of Origin）**。每个 commit 必须包含：

```
Signed-off-by: Your Name <your.email@example.com>
```

使用 `git commit -s` 自动添加签名。

### 代码风格

- 提交前运行 `go vet ./...`
- 遵循标准 Go 格式（`gofmt`）
- 保持 reconcile 循环可读 —— 将复杂逻辑抽取到辅助函数中
- 所有 CR 更新必须通过 `updateWithRetry`，以处理冲突

### Pull Request 检查清单

- [ ] `go build ./...` 通过
- [ ] `go vet ./...` 通过
- [ ] `helm lint charts/huawei-elb-controller/` 通过
- [ ] Commit 包含 `Signed-off-by`
- [ ] 代码或 YAML 中无密钥或凭证
- [ ] 行为变更时同步更新文档