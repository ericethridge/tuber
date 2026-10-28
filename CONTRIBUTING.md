# Contributing

Thanks for helping improve tuber. Bug reports, documentation fixes, and pull
requests are welcome.

## Reporting issues

Open a GitHub issue with the tuber version, cloud provider (GKE or EKS), relevant
log lines, and your configuration with any tokens, project IDs, and other
environment-specific values removed. Report security problems privately as
described in [SECURITY.md](SECURITY.md).

## Development

You need the Go version in [go.mod](go.mod) and, for chart changes, Helm 3 or 4.

```bash
go build -o bin/tuber ./cmd/tuber
go vet ./...
go test -race ./...
helm lint charts/tuber -f charts/tuber/examples/gke.yaml --strict
helm lint charts/tuber -f charts/tuber/examples/eks.yaml --strict
```

See the [README](README.md) for dry-run and configuration commands.

## Pull requests

- Branch from `main` and keep each pull request focused on one change.
- Add or update tests and documentation with the code change.
- Make sure the commands above pass; CI runs them on every pull request.
- Never commit tokens, personal project IDs, or other environment-specific values.
  Use obvious placeholders such as `YOUR_PROJECT_ID` in examples.
- Maintainers handle versioning and releases; see the maintainer release process
  in the [chart guide](charts/tuber/README.md#maintainer-release-process).

By contributing you agree that your contribution is licensed under the MIT License.
