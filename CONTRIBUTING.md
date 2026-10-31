# Contributing to composecd

Thanks for your interest in composecd! Bug reports, feature ideas, documentation fixes and pull requests are
all welcome.

## Issues

- **Bugs:** [open a bug report](https://github.com/trevorswanson/composecd/issues/new?template=bug_report.yml).
  The most useful reports include:
  - `composecd version`, `composetpl version` and `docker compose version`;
  - the Application and Host objects, and the smallest chart that shows the problem;
  - what you expected, and what happened instead (`composecd app get NAME -o yaml` helps).
- **Ideas:** [open a feature request](https://github.com/trevorswanson/composecd/issues/new?template=feature_request.yml).
  If Argo CD already does it, say how; composecd follows Argo CD wherever compose allows.
  [docs/PLAN.md](docs/PLAN.md) lists what is already planned.

Please don't report security vulnerabilities in public issues. Use GitHub's
[private vulnerability reporting](https://github.com/trevorswanson/composecd/security/advisories/new) instead.

## Development

You need Go (see `go.mod`) and git. Docker is only needed for the integration tests and to run composecd for real.

```bash
go test ./...                       # unit and end-to-end tests with a fake compose runner
go test -race ./...
golangci-lint run ./...
go test -tags integration -run Integration -v ./...   # against the local Docker engine
```

Some guidelines:
- **Unit tests don't need Docker.** The controller and CLI tests build real Git repositories in temporary
  directories, and use composetpl's fake compose runner (`pkg/compose/composetest`) for the hosts.
- **Integration tests** (`//go:build integration`, named `TestIntegration...`) run the controller against
  the local Docker engine, so they need docker with the compose plugin; they pull `busybox` and remove
  everything they create. When the `composetpl-releases` volume existed before the run, it and its
  `composetpl-releases-accessor` container are left in place, with the tests' records removed. They skip without Docker, except when `CI` is set. CI runs them on every pull request.
- **Changes that belong in composetpl** (rendering, releases, hooks, compose) go to
  [composetpl](https://github.com/trevorswanson/composetpl) first. composecd uses its public `pkg/` API.
- **Match the code around you:** its naming, comment density and structure. `gofmt` and `goimports`
  formatting; CI enforces both.
- **Errors** should say what failed and why, for someone who only sees `composecd app get` or the server log.

## Pull requests

1. Fork, branch, and keep each pull request to one change.
2. Add or update tests, and the README if users will notice the change.
3. Make sure `go test ./...` and `golangci-lint run ./...` pass.
