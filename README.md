# huawei-elb-controller

**English** | [中文](README-zh.md)

---

## Overview

`huawei-elb-controller` is a Kubernetes controller that automatically creates and manages **Huawei Cloud ELB** (Elastic Load Balancer) instances for [Percona Everest](https://docs.percona.com/everest/) (OpenEverest V1) database clusters.

**The problem it solves**: Percona Everest's `LoadBalancerConfig` CR can pass annotations to a Kubernetes Service, but it doesn't create the Huawei Cloud ELB itself. Without this controller, you'd have to manually create an ELB in the Huawei Cloud console, copy its ID, and paste it into the CR — every time.

**What it does**: Watches `LoadBalancerConfig` CRs, calls the Huawei Cloud ELB v3 API to create/delete ELBs automatically, and writes the ELB ID back into the CR. Percona Everest's operator then picks up the ELB ID, adds it to the Service, and the Huawei Cloud CCM binds the ELB — giving your database cluster an external load-balanced endpoint.

---

## How It Works

```
You create a LoadBalancerConfig (with ELB params)
    ↓
huawei-elb-controller creates ELB via Huawei Cloud API
    ↓
Controller writes ELB ID back into the LoadBalancerConfig
    ↓
You create a DatabaseCluster referencing the LoadBalancerConfig
    ↓
Percona Everest operator creates a LoadBalancer-type Service
    ↓
Huawei Cloud CCM binds the ELB → Service gets an external IP
    ↓
You connect to your database via the ELB's IP address
```

---

## Prerequisites

### 1. Kubernetes Cluster

A running Kubernetes cluster (1.26+) with:
- **Huawei Cloud CCM** (Cloud Controller Manager) installed — this is what binds the ELB to the Service
- **StorageClass** configured (for database persistent volumes)

> **For Huawei Cloud CCE clusters**: CCM is pre-installed. For self-managed clusters on Huawei Cloud ECS, install CCM separately.

### 2. Percona Everest (OpenEverest V1)

If you haven't installed Percona Everest yet:

```bash
# Add the Percona Helm repository
helm repo add percona https://percona.github.io/percona-helm-charts/
helm repo update

# Install Percona Everest
helm install everest-core percona/everest \
    --namespace everest-system \
    --create-namespace
```

This installs:
- Everest operator and server in `everest-system` namespace
- Database operators (PostgreSQL, MongoDB, PXC) in `everest` namespace

Verify the installation:

```bash
# Check Everest pods are running
kubectl get pods -n everest-system

# Get the admin password
kubectl get secret everest-accounts -n everest-system \
  -o jsonpath='{.data.users\.yaml}' | base64 --decode | yq '.admin.passwordHash'
```

> For more details, see the [Percona Everest Quickstart Guide](https://docs.percona.com/everest/quick-install.html).

### 3. Huawei Cloud Account

- An active Huawei Cloud account with **ELB service enabled**
- **AK** (Access Key) and **SK** (Secret Key) — create at: IAM → My Credentials → Access Keys
- **Project ID** — found in the console top-right dropdown under your username
- Know your **VPC ID** and **Neutron Subnet ID** (see Step 2 below)

---

## Quick Start

### Step 1: Verify Prerequisites

```bash
# Check Percona Everest is running
kubectl get pods -n everest-system
# Expected: everest-operator and everest-server pods Running

# Check CCM is running (Huawei Cloud)
kubectl get pods -A | grep cloud-controller
# Expected: cloud-controller-manager pods Running

# Check database operators are installed
kubectl get pods -n everest
# Expected: psql-operator, pxc-operator, psmdb-operator pods Running
```

### Step 2: Get VPC and Subnet Information

The controller needs a **VPC ID** and a **Neutron subnet ID** to create an ELB.

> **Which subnet?** Use the **node subnet** — the subnet where your Kubernetes worker nodes live. Do NOT use the CCE management node subnet or the container/Pod subnet.

**Method A: Via Huawei Cloud Console**

1. Go to "Virtual Private Cloud" service
2. Find the VPC where your cluster runs — record the **VPC ID**
3. Click the subnet where your node IPs belong — record the **Neutron ID** (not the subnet Resource ID)

**Method B: Via the `list-vpcs` CLI tool**

```bash
# Clone this repo and run the VPC lookup tool
git clone https://github.com/weimantian/huawei-elb-controller.git
cd huawei-elb-controller

export HUAWEI_CLOUD_AK=<your-AK>
export HUAWEI_CLOUD_SK=<your-SK>
export HUAWEI_CLOUD_PROJECT_ID=<your-ProjectID>
export HUAWEI_CLOUD_REGION=cn-north-4

go run ./cmd/list-vpcs/
```

Example output:
```
VPC: vpc-a489 (0d60646b-e3b7-4ad9-b422-015ee7da9a48) CIDR: 192.168.0.0/16
  Subnet: subnet-a489
    Resource ID:  566342ef-...  ← NOT this one
    Neutron ID:   c265b187-...  ← Use THIS one
    CIDR:         192.168.0.0/24
```

> **Important**: `huawei-elb.io/subnet-id` requires the **Neutron subnet ID**, NOT the VPC subnet Resource ID. Using the wrong ID will cause ELB creation to fail.

### Step 3: Deploy the Controller

#### Option A: Using Helm (Recommended)

```bash
git clone https://github.com/weimantian/huawei-elb-controller.git
cd huawei-elb-controller

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

#### Option B: Using Raw Manifests

1. Create the credentials Secret:

```bash
kubectl create secret generic huawei-cloud-credentials \
  --namespace everest-system \
  --from-literal=ak=<your-AK> \
  --from-literal=sk=<your-SK> \
  --from-literal=project-id=<your-ProjectID> \
  --from-literal=region=cn-north-4
```

2. Build and import the container image:

```bash
GOOS=linux GOARCH=amd64 go build -o huawei-elb-controller ./cmd/
docker buildx build --platform linux/amd64 -t huawei-elb-controller:latest .
# Push to SWR for CCE, or docker save + ctr import for self-managed clusters
```

3. Apply the manifests:

```bash
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml
kubectl apply -f deploy/deployment.yaml
```

### Step 4: Verify the Controller is Running

```bash
kubectl get pods -n everest-system -l app=huawei-elb-controller
```

Expected:
```
NAME                                     READY   STATUS    RESTARTS   AGE
huawei-elb-controller-xxxxxxxxxx-xxxxx   1/1     Running   0          1m
```

Check logs:
```bash
kubectl logs -n everest-system deployment/huawei-elb-controller
```

Expected:
```
INFO    starting huawei-elb-controller    {"region": "cn-north-4"}
INFO    Starting Controller               {"controller": "loadbalancerconfig"}
INFO    Starting workers                  {"controller": "loadbalancerconfig", "worker count": 1}
```

### Step 5: Create a LoadBalancerConfig

Create an **internal ELB** (VPC-internal access only):

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: huawei-internal-elb
  labels:
    huawei-elb.io/controlled: "true"
  annotations:
    huawei-elb.io/vpc-id: "0d60646b-e3b7-4ad9-b422-015ee7da9a48"
    huawei-elb.io/subnet-id: "c265b187-a0a8-45cf-9cb3-7c3b757f8ff8"
    huawei-elb.io/availability-zones: "cn-north-4a,cn-north-4b"
    huawei-elb.io/public: "false"
spec:
  annotations: {}
EOF
```

Or create a **public ELB** (internet-accessible, with floating IP):

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
    huawei-elb.io/bandwidth-size: "20"
    huawei-elb.io/bandwidth-charge-mode: "traffic"
    huawei-elb.io/public-ip-network-type: "5_bgp"
spec:
  annotations: {}
EOF
```

### Step 6: Wait for ELB to be Ready

```bash
# Wait for the ELB to be created and active (up to 120s)
kubectl wait loadbalancerconfig huawei-internal-elb \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# Verify ELB status
kubectl get loadbalancerconfig huawei-internal-elb -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-status}'
# Expected: ACTIVE

# Verify ELB ID was written
kubectl get loadbalancerconfig huawei-internal-elb -o jsonpath='{.spec.annotations}'
# Expected: {"kubernetes.io/elb.id":"xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"}
```

> **Important**: Wait for `ready=true` before creating a DatabaseCluster. This ensures the ELB ID is written into the LoadBalancerConfig before Percona Everest's operator reads it.

### Step 7: Create a DatabaseCluster

Create a PostgreSQL database cluster that uses the LoadBalancerConfig:

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
      loadBalancerConfigName: huawei-internal-elb
EOF
```

### Step 8: Verify Database Access

```bash
# 1. Check the database cluster is running
kubectl get databasecluster -n everest
# Expected: my-pg is ready

# 2. Find the Service created by Percona Everest
kubectl get svc -n everest -l app.kubernetes.io/instance=my-pg
# Expected: a LoadBalancer-type Service with an EXTERNAL-IP

# 3. Verify the Service has the ELB ID annotation
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.id}'
# Expected: same ELB ID as in the LoadBalancerConfig

# 4. Connect to the database via the ELB's IP
# For internal ELB:
psql -h <ELB-VIP> -U postgres -d mydb

# For public ELB:
psql -h <EIP-address> -U postgres -d mydb
```

Example output:
```
NAME                TYPE           CLUSTER-IP      EXTERNAL-IP      PORT(S)          AGE
my-pg-pgbouncer     LoadBalancer   10.96.145.200   192.168.0.235    5432:31234/TCP   5m
```

The `EXTERNAL-IP` is the ELB's VIP address — this is what clients connect to.

---

## Configuration Reference

### LoadBalancerConfig Annotations

#### Required Annotations

| Annotation | Description | Example |
|---|---|---|
| `huawei-elb.io/vpc-id` | VPC ID where the ELB will be created | `0d60646b-...` |
| `huawei-elb.io/subnet-id` | Neutron subnet ID (NOT the VPC subnet Resource ID) | `c265b187-...` |
| `huawei-elb.io/availability-zones` | Comma-separated availability zone list | `cn-north-4a,cn-north-4b` |

#### Optional Annotations

| Annotation | Default | Description |
|---|---|---|
| `huawei-elb.io/public` | `false` | `true` = public ELB (with EIP); `false` = internal ELB |
| `huawei-elb.io/bandwidth-size` | `10` | EIP bandwidth (Mbit/s) — public ELB only |
| `huawei-elb.io/bandwidth-charge-mode` | `traffic` | `traffic` (pay-per-traffic) or `bandwidth` (pay-per-bandwidth) |
| `huawei-elb.io/public-ip-network-type` | `5_bgp` | EIP network type; `5_bgp` for BGP |
| `huawei-elb.io/region` | Global REGION | Override Huawei Cloud region for a specific CR |

#### Controller-Written Annotations

| Location | Annotation | Description |
|---|---|---|
| `spec.annotations` | `kubernetes.io/elb.id` | ELB ID — Percona Everest copies this to the Service; CCM uses it to bind the ELB |
| `metadata.annotations` | `huawei-elb.io/ready` | `true` when ELB is ready; `false` during creation/error |
| `metadata.annotations` | `huawei-elb.io/elb-status` | ELB status: `ACTIVE`, `PENDING_CREATE`, etc. |
| `metadata.annotations` | `huawei-elb.io/public-ip` | EIP address (public ELB only) |
| `metadata.annotations` | `huawei-elb.io/error` | Last error message (empty when healthy) |

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

---

## Troubleshooting

### Controller Pod Won't Start

```bash
kubectl describe pod -n everest-system -l app=huawei-elb-controller
```

Common causes:
- **Image not found** → ensure the image is imported into the cluster
- **Secret missing** → check `huawei-cloud-credentials` Secret exists in `everest-system`
- **RBAC insufficient** → check ClusterRole and ClusterRoleBinding

### ELB Creation Failed

```bash
# Check error annotation
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/error}'

# Check controller logs for API error details
kubectl logs -n everest-system deployment/huawei-elb-controller
```

Common errors:
- `missing required annotations` → check `vpc-id`, `subnet-id`, `availability-zones`
- `vip_subnet_cidr_id not found` → you used the VPC subnet Resource ID instead of the Neutron ID
- `creating ELB: ...` → check controller logs for Huawei Cloud API error details

### Service Has No External IP

```bash
# 1. Check if LoadBalancerConfig is ready
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'
# Should be "true"

# 2. Check Service has elb.id annotation
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.id}'
# Should show the ELB ID

# 3. Check CCM is running
kubectl get pods -A | grep cloud-controller
```

### LoadBalancerConfig Deletion Stuck

```bash
# Check if finalizer exists
kubectl get loadbalancerconfig <name> -o jsonpath='{.metadata.finalizers}'
# Should include "huawei-elb.io/finalizer"

# Check controller logs
kubectl logs -n everest-system deployment/huawei-elb-controller --tail=20

# If the ELB was manually deleted in Huawei Cloud console,
# the controller will detect the 404 and remove the finalizer automatically.
```

---

## Development

For build instructions, architecture details, and contributing guidelines, see [DEVELOPMENT.md](DEVELOPMENT.md).
