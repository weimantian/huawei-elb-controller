# Development Guide

This document is for developers who want to build, modify, or contribute to `huawei-elb-controller`.

For user-facing installation and usage, see [README.md](README.md).

> **Note**: OpenEverest (formerly Percona Everest) is the database platform this controller integrates with. The `everest.percona.com/v1alpha1` API group is unchanged. Source code: [openeverest/everest-operator](https://github.com/openeverest/everest-operator).

---

## Table of Contents

- [Build from Source](#build-from-source)
- [Project Structure](#project-structure)
- [Architecture](#architecture)
- [Reconciliation Loop](#reconciliation-loop)
- [CRD Reference](#crd-reference)
- [End-to-End Data Flow](#end-to-end-data-flow)
- [Timing Protection](#timing-protection)
- [Error Handling Strategy](#error-handling-strategy)
- [Multi-Region Support](#multi-region-support)
- [Testing](#testing)
- [Contributing](#contributing)

---

## Build from Source

### Prerequisites

- **Go 1.26+**
- **Docker** (for container builds)
- **kubectl** + a Kubernetes cluster (for deployment)
- **Helm 3** (for chart deployment)

### Build

```bash
# Download dependencies
go mod tidy

# Build all packages (type-check + compile)
go build ./...

# Lint
go vet ./...

# Build binary for current platform
go build -o huawei-elb-controller ./cmd/

# Cross-compile for Linux/amd64 (for container image)
GOOS=linux GOARCH=amd64 go build -o huawei-elb-controller ./cmd/
```

### Run Locally

The controller can run outside the cluster (requires kubeconfig and Huawei Cloud credentials):

```bash
export HUAWEI_CLOUD_AK=<your-AK>
export HUAWEI_CLOUD_SK=<your-SK>
export HUAWEI_CLOUD_PROJECT_ID=<your-ProjectID>
export HUAWEI_CLOUD_REGION=cn-north-4

go run ./cmd/
```

### Build Container Image

```bash
# Build for linux/amd64
docker buildx build --platform linux/amd64 -t huawei-elb-controller:latest .

# For CCE clusters: push to SWR (Huawei Cloud SWR)
docker tag huawei-elb-controller:latest <swr-endpoint>/<namespace>/huawei-elb-controller:latest
docker push <swr-endpoint>/<namespace>/huawei-elb-controller:latest

# For self-managed clusters: save and import via containerd
docker save huawei-elb-controller:latest | gzip > huawei-elb-controller.tar.gz
# On node: ctr -n k8s.io images import huawei-elb-controller.tar.gz
```

### VPC/Subnet Lookup Tool

A utility for finding the correct VPC and Neutron subnet IDs:

```bash
export HUAWEI_CLOUD_AK=<your-AK>
export HUAWEI_CLOUD_SK=<your-SK>
export HUAWEI_CLOUD_PROJECT_ID=<your-ProjectID>
export HUAWEI_CLOUD_REGION=cn-north-4

go run ./cmd/list-vpcs/
```

---

## Project Structure

```
huawei-elb-controller/
├── cmd/
│   ├── main.go                          # Controller entry point
│   └── list-vpcs/
│       └── main.go                      # VPC/subnet lookup utility
├── internal/
│   ├── controller/
│   │   └── loadbalancerconfig_controller.go  # Core reconcile logic
│   └── huaweicloud/
│       ├── client.go                    # Huawei Cloud client builder
│       └── elb.go                       # ELB CRUD operations
├── deploy/                              # Raw Kubernetes manifests
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
├── examples/                            # Example LoadBalancerConfig YAMLs
│   ├── internal-elb.yaml
│   └── public-elb.yaml
├── Dockerfile
├── Makefile
├── go.mod
└── go.sum
```

---

## Architecture

### Component Overview

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

### Key Design Decisions

1. **`unstructured.Unstructured` for CR access** — The controller interacts with `LoadBalancerConfig` CRs via `unstructured.Unstructured` rather than generated typed clients. This avoids importing OpenEverest Go types and keeps the controller decoupled from OpenEverest's API evolution.

2. **Annotations as configuration channel** — ELB creation parameters are passed via `spec.annotations` (`huawei-elb.io/*`), and the ELB ID is also written back to `spec.annotations["kubernetes.io/elb.id"]`. This design means:
   - OpenEverest operator reads `spec.annotations` and copies them to the Service (existing OpenEverest behavior)
   - CCM reads `kubernetes.io/elb.id` from the Service and binds the ELB (existing CCM behavior)
   - The controller doesn't need to create or manage Services directly

3. **Finalizer-based cleanup** — A finalizer (`huawei-elb.io/finalizer`) ensures the Huawei Cloud ELB is deleted before the CR is removed from the cluster, preventing orphaned cloud resources.

4. **Annotation-based filtering** — CRs with `huawei-elb.io/vpc-id` in `spec.annotations` (user-specified) or `metadata.annotations` (auto-detected) are processed. CRs without this annotation trigger auto-detection from cluster nodes. CRs using `kubernetes.io/elb.autocreate` are skipped (managed by CCE CCM directly). This allows coexistence with other ELB management solutions and enables users to create LoadBalancerConfig via the OpenEverest UI, which exposes `spec.annotations` as editable fields.


---

## CRD Reference

The controller interacts with two OpenEverest CRDs. The field references below are from the [everest-operator source](https://github.com/openeverest/everest-operator/blob/main/api/everest/v1alpha1/databasecluster_types.go).

### LoadBalancerConfig

```yaml
apiVersion: everest.percona.com/v1alpha1
kind: LoadBalancerConfig
metadata:
  name: <config-name>
  annotations:
    # Controller writes status here (metadata.annotations):
    huawei-elb.io/ready: "true"
    huawei-elb.io/elb-status: "ACTIVE"
    huawei-elb.io/error: ""
    spec:
  annotations:
    # User sets these (huawei-elb.io/*):
    huawei-elb.io/vpc-id: "..."
    huawei-elb.io/subnet-id: "..."
    huawei-elb.io/availability-zones: "..."
    huawei-elb.io/public: "false"
    # Controller writes ELB ID here:
    kubernetes.io/elb.id: "<elb-uuid>"
```

### DatabaseCluster — `spec.proxy.expose`

From the [`Expose` struct](https://github.com/openeverest/everest-operator/blob/b296204ed61cbf540d3984c4b62451a1c572878a/api/everest/v1alpha1/databasecluster_types.go#L225-L242):

```go
type Expose struct {
    // Type: ClusterIP | LoadBalancer | NodePort
    // (legacy values "internal" and "external" are deprecated)
    Type ExposeType `json:"type,omitempty"`

    // IPSourceRanges: optional IP whitelist (CIDR notation)
    IPSourceRanges []IPSourceRange `json:"ipSourceRanges,omitempty"`

    // LoadBalancerConfigName: references a LoadBalancerConfig CR
    // ⚠️ Once set, cannot be cleared (XValidation rule)
    LoadBalancerConfigName string `json:"loadBalancerConfigName,omitempty"`
}
```

### Supported Engine & Proxy Types

| `spec.engine.type` | Engine | `spec.proxy.type` |
|---|---|---|
| `postgresql` | PostgreSQL | `pgbouncer` |
| `pxc` | MySQL (Percona XtraDB Cluster) | `haproxy` |
| `psmdb` | MongoDB | `mongos` |

> `spec.engine.type` is immutable after creation. `spec.proxy.expose.loadBalancerConfigName` cannot be cleared once set.
---

## Reconciliation Loop

The controller's reconcile loop follows this logic:

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

### Requeue Intervals

| State | Requeue | Reason |
|---|---|---|
| ELB ACTIVE and healthy | 5min | Periodic status sync |
| ELB creating/updating | 30s | Fast feedback during provisioning |
| Permanent error (bad params, not found) | 5min | Don't hammer API for unfixable errors |
| Transient error (network, throttling) | 10s | Retry quickly for recoverable errors |

---

## End-to-End Data Flow

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
│ OpenEverest operator     │
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

**Step-by-step:**

1. User creates a `LoadBalancerConfig` CR with ELB parameters in `spec.annotations` (`huawei-elb.io/*`)
2. `huawei-elb-controller` detects the CR, calls Huawei Cloud ELB v3 API to create an ELB
3. Controller writes the ELB ID back to `spec.annotations["kubernetes.io/elb.id"]` and sets `ready=true`
4. User creates a `DatabaseCluster` CR referencing the `LoadBalancerConfig`
5. OpenEverest operator creates a K8s `LoadBalancer`-type Service, copying `spec.annotations` (including `elb.id`)
6. CCM reads `kubernetes.io/elb.id` from the Service, binds the pre-created ELB → Service gets an external IP

---

## Timing Protection

The controller and OpenEverest operator both modify `LoadBalancerConfig` CRs. To ensure correct ordering:

### The Problem

```
Time →  T1                    T2                    T3
        Controller creates    OpenEverest op. reads CCM binds ELB
        ELB, writes elb.id    spec.annotations      to Service
```

If the OpenEverest operator reads `spec.annotations` **before** the controller writes `elb.id`, the Service won't have the annotation, and CCM won't bind the ELB.

### The Solution: `huawei-elb.io/ready` Annotation

| State | `ready` value | Meaning |
|---|---|---|
| ELB being created | `false` | Not ready — don't create DatabaseCluster yet |
| ELB ACTIVE + ONLINE | `true` | Ready — safe to create DatabaseCluster |
| ELB being deleted | `false` | Not ready — cleanup in progress |

**Recommended workflow:**

```bash
# Create LoadBalancerConfig
kubectl apply -f examples/internal-elb.yaml

# Wait for ready=true before creating DatabaseCluster
kubectl wait loadbalancerconfig <name> \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# Only now create the DatabaseCluster
kubectl apply -f database-cluster.yaml
```

### Concurrent Update Protection

Both the controller and OpenEverest operator may update the CR simultaneously. The controller uses:

- **`retry.RetryOnConflict`**: Automatically re-fetches and retries on 409 Conflict errors
- **`updateWithRetry` helper**: All CR updates go through a callback that re-gets the latest version before applying changes

---

## Error Handling Strategy

### Error Classification

| Type | Examples | Requeue | Annotation |
|---|---|---|---|
| **Permanent** | Missing required annotations, invalid VPC ID, ELB not found | 5 minutes | `huawei-elb.io/error` set |
| **Transient** | Network timeout, API throttling, 5xx server error | 10 seconds | `huawei-elb.io/error` set |
| **Success** | ELB ACTIVE, status synced | 5 minutes | `huawei-elb.io/error` cleared |

### `errorAnnotation` Mechanism

The controller records the last error message in `metadata.annotations["huawei-elb.io/error"]`. This value is:
- Set when a reconciliation fails
- Cleared when reconciliation succeeds
- Only updated when the value changes (avoids unnecessary writes/conflicts)

---

## Multi-Region Support

The controller supports deploying ELBs in different Huawei Cloud regions:

1. **Global region** (default): Set via `HUAWEI_CLOUD_REGION` environment variable from the Secret
2. **Per-CR override**: Set `huawei-elb.io/region` annotation on a specific `LoadBalancerConfig`

When a CR specifies a region different from the global one, the controller creates a dedicated ELB client for that CR (reusing global AK/SK/ProjectID).

```go
func (r *LoadBalancerConfigReconciler) getELBClient(lbc *unstructured.Unstructured) (*elb.ElbClient, error) {
    // Check for per-CR region override
    region := getSpecAnnotation(lbc, "huawei-elb.io/region")
    if region == "" || region == r.Creds.Region {
        return r.ELBClient, nil // Use global client
    }
    // Create region-specific client
    return huaweicloud.NewELBClient(&huaweicloud.Credentials{
        AK:        r.Creds.AK,
        SK:        r.Creds.SK,
        Region:    region,
        ProjectID: r.Creds.ProjectID,
    })
}
```

---

## Testing

### Manual Testing Flow

```bash
# 1. Deploy controller
kubectl apply -f deploy/

# 2. Create LoadBalancerConfig
kubectl apply -f examples/internal-elb.yaml

# 3. Wait for ELB creation
kubectl wait loadbalancerconfig internal-elb \
  --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true \
  --timeout=120s

# 4. Verify ELB exists in Huawei Cloud (should show ACTIVE)
kubectl get loadbalancerconfig internal-elb -o jsonpath='{.metadata.annotations.huawei-elb\.io/elb-status}'

# 5. Delete CR and verify ELB is cleaned up
kubectl delete loadbalancerconfig internal-elb
# Controller logs should show "deleting ELB" → "ELB deleted successfully"
```

### Verifying ELB Cleanup

```bash
# After deleting the CR, verify the ELB is gone from Huawei Cloud
# The controller should log: "ELB deleted successfully"

# If the ELB was manually deleted in Huawei Cloud console,
# the controller detects 404 and removes the finalizer gracefully.
```

---

## Contributing

### Commit Convention

This project follows the **DCO (Developer Certificate of Origin)**. Every commit must include:

```
Signed-off-by: Your Name <your.email@example.com>
```

Use `git commit -s` to automatically add the sign-off.

### Code Style

- Run `go vet ./...` before committing
- Follow standard Go formatting (`gofmt`)
- Keep the reconcile loop readable — extract complex logic into helper functions
- All CR updates must go through `updateWithRetry` to handle conflicts

### Pull Request Checklist

- [ ] `go build ./...` passes
- [ ] `go vet ./...` passes
- [ ] `helm lint charts/huawei-elb-controller/` passes
- [ ] Commit includes `Signed-off-by`
- [ ] No secrets or credentials in code/YAML
- [ ] Documentation updated if behavior changed
