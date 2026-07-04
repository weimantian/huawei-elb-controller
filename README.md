# huawei-elb-controller

**English** | [中文](README-zh.md)

---

## Table of Contents

- [Overview](#overview)
- [How It Works](#how-it-works)
- [Features](#features)
- [Quick Start](#quick-start)
- [Configuration Reference](#configuration-reference)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

---

## Overview

`huawei-elb-controller` is a Kubernetes controller that manages **Huawei Cloud ELB** (Elastic Load Balancer) instances for [OpenEverest V1](https://github.com/openeverest/openeverest) (Percona Everest).

It solves a specific gap: OpenEverest V1's `LoadBalancerConfig` CR can inject annotations into a K8s Service, but **does not create the Huawei Cloud ELB itself**. This controller fills that gap — it watches `LoadBalancerConfig` CRs, automatically creates/deletes Huawei Cloud ELBs via the ELB v3 API, and writes the ELB ID back into the CR so that V1's operator-created Service can bind the pre-created ELB.

---

## How It Works

### Background

OpenEverest V1 is a database cluster management platform by Percona. When a user creates a `DatabaseCluster` CR, the V1 Operator creates a Kubernetes `LoadBalancer`-type Service for external access.

V1's `LoadBalancerConfig` CR mechanism lets users customize the Service's annotations:

```
DatabaseCluster.spec.proxy.expose.loadBalancerConfigName → points to a LoadBalancerConfig CR
LoadBalancerConfig.spec.annotations → V1 Operator copies these annotations to the K8s Service
```

In Huawei Cloud CCE (Cloud Container Engine) environments, the Cloud Controller Manager (CCM) binds a pre-created ELB via the `kubernetes.io/elb.id` annotation. But V1 doesn't create the ELB — it only passes annotations.

### The Problem

```
User creates LoadBalancerConfig (spec.annotations is empty)
    ↓
V1 Operator creates Service (no elb.id annotation)
    ↓
CCM can't find an ELB → Service never gets an external IP ❌
```

### The Solution

`huawei-elb-controller` automates ELB creation and write-back:

```
User creates LoadBalancerConfig (with label + ELB params)
    ↓
huawei-elb-controller detects the CR, calls Huawei Cloud ELB v3 API to create ELB
    ↓
Controller writes ELB ID back to LoadBalancerConfig.spec.annotations["kubernetes.io/elb.id"]
    ↓
User creates DatabaseCluster referencing the LoadBalancerConfig
    ↓
V1 Operator creates Service, copies spec.annotations (including elb.id)
    ↓
CCM reads elb.id, binds pre-created ELB → Service gets external IP ✅
```

### End-to-End Data Flow

```
┌─────────────────────────────────────────────────────────────────────┐
│                         User Actions                                 │
└──────────────┬──────────────────────────────────┬───────────────────┘
               │                                  │
    ① Create LoadBalancerConfig         ④ Create DatabaseCluster
    (with label + ELB params)           (references LoadBalancerConfig)
               │                                  │
               ▼                                  ▼
┌──────────────────────┐              ┌──────────────────────────┐
│ huawei-elb-controller │              │   V1 Operator             │
│                      │              │                          │
│ ② Watches CR         │              │ ⑤ Creates K8s LoadBalancer │
│   Calls ELB v3 API   │              │   Service                │
│   Creates Huawei ELB │              │   Copies spec.annotations │
│                      │              │   (includes elb.id)       │
│ ③ Writes elb.id to   │              │                          │
│   spec.annotations   │              │ ⑥ CCM reads elb.id       │
│   Sets ready=true    │              │   Binds pre-created ELB  │
└──────────────────────┘              │   Service gets EXTERNAL-IP│
                                      └──────────────────────────┘
```

### Timing Protection

To ensure the ELB ID is written before the V1 Operator reads `spec.annotations`, the controller provides a `huawei-elb.io/ready` annotation:

- ELB being created: `ready=false`
- ELB ACTIVE and ONLINE: `ready=true`
- ELB being deleted: `ready=false`

Users should wait for `ready=true` before creating a `DatabaseCluster`:

```bash
kubectl wait loadbalancerconfig <name> \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s
```

---

## Features

### Core Features

| Feature | Description |
|---|---|
| **ELB Creation** | Creates a Huawei Cloud ELB via the ELB v3 API based on `LoadBalancerConfig` annotations |
| **ELB Deletion** | Automatically deletes the corresponding Huawei Cloud ELB when the CR is deleted, via finalizer mechanism — no leftover resources |
| **Status Reconciliation** | Continuously polls ELB status, updates status info in `metadata.annotations` |
| **Idempotency** | After controller restart, finds existing ELB by name — no duplicate creation |
| **Timing Protection** | `huawei-elb.io/ready` annotation marks whether the ELB is ready; users can wait before creating database clusters |

### Production Features

| Feature | Description |
|---|---|
| **Label Filtering** | Only processes CRs with `huawei-elb.io/controlled=true` label — doesn't touch other LoadBalancerConfigs |
| **Error Classification** | Permanent errors (bad params) retry every 5 min; transient errors (network/throttling) retry every 10 sec |
| **Error Recording** | `huawei-elb.io/error` annotation records the last reconciliation error for troubleshooting |
| **Conflict Handling** | Uses `retry.RetryOnConflict` to handle concurrent update conflicts with V1 Operator |
| **Multi-Region** | Supports per-CR region override via `huawei-elb.io/region` annotation |
| **Health Probes** | Built-in `/readyz` and `/healthz` endpoints for Kubernetes readiness/liveness probes |
| **Credential Security** | AK/SK injected via Kubernetes Secret — no hardcoding in image |
| **Helm Chart** | Full Helm Chart with parameterized deployment |

### Supported ELB Types

| Type | `huawei-elb.io/public` | Description |
|---|---|---|
| **Internal ELB** | `false` (default) | Private VPC IP only — suitable for intra-VPC access |
| **Public ELB** | `true` | Includes a floating IP (EIP) — accessible from the internet |

---

## Quick Start

### Prerequisites

Before you begin, ensure you have:

1. **Huawei Cloud account**: ELB service enabled, with AK (Access Key) and SK (Secret Key)
2. **Kubernetes cluster**: OpenEverest V1 and Huawei Cloud CCM installed
3. **kubectl**: configured with cluster access
4. **Helm 3** (optional): if deploying via Helm Chart
5. **Go 1.26+** (optional): if building from source

### Step 1: Get Huawei Cloud Credentials

1. Log in to the [Huawei Cloud Console](https://console.huaweicloud.com/)
2. Navigate to "Identity and Access Management" → "My Credentials" → "Access Keys"
3. Create an access key — record your AK and SK
4. Get your Project ID: found in the top-right dropdown under your username

### Step 2: Get VPC and Subnet Information

The controller needs a VPC ID and a Neutron subnet ID to create an ELB.

**Method A: Via Console**

1. Navigate to "Virtual Private Cloud" service
2. Find the VPC where your cluster runs — record the VPC ID (e.g., `0d60646b-xxxx-xxxx-xxxx-xxxxxxxxxxxx`)
3. Click the subnet — record the **Neutron Network ID** (not the subnet resource ID)

**Method B: Via CLI tool**

If you have the controller source code, use the `list-vpcs` tool:

```bash
# Set credentials
export HUAWEI_CLOUD_AK=<your-AK>
export HUAWEI_CLOUD_SK=<your-SK>
export HUAWEI_CLOUD_PROJECT_ID=<your-ProjectID>
export HUAWEI_CLOUD_REGION=cn-north-4

# List all VPCs and subnets
go run ./cmd/list-vpcs/
```

Example output:
```
VPC: vpc-a489 (0d60646b-e3b7-4ad9-b422-015ee7da9a48) CIDR: 192.168.0.0/16
  Subnet: subnet-a489
    Resource ID:  566342ef-db1b-4ffa-a5ec-4185f5d61d40  ← NOT this one
    Neutron ID:   c265b187-a0a8-45cf-9cb3-7c3b757f8ff8  ← Use THIS one!
    CIDR:         192.168.0.0/24
```

> **⚠️ Important**: `huawei-elb.io/subnet-id` requires the **Neutron subnet ID** (SubnetCidr.Id), NOT the VPC subnet resource ID (Virsubnet.Id). Using the wrong ID will cause ELB creation to fail.

### Step 3: Deploy the Controller

#### Option A: Using Helm Chart (Recommended)

```bash
# Deploy with a values file
cat > my-values.yaml << 'EOF'
image:
  repository: huawei-elb-controller
  tag: latest
  pullPolicy: IfNotPresent

credentials:
  ak: "<your-AK>"
  sk: "<your-SK>"
  projectId: "<your-ProjectID>"
  region: "cn-north-4"

namespace: everest-system
EOF

helm install huawei-elb-controller \
  ./charts/huawei-elb-controller \
  -f my-values.yaml
```

#### Option B: Manual Deployment

1. Create the credentials Secret:

```bash
kubectl create secret generic huawei-cloud-credentials \
  --namespace everest-system \
  --from-literal=ak=<your-AK> \
  --from-literal=sk=<your-SK> \
  --from-literal=project-id=<your-ProjectID> \
  --from-literal=region=cn-north-4
```

2. Build and import the image:

```bash
# Build linux/amd64 binary
GOOS=linux GOARCH=amd64 go build -o huawei-elb-controller ./cmd/

# Build Docker image
docker buildx build --platform linux/amd64 -t huawei-elb-controller:latest .

# Import to cluster (push to SWR for CCE, or docker save + ctr import for self-managed)
```

3. Apply RBAC and Deployment:

```bash
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml
kubectl apply -f deploy/deployment.yaml
```

### Step 4: Verify the Controller is Running

```bash
# Check Pod status
kubectl get pods -n everest-system -l app=huawei-elb-controller

# View controller logs
kubectl logs -n everest-system deployment/huawei-elb-controller
```

Expected output:
```
NAME                                     READY   STATUS    RESTARTS   AGE
huawei-elb-controller-xxxxxxxxxx-xxxxx   1/1     Running   0          1m
```

Logs should show:
```
INFO    starting huawei-elb-controller    {"region": "cn-north-4", "metrics": ":8081"}
INFO    Starting Controller               {"controller": "loadbalancerconfig", ...}
INFO    Starting workers                  {"controller": "loadbalancerconfig", ..., "worker count": 1}
```

### Step 5: Create a LoadBalancerConfig

Create an internal ELB (intra-VPC access):

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: huawei-internal-elb
  labels:
    huawei-elb.io/controlled: "true"    # Required — controller only processes CRs with this label
  annotations:
    huawei-elb.io/vpc-id: "0d60646b-e3b7-4ad9-b422-015ee7da9a48"
    huawei-elb.io/subnet-id: "c265b187-a0a8-45cf-9cb3-7c3b757f8ff8"
    huawei-elb.io/availability-zones: "cn-north-4a,cn-north-4b"
    huawei-elb.io/public: "false"
spec:
  annotations: {}   # Controller auto-fills kubernetes.io/elb.id here
EOF
```

Create a public ELB (with floating IP, internet-accessible):

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
    huawei-elb.io/bandwidth-size: "20"               # 20 Mbit/s bandwidth
    huawei-elb.io/bandwidth-charge-mode: "traffic"    # Pay by traffic
    huawei-elb.io/public-ip-network-type: "5_bgp"     # BGP public IP
spec:
  annotations: {}
EOF
```

### Step 6: Wait for ELB to be Ready

```bash
# Wait for ready annotation to become true (up to 120 seconds)
kubectl wait loadbalancerconfig huawei-internal-elb \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# Check status
kubectl get loadbalancerconfig huawei-internal-elb -o jsonpath='{.spec.annotations}'
# Expected: {"kubernetes.io/elb.id":"xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"}

kubectl get loadbalancerconfig huawei-internal-elb -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-status}'
# Expected: ACTIVE
```

### Step 7: Create a DatabaseCluster

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
      loadBalancerConfigName: huawei-internal-elb   # Reference the LBC from Step 5
EOF
```

### Step 8: Verify Service ELB Binding

```bash
# Check the Service created by V1
kubectl get svc -n everest -l app.kubernetes.io/instance=my-database

# Check Service annotations for elb.id
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.id}'
# Expected: same ELB ID as in the LoadBalancerConfig

# Check if Service got an external IP
kubectl get svc <service-name> -n everest
# Expected: EXTERNAL-IP column shows the ELB's VIP address
```

---

## Configuration Reference

### LoadBalancerConfig Annotations

#### Required Annotations (`metadata.annotations`)

| Annotation | Description | Example |
|---|---|---|
| `huawei-elb.io/vpc-id` | VPC ID where the ELB will be created | `0d60646b-e3b7-4ad9-b422-015ee7da9a48` |
| `huawei-elb.io/subnet-id` | Neutron subnet ID (not VPC subnet resource ID) | `c265b187-a0a8-45cf-9cb3-7c3b757f8ff8` |
| `huawei-elb.io/availability-zones` | Comma-separated availability zone list | `cn-north-4a,cn-north-4b` |

#### Optional Annotations (`metadata.annotations`)

| Annotation | Default | Description |
|---|---|---|
| `huawei-elb.io/public` | `false` | `true` creates a public ELB (with EIP); `false` creates an internal ELB |
| `huawei-elb.io/bandwidth-size` | `10` | EIP bandwidth size (Mbit/s) — public ELB only |
| `huawei-elb.io/bandwidth-charge-mode` | `traffic` | Billing mode: `traffic` (pay-per-traffic) or `bandwidth` (pay-per-bandwidth) |
| `huawei-elb.io/public-ip-network-type` | `5_bgp` | EIP network type; `5_bgp` for BGP public IP |
| `huawei-elb.io/region` | Global REGION | Override the Huawei Cloud region for a specific CR |

#### Controller-Written Annotations

| Location | Annotation | Description |
|---|---|---|
| `spec.annotations` | `kubernetes.io/elb.id` | Huawei Cloud ELB ID — V1 Operator copies this to the Service; CCM uses it to bind the ELB |
| `metadata.annotations` | `huawei-elb.io/ready` | `true` when ELB is ready; `false` during creation or error |
| `metadata.annotations` | `huawei-elb.io/elb-status` | ELB provisioning status (`ACTIVE`, `PENDING_CREATE`, etc.) |
| `metadata.annotations` | `huawei-elb.io/public-ip` | EIP address for public ELBs (empty for internal) |
| `metadata.annotations` | `huawei-elb.io/error` | Last reconciliation error message (empty when healthy) |

### Helm Values

| Parameter | Default | Description |
|---|---|---|
| `image.repository` | `huawei-elb-controller` | Image repository |
| `image.tag` | `latest` | Image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `credentials.ak` | `""` | Huawei Cloud AK |
| `credentials.sk` | `""` | Huawei Cloud SK |
| `credentials.projectId` | `""` | Huawei Cloud Project ID |
| `credentials.region` | `cn-north-4` | Huawei Cloud region |
| `existingSecret` | `""` | Use an existing Secret (overrides credentials) |
| `namespace` | `everest-system` | Deployment namespace |
| `resources.requests.cpu` | `100m` | CPU request |
| `resources.requests.memory` | `128Mi` | Memory request |
| `resources.limits.cpu` | `500m` | CPU limit |
| `resources.limits.memory` | `256Mi` | Memory limit |
| `healthProbe.readinessProbe.initialDelaySeconds` | `5` | Readiness probe initial delay |
| `healthProbe.livenessProbe.initialDelaySeconds` | `15` | Liveness probe initial delay |

---

## Troubleshooting

### Controller Pod Won't Start

```bash
# Check Pod events
kubectl describe pod -n everest-system -l app=huawei-elb-controller

# Common causes:
# 1. Image not found → ensure the image is imported into the cluster
# 2. Secret missing → check huawei-cloud-credentials Secret
# 3. RBAC insufficient → check ClusterRole and ClusterRoleBinding
```

### ELB Creation Failed

```bash
# Check error annotation
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/error}'

# Common errors:
# "missing required annotations" → check vpc-id, subnet-id, availability-zones
# "creating ELB: ..." → check controller logs for Huawei Cloud API error details
```

### Wrong subnet-id

> **Most common error**: using the VPC subnet resource ID instead of the Neutron subnet ID.

```
Error: "creating ELB: ... vip_subnet_cidr_id ... not found"
Cause: subnet-id was set to Virsubnet.Id instead of SubnetCidr.Id (Neutron ID)
Fix: Run go run ./cmd/list-vpcs/ to get the correct Neutron subnet ID
```

### Service Has No External IP

```bash
# 1. Check if LoadBalancerConfig is ready
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'
# Should be "true"

# 2. Check Service annotations
kubectl get svc <service-name> -o jsonpath='{.metadata.annotations}'
# Should include kubernetes.io/elb.id

# 3. If annotations are correct but no IP, check if CCM is running
kubectl get pods -A | grep cloud-controller
```

### LoadBalancerConfig Deletion Stuck

```bash
# Check if finalizer exists
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.finalizers}'
# Should include "huawei-elb.io/finalizer"

# Check controller logs to confirm deletion was attempted
kubectl logs -n everest-system deployment/huawei-elb-controller --tail=10

# If the ELB was manually deleted, the controller will skip and remove the finalizer
```

---

## Development

### Build from Source

```bash
# Dependencies
go mod tidy

# Build
go build ./...

# Lint
go vet ./...

# Run locally (requires kubeconfig and credentials)
export HUAWEI_CLOUD_AK=...
export HUAWEI_CLOUD_SK=...
export HUAWEI_CLOUD_PROJECT_ID=...
export HUAWEI_CLOUD_REGION=cn-north-4
go run ./cmd/
```

### Project Structure

```
huawei-elb-controller/
├── cmd/
│   ├── main.go              # Controller entry point
│   └── list-vpcs/           # VPC/subnet lookup tool
├── internal/
│   ├── controller/
│   │   └── loadbalancerconfig_controller.go  # Core reconcile logic
│   └── huaweicloud/
│       ├── client.go         # Huawei Cloud client builder
│       └── elb.go            # ELB CRUD operations
├── deploy/                   # Kubernetes deployment manifests
│   ├── serviceaccount.yaml
│   ├── clusterrole.yaml
│   ├── clusterrolebinding.yaml
│   └── deployment.yaml
├── charts/                   # Helm Chart
│   └── huawei-elb-controller/
├── examples/                 # Example YAML
│   ├── internal-elb.yaml
│   └── public-elb.yaml
├── Dockerfile
├── Makefile
└── go.mod
```

### Reconciliation Loop

The controller's reconcile loop follows this logic:

1. **Fetch CR**: Get the LoadBalancerConfig from the cluster
2. **Label check**: Skip CRs without `huawei-elb.io/controlled=true` label
3. **Deletion handling**: If deletion timestamp is set, delete the ELB and remove the finalizer
4. **Finalizer ensure**: If no finalizer exists, add one and requeue
5. **ELB creation**: If no `elb.id` in `spec.annotations`, create a new ELB
6. **ELB status check**: If `elb.id` exists, query ELB status and update `ready` annotation
7. **Requeue**: Schedule next reconciliation based on state (30s/5min/10s/5min)
