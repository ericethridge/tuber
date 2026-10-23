# CLI usability improvements

## Accepted scope and assumptions

Improve onboarding, configuration previews, startup diagnostics, and documentation
readability for cluster operators. Preserve workload evaluation and deployment
settings. Previews run locally with no Kubernetes or DoiT calls, and do not require
credentials. No new dependencies or ongoing operational services are needed.
Monitoring throughput and availability requirements remain unchanged. Maintain the
feature alongside the existing Go CLI and unit tests.

## Decisions

- Use a focused CLI and documentation update. Documentation alone would leave
  configuration difficult to inspect; a separate diagnostics subsystem would add
  unnecessary maintenance. The user accepted the scope and approach.
- Add `--preview-config` with a separate loader that skips credential files.
  Use explicit output fields to exclude credentials and authentication metadata.
- Reuse the runtime defaults, environment overrides, validation, and formatter.
  Label sample text as synthetic; it does not verify routing or live access.
- Send logs to stderr so preview stdout contains valid YAML. Existing JSON log
  messages remain structured for container logging.
- Add targeted startup hints and a troubleshooting guide. Keep runtime health and
  event processing behavior intact.

## Verification

Passed `go test ./...`, `go vet ./...`, CLI build, formatting checks, and
`git diff --check`. Tests cover environment overrides, defaults, credential
exclusion, inaccessible credential files, malformed configuration, template errors,
and output write failures. Built CLI checks passed for both shipped overlays with
an invalid kubeconfig path, confirming offline preview and clean YAML stdout.
Invalid configuration and Kubernetes client initialization failures returned exit
code 1 with actionable JSON diagnostics on stderr.

Live Kubernetes access and DoiT delivery were not exercised; this change preserves
those runtime paths and the existing tests pass. No concurrency behavior changed.
