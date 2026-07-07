# huawei-elb-controller

**English** | [中文](README-zh.md)

---

## Overview

`huawei-elb-controller` is a Kubernetes controller that automatically creates and manages **Huawei Cloud ELB** (Elastic Load Balancer) instances for [OpenEverest](https://openeverest.io/documentation/current/) (formerly Percona Everest) database clusters.

**The problem it solves**: OpenEverest's `LoadBalancerConfig` CR can pass annotations to a Kubernetes Service, but it doesn't create the Huawei Cloud ELB itself. Without this controller, you'd have to manually create an ELB in the Huawei Cloud console, copy its ID, and paste it into the CR — every time.

**What it does**: Watches `LoadBalancerConfig` CRs, calls the Huawei Cloud ELB v3 API to create/delete ELBs automatically, and writes the ELB ID back into the CR. OpenEverest's operator then picks up the ELB ID, adds it to the Service, and the Huawei Cloud CCM binds the ELB — giving your database cluster an external load-balanced endpoint.

---

## Features

- **Zero-config auto-detection** — automatically detects VPC, subnet, and availability zones from cluster nodes (like EKS/GKE)
- **Public ELB by default** — creates a public ELB with EIP out of the box; set `huawei-elb.io/public: "false"` for internal
- **Full lifecycle management** — creates, monitors, and deletes ELBs via Huawei Cloud ELB v3 API with finalizer safety
- **Status visibility** — exposes `ready`, `elb-status`, `error`, and `public-ip` annotations on the CR
- **UI-friendly** — works seamlessly with the OpenEverest web UI; no `kubectl` required for end-to-end setup
- **Multi-region support** — override the region per CR via the `huawei-elb.io/region` annotation

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
OpenEverest operator creates a LoadBalancer-type Service
    ↓
Huawei Cloud CCM binds the ELB → Service gets an external IP
    ↓
You connect to your database via the ELB's IP address
```

The controller creates a Kubernetes `Service` of type `LoadBalancer` in the `everest-system` namespace. This Service carries Huawei Cloud ELB annotations that tell the CCE Cloud Controller Manager (CCM) how to bind the Service to a Huawei Cloud ELB instance:

- `kubernetes.io/elb.id: <elbID>` — binds the Service to a pre-created ELB instance (the one created by this controller)
- `kubernetes.io/elb.class: union` — uses the Huawei Cloud ELB load balancing mode

When the CCE CCM detects a `LoadBalancer` Service with these annotations, it configures the ELB listener and backend members based on the Service's ports and the pod endpoints. The controller creates the ELB via the Huawei Cloud ELB v3 API, then creates the Service with the `kubernetes.io/elb.id` annotation pointing to the new ELB. The CCE CCM then completes the binding.

This is the standard mechanism for integrating external load balancers with CCE — see the [CCE documentation](https://support.huaweicloud.com/usermanual-cce/cce_10_0385.html) for details.

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
# 1. Build the image
#    Use --provenance=false to avoid SWR's "Invalid image, fail to parse 'manifest.json'" error
git clone https://github.com/weimantian/huawei-elb-controller.git
cd huawei-elb-controller

# Docker:
docker buildx build --platform linux/amd64 --provenance=false -t huawei-elb-controller:latest .
# nerdctl (containerd-only environments, no Docker):
# nerdctl build --platform linux/amd64 -t huawei-elb-controller:latest .

# 2. Login to SWR and push the image
#    Get the login command from the SWR console overview page
# Docker:
docker login -u <your-namespace> -p <login-token> <swr-registry>
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest
# nerdctl:
# nerdctl login -u <your-namespace> -p <login-token> <swr-registry>
# nerdctl tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
# nerdctl push <swr-registry>/huawei-elb-controller:latest

# 3. Create a values file with your Huawei Cloud credentials
cat > my-values.yaml << 'EOF'
image:
  repository: <swr-registry>/huawei-elb-controller
  tag: latest
  pullPolicy: Always

# Required on CCE: nodes have no SWR auth by default.
# Use CCE's built-in `default-secret` (exists in every namespace)
# or create your own image pull secret.
imagePullSecrets:
  - name: default-secret

credentials:
  ak: "<your-AK>"
  sk: "<your-SK>"
  projectId: "<your-ProjectID>"
  region: "<your-region>"  # e.g. cn-north-4, sa-brazil-1

namespace: everest-system
EOF

# 4. Install via Helm
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

2. Build and push the container image to SWR:

```bash
# Docker:
docker buildx build --platform linux/amd64 --provenance=false -t huawei-elb-controller:latest .
# nerdctl (containerd-only environments, no Docker):
# nerdctl build --platform linux/amd64 -t huawei-elb-controller:latest .

# Login to SWR and push the image
# Get the login command from the SWR console overview page
# Docker:
docker login -u <your-namespace> -p <login-token> <swr-registry>
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest
# nerdctl:
# nerdctl login -u <your-namespace> -p <login-token> <swr-registry>
# nerdctl tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
# nerdctl push <swr-registry>/huawei-elb-controller:latest
```

Then update `deploy/deployment.yaml` to use `<swr-registry>/huawei-elb-controller:latest` as the container image.

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

On CCE, the controller auto-detects VPC, subnet, and availability zones from cluster nodes — no manual configuration needed. The default ELB type is **public** (with EIP). Set `huawei-elb.io/public: "false"` for an internal ELB.

#### Option A (Recommended): Zero-config via OpenEverest UI

Create a LoadBalancerConfig from the OpenEverest web UI — no `kubectl` needed:

1. Open the OpenEverest UI in your browser (e.g., `http://localhost:8080` if port-forwarded).
2. Navigate to **Settings → Policies & Configurations → Load Balancer Configuration**.
3. Click **Create configuration**.
4. Fill in a **Name** (e.g., `huawei-elb`).
5. For an **internal ELB**, add one annotation:
   - Key: `huawei-elb.io/public`, Value: `false`
   - For a **public ELB** (default), skip this step — leave annotations empty.
6. Click **Save**.

The controller will automatically detect the new CR, auto-detect VPC/subnet/AZ from nodes, and create the ELB within a few seconds. Verify with `kubectl get loadbalancerconfig`.

#### Option B: Zero-config via kubectl

**Public ELB** (default, internet-accessible with EIP):

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

**Internal ELB** (VPC-internal access only — one annotation):

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

The controller will:

1. List all nodes and collect their internal IPs and zone labels (`topology.kubernetes.io/zone`)
2. Call the Huawei Cloud VPC API to find the VPC and subnet containing the node IPs
3. Write the detected values into `metadata.annotations` (marked with `huawei-elb.io/auto-detected: "true"`)
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
| `huawei-elb.io/public` | Defaults to `true` (public) | Set `"false"` for internal ELB |

Optional public ELB parameters (only effective when `public: "true"`):

| Annotation | Default | Description |
|---|---|---|
| `huawei-elb.io/bandwidth-size` | `10` | EIP bandwidth (Mbit/s) |
| `huawei-elb.io/bandwidth-charge-mode` | `traffic` | `traffic` (pay-per-traffic) or `bandwidth` (pay-per-bandwidth) |
| `huawei-elb.io/public-ip-network-type` | `5_bgp` | EIP network type |

### Step 5: Wait for ELB to be Ready

```bash
# Wait for the ELB to be created and active (up to 120s)
kubectl wait loadbalancerconfig huawei-elb \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# Verify ELB status
kubectl get loadbalancerconfig huawei-elb -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-status}'
# Expected: ACTIVE

# Verify ELB ID was written
kubectl get loadbalancerconfig huawei-elb -o jsonpath='{.spec.annotations}'
# Expected: {"kubernetes.io/elb.id":"xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"}
```

The ELB ID in `spec.annotations` is what OpenEverest's operator reads when creating a Service for your database cluster in the next step.

### Step 6: Create a DatabaseCluster

Create a database cluster that uses the LoadBalancerConfig created in Step 4.

#### Option A (Recommended): Via OpenEverest UI

1. Navigate to **Databases** in the OpenEverest UI.
2. Click **Create database**.
3. **Step 1 — Basic Information**: Select engine (e.g., PostgreSQL), fill in a name (e.g., `my-pg`), choose a version.
4. **Step 2 — Resources**: Set CPU, memory, disk size, and number of nodes.
5. **Step 3 — Backups**: Configure backup storage (or skip).
6. **Step 4 — Advanced Configurations**:
   - Set **Storage class** (e.g., `csi-disk`).
   - Enable **External access** (LoadBalancer).
   - Select the **Load Balancer config** created in Step 4 (e.g., `huawei-elb`).
7. **Step 5 — Monitoring**: Configure monitoring (or skip).
8. Click **Create database**.

> **Note**: If the LoadBalancer config dropdown shows "- No configuration -", the ELB may not be ready yet. Go back to Step 5 and wait for `ready=true`.

#### Option B: Via kubectl

Create a PostgreSQL database cluster:

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

#### 1. Check the database cluster is running

```bash
# List all database clusters and their status
kubectl get databasecluster -n everest
```

Expected output:
```
NAME        SIZE   READY   STATUS   HOSTNAME        AGE
my-pg       1      1       ready    <ELB-VIP>   5m
```

- `READY`: ready replicas / total replicas (should match `SIZE`)
- `STATUS`: should be `ready`
- `HOSTNAME`: the ELB's VIP address (internal IP)

#### 2. Find the LoadBalancer Service

```bash
# List the Service created by OpenEverest for the database
# Replace <db-name> with your database name (e.g., my-pg)
kubectl get svc -n everest -l app.kubernetes.io/instance=<db-name>
```

Expected output:
```
NAME                TYPE           CLUSTER-IP      EXTERNAL-IP      PORT(S)          AGE
my-pg-pgbouncer     LoadBalancer   <CLUSTER-IP>   <ELB-VIP>    5432:31234/TCP   5m
```

- `TYPE`: should be `LoadBalancer`
- `EXTERNAL-IP`: the ELB's VIP address (internal IP for both internal and public ELBs)
- `PORT(S)`: database port — 5432 for PostgreSQL, 3306 for MySQL, 27017 for MongoDB

#### 3. Get the connection IP

**For internal ELB** (VPC-internal access only):

The `EXTERNAL-IP` from step 2 is the connection address:
```bash
# Extract the internal VIP from the Service status
kubectl get svc <service-name> -n everest -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
# Output: <ELB-VIP>
```

**For public ELB** (internet access):

Get the public IP (EIP) from the LoadBalancerConfig:
```bash
# Read the public IP annotation written by the controller
kubectl get loadbalancerconfig <lbc-name> -o jsonpath='{.metadata.annotations.huawei-elb\.io/public-ip}'
# Output: <EIP-address>
```

#### 4. Verify ELB is bound to the Service

```bash
# Check that the Service carries the ELB ID annotation
# This is the ELB's UUID (NOT an IP) — CCM uses it to bind the pre-created ELB to the Service
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.id}'
# Output: <ELB-UUID> (ELB UUID, not an IP)
```

This should match the ELB ID in the LoadBalancerConfig:
```bash
# Verify the same ELB ID is stored in the LBC CR
kubectl get loadbalancerconfig <lbc-name> -o jsonpath='{.spec.annotations.kubernetes\.io/elb\.id}'
```

> **Note**: The ELB ID is a UUID used internally by CCM. To connect to the database, use the IP from step 3, not this UUID.

#### 5. Install the database client (if not already installed)

**PostgreSQL (`psql`)**:

| OS | Command |
|---|---|
| macOS | `brew install postgresql` |
| Ubuntu/Debian | `sudo apt install postgresql-client` |
| CentOS/RHEL | `sudo yum install postgresql` |

**MySQL (`mysql`)**:

| OS | Command |
|---|---|
| macOS | `brew install mysql-client` |
| Ubuntu/Debian | `sudo apt install mysql-client` |
| CentOS/RHEL | `sudo yum install mysql` |

**MongoDB (`mongosh`)**:

| OS | Command |
|---|---|
| macOS | `brew install mongosh` |
| Ubuntu/Debian | See [official guide](https://www.mongodb.com/docs/mongodb-shell/install/) |
| CentOS/RHEL | See [official guide](https://www.mongodb.com/docs/mongodb-shell/install/) |

#### 6. Connect to the database

Replace `<IP>` with the IP from step 3, and `<db-name>` with your database name.

**PostgreSQL** (port 5432):

```bash
# Get the database password
kubectl get secret everest-secrets-<db-name> -n everest -o jsonpath='{.data.postgres}' | base64 -d

# Connect via psql
psql -h <IP> -U postgres -d <db-name>
# Public ELB example:  psql -h <EIP-address> -U postgres -d my-pg
# Internal ELB example: psql -h <ELB-VIP> -U postgres -d my-pg
```

**MySQL / PXC** (port 3306):

```bash
# Get the database password
kubectl get secret everest-secrets-<db-name> -n everest -o jsonpath='{.data.root}' | base64 -d

# Connect via mysql client
mysql -h <IP> -u root -p -e "SELECT VERSION();"
# Public ELB example:  mysql -h <EIP-address> -u root -p
# Internal ELB example: mysql -h <ELB-VIP> -u root -p
```

**MongoDB / PSMDB** (port 27017):

```bash
# Get the database password
kubectl get secret everest-secrets-<db-name> -n everest -o jsonpath='{.data.clusterAdmin}' | base64 -d

# Connect via mongosh
mongosh "mongodb://clusterAdmin:<password>@<IP>:27017/?replicaSet=rs0"
# Public ELB example:  mongosh "mongodb://clusterAdmin:<password>@<EIP-address>:27017/?replicaSet=rs0"
```

> **Note**: For internal ELBs, the VIP is only reachable from within the VPC. If testing from your local machine outside the VPC, use a public ELB, or connect from within a Pod:
> ```bash
> kubectl exec -it <pod-name> -n everest -- psql -h <IP> -U postgres -d <db-name>
> ```

---

## Configuration Reference

### LoadBalancerConfig Annotations

#### Optional Annotations

| Annotation | Default | Description |
|---|---|---|
| `huawei-elb.io/public` | `true` | `false` = internal ELB; default `true` = public ELB (with EIP) |
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

> ⚠️ **Important**: `credentials.region` must match your CCE cluster's region (e.g. `cn-north-4`, `sa-brazil-1`). The default value is empty — deployment will fail if you don't set it.

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

## Annotations

The controller reads the following annotations from the `LoadBalancerConfig` CR's `spec.annotations` (or `metadata.annotations`). All are optional — if omitted, the controller auto-detects them from the CCE cluster nodes.

| Annotation | Description | Auto-detected from |
|---|---|---|
| `huawei-elb.io/vpc-id` | VPC ID for ELB creation | ECS server metadata via node's `machineID` |
| `huawei-elb.io/subnet-id` | Neutron subnet ID for ELB creation | Node label `node.kubernetes.io/subnetid` |
| `huawei-elb.io/availability-zones` | Availability zones, comma-separated (e.g. `cn-north-4a,cn-north-4b`) | Node label `topology.kubernetes.io/zone` |

### Manual override

If auto-detection fails or you want to override, annotate the `LoadBalancerConfig` CR:

```yaml
apiVersion: database.openeverest.io/v1
kind: LoadBalancerConfig
metadata:
  name: my-elb-config
  annotations:
    huawei-elb.io/vpc-id: "<your-vpc-id>"
    huawei-elb.io/subnet-id: "<your-subnet-id>"
    huawei-elb.io/availability-zones: "cn-north-4a"
spec:
  # ... rest of spec
```

When any of these annotations is present, the controller uses the provided values and skips auto-detection for that field.

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

## Uninstall

### 1. Delete all LoadBalancerConfigs first (important)

**Order matters.** Deleting a `LoadBalancerConfig` triggers the controller to delete the corresponding Huawei Cloud ELB via its finalizer. If you uninstall the controller first, the ELBs will be orphaned and **continue to incur charges** (public ELB EIPs are billed hourly).

```bash
# List all LoadBalancerConfigs
kubectl get loadbalancerconfig -A

# Delete each one (the controller deletes the corresponding Huawei Cloud ELB)
kubectl delete loadbalancerconfig <name>
```

Verify ELB deletion in the controller logs before proceeding:

```bash
kubectl logs -n everest-system deployment/huawei-elb-controller --tail=5
# Wait until you see "deleted ELB" for each LBC
```

> If a `LoadBalancerConfig` has the `everest.percona.com/in-use-protection` finalizer, it is still referenced by a database cluster. Delete the database (or switch it to a different LBC) first.

### 2. Uninstall the controller

#### Option A: Helm

```bash
helm uninstall huawei-elb-controller

# Remove the credentials Secret (Helm may leave it behind)
kubectl delete secret -n everest-system huawei-elb-controller-credentials 2>/dev/null
```

#### Option B: Raw Manifests

```bash
kubectl delete -f deploy/deployment.yaml
kubectl delete -f deploy/clusterrolebinding.yaml
kubectl delete -f deploy/clusterrole.yaml
kubectl delete -f deploy/serviceaccount.yaml
kubectl delete secret -n everest-system huawei-cloud-credentials
```

### 3. (Optional) Remove the CRD

This permanently removes the `LoadBalancerConfig` custom resource definition. Skip this step if you plan to reinstall.

```bash
kubectl delete crd loadbalancerconfigs.everest.percona.com
```

---

## Comparison with EKS/GKE

On Amazon EKS and Google GKE, creating a `type: LoadBalancer` Service automatically provisions a cloud load balancer — no controller deployment, no manual VPC/subnet configuration. The cloud's CCM reads VPC/subnet info directly from node metadata.

Huawei Cloud CCE's CCM lacks this auto-detection capability — users must manually look up and fill in VPC/subnet/AZ parameters. This controller bridges that gap by adding the auto-detection layer:

| Feature | EKS / GKE | CCE + this controller |
|---|---|---|
| Extra controller deployment | Not needed | Needed |
| User fills VPC/subnet/AZ | No | **No (auto-detected)** |
| Configuration complexity | Zero | **Zero** |
| ELB lifecycle management | CCM | Controller + finalizer |
| Status visibility | Service events | LBC annotations (`ready`, `elb-status`, `error`) |
| Deletion safety | CCM handles | Finalizer ensures ELB deleted before CR |
| Fine-grained ELB control | Limited | Full (tags, naming, params) |
| Error feedback | Service events | `huawei-elb.io/error` annotation on LBC |

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


## License

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE) for details.
