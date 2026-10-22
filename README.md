<p align="center">
  <img src="docs/assets/tuber-banner.png" alt="tuber — Kubernetes Monitoring Integration for DoiT Cloud Intelligence Annotations (For GKE and EKS)" width="100%" />
</p>

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

## DoiT Service Account Authentication & Permissions

Per [DoiT Getting Started — API Tokens](https://developer.doit.com/docs/start) and the [Annotations API documentation](https://developer.doit.com/docs/annotations-api), `tuber` is designed for **DoiT Service Account API tokens** (never deprecated legacy individual user API tokens):

1. **Create a DoiT Service Account API Token**:
   - Sign in to the [DoiT Console](https://console.doit.com/), click the **Gear icon** in the top navigation bar, and select **Users and access**.
   - Select **Service Accounts** from the left-hand menu.
   - Select your service account (or create one with the **Cloud Analytics Admin** role) and click **Create API token**.
2. **Startup Guardrail Against Legacy / Individual User Tokens**:
   - When `doit.validateOnStartup: true` and `doit.rejectUserTokens: true` (enabled by default in the Kustomize manifests), `tuber` validates the token against `GET https://api.doit.com/auth/v1/validate` at startup.
   - Because DoiT Service Accounts have no user email (`email: ""`), any legacy or personal token that resolves to an individual user's email address is rejected immediately with a clear error message.
3. **Service Account Report Visibility**:
   - Because service accounts have no user email, any DoiT Cloud Analytics reports referenced in `defaultReports`, rule `reports`, or `tuber.doit.com/reports` must be **shared organization-wide** or be **preset reports** (private user-only reports are unreachable by service account tokens and return `403 Forbidden`).
4. **Multi-Tenant Service Accounts (`X-Tenant-Id`)**:
   - Service account tokens ignore the deprecated `?customerContext` query parameter used by legacy keys. If your service account can access multiple tenants/customers, set `doit.tenantId` (or `DOIT_TENANT_ID`), which sends the `X-Tenant-Id` request header.

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

### 1. Create the DoiT Service Account Credentials Secret
Either apply `deploy/kustomize/base/secret.example.yaml` with your DoiT Service Account API token or use External Secrets Operator with **GCP Secret Manager** (`deploy/kustomize/overlays/gke/externalsecret-gcp.example.yaml`) or **AWS Secrets Manager** (`deploy/kustomize/overlays/eks/externalsecret-aws.example.yaml`):

```bash
kubectl create namespace tuber-system
kubectl -n tuber-system create secret generic tuber-doit-credentials \
  --from-literal=service-account-token="YOUR_DOIT_SERVICE_ACCOUNT_TOKEN" \
  --from-literal=tenant-id=""
```

### 2. Customize the `ConfigMap` Before Deploying

Choose the overlay file matching your cloud provider (or edit `deploy/kustomize/base/configmap.yaml` for shared defaults):
- **GKE (GCP):** `deploy/kustomize/overlays/gke/patch-config.yaml`
- **EKS (AWS):** `deploy/kustomize/overlays/eks/patch-config.yaml`

Validate your changes locally before applying:

```bash
./bin/tuber --config=deploy/kustomize/overlays/gke/patch-config.yaml --validate-only
./bin/tuber --config=deploy/kustomize/overlays/eks/patch-config.yaml --validate-only
```

### 3. Deploy the GKE or EKS Overlay

```bash
# Deploy on Google Kubernetes Engine (GKE)
kubectl apply -k deploy/kustomize/overlays/gke

# Deploy on Amazon EKS
kubectl apply -k deploy/kustomize/overlays/eks
```

---

## Customizing & Expanding the `ConfigMap`

The `tuber-config` `ConfigMap` mounts `config.yaml` into the pod at `/etc/tuber/config.yaml`. Below is a complete guide to expanding namespaces, targeting DoiT reports and labels, customizing templates, and adding routing rules.

### 1. Adding Watched Namespaces (`watchNamespaces` + RBAC)

`tuber` enforces an explicit `watchNamespaces` allowlist (wildcards like `*` or `all` are rejected). Out of the box, `default` and `production` are watched.

Because `tuber` uses least-privilege, namespace-scoped `RoleBinding` resources in `deploy/kustomize/base/rbac.yaml`, adding a new namespace (for example, `payments` or `staging`) is a **two-step process**:

**Step A — Add the namespace to `watchNamespaces` in your overlay `ConfigMap`** (`patch-config.yaml`):
```yaml
    watchNamespaces:
      - default
      - production
      - payments
      - staging
```

**Step B — Grant read-only watch permissions in the new namespace** (`deploy/kustomize/base/rbac.yaml` or an overlay patch):
```yaml
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: tuber-workload-watcher
  namespace: payments # <-- Must match the newly added namespace
  labels:
    app.kubernetes.io/name: tuber
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: tuber-workload-watcher
subjects:
  - kind: ServiceAccount
    name: tuber
    namespace: tuber-system
```

> **Tip (Watching Many Namespaces):** If you watch a large or frequently changing list of namespaces in `watchNamespaces` and prefer not to manage one `RoleBinding` per namespace, you can replace the per-namespace `RoleBinding` entries in `deploy/kustomize/base/rbac.yaml` with a single `ClusterRoleBinding` referencing `ClusterRole/tuber-workload-watcher`. `tuber` will still only instantiate informers for the explicit `watchNamespaces` list in `config.yaml`.

---

### 2. Targeting DoiT Reports & Labels (`doit.defaultReports` & `doit.defaultLabels`)

You can attach annotations globally to specific DoiT Cloud Analytics reports and labels, per rule, or per Kubernetes workload:

* **Finding a Report ID:** Open the report in the DoiT Console (`https://console.doit.com`) and copy the alphanumeric ID from the URL (`/reports/<REPORT_ID>`). Remember that for Service Account tokens, reports must be **shared organization-wide** or be **preset reports**.
* **Finding a Label ID:** Labels must already exist in DoiT Console. You can retrieve label IDs from the DoiT Console or via `GET https://api.doit.com/analytics/v1/labels`.

```yaml
    doit:
      baseURL: https://api.doit.com
      serviceAccountTokenFile: /var/run/secrets/doit/service-account-token
      validateOnStartup: true
      rejectUserTokens: true
      defaultReports:
        - HmqMNr1uIMQQySpiBWyt
      defaultLabels:
        - utHqK0OmwbPy1au3Xy22
```

When an event occurs, `tuber` merges `defaultReports` + matched `rule.reports` + workload annotation `tuber.doit.com/reports` (deduplicated, up to 20 IDs).

---

### 3. Customizing Annotation Text (`doit.contentTemplate`)

`doit.contentTemplate` uses Go's `text/template` syntax and is validated at startup.

**Default template:**
```yaml
contentTemplate: "[{{.CloudProvider}}/{{.ClusterName}}] {{.Namespace}}/{{.Kind}}/{{.Name}} {{.Action}}: {{.Summary}}"
```

| Template Variable | Example Value | Description |
| :--- | :--- | :--- |
| `{{.CloudProvider}}` | `gcp-gke` / `aws-eks` | Value of `cluster.provider` |
| `{{.ClusterName}}` | `gke-prod-us-central1` | Value of `cluster.name` |
| `{{.Region}}` | `us-central1` | Value of `cluster.region` |
| `{{.Namespace}}` | `production` | Kubernetes resource namespace |
| `{{.Kind}}` | `Deployment` / `Job` / `Pod` | Kubernetes resource kind |
| `{{.APIVersion}}` | `apps/v1` / `batch/v1` / `v1` | Kubernetes resource `apiVersion` |
| `{{.Name}}` | `checkout-api` | Kubernetes resource name |
| `{{.UID}}` | `f47ac10b-58cc-...` | Kubernetes resource UID |
| `{{.Action}}` | `Created`, `Scaled`, `RolledOut`, `Deleted`, `Completed`, `Failed`, `OOMKilled` | Lifecycle transition action |
| `{{.Summary}}` | `replicas 2 -> 5 (images=api=app:v2)` | Human-readable change summary |

---

### 4. Expanding Rules for Multi-Team / Multi-Namespace Routing

You can expand the `rules:` array in `config.yaml` to filter by `namespaces`, Kubernetes `labelSelector`, and `actions`, and route matching events to team-specific DoiT reports and labels. (`tuber` evaluates `rules` in order and uses the first matching rule for a given `(apiVersion, kind, namespace, action, labels)`):

```yaml
    rules:
      # Route checkout team Deployments in production to the Checkout Cost Report
      - name: checkout-deployments
        apiVersion: apps/v1
        kind: Deployment
        namespaces:
          - production
        labelSelector: "team=checkout"
        actions:
          - Created
          - Scaled
          - RolledOut
          - Deleted
        reports:
          - 2fhAJPvPz950syFUh0Zw
        labels:
          - D8Ko0n1ze0Z185HlRfOQ

      # Fallback rule for all other Deployments in watched namespaces
      - name: default-deployments
        apiVersion: apps/v1
        kind: Deployment
        actions:
          - Created
          - Scaled
          - RolledOut
          - Deleted

      # Track batch Job completions and failures
      - name: jobs-lifecycle
        apiVersion: batch/v1
        kind: Job
        actions:
          - Created
          - Completed
          - Failed

      # Track standalone Pods and OOMKilled containers across all watched Pods
      - name: pods-standalone-and-oom
        apiVersion: v1
        kind: Pod
        actions:
          - Created
          - Deleted
          - OOMKilled
```

---

### 5. Environment Variable Overrides

Any setting in `config.yaml` can be overridden via environment variables on the `tuber` container:

| Environment Variable | Overrides `config.yaml` Field | Description |
| :--- | :--- | :--- |
| `TUBER_CONFIG` | `--config` flag | Path to configuration file (default `/etc/tuber/config.yaml`) |
| `TUBER_CLUSTER_NAME` | `cluster.name` | Cluster identifier shown in annotations |
| `TUBER_CLOUD_PROVIDER` | `cluster.provider` | Cloud provider (`gcp-gke`, `aws-eks`, etc.) |
| `TUBER_CLUSTER_REGION` | `cluster.region` | Cloud region (`us-central1`, `us-east-1`, etc.) |
| `TUBER_WATCH_NAMESPACES` | `watchNamespaces` | Comma-separated namespace allowlist (e.g. `default,production,payments`) |
| `DOIT_BASE_URL` | `doit.baseURL` | DoiT API base URL (default `https://api.doit.com`) |
| `DOIT_SERVICE_ACCOUNT_TOKEN` | `doit.serviceAccountToken` | DoiT Service Account Bearer token |
| `DOIT_SERVICE_ACCOUNT_TOKEN_FILE` | `doit.serviceAccountTokenFile` | Path to mounted DoiT Service Account token file |
| `DOIT_TENANT_ID` | `doit.tenantId` | Customer tenant ID sent via `X-Tenant-Id` header |
| `DOIT_VALIDATE_ON_STARTUP` | `doit.validateOnStartup` | Set `true` or `false` to toggle `/auth/v1/validate` startup check |
| `TUBER_DRY_RUN` | `dryRun` | Set `true` to log annotation payloads without sending HTTP POSTs |

---

### 6. Applying `ConfigMap` Updates to a Running Cluster

After editing your `ConfigMap` (and `rbac.yaml` if adding namespaces):

```bash
# 1. Validate the updated ConfigMap locally
./bin/tuber --config=deploy/kustomize/overlays/gke/patch-config.yaml --validate-only

# 2. Apply the updated Kustomize overlay
kubectl apply -k deploy/kustomize/overlays/gke

# 3. Restart the tuber Deployment to pick up the updated ConfigMap
kubectl -n tuber-system rollout restart deployment/tuber
kubectl -n tuber-system rollout status deployment/tuber
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
