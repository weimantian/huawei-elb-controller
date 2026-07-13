# huawei-elb-controller

**English** | [中文](README-zh.md)

---

## Overview

`huawei-elb-controller` is a Kubernetes controller that automatically creates and manages **Huawei Cloud ELB** (Elastic Load Balancer) instances for [OpenEverest](https://openeverest.io/documentation/current/) database clusters. It watches `LoadBalancer` Services created by OpenEverest, injects CCE autocreate annotations for automatic ELB creation, and handles parameter updates.

**Two usage modes**:

- **Auto mode**: Create DBC without LBC → ELB auto-created (aligns with EKS/GKE experience)
- **Manual mode**: Create LBC as parameter template → DBC references it → custom ELB parameters

---

## Features

- **Zero-config auto mode** — no LBC needed; just create a database and the controller auto-creates an ELB (like EKS/GKE)
- **Manual mode with LBC parameter template** — LBC stores ELB parameters (bandwidth, EIP type, etc.) instead of an ELB ID; each Service gets its own independent ELB with zero port conflicts
- **Auto-detection of VPC/subnet/AZ** — automatically detects network topology from cluster nodes
- **ACL auto-handling** — `loadBalancerSourceRanges` → IP groups on the ELB
- **ELB parameter updates** — `kubectl annotate` on the Service triggers live ELB parameter updates via Huawei Cloud API (manual mode updates go through the LBC)
- **Default ELB**: public, 10 Mbit/s, traffic billing, 5_bgp
- **Full lifecycle management** — ELB creation/deletion via CCM, parameter updates via controller API calls
- **UI-friendly** — works seamlessly with the OpenEverest web UI; no `kubectl` required for end-to-end setup

---

## How It Works

### Auto mode

```
Create DBC (LBC = "No configuration")
  → OpenEverest creates LoadBalancer Service
  → Service Reconciler detects Service, auto-detects VPC/subnet/AZ
  → Injects elb.autocreate + elb.class + reclaim-policy
  → CCM creates ELB, writes elb.id, binds listener
  → Service gets EXTERNAL-IP ✅
```

### Manual mode (LBC parameter template)

```
Create LBC with huawei-elb.io/* annotations
  → Create DBC referencing the LBC
  → OpenEverest syncs annotations to Service
  → Service Reconciler reads params, injects autocreate JSON
  → CCM creates custom ELB
```

LBCs function as **parameter templates** (like EKS/GKE), not as instance references. Multiple Services referencing the same LBC each get their own independent ELB — zero port conflicts.

### Parameter updates

```
For **manual mode**:
User modifies LBC annotations (e.g., bandwidth 10M → 20M)
  → OpenEverest syncs to Service
  → Service Reconciler detects change
  → Calls Huawei Cloud ELB API to update parameters ✅

For **auto mode**:
  Annotate the Service directly
  → Service Reconciler detects change
  → Calls Huawei Cloud ELB API to update parameters ✅
```

### Service Reconciler

| Reconciler | Watches | Purpose |
|---|---|---|
| Service Reconciler | `Service` (type=LoadBalancer) | Injects autocreate annotations, handles parameter updates |
---

## Prerequisites

### 1. Kubernetes Cluster

A running Kubernetes cluster with:
- **Huawei Cloud CCM** (Cloud Controller Manager) installed — this is what creates and binds the ELB via autocreate
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

### 2. OpenEverest

OpenEverest must be installed and running in the cluster. The `everest.percona.com/v1alpha1` API group is used (the name remains from the former "Percona Everest" project).

For installation and the admin password retrieval, see the [OpenEverest documentation](https://openeverest.io/documentation/current/). Quick check:

```bash
kubectl get pods -n everest-system
# Expected: everest-operator and everest-server pods Running

kubectl get dbengine -n everest
# Expected: percona-postgresql-operator, percona-psmdb-operator, percona-pxc-operator
```

### 3. Huawei Cloud Account

- An active Huawei Cloud account with **ELB service enabled**
- **AK** (Access Key) and **SK** (Secret Key) — create at: IAM → My Credentials → Access Keys
- **Project ID** — found in the console top-right dropdown under your username

> ⚠️ **Important**: Must use **permanent** AK/SK (main account or IAM user with sufficient permissions). **Temporary AK/SK** (STS tokens) are not supported because they require a security token the controller does not handle. Required permissions: ELB Administrator, EIP Administrator, VPC ReadOnly, ECS ReadOnly.

---

## Quick Start

### Step 1: Deploy the Controller

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

# Default ELB parameters used in auto mode (no LBC)
defaults:
  public: true
  bandwidthSize: 10
  bandwidthChargeMode: "traffic"
  eipType: "5_bgp"
  bandwidthShareType: "PER"

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

### Step 2: Verify the Controller is Running

```bash
kubectl get pods -n everest-system -l app.kubernetes.io/name=huawei-elb-controller
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
INFO    Starting Controller               {"controller": "service"}
INFO    Starting workers                  {"controller": "service", "worker count": 1}
```

### Step 3: Create a Database (Auto Mode)

The easiest path: create a database cluster without a LoadBalancerConfig.

In the OpenEverest UI, select **"No configuration"** in the Load Balancer Configuration dropdown, or omit `loadBalancerConfigName` in the CR:

```yaml
spec:
  proxy:
    expose:
      type: LoadBalancer
      # loadBalancerConfigName omitted → auto mode
```

The controller will:
1. Detect the new LoadBalancer Service created by OpenEverest
2. Auto-detect VPC, subnet, and availability zones from cluster nodes
3. Inject `elb.autocreate` with default parameters (public, 10 Mbit/s, traffic billing, 5_bgp)
4. CCM creates the ELB and binds it to the Service

### Step 4: Get the Connection IP

```bash
# Get the external IP from the Service
kubectl get svc <service-name> -n everest -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
```

For database connection details (passwords, client installation, engine-specific commands), see the [OpenEverest documentation](https://openeverest.io/documentation/current/). Quick reference:

| Engine | Port | Password key | Connect command |
|---|---|---|---|
| PostgreSQL | 5432 | `.data.postgres` | `psql -h <IP> -U postgres -d <db-name>` |
| MySQL / PXC | 3306 | `.data.root` | `mysql -h <IP> -u root -p` |
| MongoDB / PSMDB | 27017 | `.data.clusterAdmin` | `mongosh "mongodb://clusterAdmin:<password>@<IP>:27017/?replicaSet=rs0"` |

Get the password:
```bash
kubectl get secret everest-secrets-<db-name> -n everest -o jsonpath='{.data.<password-key>}' | base64 -d
```

---

## Configuration Reference

### Auto Mode Defaults

When no LBC is used (UI: "No configuration"), the Service Reconciler applies these defaults:

| Parameter | Default | Description |
|---|---|---|
| ELB type | `public` | Public ELB with EIP |
| Bandwidth | `10` Mbit/s | EIP bandwidth |
| Billing mode | `traffic` | Pay-per-traffic |
| EIP type | `5_bgp` | BGP multi-line |

To customize defaults, set `defaults.*` in `my-values.yaml`.

> **Note**: The Service Reconciler auto-detects VPC, subnet, and availability zones from cluster node metadata. No manual configuration is needed.

### Manual Mode LBC Annotations

Create an LBC as a parameter template with `huawei-elb.io/*` annotations:

```yaml
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: my-elb-params
spec:
  annotations:
    huawei-elb.io/public: "true"
    huawei-elb.io/bandwidth-size: "20"
    huawei-elb.io/bandwidth-charge-mode: "traffic"
    huawei-elb.io/eip-type: "5_bgp"
```

| Annotation | Type | Default | Description |
|---|---|---|---|
| `huawei-elb.io/public` | string | `"true"` | `"false"` for internal ELB |
| `huawei-elb.io/bandwidth-size` | int (1–2000) | `10` | EIP bandwidth in Mbit/s |
| `huawei-elb.io/bandwidth-charge-mode` | string | `"traffic"` | `"traffic"` or `"bandwidth"` |
| `huawei-elb.io/eip-type` | string | `"5_bgp"` | `"5_bgp"`, `"5_sbgp"`, `"5_telcom"`, or `"5_union"` |
| `huawei-elb.io/bandwidth-share-type` | string | `"PER"` | `"PER"` (dedicated) or `"WHOLE"` (shared) |
| `huawei-elb.io/name` | string | `cce-lb-<ns>-<svc>` | Custom ELB name (≤ 64 chars) |
| `huawei-elb.io/region` | string | Controller region | Override Huawei Cloud region |

> **Note**: `eip-type` is immutable after ELB creation (Huawei Cloud API restriction). To change it, delete and recreate the ELB.

### ACL Annotations

Control ELB access with `loadBalancerSourceRanges` on the Service, or directly via ELB ACL annotations:

| Annotation | Description |
|---|---|
| `elb.acl-status` | `"on"` / `"off"` |
| `elb.acl-type` | `"white"` (allow) / `"black"` (deny) |
| `elb.acl-id` | Existing IP group ID (avoids creating a new one) |

### Controller-Written Annotations

| Annotation | Description |
|---|---|
| `kubernetes.io/elb.autocreate` | JSON with ELB parameters — injected by Service Reconciler, consumed by CCM |
| `kubernetes.io/elb.class` | `"union"` — required for Huawei Cloud ELB |
| `kubernetes.io/elb.instance-reclaim-policy` | `"alwaysDelete"` — CCM deletes ELB when Service is deleted |
| `kubernetes.io/elb.id` | ELB ID — written by CCM after creation |

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
| `defaults.public` | `true` | Default ELB type for auto mode (public/internal) |
| `defaults.bandwidthSize` | `10` | Default EIP bandwidth (Mbit/s) |
| `defaults.bandwidthChargeMode` | `traffic` | Default billing mode |
| `defaults.eipType` | `5_bgp` | Default EIP type |
| `defaults.bandwidthShareType` | `PER` | Default bandwidth share type |
| `resources.requests.cpu` | `100m` | CPU request |
| `resources.requests.memory` | `128Mi` | Memory request |
| `resources.limits.cpu` | `500m` | CPU limit |
| `resources.limits.memory` | `256Mi` | Memory limit |

> ⚠️ **Important**: `credentials.region` must match your CCE cluster's region (e.g. `cn-north-4`, `sa-brazil-1`). The default value is empty — deployment will fail if you don't set it.

---

## Troubleshooting

### Controller Pod Won't Start

```bash
kubectl describe pod -n everest-system -l app.kubernetes.io/name=huawei-elb-controller
```

Common causes:
- **Image not found** → ensure the image is imported into the cluster
- **Secret missing** → check `huawei-cloud-credentials` Secret exists in `everest-system`
- **RBAC insufficient** → check ClusterRole and ClusterRoleBinding

### ELB Creation Failed

```bash
# Check controller logs for details
kubectl logs -n everest-system deployment/huawei-elb-controller

# Check Service annotations for autocreate status
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.autocreate}'

# Check CCM logs
kubectl get pods -A | grep cloud-controller
kubectl logs -n kube-system <ccm-pod>
```

Common errors:
- `auto-detection failed: ...` → check that all nodes are in the same VPC; see controller logs for details
- `CCM failed to create ELB: ...` → check CCM logs for Huawei Cloud API error details

### Service Has No External IP

```bash
# 1. Check that autocreate was injected
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.autocreate}'

# 2. Check CCM is running
kubectl get pods -A | grep cloud-controller

# 3. Wait for ELB creation (up to 120s)
kubectl wait svc <service-name> -n everest \
  --for=jsonpath='{.status.loadBalancer.ingress[0].ip}' \
  --timeout=120s
```

### ELB Not Deleted When Service Is Deleted

```bash
# Check that the Service has the correct reclaim-policy annotation
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.kubernetes\.io/elb\.instance-reclaim-policy}'
# Should return "alwaysDelete"

# If reclaim-policy is missing or wrong, CCM will not delete the ELB.
# The controller sets reclaim-policy to "alwaysDelete" by default;
# if it's missing, the Service may have been created before the controller was running.
# Fix: delete and recreate the Service (or delete the database cluster and recreate it).
```

> **Note**: ELB deletion is handled by CCM via `reclaim-policy: alwaysDelete`. Deleting the Service triggers CCM to delete the ELB automatically. The controller does not use finalizers.

## Uninstall

### 1. Delete all LoadBalancerConfigs and Database Clusters

**Order matters.** Delete the Service or the database cluster — CCM will delete the ELB automatically.

```bash
# List all LoadBalancerConfigs
kubectl get loadbalancerconfig -A

# Delete database clusters first (removes Services, triggers ELB cleanup)
kubectl delete dbc <name> -n <namespace>

# Delete any remaining LBCs
kubectl delete loadbalancerconfig <name>
```

### 2. Uninstall the controller

**First, check how you installed:**

```bash
# If this shows a release, use Helm (Option A)
helm list -A | grep huawei-elb
# Otherwise, use Raw Manifests (Option B)
```

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

## License

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE) for details.
