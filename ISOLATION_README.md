# Set up GitHub Actions deployments from a repository to GKE

The goal of this guide is to connect a GitHub repository to a GKE cluster so that
merging a PR builds the application, publishes its container image, and deploys
that exact image to the cluster. It uses this tuber repository as the working
example, Artifact Registry for images, Workload Identity Federation for Google
authentication, and GKE's DNS endpoint for Kubernetes access.

```text
Merge PR → GitHub Actions checks out the merged commit
         → Docker builds the repository's Dockerfile
         → GitHub OIDC authenticates a Google service account
         → Image is pushed to Artifact Registry
         → GKE DNS endpoint receives the Deployment update
         → GKE nodes pull the image and complete the rollout
```

The DNS endpoint remains reachable through Google APIs. IAM controls connection
access, and Kubernetes authorization controls workload operations. This setup
disables IP-based control plane endpoints after DNS access is verified. It does
not make the DNS endpoint accessible only from your VPC.

There are two phases: an administrator prepares the registry, cluster resources,
and permissions once; GitHub Actions then handles routine image deployments.
The full tuber overlay includes cluster-wide RBAC, which routine deployments do
not need to modify. This guide changes the running Deployment's image; changes
to configuration, Secrets, and cluster RBAC still use the administrator process.

You are done when a merge to `main` produces a successful Actions run and the
`tuber` Deployment runs the image digest printed by that run. Endpoint isolation
is the final step, after deployment works.

## Prerequisites

- An existing GKE cluster with DNS endpoint support.
- A local shell with `gcloud`, `gh`, and `kubectl`, authenticated to Google Cloud
  and GitHub. Local `kubectl` access using Google credentials also requires the
  `gke-gcloud-auth-plugin`.
- Administrator permissions to enable APIs, update cluster endpoints, create IAM
  roles and federation resources, and grant Kubernetes RBAC.
- A GitHub repository containing a working Dockerfile and Kubernetes manifests.
  The workflow below uses this repository's Dockerfile and the `tuber` Deployment.
- Permission to create an Artifact Registry repository and grant its writer and
  reader roles. The cluster's nodes need network access to Artifact Registry.

Run the setup commands in the same shell so the variables remain available.
Replace all `YOUR_*` placeholders before running commands.

## 1. Set the setup variables

```bash
PROJECT_ID="YOUR_PROJECT_ID"
CLUSTER="YOUR_ACTUAL_GKE_CLUSTER_NAME"
LOCATION="YOUR_CLUSTER_REGION_OR_ZONE"
GITHUB_REPO="ericethridge/tuber"
AR_LOCATION="YOUR_ARTIFACT_REGISTRY_REGION"
AR_REPOSITORY="tuber"
IMAGE_NAME="tuber"
NODE_SA_EMAIL="YOUR_GKE_NODE_SERVICE_ACCOUNT_EMAIL"

PROJECT_NUMBER="$(gcloud projects describe "$PROJECT_ID" \
  --format='value(projectNumber)')"

REPO_ID="$(gh api "repos/$GITHUB_REPO" --jq '.id')"
OWNER_ID="$(gh api "repos/$GITHUB_REPO" --jq '.owner.id')"

POOL_ID="github-tuber"
PROVIDER_ID="github"
SA_NAME="github-tuber-deploy"
SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
```

Use the actual GKE cluster name. Tuber's `cluster.name` setting is annotation
metadata and might differ. Use the cluster's region for regional clusters or its
zone for zonal clusters.
`AR_LOCATION` is the Artifact Registry region, which can differ from a zonal
cluster's `LOCATION`. `NODE_SA_EMAIL` is the Google service account used by your
GKE nodes for pulling images, not the Kubernetes ServiceAccount in the tuber pod.
If node pools use different Google service accounts, grant reader access to each
one that can run tuber.

## 2. Enable and verify DNS access

Keep existing access while testing:

```bash
gcloud container clusters update "$CLUSTER" \
  --project="$PROJECT_ID" \
  --location="$LOCATION" \
  --enable-dns-access

gcloud container clusters get-credentials "$CLUSTER" \
  --project="$PROJECT_ID" \
  --location="$LOCATION" \
  --dns-endpoint

kubectl get namespaces
```

See [GKE endpoint configuration](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/latest/network-isolation).

## 3. Create the GitHub identity federation

Enable the required APIs and create a dedicated deployment service account:

```bash
gcloud services enable \
  container.googleapis.com \
  iam.googleapis.com \
  iamcredentials.googleapis.com \
  sts.googleapis.com \
  artifactregistry.googleapis.com \
  --project="$PROJECT_ID"

gcloud iam service-accounts create "$SA_NAME" \
  --project="$PROJECT_ID" \
  --display-name="GitHub tuber deployment"

gcloud iam workload-identity-pools create "$POOL_ID" \
  --project="$PROJECT_ID" \
  --location=global \
  --display-name="GitHub tuber"
```

Create a provider restricted to your repository and its `main` branch:

```bash
gcloud iam workload-identity-pools providers create-oidc "$PROVIDER_ID" \
  --project="$PROJECT_ID" \
  --location=global \
  --workload-identity-pool="$POOL_ID" \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository_id=assertion.repository_id,attribute.repository_owner_id=assertion.repository_owner_id,attribute.ref=assertion.ref" \
  --attribute-condition="assertion.repository_owner_id == '${OWNER_ID}' && assertion.repository_id == '${REPO_ID}' && assertion.ref == 'refs/heads/main'"
```

Allow that repository identity to impersonate the deployment service account:

```bash
gcloud iam service-accounts add-iam-policy-binding "$SA_EMAIL" \
  --project="$PROJECT_ID" \
  --role="roles/iam.workloadIdentityUser" \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${POOL_ID}/attribute.repository_id/${REPO_ID}"
```

Numeric repository and owner IDs avoid trusting names that could later be reused.
Deployment runs from `main` after merging a PR; make source and workflow changes
on working branches. These creation commands are for initial setup; if the named
resources already exist, inspect and update them rather than creating duplicates.

See [Google's federation setup](https://docs.cloud.google.com/iam/docs/workload-identity-federation-with-deployment-pipelines).

## 4. Create the image registry and grant access

Create a Docker repository for the images Actions will build:

```bash
gcloud artifacts repositories create "$AR_REPOSITORY" \
  --project="$PROJECT_ID" \
  --location="$AR_LOCATION" \
  --repository-format=docker \
  --description="Container images deployed from GitHub"

gcloud artifacts repositories add-iam-policy-binding "$AR_REPOSITORY" \
  --project="$PROJECT_ID" \
  --location="$AR_LOCATION" \
  --member="serviceAccount:${SA_EMAIL}" \
  --role="roles/artifactregistry.writer"

gcloud artifacts repositories add-iam-policy-binding "$AR_REPOSITORY" \
  --project="$PROJECT_ID" \
  --location="$AR_LOCATION" \
  --member="serviceAccount:${NODE_SA_EMAIL}" \
  --role="roles/artifactregistry.reader"
```

Writer access is scoped to this repository. The GitHub identity pushes images;
the node identity pulls them. GKE node access scopes must also allow image pulls,
and private nodes need a network path to Google APIs. See
[Artifact Registry and GKE](https://docs.cloud.google.com/artifact-registry/docs/integrate-gke).

## 5. Prepare the application and grant deployment permissions

If tuber is already installed, keep its existing resources. Otherwise, follow
the [README deployment setup](README.md#deploying-to-gke-gcp-and-eks-aws) to create
the `tuber-system` namespace and DoiT credentials Secret, review the GKE overlay's
configuration and service account patch, and apply it as an administrator:

```bash
kubectl apply -k deploy/kustomize/overlays/gke
```

The Deployment must exist before Actions can update it. If its initial image
reference is unavailable, image pulls can fail until the first successful Actions
run publishes and selects the new image. Do not wait for that initial image's
rollout as a prerequisite for setting up Actions. For an installation that must
be healthy immediately, use the README's manual image build and selection steps
before applying the overlay.

Create an IAM role containing cluster discovery and DNS connection permissions:

```bash
gcloud iam roles create tuberClusterConnect \
  --project="$PROJECT_ID" \
  --title="Tuber cluster connection" \
  --permissions="container.clusters.get,container.clusters.connect" \
  --stage=GA

gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${SA_EMAIL}" \
  --role="projects/${PROJECT_ID}/roles/tuberClusterConnect"
```

This IAM grant applies at project scope. Grant workload access separately, in the
target cluster, using your administrator credentials:

```bash
kubectl -n tuber-system create role github-tuber-deploy \
  --verb=get,list,watch,patch \
  --resource=deployments.apps \
  --resource-name=tuber

kubectl -n tuber-system create rolebinding github-tuber-deploy \
  --role=github-tuber-deploy \
  --user="$SA_EMAIL"
```

This supports updating and watching the existing Deployment. Namespace creation,
Secrets, ConfigMaps, and the workload watcher's RBAC remain part of administrator
setup. The GitHub deployment identity is separate from the Kubernetes
ServiceAccount used by the running tuber pod.

See [GKE RBAC](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/role-based-access-control).

## 6. Configure GitHub variables and the production environment

In the repository, open **Settings → Secrets and variables → Actions → Variables**.
Add these repository variables:

| Variable | Value |
| :--- | :--- |
| `GCP_PROJECT_ID` | Your project ID |
| `GCP_DEPLOY_SERVICE_ACCOUNT` | The value of `$SA_EMAIL` |
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | Resource name printed below |
| `GKE_CLUSTER` | Actual cluster name |
| `GKE_LOCATION` | Cluster region or zone |
| `AR_LOCATION` | The value of `$AR_LOCATION` |
| `AR_REPOSITORY` | The value of `$AR_REPOSITORY` |
| `IMAGE_NAME` | The value of `$IMAGE_NAME` (`tuber` for this repo) |

Print the provider resource name:

```bash
echo "projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${POOL_ID}/providers/${PROVIDER_ID}"
```

Create a GitHub Environment named `production`. Restrict its deployment branches
to `main` and configure any required deployment approvals. Protect `main` with
your repository's PR review and merge rules.

## 7. Add the build and deployment workflow on a working branch

```bash
git switch -c ci/gke-dns-wif
mkdir -p .github/workflows
```

Create `.github/workflows/deploy-gke.yml`:

```yaml
name: Build and deploy tuber to GKE

on:
  push:
    branches: [main]
  workflow_dispatch:

permissions:
  contents: read
  id-token: write

concurrency:
  group: tuber-production
  cancel-in-progress: false

jobs:
  deploy:
    if: github.ref == 'refs/heads/main'
    runs-on: ubuntu-latest
    environment: production
    env:
      REGISTRY_HOST: ${{ vars.AR_LOCATION }}-docker.pkg.dev
      IMAGE_BASE: ${{ vars.AR_LOCATION }}-docker.pkg.dev/${{ vars.GCP_PROJECT_ID }}/${{ vars.AR_REPOSITORY }}/${{ vars.IMAGE_NAME }}

    steps:
      - uses: actions/checkout@v7

      - name: Build image from this commit
        run: |
          set -euo pipefail
          IMAGE="${IMAGE_BASE}:${GITHUB_SHA}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
          docker build --platform=linux/amd64 -t "$IMAGE" .
          echo "IMAGE=$IMAGE" >> "$GITHUB_ENV"

      - name: Authenticate to Google Cloud
        uses: google-github-actions/auth@v3
        with:
          project_id: ${{ vars.GCP_PROJECT_ID }}
          workload_identity_provider: ${{ vars.GCP_WORKLOAD_IDENTITY_PROVIDER }}
          service_account: ${{ vars.GCP_DEPLOY_SERVICE_ACCOUNT }}

      - name: Install Kubernetes CLI
        uses: google-github-actions/setup-gcloud@v3
        with:
          install_components: kubectl

      - name: Publish image to Artifact Registry
        run: |
          set -euo pipefail
          gcloud auth configure-docker "$REGISTRY_HOST" --quiet
          docker push "$IMAGE"
          IMAGE_DIGEST="$(docker inspect --format='{{index .RepoDigests 0}}' "$IMAGE")"
          echo "IMAGE_DIGEST=$IMAGE_DIGEST" >> "$GITHUB_ENV"

      - name: Connect through GKE DNS endpoint
        uses: google-github-actions/get-gke-credentials@v3
        with:
          project_id: ${{ vars.GCP_PROJECT_ID }}
          cluster_name: ${{ vars.GKE_CLUSTER }}
          location: ${{ vars.GKE_LOCATION }}
          use_dns_based_endpoint: true

      - name: Deploy the published image digest
        run: |
          set -euo pipefail
          : "${IMAGE_DIGEST:?Image push must produce a digest}"
          kubectl -n tuber-system set image \
            deployment/tuber tuber="$IMAGE_DIGEST"
          kubectl -n tuber-system rollout status \
            deployment/tuber --timeout=180s
          echo "Deployed $IMAGE_DIGEST from commit $GITHUB_SHA" >> "$GITHUB_STEP_SUMMARY"
```

The runner builds the checked-out commit before authenticating, then publishes
the image and deploys it by digest. Tags identify the commit and Actions attempt;
the digest identifies the exact image deployed. The example targets Linux amd64
nodes. For arm64 nodes, choose a matching runner/build configuration and change
the Docker platform; this example does not configure multi-platform builds.

Keep the image reference in your Kustomize overlay aligned with the deployed
release so a later manual overlay apply does not restore an older image. Review
and run your project's tests in CI before deployment; this example's build step
compiles tuber but does not run its Go test suite.

Add these patterns to `.gitignore` and `.dockerignore`:

```gitignore
gha-creds-*.json
gha-kubeconfig-*
```

Commit on the working branch, push that branch, and merge through a PR. A merge
to `main` starts the build and deployment. You can also run the workflow manually
from the Actions tab against `main`; it rebuilds that commit. Repository settings
must allow the actions used above and the workflow's OIDC permission.

References: [Authentication action](https://github.com/google-github-actions/auth),
[GKE credentials action](https://github.com/google-github-actions/get-gke-credentials),
[Cloud SDK setup action](https://github.com/google-github-actions/setup-gcloud).
Docker authentication is described in
[Artifact Registry authentication](https://docs.cloud.google.com/artifact-registry/docs/docker/authentication).

## 8. Verify a deployment from the repository

Open **Actions → Build and deploy tuber to GKE** and check that build, publication,
credential retrieval, and rollout all succeed. The run summary prints the deployed
image digest and source commit. Confirm the Deployment uses that digest:

```bash
kubectl -n tuber-system get deployment tuber \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="tuber")].image}'
echo
kubectl -n tuber-system rollout status deployment/tuber --timeout=180s
```

| Failure | Check |
| :--- | :--- |
| OIDC authentication denied | Numeric repository/owner IDs, provider resource name, service account impersonation binding, and running from `main` |
| Image push denied | Artifact Registry API, repository location/name, and repository writer grant for the GitHub service account |
| GKE credential retrieval or DNS connection denied | Cluster name/location, DNS access enabled, IAM `get`/`connect` permissions, and any VPC Service Controls policy |
| Deployment update forbidden | RoleBinding targets the impersonated Google service account email as a Kubernetes `User`, in the correct cluster and namespace |
| Deployment not found | Complete the administrator bootstrap; verify the Deployment is named `tuber` in `tuber-system` |
| `ImagePullBackOff` | Reader grant for the actual node service account, node access scopes, registry network access, and the published image digest |
| Rollout timeout | Pod events, DoiT Secret/configuration, application logs, and readiness; API connectivity alone does not prove application health |

For another repository, substitute its numeric identity and Docker build context,
registry image name, namespace, Deployment, and container name. Update the
Kubernetes Role's resource name to match. The sample Role permits updating the
existing named Deployment; broader manifest deployments require additional
resource-specific RBAC and should not be given cluster administrator access.

## 9. Disable IP endpoints after a successful deployment

Once the GitHub workflow and your administrator DNS access both work:

```bash
gcloud container clusters update "$CLUSTER" \
  --project="$PROJECT_ID" \
  --location="$LOCATION" \
  --no-enable-ip-access
```

This disables both external and internal IP-based control plane endpoints,
leaving DNS access enabled. External tooling using an IP endpoint must switch to
DNS. The DNS endpoint remains reachable through Google APIs and protected by IAM;
existing VPC Service Controls policies must also permit the workflow's access.

Verify another deployment and confirm local administrator access still works:

```bash
gcloud container clusters get-credentials "$CLUSTER" \
  --project="$PROJECT_ID" \
  --location="$LOCATION" \
  --dns-endpoint

kubectl -n tuber-system get deployment tuber
```

See [GKE network isolation](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/latest/network-isolation).
