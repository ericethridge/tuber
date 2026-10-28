# Install tuber with Helm

The chart installs a single workload monitor into an existing GKE or EKS cluster.
Supply cluster identity, watched namespaces, and an existing DoiT credentials
Secret. No cloud IAM annotation or registry credentials are required for public
release images. Cluster network policy must permit Kubernetes API and DoiT API
access and node image pulls.

## Release availability

The current chart version is `0.1.1` (first released as `0.1.0`). The OCI installation command below works
after maintainers publish the matching release and make both GHCR packages public.
Until then, install the chart from this checkout and override `image.repository`
and `image.tag` with an image you have already published. Do not assume that
`v0.1.1` is available just because the chart source exists.

## 1. Prepare the credentials Secret

Prerequisites: Helm 3.18+ or Helm 4, `kubectl` configured for the intended cluster,
and permission to create resources in the release namespace and Roles and
RoleBindings in each watched namespace. Watched namespaces must already exist.

Confirm the context and create the installation namespace if necessary:

```bash
kubectl config current-context
kubectl create namespace tuber-system --dry-run=client -o yaml | kubectl apply -f -
```

Create a DoiT service account token as described in the
[authentication guide](../../README.md#doit-service-account-authentication--permissions).
Put it in a local file readable only by you, then supply that file to Kubernetes:

```bash
kubectl -n tuber-system create secret generic tuber-doit-credentials \
  --from-file=service-account-token=/path/to/your/token-file
```

Replace the file path with your existing token file. If your Secret already
exists (including one managed by External Secrets), reuse it. For multi-tenant
accounts, also create a `tenant-id` key. Never put the token in chart values,
`--set`, or Git. Helm does not create, store in its release, or delete this Secret.

## 2. Create a small values file

Save this as `tuber-values.yaml` and edit the identity for your cluster:

```yaml
cluster:
  name: my-gke-cluster
  provider: gcp-gke
  region: us-central1
watchNamespaces:
  - default
credentials:
  existingSecret: tuber-doit-credentials
```

For EKS, use `provider: aws-eks` and your cluster name/region. Examples for both
providers are in [examples/gke.yaml](examples/gke.yaml) and
[examples/eks.yaml](examples/eks.yaml). No changes to ServiceAccount IAM annotations
are needed for ordinary Kubernetes watches and DoiT token authentication.

## 3. Preview and install

After release `0.1.1` is published, preview the Kubernetes resources:

```bash
helm template tuber oci://ghcr.io/ericethridge/charts/tuber \
  --version 0.1.1 --namespace tuber-system \
  --values tuber-values.yaml
```

Install and wait for readiness:

```bash
helm upgrade --install tuber oci://ghcr.io/ericethridge/charts/tuber \
  --version 0.1.1 --namespace tuber-system \
  --values tuber-values.yaml --wait --timeout 3m
```

From this repository checkout, replace the OCI reference with `./charts/tuber`
and omit `--version`. Before the first release, add your published image override:

```bash
helm upgrade --install tuber ./charts/tuber \
  --namespace tuber-system --values tuber-values.yaml \
  --set image.repository=us-central1-docker.pkg.dev/YOUR_PROJECT_ID/tuber/tuber \
  --set image.tag=v1 --wait --timeout 3m
```

That override is a sample image path; substitute your own published image. It
is only needed until the first release is published; after that, drop it and use
the release image. Private images require appropriate node access or
`imagePullSecrets`. Keep overrides in your values file for future
upgrades. If tuber is already installed with Kustomize, use the migration steps
below before installing Helm resources with conflicting names.

The chart validates required settings, namespace syntax, image references, and
supported rule shapes before rendering. It does not check cluster connectivity,
Secret contents, namespace existence, DoiT report access, label selector syntax,
or Go template execution offline. `--wait` waits for informer readiness, not proof
of a delivered annotation.

## 4. Test the installation

Wait for cache synchronization, then create a standalone Pod in a watched namespace:

```bash
kubectl -n tuber-system logs deployment/tuber --tail=100
kubectl -n default run tuber-smoke --image=nginx:stable --restart=Never
kubectl -n tuber-system logs deployment/tuber --since=2m
kubectl -n default delete pod tuber-smoke
```

Look for successful annotation creation in logs and verify the annotation in
DoiT. Initial workloads are suppressed. Set `dryRun: true` in your values file
to log payloads without calling DoiT; the Secret name is still required, but
the pod starts even if the Secret does not exist yet. Dry-run logs demonstrate formatting but not delivery.

For failures, inspect the actual events and pod state:

```bash
kubectl -n tuber-system get pods
kubectl -n tuber-system get events --sort-by=.lastTimestamp
kubectl -n tuber-system logs deployment/tuber --tail=100
helm status tuber --namespace tuber-system
```

- `ImagePullBackOff`: confirm the exact version exists and nodes can pull it.
- `FailedMount`: check the external Secret name and token key in the release namespace.
- RBAC errors: check Roles and RoleBindings in every watched namespace.
- Readiness timeout: inspect informer errors, namespace access, and DoiT startup
  authentication errors. A newly created Autopilot node may take longer than 3m;
  inspect events before increasing the timeout.

## Configuration reference

All defaults are in [values.yaml](values.yaml). Helm merges your overrides with
these defaults. Unknown values are rejected by the schema to catch misspellings.

| Value | Purpose |
| :--- | :--- |
| `cluster.name`, `cluster.provider` | Required annotation identity; provider is `gcp-gke` or `aws-eks` |
| `cluster.region` | Optional region metadata |
| `watchNamespaces` | Unique, explicit allowlist; defaults to `default`; chart creates read-only Roles and RoleBindings there for Pods, Deployments, and Jobs. This access is fixed because tuber always watches all three, whatever `rules` select |
| `credentials.existingSecret` | Required Secret in the release namespace; remains external to Helm |
| `credentials.tokenKey` | Existing Secret key; defaults to `service-account-token` |
| `credentials.tenantIdKey` | Optional tenant Secret key; defaults to `tenant-id`; empty omits the environment reference |
| `image.repository`, `image.tag` | Public versioned image; tag defaults to `v<appVersion>` |
| `image.digest` | Optional `sha256:...`; replaces the tag to avoid invalid digest/tag combinations |
| `imagePullSecrets` | List of existing registry Secret references such as `[{name: registry-pull}]` |
| `doit.defaultReports`, `doit.defaultLabels` | Optional DoiT routing IDs |
| `doit.contentTemplate` | Annotation Go template; rendered by tuber, not Helm |
| `doit.*`, `rollup.*`, `rules` | API settings, suppression/concurrency settings, and first-match rules; empty `rules` uses runtime defaults |
| `serviceAccount.create`, `serviceAccount.name` | Create a dedicated ServiceAccount, or select an existing one |
| `serviceAccount.annotations` | Optional annotations for environments needing additional cloud integration |
| `rbac.create` | Defaults true; if false, administrator must provision workload and Lease access |
| `resources` | CPU/memory requests and limits; defaults to requests 100m/128Mi and limits 500m/256Mi. Raise memory for namespaces with thousands of Pods |
| `nodeSelector`, `tolerations` | Optional scheduling requirements |

The chart runs one replica with leader election and a `Recreate` strategy. A
rolling standby cannot become ready while the existing leader holds the Lease.
Stopping the old pod before starting the new one also avoids overlapping watchers.
This introduces brief downtime; lifecycle changes during downtime are not replayed
when the new informer performs its initial list. Do not run multiple independent
releases watching the same namespaces unless duplicate annotations are intended.

## Upgrade, rotate credentials, and uninstall

Upgrade with the same values file and the explicitly selected next chart version.
The chart's default image version follows its app version. Remove development
image overrides when switching to the published release image.

```bash
helm upgrade tuber oci://ghcr.io/ericethridge/charts/tuber \
  --version 0.1.1 --namespace tuber-system \
  --values tuber-values.yaml --wait --timeout 3m
```

Replace `0.1.1` with the next published version when upgrading. ConfigMap changes
automatically change the pod template checksum and trigger a rollout. Changing
the external credentials Secret requires an explicit restart:

```bash
kubectl -n tuber-system rollout restart deployment/tuber
kubectl -n tuber-system rollout status deployment/tuber --timeout=180s
```

To remove the Helm installation:

```bash
helm uninstall tuber --namespace tuber-system
```

The external Secret, release namespace, and watched namespaces remain. Helm
removes the Roles/RoleBindings it installed, including in watched namespaces.

## Migrate an existing Kustomize installation

Helm cannot silently adopt the existing Kustomize Deployment, ConfigMap,
ServiceAccount, and RBAC. Use a distinct release name, such as `tuber-helm`, to
avoid name/ownership conflicts. Reuse the credentials Secret in `tuber-system`.

First stop the old watcher to avoid duplicate annotations:

```bash
kubectl -n tuber-system scale deployment/tuber --replicas=0
helm upgrade --install tuber-helm ./charts/tuber \
  --namespace tuber-system --values tuber-values.yaml \
  --wait --timeout 3m
```

Before the initial release, include the image overrides described above. The new
Deployment is named `tuber-helm-tuber`; use that name in log, rollout, and restart
commands. Verify annotation delivery before removing old resources. Keep the old
Deployment at zero replicas until migration is complete, and remove it from the
old deployment automation so it is not scaled back up by a later apply.

Do not run `kubectl delete -k` indiscriminately: the old overlay includes the
shared `tuber-system` namespace. Deleting it would also delete the new Helm
installation and credentials. Remove obsolete objects individually after review.
If the Helm install fails, uninstall that new release and scale the old Deployment
back to one replica to restore the previous installation.

## Maintainer release process

1. Update `Chart.yaml`'s `version` and `appVersion` together through a working
   branch and PR. Update the versioned examples in this README and the image tag in
   `deploy/kustomize/base/deployment.yaml` too; the release workflow rejects a
   mismatch.
2. Merge after CI passes. CI lints and tests the chart with Helm 3 and 4, and runs
   rendered configs through the same validator as the application.
3. Publish a stable GitHub release with the matching tag, initially `v0.1.0`,
   pointing to the reviewed commit on `main`. The workflow rejects mismatched
   versions and commits outside `main`'s history.
4. The release workflow tests the source, publishes `ghcr.io/ericethridge/tuber:v0.1.1`
   for amd64/arm64, and publishes the OCI chart at
   `oci://ghcr.io/ericethridge/charts/tuber:0.1.1`. It does not publish `latest`.
5. On the first publication, use each GHCR package's settings to make both the
   container package and chart package **public**. Workflow credentials need
   package write access; organization policy must allow public packages.
   A published version tag is immutable: the workflow refuses to overwrite an
   existing image tag, so if a later step fails, publish a new version instead of
   re-running. The workflow also requires `values.yaml` `image.repository` to equal
   `ghcr.io/<owner>/<repo>`.
6. A separate job verifies anonymous chart access and both image platforms. If
   this fails because new packages were private, make them public and rerun the
   failed verification job before announcing the release.

Release publishing and public visibility need maintainer access and are not
performed by a Helm installation. Fork maintainers must adjust the chart's image
repository and documentation to their own package locations.

References: [Helm charts](https://helm.sh/docs/topics/charts/),
[OCI registries](https://helm.sh/docs/topics/registries/),
[GitHub container publishing](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images).
