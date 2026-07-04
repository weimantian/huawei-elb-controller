# huawei-elb-controller

**English** | [中文](README-zh.md)

---

## Overview

`huawei-elb-controller` is a Kubernetes controller that automatically creates and manages **Huawei Cloud ELB** (Elastic Load Balancer) instances for [OpenEverest](https://openeverest.io/documentation/current/) (formerly Percona Everest) database clusters.

**The problem it solves**: OpenEverest's `LoadBalancerConfig` CR can pass annotations to a Kubernetes Service, but it doesn't create the Huawei Cloud ELB itself. Without this controller, you'd have to manually create an ELB in the Huawei Cloud console, copy its ID, and paste it into the CR — every time.

**What it does**: Watches `LoadBalancerConfig` CRs, calls the Huawei Cloud ELB v3 API to create/delete ELBs automatically, and writes the ELB ID back into the CR. OpenEverest's operator then picks up the ELB ID, adds it to the Service, and the Huawei Cloud CCM binds the ELB — giving your database cluster an external load-balanced endpoint.

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

A running Kubernetes cluster with:
- **Huawei Cloud CCM** (Cloud Controller Manager) installed — this is what binds the ELB to the Service
- **StorageClass** configured (for database persistent volumes)

OpenEverest is certified on the following platforms:

| Platform | Kubernetes Version |
|---|---|
| Google GKE | 1.31 – 1.33 |
| Amazon EKS | 1.31 – 1.33 |
| OpenShift | 4.16 – 4.18 |

> Other platforms (AKS, DigitalOcean, vanilla kubeadm) work but are not fully certified. Local clusters (minikube, kind, k3d) are not recommended due to network limitations.
>
> **For Huawei Cloud CCE clusters**: CCM is pre-installed. For self-managed clusters on Huawei Cloud ECS, install CCM separately.

### 2. OpenEverest (formerly Percona Everest)

> **Note**: The project formerly known as "Percona Everest" has been rebranded to **OpenEverest**. The `everest.percona.com/v1alpha1` API group remains unchanged. The old Percona Helm repo still works, but the new OpenEverest repo is recommended.

If you haven't installed OpenEverest yet:

**Prerequisites**: Helm v3 and [yq](https://github.com/mikefarah/yq) must be installed on your workstation. Air-gapped environments are not supported.

#### Option A: Helm (recommended)

```bash
# Add the OpenEverest Helm repository
helm repo add openeverest https://openeverest.github.io/helm-charts/
helm repo update

# Install OpenEverest
helm install everest-core openeverest/openeverest \
    --namespace everest-system \
    --create-namespace
```

This installs:
- Everest operator and server in `everest-system` namespace
- Database engine operators (PostgreSQL, MongoDB, PXC) in `everest` namespace

**Optional flags**:

| Flag | Purpose |
|---|---|
| `--set dbNamespace.enabled=false` | Don't auto-provision the `everest` db namespace |
| `--set dbNamespace.namespaceOverride=<name>` | Use a custom db namespace name |
| `--set dbNamespace.pxc=false` | Skip PXC operator installation |
| `--set dbNamespace.postgresql=false` | Skip PostgreSQL operator installation |
| `--set dbNamespace.psmdb=false` | Skip MongoDB operator installation |
| `--set server.tls.enabled=true` | Enable TLS for Everest component communication |

> ⚠️ Do NOT use `--no-hooks` — installation without chart hooks is unsupported.

#### Option B: everestctl CLI

```bash
# Download everestctl (macOS Apple Silicon)
curl -sSL -o everestctl-darwin-arm64 \
  https://github.com/openeverest/openeverest/releases/latest/download/everestctl-darwin-arm64
sudo install -m 555 everestctl-darwin-arm64 /usr/local/bin/everestctl
rm everestctl-darwin-arm64

# Install interactively
everestctl install

# Or install headless
everestctl install \
  --namespaces everest \
  --operator.postgresql=true \
  --operator.mysql=true \
  --operator.mongodb=true \
  --skip-wizard
```

#### Verify installation

```bash
# Check Everest pods are running
kubectl get pods -n everest-system

# Check database engine operators are registered
kubectl get dbengine -n everest
# Expected: percona-postgresql-operator, percona-psmdb-operator, percona-pxc-operator

# Get the admin password
kubectl get secret everest-accounts -n everest-system \
  -o jsonpath='{.data.users\.yaml}' | base64 --decode | yq '.admin.passwordHash'
```

> For more details, see the [OpenEverest Quickstart Guide](https://docs.percona.com/everest/quick-install.html) or the [OpenEverest documentation](https://openeverest.io/documentation/current/).

### 3. Huawei Cloud Account

- An active Huawei Cloud account with **ELB service enabled**
- **AK** (Access Key) and **SK** (Secret Key) — create at: IAM → My Credentials → Access Keys
- **Project ID** — found in the console top-right dropdown under your username

---

## Quick Start

### Step 1: Verify Prerequisites

```bash
# Check OpenEverest is running
kubectl get pods -n everest-system
# Expected: everest-operator and everest-server pods Running

# Check database engine operators are registered
kubectl get dbengine -n everest
# Expected: percona-postgresql-operator, percona-psmdb-operator, percona-pxc-operator

# Check CCM is running (Huawei Cloud)
kubectl get pods -A | grep cloud-controller
# Expected: cloud-controller-manager pods Running
```

### Step 2: Deploy the Controller

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

# For CCE clusters: push to SWR (Huawei Cloud Container Registry)
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest

# For self-managed clusters without SSH access to nodes,
# import the image via a helper pod with hostPath access:
docker save huawei-elb-controller:latest -o /tmp/image.tar

# Create a helper pod with access to the host filesystem
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: image-importer
spec:
  hostNetwork: true
  tolerations:
    - operator: Exists
  containers:
    - name: importer
      image: ubuntu
      command: ["sleep", "3600"]
      volumeMounts:
        - name: host
          mountPath: /host
  volumes:
    - name: host
      hostPath:
        path: /
EOF

kubectl wait --for=condition=Ready pod/image-importer --timeout=120s

# Copy the image tar into the helper pod, then import via ctr
kubectl cp /tmp/image.tar image-importer:/tmp/image.tar
kubectl exec image-importer -- chroot /host /usr/local/bin/ctr -n k8s.io image import /tmp/image.tar

# Clean up
kubectl delete pod image-importer
```

3. Apply the manifests:

```bash
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml
kubectl apply -f deploy/deployment.yaml
```

### Step 3: Verify the Controller is Running

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

### Step 4: Create a LoadBalancerConfig

On CCE, the controller auto-detects VPC, subnet, and availability zones from cluster nodes — no manual configuration needed. You only need to specify whether you want an internal or public ELB.

#### Option A (Recommended): Zero-config via kubectl

**Internal ELB** (default, VPC-internal access only):

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: huawei-internal-elb
spec:
  annotations: {}
EOF
```

**Public ELB** (internet-accessible, with floating IP — only one annotation needed):

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: huawei-public-elb
spec:
  annotations:
    huawei-elb.io/public: "true"
EOF
```

The controller will:

1. List all nodes and collect their internal IPs and zone labels (`topology.kubernetes.io/zone`)
2. Call the Huawei Cloud VPC API to find the VPC and subnet containing the node IPs
3. Write the detected values back into `spec.annotations` (marked with `huawei-elb.io/auto-detected: "true"`)
4. Create the ELB using the detected parameters

This gives a zero-config experience similar to EKS/GKE — just create the config and the controller figures out the rest.

> **Note**: Auto-detection works on CCE clusters where all nodes are in the same VPC. If nodes span multiple VPCs, the controller reports an error in the `huawei-elb.io/error` annotation.

##### Public vs Internal ELB

Auto-detection covers VPC, subnet, and availability zones — but **public vs internal is a user choice** and cannot be auto-detected:

| Annotation | Not set (auto-detect) | User sets manually |
|---|---|---|
| `huawei-elb.io/vpc-id` | ✅ Auto-detected from node IPs | Override if needed |
| `huawei-elb.io/subnet-id` | ✅ Auto-detected from node IPs | Override if needed |
| `huawei-elb.io/availability-zones` | ✅ Auto-detected from node labels | Override if needed |
| `huawei-elb.io/public` | Defaults to `false` (internal) | Set `"true"` for public ELB |

Optional public ELB parameters (only effective when `public: "true"`):

| Annotation | Default | Description |
|---|---|---|
| `huawei-elb.io/bandwidth-size` | `10` | EIP bandwidth (Mbit/s) |
| `huawei-elb.io/bandwidth-charge-mode` | `traffic` | `traffic` (pay-per-traffic) or `bandwidth` (pay-per-bandwidth) |
| `huawei-elb.io/public-ip-network-type` | `5_bgp` | EIP network type |

#### Option B (Recommended): Zero-config via OpenEverest UI

Create a LoadBalancerConfig from the OpenEverest web UI — no `kubectl` needed:

1. Open the OpenEverest UI in your browser (e.g., `http://localhost:8080` if port-forwarded).
2. Navigate to **Settings → Policies & Configurations → Load Balancer Configuration**.
3. Click **Create configuration**.
4. Fill in a **Name** (e.g., `huawei-internal-elb`).
5. For a **public ELB**, add one annotation:
   - Key: `huawei-elb.io/public`, Value: `true`
   - For an **internal ELB**, skip this step — leave annotations empty.
6. Click **Save**.

The controller will automatically detect the new CR, auto-detect VPC/subnet/AZ from nodes, and create the ELB within a few seconds. Verify with `kubectl get loadbalancerconfig`.

### Step 5: Wait for ELB to be Ready

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

### Step 6: Create a DatabaseCluster

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
      loadBalancerConfigName: huawei-internal-elb
EOF
```

> **Supported engine types**: `postgresql`, `pxc` (MySQL), `psmdb` (MongoDB). Supported proxy types: `pgbouncer` (PostgreSQL), `haproxy` (MySQL), `mongos` (MongoDB).
>
> **Optional**: Add `ipSourceRanges` under `expose` to restrict access to trusted IPs (CIDR notation):
> ```yaml
>     expose:
>       type: LoadBalancer
>       loadBalancerConfigName: huawei-internal-elb
>       ipSourceRanges:
>         - "10.0.0.0/24"
> ```

### Step 7: Verify Database Access

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
- `auto-detection failed: ...` → check that all nodes are in the same VPC; see controller logs for details
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

## Comparison with EKS/GKE

On Amazon EKS and Google GKE, creating a `type: LoadBalancer` Service automatically provisions a cloud load balancer — no controller deployment, no manual VPC/subnet configuration. The cloud's CCM reads VPC/subnet info directly from node metadata.

Huawei Cloud CCE's CCM also supports auto-creation via the `kubernetes.io/elb.autocreate` annotation, but it requires a verbose JSON spec with VPC/subnet/AZ parameters — the user must look up and fill in these values manually.

This controller bridges that gap by adding the auto-detection layer that Huawei Cloud's CCM lacks:

| Feature | EKS / GKE | CCE + autocreate | CCE + this controller |
|---|---|---|---|
| Extra controller deployment | Not needed | Not needed | Needed |
| User fills VPC/subnet/AZ | No | Yes (JSON) | **No (auto-detected)** |
| Configuration complexity | Zero | High (verbose JSON) | **Zero** |
| ELB lifecycle management | CCM | CCM | Controller + finalizer |
| Status visibility | Service events | Service events | LBC annotations (`ready`, `elb-status`, `error`) |
| Deletion safety | CCM handles | CCM handles | Finalizer ensures ELB deleted before CR |
| Fine-grained ELB control | Limited | Limited | Full (tags, naming, params) |
| Error feedback | Service events | Service events | `huawei-elb.io/error` annotation on LBC |

**Architecture difference**:

```
EKS/GKE:    Service → CCM creates LB (reads VPC from node metadata)

CCE + this controller:
            LBC → controller detects VPC/subnet/AZ from nodes
                → controller creates ELB via API
                → writes elb.id back to LBC
                → Everest copies elb.id to Service
                → CCM binds ELB to Service
```

The user experience is the same: create config → get load balancer → connect. The internal flow has an extra hop (controller creates ELB separately, then CCM binds it), but this gives better control, status reporting, and deletion safety.

## Development

For build instructions, architecture details, and contributing guidelines, see [DEVELOPMENT.md](DEVELOPMENT.md).
