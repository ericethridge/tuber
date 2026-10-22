# `tuber` — Kubernetes Workload Monitor for DoiT Cloud Analytics Annotations

`tuber` is a lightweight, in-cluster Kubernetes monitoring agent written in Go (`client-go`) for **Google Kubernetes Engine (GKE)** and **Amazon EKS** clusters. It watches workload lifecycle changes across an explicit allowlist of namespaces, applies **smart workload rollup** so replica pod churn does not flood your cost charts, and automatically creates contextual annotations in **DoiT Cloud Intelligence** via the [DoiT Annotations API](https://developer.doit.com/docs/annotations-api) (`POST https://api.doit.com/analytics/v1/annotations`).

---

## Key Features

1. **Smart Workload Rollup (Noise Control)**:
   - **Deployments (`apps/v1`)**: Emits annotations on `Created`, `Scaled` (`spec.replicas` changes), `RolledOut` (container image or resource request/limit updates), and `Deleted`. Ignores routine replica status heartbeats.
   - **Jobs (`batch/v1`)**: Emits annotations on `Created`, `Completed` (`Complete=True`), and `Failed` (`Failed=True`).
   - **Pods (`core/v1`)**: Suppresses routine `Created`/`Deleted` churn for Pods owned by a `ReplicaSet` or `Job` (`ignoreOwnedPods: true`), while capturing **standalone Pods** (`Created`, `Deleted`) and **`OOMKilled` container terminations** across all watched Pods.
2. **Cold-Start Suppression**:
   - Uses `client-go` `ResourceEventHandlerDetailedFuncs` (`isInInitialList`) so restarting or rolling out `tuber` itself never re-emits annotations for workloads already running in the cluster.
3. **Explicit Namespace Allowlist**:
   - Instantiates namespace-scoped `SharedInformer` factories strictly for the namespaces listed in `watchNamespaces` (wildcard `*` or empty lists are rejected at validation time).
4. **DoiT Annotations API Integration**:
   - Authenticates via `Authorization: Bearer <API_TOKEN>` and optional `X-Tenant-Id: <TENANT_ID>`.
   - Client-side token-bucket rate limiter (`golang.org/x/time/rate`) and exponential backoff on HTTP `429`/`5xx` to stay comfortably within DoiT's 1,000 req/min limit.
5. **Per-Workload Kubernetes Annotation Overrides**:
   - `tuber.doit.com/ignore: "true"` — Skip this workload completely.
   - `tuber.doit.com/watch: "true"` — Force watching an owned Pod's `Created`/`Deleted` lifecycle even when `ignoreOwnedPods: true`.
   - `tuber.doit.com/reports: "reportId1,reportId2"` — Attach annotations for this workload to specific DoiT Cloud Analytics report IDs.
   - `tuber.doit.com/labels: "labelId1,labelId2"` — Attach pre-existing DoiT label IDs to annotations for this workload.

---

## DoiT API Authentication & Permissions

Per the [DoiT Annotations API documentation](https://developer.doit.com/docs/annotations-api):
- **Required Permission**: Your DoiT API token (Personal API token or Service Account token) must have the **Cloud Analytics Admin** permission to create annotations.
- **Service Account Report Visibility**: Service accounts have no user email. Any DoiT Cloud Analytics reports referenced in `defaultReports`, rule `reports`, or `tuber.doit.com/reports` must be **shared organization-wide** or be **preset reports**.
- **Multi-Tenant Tokens**: If your credential can access multiple tenants/customers, set `doit.tenantId` (or `DOIT_TENANT_ID`), which sends the `X-Tenant-Id` request header.

---

## Local Build, Validation & Dry-Run Testing

```bash
# Run all unit and end-to-end fake-cluster tests with race detector
go test -v -race ./...

# Build the tuber binary
go build -o bin/tuber ./cmd/tuber

# Validate a configuration file without connecting to a cluster
./bin/tuber --config=deploy/kustomize/base/configmap.yaml --validate-only

# Run locally against your current kubeconfig in dry-run mode (logs DoiT payloads without POSTing)
./bin/tuber \
  --config=./my-config.yaml \
  --kubeconfig=$HOME/.kube/config \
  --dry-run
```

---

## Deploying to GKE (GCP) and EKS (AWS)

### 1. Create the DoiT Credentials Secret
Either apply `deploy/kustomize/base/secret.example.yaml` with your real DoiT API token or use External Secrets Operator with **GCP Secret Manager** (`deploy/kustomize/overlays/gke/externalsecret-gcp.example.yaml`) or **AWS Secrets Manager** (`deploy/kustomize/overlays/eks/externalsecret-aws.example.yaml`):

```bash
kubectl create namespace tuber-system
kubectl -n tuber-system create secret generic tuber-doit-credentials \
  --from-literal=api-token="YOUR_DOIT_API_TOKEN" \
  --from-literal=tenant-id=""
```

### 2. Deploy the GKE or EKS Overlay

```bash
# Deploy on Google Kubernetes Engine (GKE)
kubectl apply -k deploy/kustomize/overlays/gke

# Deploy on Amazon EKS
kubectl apply -k deploy/kustomize/overlays/eks
```

---

## Expanding to Arbitrary Kubernetes Primitives (Phase 2 Rule Engine)

The `rules:` list in `config.yaml` is already structured around `(apiVersion, kind, namespaces, labelSelector, actions, reports, labels)`:

```yaml
rules:
  - name: deployments-lifecycle
    apiVersion: apps/v1
    kind: Deployment
    namespaces:
      - production
    labelSelector: "cost-center=checkout"
    actions:
      - Created
      - Scaled
      - RolledOut
      - Deleted
    reports:
      - HmqMNr1uIMQQySpiBWyt
    labels:
      - utHqK0OmwbPy1au3Xy22
```

To expand from typed `Deployment`/`Job`/`Pod` informers to arbitrary Kubernetes resources (e.g., `StatefulSet`, `DaemonSet`, `HorizontalPodAutoscaler`, `Ingress`, or CRDs), add a `dynamicinformer.DynamicSharedInformerFactory` in `internal/watcher/manager.go` that maps each `WatchRule`'s `apiVersion` + `kind` via RESTMapper to a `schema.GroupVersionResource` and feeds `unstructured.Unstructured` events into the same `Evaluator` $\rightarrow$ `Deduplicator` $\rightarrow$ `Formatter` $\rightarrow$ `DoiT Sink` pipeline.
