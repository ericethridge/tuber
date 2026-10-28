# Generic Kubernetes cluster support

Status: design validated, not yet implemented.

## Understanding summary

- **What:** let tuber run on any conformant Kubernetes cluster, self-hosted on VMs
  or managed, and annotate DoiT reports from it. The runtime already accepts any
  `cluster.provider` string; the restriction is in the Helm schema, docs, and examples.
- **Detection:** at startup tuber detects the Kubernetes version and distribution
  (k3s, RKE2, OpenShift, kubeadm, EKS, GKE, AKS) from the API server's `/version`
  `gitVersion`. No extra permissions are needed for this.
- **Cluster ID:** a stable ID comes from the `kube-system` namespace UID. It needs an
  opt-in, narrow ClusterRole (`get` on `kube-system` only). `cluster.id` overrides it.
- **Output:** three new template variables, `{{.ClusterVersion}}`, `{{.Distribution}}`,
  and `{{.ClusterID}}`. The default content template is unchanged.
- **Who:** customers who run their own clusters on VMs and want workload-change
  annotations in their DoiT reports, as GKE and EKS users have today.
- **Non-goals:** region, zone, and cloud-hint detection; node or VM identifiers in
  annotations; new DoiT API fields; changes to existing GKE and EKS behavior;
  an automatic "ClusterUpgraded" annotation (future idea).

## Assumptions

- Detection failures log a warning and never block startup; fields render empty.
- Detection runs at startup and then on an in-process timer (default hourly).
- The chart keeps its namespace-scoped, read-only default; the ClusterRole is opt-in.
- Scale is unchanged. Detection adds a few API calls at startup and one per interval.
- This ships as a backward-compatible minor release (`v0.2.0`).

## Decision log

| # | Decision | Alternatives | Why |
| :--- | :--- | :--- | :--- |
| 1 | Scope: allow any provider label, plus detect version, distribution, and cluster ID | Allow-only; add cost linkage with node/VM IDs | Detection helps labeling without cluster-wide access; cost linkage is a separate feature |
| 2 | Cluster ID is opt-in (`cluster.detectID`), via a ClusterRole limited to `get` on `kube-system`; `cluster.id` overrides | No permission, manual ID only; on by default | Keeps the namespace-scoped default; the opt-in role is small and easy to review |
| 3 | New template variables only; default template unchanged | Change the default template; a richer default for non-cloud providers | Backward compatible, no surprise changes to existing annotation text |
| 4 | Provider is a free-form lowercase string with documented values (`gcp-gke`, `aws-eks`, `azure-aks`, `self-hosted`, `other`) | Strict enum; free-form with no guidance | Supports any platform while guiding users |
| 5 | Small built-in distribution set with `unknown` fallback; users add rules via `cluster.distributions` (first match wins, user rules first) | Built-in only, extended by pull request | Customers on unusual platforms are not blocked on a release |
| 6 | Dedicated docs section and `examples/self-hosted.yaml`; Kustomize path is docs only | Docs in the existing sections only | Self-hosted setup has its own concerns (RBAC, custom rules) |
| 7 | Rerun detection on an in-process ticker (`cluster.detectInterval`, default 1h, `0` disables); keep the last good value on failure; a detected ID is permanent | Detect once at startup; a Kubernetes CronJob | Upgrades happen without pod restarts; a ticker needs no new objects |
| 8 | Approach 1: detector in the runtime with a table-driven rule list | Built-in table only; richer detection from node labels or ConfigMaps | Meets the extensibility need without cluster-wide read access |

## Design

### Components

- New package `internal/cluster` exposing `Detect(ctx, clientset, cfg) Info` with
  `Version`, `Distribution`, and `ID`.
- `Version` and `Distribution` come from `ServerVersion().GitVersion`. Distribution
  rules are ordered regexes (user rules first, then built-ins); no match gives
  `unknown`. The ID is read from the `kube-system` namespace UID when `detectID` is
  true and `cluster.id` is empty.
- `cmd/tuber/main.go` calls `Detect` once after creating the Kubernetes client, with
  a timeout of about 5 seconds, and passes the result to the watcher manager.
- `model.WorkloadEvent` gains `ClusterVersion`, `Distribution`, and `ClusterID`,
  stamped on every event next to `ClusterName` and `CloudProvider`.
- The validator's sample event gains sample values so templates using the new
  variables pass `--validate-only` and `--preview-config` offline.

### Behavior

- Startup order: load and validate config (compiling user regexes), create the
  client, run `Detect`, log one summary line, start watchers.
- Precedence. ID: `cluster.id`, else detected UID, else empty. Distribution: first
  matching user rule, else built-in, else `unknown` (empty if the version could not be
  read). Version: as reported, else empty.
- Rendering: the fields always exist, so `missingkey=error` is never triggered;
  empty values render as empty text.
- Offline modes use fixed samples: `v1.31.0`, `k3s`, `sample-cluster-id`. `--dry-run`
  runs detection normally.
- Rechecks run on the ticker behind an atomic value or lock. A failed or timed-out
  recheck keeps the previous value and logs a warning; a changed version or
  distribution is logged. A forbidden `kube-system` read logs a clear warning naming
  the missing permission and continues with an empty ID.

### Config, chart, and docs

```yaml
cluster:
  name: my-k3s-cluster
  provider: self-hosted
  id: ""
  detectID: false
  detectInterval: 1h
  distributions:
    - match: "\\+my-distro"
      name: my-distro
```

- Validation: regexes must compile; names are 1-63 characters of `a-z`, `0-9`, `-`, `.`;
  at most 20 rules; `detectInterval` is `0` or at least `1m`. Environment override:
  `TUBER_CLUSTER_ID` only.
- Chart: new values and schema entries; free-form provider pattern (lowercase,
  up to 63 characters); a ClusterRole and ClusterRoleBinding rendered only when
  `cluster.detectID` and `rbac.create` are true, allowing `get` on the namespace
  `kube-system` only, with a per-release name.
- Docs: a "Self-hosted and generic clusters" section in the root and chart READMEs
  (provider values, detection, template variables with a sample `contentTemplate`,
  the RBAC note, and the recipe for adding a distribution rule) and
  `charts/tuber/examples/self-hosted.yaml`.

### Testing

- Table-driven distribution rule tests, including user precedence and invalid regex.
- Detector tests with the fake clientset: success, forbidden, not found, timeout,
  version failure.
- Refresh tests: last good value kept, sticky ID, ID retried after an early failure,
  change logging, and a `-race` test with concurrent readers.
- Config tests: interval bounds, rule limits, env override, offline sample values.
- Formatter tests for the new variables, including empty values.
- Chart tests (update the existing "no ClusterRole" assertion): the ClusterRole
  appears only with `detectID`, is limited to `kube-system`, the default render has
  none, the schema accepts and rejects the right providers, and `helm lint --strict`
  passes on the new example under Helm 3 and 4.
- One manual run on a kind or k3s cluster.

### Risks and edge cases

- Go's regex engine is linear-time, so user rules cannot cause catastrophic backtracking.
- A custom build can match a built-in rule by accident; user rules go first so it can
  be overridden.
- Clusters restored or cloned from backups can share the same `kube-system` UID. The
  ID is stable per cluster, not guaranteed unique across clones; use `cluster.id`.
- The UID is not secret, but it appears in annotation text visible to anyone who can
  read the DoiT annotations. Detection is off by default.

### Rollout

- Ships as `v0.2.0`: backward compatible, defaults unchanged, Kustomize docs only.
- Normal release steps: chart version, `appVersion`, README examples, and the
  Kustomize base image tag, then a GitHub release.
