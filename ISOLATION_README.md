# GKE control plane isolation with GitHub Actions

Deploy tuber from GitHub Actions through GKE's DNS endpoint using Workload
Identity Federation. GitHub exchanges its OIDC identity for short-lived Google
credentials; no Google service account JSON key is needed.

```text
Merge PR → GitHub Actions → GitHub OIDC token
         → Google service account → GKE DNS endpoint → deploy
```

The DNS endpoint remains reachable through Google APIs. IAM controls connection
access, and Kubernetes authorization controls workload operations. This setup
disables IP-based control plane endpoints after DNS access is verified. It does
not make the DNS endpoint accessible only from your VPC.

For tuber, bootstrap the cluster resources once as an administrator using the
[deployment instructions](README.md#deploying-to-gke-gcp-and-eks-aws). Give GitHub
permission to update the existing `tuber` Deployment. The full Kustomize overlay
includes cluster-wide RBAC, which routine image deployments do not need to modify.

## Prerequisites

- An existing GKE cluster with DNS endpoint support.
- A local shell with `gcloud`, `gh`, and `kubectl`, authenticated to Google Cloud
  and GitHub. Local `kubectl` access using Google credentials also requires the
  `gke-gcloud-auth-plugin`.
- Administrator permissions to enable APIs, update cluster endpoints, create IAM
  roles and federation resources, and grant Kubernetes RBAC.
- An existing tuber installation, including its namespace, configuration,
  credentials Secret, ServiceAccount, and workload watcher RBAC.
- A built tuber image in a registry the cluster can pull from.

Run the setup commands in the same shell so the variables remain available.
Replace all `YOUR_*` placeholders before running commands.

## 1. Set the setup variables

```bash
PROJECT_ID="YOUR_PROJECT_ID"
CLUSTER="YOUR_ACTUAL_GKE_CLUSTER_NAME"
LOCATION="YOUR_CLUSTER_REGION_OR_ZONE"
GITHUB_REPO="ericethridge/tuber"

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

kubectl -n tuber-system get deployment tuber
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

## 4. Grant connection permissions and deployment RBAC

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

## 5. Configure GitHub variables and the production environment

In the repository, open **Settings → Secrets and variables → Actions → Variables**.
Add these repository variables:

| Variable | Value |
| :--- | :--- |
| `GCP_PROJECT_ID` | Your project ID |
| `GCP_DEPLOY_SERVICE_ACCOUNT` | The value of `$SA_EMAIL` |
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | Resource name printed below |
| `GKE_CLUSTER` | Actual cluster name |
| `GKE_LOCATION` | Cluster region or zone |
| `TUBER_IMAGE` | An existing image the cluster can pull, preferably `registry/tuber@sha256:...` |

Print the provider resource name:

```bash
echo "projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${POOL_ID}/providers/${PROVIDER_ID}"
```

Create a GitHub Environment named `production`. Restrict its deployment branches
to `main` and configure any required deployment approvals. Protect `main` with
your repository's PR review and merge rules.

## 6. Add the workflow on a working branch

```bash
git switch -c ci/gke-dns-wif
mkdir -p .github/workflows
```

Create `.github/workflows/deploy-gke.yml`:

```yaml
name: Deploy tuber to GKE

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

    steps:
      - uses: actions/checkout@v7

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

      - name: Connect through GKE DNS endpoint
        uses: google-github-actions/get-gke-credentials@v3
        with:
          project_id: ${{ vars.GCP_PROJECT_ID }}
          cluster_name: ${{ vars.GKE_CLUSTER }}
          location: ${{ vars.GKE_LOCATION }}
          use_dns_based_endpoint: true

      - name: Deploy existing image
        env:
          TUBER_IMAGE: ${{ vars.TUBER_IMAGE }}
        run: |
          set -euo pipefail
          : "${TUBER_IMAGE:?Set the TUBER_IMAGE repository variable}"
          kubectl -n tuber-system set image \
            deployment/tuber tuber="$TUBER_IMAGE"
          kubectl -n tuber-system rollout status \
            deployment/tuber --timeout=180s
```

This workflow deploys an already-built image. Image building and registry
publishing can be added as a separate job with separate permissions. Keep the
image reference in your Kustomize overlay aligned with the deployed release so a
later manual overlay apply does not restore an older image.

Add these patterns to `.gitignore` and `.dockerignore`:

```gitignore
gha-creds-*.json
gha-kubeconfig-*
```

Commit on the working branch, push that branch, and merge through a PR. A merge
to `main` starts the deployment; later, you can also run it manually from the
Actions tab against `main` after updating `TUBER_IMAGE`.

References: [Authentication action](https://github.com/google-github-actions/auth),
[GKE credentials action](https://github.com/google-github-actions/get-gke-credentials),
[Cloud SDK setup action](https://github.com/google-github-actions/setup-gcloud).

## 7. Disable IP endpoints after a successful deployment

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
