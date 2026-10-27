# Helm installation design

## Accepted scope

Provide an installation for existing GKE and EKS clusters that requires no local
Go build or image publication. Users supply cluster identity, an explicit
namespace allowlist, and an existing DoiT credentials Secret. Preserve runtime
event handling, credential separation, and the current Kustomize installation.
Keep distribution and ongoing maintenance within this repository.

## Decisions

- The user selected Helm after comparing Helm, Kustomize, and a custom installer.
  Helm supplies release ownership, upgrades, and uninstall without another CLI.
- Package the Deployment, ConfigMap, ServiceAccount, and namespace-scoped RBAC.
  The chart owns neither watched namespaces nor the credentials Secret.
- Mount credentials from an existing Secret; do not accept inline tokens in chart
  values or embed them in Helm release metadata.
- Use one replica and a Recreate strategy. Informer readiness reflects leadership,
  so a standby replica would block a rolling upgrade while the old leader runs.
  Recreate also prevents overlapping watchers. Upgrades have brief downtime and
  changes during downtime are not replayed because tuber suppresses initial lists.
- Hash the rendered ConfigMap into the pod template to restart on config changes.
  Existing Secret rotation requires an explicit rollout restart.
- Publish semver image tags for amd64/arm64 and an OCI chart through GitHub
  releases. Require matching chart/app/release versions and verify anonymous
  access. Initial GHCR package visibility is a maintainer setting.
- Validate values with JSON Schema and run rendered configuration through the
  actual tuber validator. Test namespace RBAC, Secret references, configuration
  checksum changes, and invalid input rejection offline.

## Assumptions and limits

Cluster connectivity, existing namespaces, and installer's permission to manage
Roles and RoleBindings are prerequisites. No cloud resources, registry credentials,
or service-account IAM bindings are provisioned by the chart. Maintainers own
release publication and public package visibility. Existing Kustomize installs
need a deliberate migration to avoid two agents watching the same workloads.

## Verification

Helm 3.18.4 and Helm 4.3.1 lint both provider examples and pass chart integration
tests. Tests validate rendered configuration with the application loader, scoped
RBAC, external Secret references, digest selection, config-triggered rollouts,
and rejection of incomplete or unsafe inputs. Full Go tests and vet pass. Chart
packaging, client-only install rendering with NOTES, workflow actionlint, and
shell/YAML syntax checks also pass. No live cluster installation, upgrade, or
registry publication was performed on this branch.
