# huawei-elb-controller

**English** | [中文](README-zh.md)

---

## Overview

`huawei-elb-controller` is a Kubernetes controller that automatically creates and manages **Huawei Cloud ELB** (Elastic Load Balancer) instances for [OpenEverest](https://openeverest.io/documentation/current/) database clusters. It watches `LoadBalancer` Services created by OpenEverest, auto-detects VPC/subnet/AZ, and **directly calls the Huawei Cloud ELB API** to create the full ELB resource stack (ELB + Listener + Pool + Members + HealthCheck) -- without relying on CCM autocreate. This permanently eliminates `kubernetes.io/elb.*` annotation conflicts between the PSMDB operator and the CCE webhook.

**Two usage modes**:

- **Auto mode**: Create DBC without LBC -> ELB auto-created (aligns with EKS/GKE experience)
- **Manual mode**: Create LBC as parameter template -> DBC references it -> custom ELB parameters

---

## Features

- **Direct API management** -- Controller creates/updates/deletes ELB and sub-resources via Huawei Cloud ELB v3 API, no CCM dependency
- **Zero-config auto mode** - no LBC needed; just create a database and the controller auto-creates an ELB (like EKS/GKE)
- **Manual mode with LBC parameter template** - LBC stores ELB parameters (bandwidth, EIP type, etc.); each Service gets its own independent ELB with zero port conflicts
- **Auto-detection of VPC/subnet/AZ** - automatically detects network topology from cluster nodes
- **Node-aware** - watches node changes and syncs ELB backend members (NodePort mode)
- **ACL auto-handling** - `loadBalancerSourceRanges` -> IP groups created and bound to all ELB listeners
- **Hot parameter updates** - modifying LBC or Service annotations triggers live ELB parameter updates via API
- **Finalizer cleanup** - Service deletion triggers automatic cleanup of ELB, IP groups, and EIP, preventing orphaned resources
- **No annotation conflicts** - writes zero `kubernetes.io/elb.*` annotations, permanently resolving PSMDB operator conflicts
- **Duplicate ELB prevention** - when OpenEverest overwrites the `elb-id` annotation, the controller recovers the association via name-based reverse lookup, avoiding duplicate orphan ELBs

---

## How It Works

### Auto mode

```
Create DBC (LBC = "No configuration")
  -> OpenEverest creates LoadBalancer Service
  -> Service Reconciler auto-detects VPC/subnet/AZ
  -> Directly calls ELB API: create ELB + Listener + Pool + Members + HealthCheck
  -> Writes huawei-elb.io/elb-id annotation + finalizer + updates status
  -> Service gets EXTERNAL-IP ✅
```

### Manual mode (LBC parameter template)

```
Create LBC with huawei-elb.io/* annotations
  -> Create DBC referencing the LBC
  -> OpenEverest syncs annotations to Service
  -> Service Reconciler reads params, detects VPC/subnet/AZ
  -> Directly calls ELB API: create independent ELB ✅
```

### Parameter updates

```
For manual mode:
User modifies LBC annotations (e.g., bandwidth 10M -> 20M)
  -> OpenEverest syncs to Service
  -> Service Reconciler detects change
  -> Calls Huawei Cloud ELB API to update bandwidth ✅

For auto mode:
  Annotate the Service directly
  -> Service Reconciler detects change
  -> Calls Huawei Cloud ELB API to update ✅
```

---

## Prerequisites

### 1. Kubernetes Cluster

A running Kubernetes cluster with:
- **Huawei Cloud CCE** (or self-managed cluster on Huawei Cloud ECS)
- **StorageClass** configured (for database persistent volumes)

> The controller does **not depend on CCM** for ELB creation -- it calls the Huawei Cloud API directly. CCE's pre-installed CCM does not interfere.

OpenEverest is certified on the following platforms:

| Platform | Kubernetes Version |
|---|---|
| Google GKE | 1.31 – 1.33 |
| Amazon EKS | 1.31 – 1.33 |
| OpenShift | 4.16 – 4.18 |

### 2. OpenEverest

OpenEverest must be installed and running in the cluster. The `everest.percona.com/v1alpha1` API group is used.

For installation and the admin password retrieval, see the [OpenEverest documentation](https://openeverest.io/documentation/current/). Quick check:

```bash
kubectl get pods -n everest-system
# Expected: everest-operator and everest-server pods Running

kubectl get dbengine -n everest
# Expected: percona-postgresql-operator, percona-psmdb-operator, percona-pxc-operator
```

### 3. Huawei Cloud Account

- An active Huawei Cloud account with **ELB service enabled**
- **AK** (Access Key) and **SK** (Secret Key) - create at: IAM -> My Credentials -> Access Keys
- **Project ID** - found in the console top-right dropdown under your username

> ⚠️ **Important**: Must use **permanent** AK/SK (main account or IAM user with sufficient permissions). **Temporary AK/SK** (STS tokens) are not supported. Required permissions: ELB Administrator, EIP Administrator, VPC ReadOnly, ECS ReadOnly.

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

namespace: everest-system
EOF

# 4. Install via Helm
helm install huawei-elb-controller \
  ./charts/huawei-elb-controller \
  -f my-values.yaml \
  -n everest-system
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

2. Build and push the container image to SWR (same as Option A).

Then update the image in `deploy/deployment.yaml` (line 21): `image: <swr-registry>/huawei-elb-controller:latest`

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

### Redeploy After Code Changes

**Helm**:
```bash
git pull
docker buildx build --platform linux/amd64 --provenance=false -t huawei-elb-controller:latest .
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest
helm upgrade huawei-elb-controller ./charts/huawei-elb-controller -f my-values.yaml -n everest-system
```

**Raw Manifests**:
```bash
git pull
docker buildx build --platform linux/amd64 --provenance=false -t huawei-elb-controller:latest .
docker tag huawei-elb-controller:latest <swr-registry>/huawei-elb-controller:latest
docker push <swr-registry>/huawei-elb-controller:latest
kubectl rollout restart deploy huawei-elb-controller -n everest-system
```

### Step 3: Create a Database (Auto Mode)

The easiest path: create a database cluster without a LoadBalancerConfig.

In the OpenEverest UI, select **"No configuration"** in the Load Balancer Configuration dropdown, or omit `loadBalancerConfigName` in the CR:

```yaml
spec:
  proxy:
    expose:
      type: LoadBalancer
      # loadBalancerConfigName omitted -> auto mode
```

The controller will:
1. Detect the new LoadBalancer Service created by OpenEverest
2. Auto-detect VPC, subnet, and availability zones from cluster nodes
3. Call the Huawei Cloud ELB API to create ELB + Listener + Pool + Members + HealthCheck
4. Write the `huawei-elb.io/elb-id` annotation and cleanup finalizer
5. Update the Service status with the ELB IP

> **Default parameters**: public ELB, 10 Mbit/s bandwidth, traffic billing, 5_bgp EIP, TCP health check (10s/10s/3 retries).

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

### (Optional) Manual Mode: Using LBC Parameter Template

To customize ELB parameters (bandwidth, billing mode, etc.), create an LBC as a parameter template:

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

Then reference the LBC when creating the DBC (`loadBalancerConfigName: my-elb-config`). The Service Reconciler reads the LBC parameters and calls the API to create an independent ELB.

> **Important**: LBCs are parameter templates, not instance references. Multiple DBCs referencing the same LBC each get their own independent ELB -- zero port conflicts, fully aligned with EKS/GKE behavior.

---

## Configuration Reference

### Auto Mode Defaults

When no LBC is used (auto mode), the Service Reconciler applies these defaults:

| Parameter | Default | Description |
|---|---|---|
| ELB type | `public` | Public ELB with EIP |
| Bandwidth | `10` Mbit/s | EIP bandwidth |
| Billing mode | `traffic` | Pay-per-traffic |
| EIP type | `5_bgp` | BGP multi-line |
| ELB name | `k8s-{ns_8}-{name_8}-{uid_10}` | EKS/GKE-aligned naming, truncated to 64 chars |
| Health check | TCP, 10s/10s/3 retries | Aligned with EKS NLB defaults |
| Backend mode | NodePort (node IP + NodePort) | Aligned with GKE default |
| VPC ID | Auto-detected | From node ECS metadata |
| Subnet ID | Auto-detected | From node labels |
| Availability zones | Auto-detected | From node zone labels |

### Manual Mode LBC Annotations

Create an LBC as a parameter template with `huawei-elb.io/*` annotations:

| Annotation | Type | Options | Default | Description |
|---|---|---|---|---|
| `huawei-elb.io/public` | string | `"true"` / `"false"` | `"true"` | `"false"` for internal ELB |
| `huawei-elb.io/bandwidth-size` | int | `1` – `2000` | `10` | EIP bandwidth in Mbit/s |
| `huawei-elb.io/bandwidth-charge-mode` | string | `"traffic"` / `"bandwidth"` | `"traffic"` | Billing mode |
| `huawei-elb.io/eip-type` | string | `"5_bgp"`, `"5_sbgp"`, `"5_telcom"`, `"5_union"` | `"5_bgp"` | EIP line type (immutable after creation) |
| `huawei-elb.io/name` | string | Custom (≤64 chars) | `k8s-{ns_8}-{name_8}-{uid_10}` | ELB instance name |

> **Note**: `eip-type` is immutable after ELB creation (Huawei Cloud API restriction). To change it, delete and recreate the ELB.

### Controller-Written Annotations

| Annotation | Description |
|---|---|
| `huawei-elb.io/elb-id` | ELB instance ID -- written by controller after ELB creation |
| `huawei-elb.io/elb-cleanup` | Cleanup finalizer -- triggers ELB cleanup on Service deletion |
| `huawei-elb.io/last-known-params` | Last synced parameter snapshot (JSON) -- used for change detection |
| `huawei-elb.io/acl-id` | ACL IP group ID (if source ranges are set) |
| `huawei-elb.io/acl-status` | `"on"` / `"off"` |
| `huawei-elb.io/acl-type` | `"white"` (allow-list mode) |
| `huawei-elb.io/acl-cleanup` | ACL cleanup finalizer |

### Helm Values

| Parameter | Default | Description |
|---|---|---|
| `image.repository` | `huawei-elb-controller` | Image repository |
| `image.tag` | `latest` | Image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `credentials.ak` | `""` | Huawei Cloud AK |
| `credentials.sk` | `""` | Huawei Cloud SK |
| `credentials.projectId` | `""` | Huawei Cloud Project ID |
| `credentials.region` | `""` | Huawei Cloud region (required, e.g. `cn-north-4`) |
| `existingSecret` | `""` | Use an existing Secret (overrides credentials) |
| `namespace` | `everest-system` | Deployment namespace |
| `resources.requests.cpu` | `100m` | CPU request |
| `resources.requests.memory` | `128Mi` | Memory request |
| `resources.limits.cpu` | `500m` | CPU limit |
| `resources.limits.memory` | `256Mi` | Memory limit |

> ⚠️ **Important**: `credentials.region` must match your CCE cluster's region (e.g. `cn-north-4`, `sa-brazil-1`). The default value is empty - deployment will fail if you don't set it.

---

## Troubleshooting

### Controller Pod Won't Start

```bash
kubectl describe pod -n everest-system -l app.kubernetes.io/name=huawei-elb-controller
```

Common causes:
- **Image not found** -> ensure the image is imported into the cluster
- **Secret missing** -> check credentials Secret exists in `everest-system`
- **RBAC insufficient** -> check ClusterRole and ClusterRoleBinding

### ELB Creation Failed

```bash
# Check controller logs for details
kubectl logs -n everest-system deployment/huawei-elb-controller
```

Common errors:
- `network detection failed: ...` -> check that all nodes are in the same VPC; see controller logs for details
- `creating ELB: ...` -> check controller logs for Huawei Cloud API error details (e.g., quota exceeded, insufficient permissions)
- `waiting for ELB active` -> ELB is being provisioned, controller will retry automatically

### Service Has No External IP

```bash
# 1. Check that the ELB ID annotation is set
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-id}'

# 2. Check controller logs for ELB creation progress
kubectl logs -n everest-system deployment/huawei-elb-controller

# 3. Check Service events
kubectl describe svc <service-name> -n everest
```

### ELB Not Deleted When Service Is Deleted

The controller uses a finalizer mechanism to ensure ELB cleanup. If a Service is stuck in deletion:

```bash
# Check if the cleanup finalizer is present
kubectl get svc <service-name> -n everest -o jsonpath='{.metadata.finalizers}'

# Check controller logs for deletion progress
kubectl logs -n everest-system deployment/huawei-elb-controller
```

If the controller has been uninstalled and the finalizer cannot be cleaned up, manually delete the ELB and remove the finalizer:
```bash
# 1. Manually delete the ELB in the Huawei Cloud console
# 2. Remove the finalizer
kubectl patch svc <service-name> -n everest --type=merge -p '{"metadata":{"finalizers":[]}}'
```

### Helm Release Conflict on Reinstall

If `helm install` fails with `cannot reuse a name that is still in use` even after `helm uninstall`:

```bash
# Check for leftover Helm secret
kubectl get secret -n everest-system | grep sh.helm.release

# Delete the stale secret
kubectl delete secret -n everest-system sh.helm.release.v1.huawei-elb-controller.v1
```

---

## Uninstall

### 1. Delete all Database Clusters First

**Order matters.** Delete the database cluster (DBC) first -- the Service will be deleted, and the controller will automatically delete the Huawei Cloud ELB via its finalizer. If you uninstall the controller first, the ELB becomes an orphaned resource and **continues to incur charges**.

```bash
# List all database clusters
kubectl get dbc -A

# Delete each one (Service deletion triggers controller ELB cleanup)
kubectl delete dbc <name> -n <namespace>

# Delete any remaining LBCs
kubectl get loadbalancerconfig -A
kubectl delete loadbalancerconfig <name>
```

### 2. Uninstall the Controller

**First, check how you installed:**

```bash
# If this shows a release, use Helm (Option A)
helm list -n everest-system | grep huawei-elb
# Otherwise, use Raw Manifests (Option B)
```

#### Option A: Helm

```bash
helm uninstall huawei-elb-controller -n everest-system

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

On Amazon EKS and Google GKE, creating a `type: LoadBalancer` Service automatically creates a cloud load balancer -- no extra controller needed. This controller provides the same experience for Huawei Cloud CCE:

| Feature | EKS / GKE | CCE + This Controller |
|---|---|---|
| Extra controller deployment | Not needed | Needed |
| User configures VPC/subnet/AZ | No | **No (auto-detected)** |
| Configuration complexity | Zero | **Zero** |
| LBC role | Parameter template | **Parameter template** ✅ |
| Multiple Services per LBC | Independent LBs | **Independent ELBs** ✅ |
| ELB creation method | Cloud CCM | **Controller direct API** |
| ELB parameter updates | ✅ (CCM API) | ✅ (Controller API) |
| ELB lifecycle management | CCM | **Controller (finalizer)** |
| Backend member sync | watch nodes/endpoints | **watch nodes (NodePort mode)** |
| ELB naming | `k8s-<ns>-<svc>-<uid>` | **`k8s-{ns_8}-{name_8}-{uid_10}`** ✅ |

---

## License

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE) for details.
