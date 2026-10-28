# composecd

GitOps for [composetpl](https://github.com/trevorswanson/composetpl) charts. Commit your compose stacks as
charts to Git, and composecd keeps them deployed on one or more Docker hosts, like Argo CD does for Helm
charts on Kubernetes.

- **Git is the source of truth.** Hosts and Applications are YAML objects in your repository. composecd reads
  them, renders each Application's chart and compares it with what is deployed.
- **Sync status and health.** Each Application is `Synced` or `OutOfSync`, with per-service diffs and live
  drift, and `Healthy`, `Progressing`, `Degraded`, `Suspended` or `Missing` from its containers.
- **Automated or manual sync.** Automated Applications are deployed whenever Git changes, can repair
  containers changed by hand (self-heal), and are uninstalled when removed from Git (prune). Any Application can
  be synced, rolled back or paused from the CLI or API.
- **Homelab controls.** Sync windows ("no restarts on movie night"), dependencies between apps (Traefik
  before the apps behind it), retries with backoff, and per-host maintenance mode.
- **Fast feedback.** Push webhooks from Gitea, Forgejo, GitHub or GitLab, Docker events from every host,
  notifications to ntfy, Gotify, Discord, Slack or any webhook, and Prometheus metrics.
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

Every refresh interval (3 minutes by default), or right away when a push webhook arrives, composecd:
1. Fetches the root of your GitOps repository and reads the `Host`, `Application` and `SyncWindow` objects
   in it.
2. Pings every host.
3. For each Application, in `dependsOn` order:
   1. Renders its chart at the current commit, with live `.Capabilities` and `lookup` from its host.
   2. Compares the result with the deployed composetpl release.
   3. Checks the live containers for drift and health.
   4. If the Application is automated, and nothing holds it back (a sync window, a dependency that isn't
      Synced and Healthy yet, host maintenance):
      - if Git differs from what's deployed, runs `composetpl upgrade --install` on it;
      - if only the containers drifted and `selfHeal` is on, runs `composetpl repair` on it.
4. Uninstalls releases whose Application left Git (or moved to another host), if they had `prune` on.

Between refreshes, each host's Docker events (a container stopped, died, was removed or turned unhealthy)
get the affected app reconciled within a couple of seconds.

A failed automatic sync is retried only if the Application has a `retry` policy. Otherwise, and once the
retries are used up, composecd waits for something to change in Git. Fix the chart, or run `app sync` by hand.

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

5. **Sign in and set up access.** Save the server and token for the CLI, then create accounts for everyday use
   (see [Users and access](#users-and-access)):

   ```bash
   composecd --server http://nas.lan:8080 login        # paste the admin token
   composecd user add trevor --role admin              # a local user for the web UI
   composecd token create grafana --role readonly      # a token for scripts and dashboards
   ```

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
  dependsOn: [traefik]            # auto-sync only once these are Synced and Healthy
  syncPolicy:
    automated:                    # omit for manual sync
      prune: true                 # uninstall when removed from Git (keeps volumes and history)
      selfHeal: true              # repair containers removed or changed by hand
    retry:                        # retry a failed automatic sync
      limit: 3
      backoff: {duration: 10s, factor: 2, maxDuration: 5m}
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

**Self-heal** repairs a container that is missing, was created from different settings, or belongs to a
service the release no longer has (orphaned). It reapplies the deployed revision without a new revision or
hooks. A container that is merely stopped is a health problem, not drift. If an app keeps drifting, self-heal
backs off from 5 seconds up to 5 minutes between repairs.

**Prune** is recorded on the release when it's synced, so composecd still knows after the Application is
gone. Pruning uninstalls the release with `composetpl uninstall --keep-history`, so named volumes and history
stay. It never happens when the root can't be read, or when the root suddenly defines no applications at all.
Releases that are left behind and not pruned show as orphaned releases on their host.

### SyncWindow

```yaml
kind: SyncWindow
metadata: {name: movie-night}
spec:
  kind: deny                      # or allow
  schedule: "0 19 * * 5,6"        # cron: when the window opens
  duration: 5h                    # how long it stays open
  timeZone: America/Chicago       # default UTC
  applications: ["plex", "media-*"]   # globs; an app must match every selector that is set
  hosts: [nas]
  labels: {tier: media}
  manualSync: true                # let manual syncs through while it blocks
```

Windows work as in Argo CD:
- An open `deny` window blocks the apps it matches.
- If any `allow` windows match an app, the app may only sync while one of them is open.
- `manualSync` lets `app sync` and `app rollback` through while the window blocks.

Windows apply to automatic syncs, self-heal and prune.

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

When automation holds off, `app get` and `app list` say why under **Waiting**: a sync window, a dependency,
host maintenance, a retry or self-heal backoff, or a failed sync waiting for a change in Git.

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
| `host list` | Reachability, engine versions, maintenance and orphaned releases. |
| `host maintenance NAME on\|off` | While on, nothing on the host is synced, healed or pruned automatically. Manual operations still work. |
| `window list` | Sync windows, whether they're open, and the apps they match. |
| `validate DIR` | Check objects offline. |
| `login`, `logout`, `whoami` | Save, forget or check the server and token the CLI uses. |
| `token create NAME --role R`, `token list`, `token revoke NAME` | API tokens (admin). |
| `user add NAME --role R`, `user passwd NAME`, `user delete NAME`, `user list` | Local users (admin; anyone can change their own password through the API). |

Hooks are composetpl's: `x-composetpl` hook services run at `pre-install`/`pre-upgrade` (Argo's PreSync),
`post-install`/`post-upgrade` (PostSync), and around rollbacks.

### API

Every `/api/v1` endpoint needs `Authorization: Bearer <token>`, or a browser session. Reads need the
`readonly` role. Everything that changes state needs `admin`:

| Endpoint | |
| --- | --- |
| `GET /api/v1/apps`, `GET /api/v1/apps/{name}` | Status |
| `GET /api/v1/apps/{name}/history`, `GET /api/v1/apps/{name}/operations` | History |
| `POST /api/v1/apps/{name}/sync`, `.../rollback` (`{"revision": N}`) | Start an operation (202, returns it) |
| `POST /api/v1/apps/{name}/pause` (`{"reason": "..."}`), `.../resume`, `.../refresh` | |
| `GET /api/v1/operations`, `GET /api/v1/operations/{id}` | Operations, with output |
| `GET /api/v1/hosts`, `POST /api/v1/hosts/{name}/maintenance` (`{"enabled": true, "reason": "..."}`) | |
| `GET /api/v1/windows`, `GET /api/v1/root`, `POST /api/v1/refresh`, `GET /api/v1/version` | |
| `GET /api/v1/me` | Who you are, your role, and how you signed in |
| `GET/POST /api/v1/tokens` (`{"name", "role"}`), `DELETE /api/v1/tokens/{name}` | API tokens (admin) |
| `GET/POST /api/v1/users` (`{"name", "role", "password"}`), `PUT .../{name}/password`, `DELETE .../{name}` | Local users (admin, or yourself for your password) |

`/healthz` and `/metrics` need no token. `POST /api/v1/webhook` checks the Git server's signature instead.

## Users and access

There are two roles. **readonly** sees everything: apps, diffs, history, operations and hosts. **admin** can
also sync, roll back, pause, refresh, toggle maintenance, and manage users and tokens. Manual operations record
who ran them.

There are four ways to authenticate:
- **The bootstrap admin token.** It's `auth.tokenFile`, or the generated `<dataDir>/admin-token`, and acts as
  the admin. Keep it for break-glass use. Browser sessions signed in with it end whenever composecd restarts.
- **API tokens.** Create them with `composecd token create NAME --role readonly|admin`.
  - A token looks like `ccd_<id>_<secret>` and is shown once. composecd stores only a hash, and records when
    each token was last used.
  - Revoking a token also ends any browser sessions signed in with it.
- **Local users.** Create them with `composecd user add NAME --role ...`.
  - Passwords need at least 10 characters and are stored as bcrypt hashes.
  - After five failed sign-ins, a name and address are locked out for one minute, doubling up to 15 minutes.
  - Changing a password signs that user out everywhere. Role changes and deletions take effect immediately.
- **OIDC.** Works with Authentik, Authelia, Keycloak, Pocket ID and others. Groups map to roles:

  ```yaml
  # composecd.yaml
  auth:
    oidc:
      issuer: https://auth.example.lan/application/o/composecd/
      clientID: composecd
      clientSecretFile: /run/secrets/oidc-secret   # omit for a public client
      redirectURL: https://composecd.example.lan/auth/callback
      adminGroups: [homelab-admins]
      readonlyGroups: [homelab-users]   # omit to let anyone else who signs in read
      # groupsClaim: groups             # the default
      # usernameClaim: preferred_username
  ```

  Register composecd with your provider as an OAuth2/OpenID client with that redirect URL. It uses the
  authorization code flow with PKCE.

**The web page.** It signs you in with a user name and password, through your identity provider, or with an API
token pasted as the password (leave the user name empty).
- A sign-in starts a server-side session. The cookie is `HttpOnly` and `SameSite=Lax`, and only its hash is
  stored. A session ends after 7 days idle, and after 30 days at most.
- Anything a session changes needs the session's CSRF token, which the page sends for you. API tokens in the
  `Authorization` header don't.

**The CLI.** `composecd login` saves the server and a token in `composecd/login.yaml`, in your user config
directory (`~/.config` on Linux), readable only by you. `--server`/`--token` and `$COMPOSECD_SERVER`/`$COMPOSECD_TOKEN` take precedence over it.

## Webhooks, notifications and metrics

**Webhooks.** Set `webhook.secretFile` in `composecd.yaml`, then add a push webhook in your Git server that
points at `https://<composecd>/api/v1/webhook`, using the same secret and the JSON content type. Gitea,
Forgejo and GitHub sign the payload with it; GitLab sends it as its token. A push to the root, or to any
Application's repository, triggers a refresh right away.

**Notifications** are configured in `composecd.yaml`, where their URLs and tokens can be read from files:

```yaml
notifications:
  - name: phone
    type: ntfy                    # ntfy, gotify, discord, slack or webhook (JSON POST of the event)
    url: https://ntfy.example.lan/homelab
    tokenFile: /run/secrets/ntfy-token
    events: [sync-failed, health-degraded, host-down, prune]   # the default
  - name: chat
    type: discord
    urlFile: /run/secrets/discord-webhook
    events: [sync-succeeded, sync-failed, self-healed]
    applications: ["media-*"]     # only these apps (host events are always sent)
```

The events are `sync-succeeded`, `sync-failed` (which also covers failed rollbacks, heals and prunes),
`self-healed`, `prune`, `health-degraded` and `out-of-sync`, each sent when an app enters that state, and
`host-down`.

**Metrics.** `GET /metrics` serves Prometheus metrics:
- `composecd_app_sync_status` and `composecd_app_health_status`, one series per app with its current status;
- `composecd_app_paused` and `composecd_app_blocked`;
- `composecd_host_up`, `composecd_host_maintenance` and `composecd_host_orphaned_releases`;
- `composecd_operations_total` and `composecd_operation_duration_seconds_total`, by kind, trigger and phase;
- `composecd_root_error`, `composecd_root_last_refresh_timestamp_seconds` and `composecd_sync_window_open`.

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
- **Admins can deploy anything in Git, and roll back.** Give most people the `readonly` role. Put composecd
  behind TLS (a reverse proxy) if it's reachable beyond localhost: browser session cookies are marked `Secure`
  only over HTTPS. `auth.disabled: true` turns authentication off entirely, which only makes sense behind a
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
| Self-heal, prune | Same; prune keeps named volumes and release history |
| Sync windows | `SyncWindow`, with the same allow/deny/manualSync rules, matched by app, host or labels |
| Sync waves across apps | `dependsOn` |
| Retry with backoff | `syncPolicy.retry` |
| Git webhooks, notifications, metrics | Same; native ntfy, Gotify, Discord, Slack and webhook notifiers |
| — | Host maintenance mode; reconciles on Docker events |
| ApplicationSets, multi-source apps, SOPS, web UI, SSO, image updater | Planned; see [docs/PLAN.md](docs/PLAN.md) |

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
