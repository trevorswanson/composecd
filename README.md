# composecd

GitOps for [composetpl](https://github.com/trevorswanson/composetpl) charts. Commit your compose stacks as
charts to Git, and composecd keeps them deployed on one or more Docker hosts, like Argo CD does for Helm
charts on Kubernetes.

- **Git is the source of truth.** Hosts and Applications are YAML objects in your repository. composecd reads
  them, renders each Application's chart and compares it with what is deployed.
- **Sync status and health.** Each Application is `Synced` or `OutOfSync`, with per-service diffs and live
  drift, and `Healthy`, `Progressing`, `Degraded`, `Suspended` or `Missing` from its containers.
- **Automated or manual sync.** Automated Applications are deployed whenever Git changes. Any Application can
  be synced, rolled back or paused from the CLI or API.
- **Many hosts.** Deploy to the local engine, or to others over `ssh://` or `tcp://` with TLS.
- **Plain composetpl releases.** Every sync is a composetpl revision stored on its host, with hooks, history and
  rollback, so `composetpl list --host ssh://pi.lan` shows the same releases composecd manages.

```bash
composecd app list
composecd app diff media
composecd app sync media
composecd app rollback media 3
```

## How it works

Every refresh interval (3 minutes by default), composecd:
1. Fetches the root of your GitOps repository and reads the `Host` and `Application` objects in it.
2. Pings every host.
3. For each Application:
   1. Renders its chart at the current commit, with live `.Capabilities` and `lookup` from its host.
   2. Compares the result with the deployed composetpl release.
   3. Checks the live containers for drift and health.
   4. If the Application is automated and Git differs from what's deployed, runs
      `composetpl upgrade --install` on it.

A failed automatic sync is not retried until something changes in Git. Fix the chart, or run `app sync` by hand.

composecd stores only its own operation history and settings (SQLite, under `dataDir`). Desired state is in
Git, live state is on the hosts, and release history is in composetpl's storage on each host.

## Quick start

1. **Write `composecd.yaml`**, which points at your repository (see [examples/composecd.yaml](examples/composecd.yaml)):

   ```yaml
   root:
     repoURL: https://git.example.lan/me/homelab.git
     targetRevision: main
     path: composecd           # where the Host and Application objects live
   repositories:
     - url: https://git.example.lan/
       username: composecd
       passwordFile: /run/secrets/git-token
   ```

2. **Put the objects in that directory**, and your charts anywhere in the repository.
   [examples/gitops](examples/gitops) is a complete example:

   ```yaml
   # composecd/hosts.yaml
   apiVersion: composecd/v1alpha1
   kind: Host
   metadata: {name: nas}
   spec: {}                    # the engine composecd itself uses
   ---
   # composecd/apps.yaml
   apiVersion: composecd/v1alpha1
   kind: Application
   metadata: {name: whoami}
   spec:
     source: {path: charts/whoami}
     destination: {host: nas}
     syncPolicy: {automated: {}}
   ```

   Check the objects with `composecd validate composecd/`, for example in your repository's CI.

3. **Run composecd.** It needs the Docker socket, and a volume for its data. Use either the container image or
   the chart in [deploy/chart](deploy/chart):

   ```bash
   docker run -d --name composecd -p 8080:8080 \
     -v /var/run/docker.sock:/var/run/docker.sock \
     -v composecd:/var/lib/composecd \
     -v $PWD/composecd.yaml:/etc/composecd/composecd.yaml:ro \
     -v /opt/composecd/secrets:/run/secrets:ro \
     ghcr.io/trevorswanson/composecd
   ```

   ```bash
   composetpl install composecd deploy/chart -f my-values.yaml
   ```

4. **Get the admin token.** composecd generates it on first start:

   ```bash
   docker exec composecd cat /var/lib/composecd/admin-token
   ```

5. **Use the token** from the CLI (`--server`/`$COMPOSECD_SERVER` and `--token`/`$COMPOSECD_TOKEN`), the API, or
   the status page at `http://<host>:8080/`.

## Objects

All objects use `apiVersion: composecd/v1alpha1`. YAML documents with any other `apiVersion` in the root are
ignored, and unknown fields are errors.

### Host

```yaml
kind: Host
metadata:
  name: pi
  labels: {arch: arm64}
spec:
  url: ssh://deploy@pi.lan        # or unix:///var/run/docker.sock, tcp://gpu.lan:2376; empty = default engine
  # context: pi                   # a docker context instead of url
  # tlsCertPath: /run/secrets/gpu # ca.pem, cert.pem, key.pem for tcp:// (DOCKER_TLS_VERIFY=1)
  projectRoot: /srv/composecd     # default /opt/composecd
  storage: volume                 # composetpl release storage: volume (default, on the host) or local
  env: {TZ: America/Chicago}      # for every docker call; visible to ${VAR} and the env function
```

### Application

```yaml
kind: Application
metadata:
  name: media                     # also the composetpl release and compose project name
spec:
  source:
    # A chart in Git...
    repoURL: https://git.example.lan/me/homelab.git   # default: the root repository
    targetRevision: main          # branch, tag or commit; default: the root's revision
    path: stacks/media
    valueFiles: [values-nas.yaml, ../shared/values.yaml]   # relative to path, inside the repository
    # ...or an OCI chart:
    # chart: oci://ghcr.io/me/charts/media
    # version: ^1.2
    values:                       # applied last
      apps: {jellyfin: {enabled: true}}
    env: {PUID: "1000"}           # over the host's env
  destination:
    host: nas
  syncPolicy:
    automated: {}                 # omit for manual sync
    options:
      wait: true                  # docker compose up --wait (default true)
      timeout: 5m                 # for --wait and each hook (default 5m)
      pull: missing               # always, missing or never
      atomic: false               # roll a failed sync back to the last deployed revision
      forceRecreate: false
      historyMax: 10              # revisions composetpl keeps
```

Values work as in Argo CD's Helm source: the chart's `values.yaml`, then `valueFiles` in order, then `values`.
Each sync replaces the release's values with these (`composetpl upgrade --reset-values`). If the chart has a
`Chart.lock`, composecd vendors its dependencies first (`composetpl dependency build`).

The `env` and `expandenv` template functions see only the host's and Application's `env`, not composecd's own
environment, so a render doesn't depend on where composecd runs.

## Status

| Sync | Meaning |
| --- | --- |
| `Synced` | The deployed release matches Git, and its containers match the release. |
| `OutOfSync` | Something differs: not deployed yet, compose resources, hooks, the project directory, or live drift (a container missing, changed or orphaned). `app get` lists the reasons, and `app diff` shows the diffs. |
| `Unknown` | composecd couldn't compare: the host is down, the chart doesn't render, the revision isn't in Git, etc. See the app's error. |

| Health | Meaning |
| --- | --- |
| `Healthy` | Every container is running (and healthy, if it has a healthcheck), or exited with code 0. |
| `Progressing` | A container is starting, or created but not started. |
| `Degraded` | A container is unhealthy, restarting, dead, or exited with a non-zero code. |
| `Suspended` | A container is paused. |
| `Missing` | Not deployed, or no containers. |

Drift alone (someone ran `docker stop` or edited a container) shows as `OutOfSync`, but doesn't trigger an
automatic sync yet. `app sync` fixes it, and so does `composetpl repair NAME`.

## Operations

| Command | |
| --- | --- |
| `app list`, `app get NAME` | Status, reasons, containers and drift. `-o json\|yaml` |
| `app diff NAME` | Per-resource unified diff from the deployed release to Git. `--exit-code` exits 1 when out of sync. |
| `app sync NAME` | Fetches and deploys now, even if paused. Waits by default; `--logs` prints compose and hook output. |
| `app rollback NAME [REVISION]` | Redeploys an earlier composetpl revision. Automated apps are then **paused**, so auto-sync doesn't undo the rollback. |
| `app pause NAME`, `app resume NAME` | Stops or restarts automatic sync. |
| `app history NAME` | composetpl revisions, with the Git commit each came from. |
| `app operations NAME`, `operation list`, `operation get ID` | Sync and rollback history, with each operation's output. |
| `app refresh NAME`, `refresh` | Fetch and reconcile now instead of waiting for the interval. |
| `host list` | Reachability and engine versions. |
| `validate DIR` | Check objects offline. |

Hooks are composetpl's: `x-composetpl` hook services run at `pre-install`/`pre-upgrade` (Argo's PreSync),
`post-install`/`post-upgrade` (PostSync), and around rollbacks.

### API

Every `/api/v1` endpoint needs `Authorization: Bearer <token>`:

| Endpoint | |
| --- | --- |
| `GET /api/v1/apps`, `GET /api/v1/apps/{name}` | Status |
| `GET /api/v1/apps/{name}/history`, `GET /api/v1/apps/{name}/operations` | History |
| `POST /api/v1/apps/{name}/sync`, `.../rollback` (`{"revision": N}`) | Start an operation (202, returns it) |
| `POST /api/v1/apps/{name}/pause` (`{"reason": "..."}`), `.../resume`, `.../refresh` | |
| `GET /api/v1/operations`, `GET /api/v1/operations/{id}` | Operations, with output |
| `GET /api/v1/hosts`, `GET /api/v1/root`, `POST /api/v1/refresh`, `GET /api/v1/version` | |

`/healthz` needs no token. The status page at `/` asks for the token once and keeps it in a cookie. That cookie
only works for reads, so a cross-site request can't trigger a sync.

## Remote hosts and paths

- **Relative paths** in compose files (`./data`, `./config.yml`) resolve under `<projectRoot>/<app>` *on the
  host's machine*, not inside the composecd container. Use `projectRoot` to choose where app data lives.
- **Config files** from a chart's `files/` don't exist on the host. Inline them with compose's `configs` and
  `content`: `content: {{ .Files.Get "files/app.conf" | quote }}`.
- **`build:` contexts** are read where composecd runs, so use a Git URL or an absolute path that exists there.
- **SSH hosts** use composecd's `ssh` client, so mount a key (and `known_hosts`) into the container. The remote
  user needs to be able to run `docker`.
- **Registry credentials** for pulling images come from composecd's Docker config (`~/.docker/config.json` in
  the container). Compose passes them to each engine.

## Security

- **Anyone who can push to the root can run anything on your hosts.** Charts can mount the Docker socket or
  `/`. Protect the branch composecd follows.
- **The admin token can deploy anything in Git, and roll back.** Keep it secret, and put the API behind TLS
  (a reverse proxy) if it's reachable beyond localhost. `auth.disabled: true` turns it off, for use behind a
  proxy that authenticates.
- **Secrets** in values end up in composetpl's release records on each host, and in rendered compose files, as
  with composetpl itself.

## Compared with Argo CD

| Argo CD | composecd |
| --- | --- |
| Application (Helm source) | `Application`: a composetpl chart from Git or OCI, valueFiles and values |
| Cluster | `Host`: a Docker engine over unix, ssh or tcp |
| Sync status, diff, health | Same idea, per compose resource and container |
| Automated sync, manual sync, rollback (disables auto-sync) | Same |
| PreSync / PostSync hooks | composetpl `x-composetpl` hooks |
| Declarative setup (app of apps) | Everything is read from the root directory in Git |
| Self-heal, prune, sync windows, retries, notifications, webhooks, ApplicationSets, SSO, image updater | Planned; see [docs/PLAN.md](docs/PLAN.md) |

## Development

```bash
go test ./...                       # unit tests; need git, but not Docker
go test -race ./...
golangci-lint run ./...
go run ./cmd/composecd validate examples/gitops/composecd
```

The controller and CLI are tested end to end against real Git repositories, with composetpl's fake compose
runner (`pkg/compose/composetest`) standing in for Docker.

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
