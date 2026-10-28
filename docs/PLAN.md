# composecd: GitOps for composetpl charts

## Context

composetpl (`/Users/trevor/git/docker-helm-template`, module `github.com/trevorswanson/composetpl`) is "Helm for docker compose": charts, releases with revisions, hooks, rollback and diff, with history stored on each Docker host in the `composetpl-releases` volume. What's missing is the Argo CD layer on top: charts committed to Git get reconciled automatically against one or more Docker hosts, and you can see sync and health status, catch drift, control sync windows and get notifications.

**composecd** is that layer. It's a single Go binary (server, controller and CLI) that ships alongside composetpl. Decisions made with you:
- **Name:** `composecd`.
- **composetpl integration:** composetpl gets a **public Go API** (`pkg/…`), and composecd calls it in-process.
- **Hosts:** the controller talks to them **directly** (unix socket, `ssh://`, `tcp://`+TLS). An outbound agent comes later.
- **Configuration:** **Git first.** Hosts, Applications, ApplicationSets and SyncWindows are YAML in a Git "root" path (app-of-apps). The UI and CLI only *act* on apps; they don't define them. SQLite holds only operation history, events and auth.

Design principle: **Git is the desired state, the Docker host is the live state, and the composetpl volume is the deployment history.** composecd keeps very little state of its own. Releases it creates are normal composetpl releases, so `composetpl list --context nas` still works, and composecd can adopt existing composetpl releases.

---

## Phase 0: composetpl prerequisites (in `/Users/trevor/git/docker-helm-template`)

1. **Promote packages to a public API.** `git mv internal/{action,chart,compose,diff,engine,hooks,release,storage,values,registry,output} pkg/`. Keep `glob` and `lint` internal unless something needs them. Update imports. Add a package doc ("pkg/ is the Go API; semver applies from v1") and a README section. Tag `v0.x`; there are no tags yet.
2. **Per-instance environment** (needed for concurrency: today `compose.execDocker` sets `cmd.Env = os.Environ()`, `internal/compose/compose.go:118`):
   - `compose.CLI.Env []string` is appended to the environment of every docker call. composecd uses it for per-app `${VAR}` interpolation and for `DOCKER_HOST`/TLS settings.
   - `engine.Options.Env func(string) (string, bool)` and `action.RenderOptions.Env` back the `env`/`expandenv` template functions. When nil, they fall back to `os.LookupEnv`, so CLI behavior doesn't change.
   - `storage.Volume` uses the same Env (it has `Exec`, `internal/storage/volume.go`).
3. **Drift support in `compose.Runner`:** `ConfigHash(ctx, p) (map[service]string, error)` wraps `docker compose config --hash '*'`. composecd compares it with each container's `com.docker.compose.config-hash` label. Extend `Container` with `Labels`/`ConfigHash` from `ps --format json`.
4. **Redeploy without a new revision:** add `action.Repair` (and an optional CLI `composetpl repair NAME`) that runs `Up` on the last deployed manifest. It runs no hooks, writes no revision and uses `--remove-orphans`. Self-heal relies on it. A plain Upgrade would fire `pre-upgrade` backup hooks every time a container crashed.
5. **Release labels:** add `Labels map[string]string` to `release.Release` (as Helm does). Set it through `Install`/`Upgrade`/`Rollback` options. composecd records `managed-by=composecd`, `composecd/app`, `composecd/source-revision=<git sha>`, `composecd/operation-id`. `composetpl list` shows "managed-by" and refuses `upgrade` on a composecd-managed release unless `--force`. The refusal is a warning by default.
6. **Optional `sync-fail` hook event:** Argo's SyncFail. It runs when install/upgrade/rollback fails, before `--atomic` recovery. Add it to `hooks.events` and to `action.Install/Upgrade` failure paths (`internal/action/action.go` `markFailed`).
7. **Context-aware storage:** today `storage.Driver` has no ctx, and `Volume.Ctx` is a field. Add ctx-taking methods, or document creating one driver per operation. composecd builds one `action.Configuration` per operation, because `Configuration.caps` and `Log` are per instance (`internal/action/action.go:28`).

Keep everything consistent with the existing tests (`internal/action/action_test.go`, `internal/compose/composetest/fake.go`). Each item ships with unit tests, and `go test -tags integration ./cmd/composetpl` stays green.

---

## Feature analysis: what to take from Argo CD

| Argo CD feature | Homelab value | composecd form | Phase |
|---|---|---|---|
| Application (source → destination) | Core | `Application` YAML: Git path or `oci://` chart, values, target host | 1 |
| Sync status / diff | Core | Rendered manifest vs. last deployed revision; per-service diff using `pkg/diff` | 1 |
| Health | Core | From compose `ps`: Healthy / Progressing / Degraded / Missing / Suspended / Unknown | 1 |
| Manual and automated sync, history, rollback | Core | composetpl Install/Upgrade/Rollback; a rollback pauses auto-sync, as in Argo | 1 |
| Hooks (PreSync/PostSync/SyncFail/PostDelete) | **Requested** | composetpl `x-composetpl` hooks, mapped: PreSync=`pre-install,pre-upgrade`, PostSync=`post-*`, SyncFail=`sync-fail`, PostDelete=`post-delete`. Shown per operation in the UI. | 1 (SyncFail 2) |
| **Multiple clusters** | **Requested** | `Host` objects: unix, ssh, tcp+TLS, labels, maintenance mode | 1 |
| Self-heal | High (manual `docker stop`/`rm` or edits) | Docker events stream + config-hash drift → `action.Repair`, with backoff | 2 |
| Prune | High | An app removed from Git → uninstall (volumes kept unless `deleteVolumes: true`). Services removed inside an app are already handled by `--remove-orphans`. | 2 |
| **Sync windows** | **Requested** | `SyncWindow` allow/deny, cron + duration + timezone, matched by app/host/label; `manualSync` override; per-host maintenance mode | 2 |
| Sync retry with backoff | High (flaky pulls) | `syncPolicy.retry {limit, backoff, factor, maxDuration}` | 2 |
| Sync waves (cross-app) | High (Traefik/DNS/db before apps) | `spec.dependsOn: [app]`: wait until dependencies are Synced and Healthy. Within an app, compose `depends_on` handles order. | 2 |
| Git webhooks + polling | High (Gitea/Forgejo/GitHub/GitLab) | `/api/webhook/{provider}` with HMAC secret; default poll 3m | 2 |
| Notifications | High | shoutrrr: ntfy, Gotify, Discord, Telegram, Pushover, Slack, SMTP, webhook. Triggers: sync succeeded/failed, degraded, out-of-sync, host down, image update | 2 |
| App-of-apps / declarative setup | High | Git root path holds every composecd object; composecd can manage itself | 1 |
| ApplicationSet | High ("every dir in `stacks/`", "node-exporter on every host") | Generators: Git directories and **hosts (by label)**; independent output (3e delivered) | 3 |
| Multi-source apps | Medium-high | OCI chart + values files from a `valuesFrom` Git repository (delivered in 3d) | 3 |
| Secrets (SOPS plugin, Vault) | High (repo may be on GitHub) | Native SOPS+age decryption of values files (`*.sops.yaml`) in memory, masked in the UI | 3 |
| Web UI | High | App tiles, tree (app → services → containers), diff, history, logs, events, sync dialog | 3 |
| SSO/RBAC, projects | Medium | Local admin + API tokens; OIDC (Authentik/Authelia/Keycloak/Pocket ID); trusted forward-auth header; roles admin/readonly. AppProjects simplified to "allowed hosts/repos". | 3 |
| Orphaned resources | High (migrating) | Lists unmanaged compose projects per host, with an **Adopt** action that creates an Application skeleton | 3 |
| Image Updater | **Very high** (replaces Watchtower/Diun) | Per image: semver/latest/digest strategies. Writes back to Git (`.composecd/<app>.yaml` override) or pins the digest in memory. Respects sync windows. | 4 |
| ignoreDifferences | Low-medium | Per service: ignore image-digest or env drift (e.g., when another tool manages it) | 4 |
| Metrics | Medium | Prometheus `/metrics`: sync and health gauges, sync durations, host up | 2 |
| GPG commit verification | Low-medium | `repository.verifySignatures` with allowed keys | 4 |
| Outbound agent | Medium (NAT/VPS hosts) | `composecd agent` dials the controller; the controller tunnels the docker API over it | 4 |
| Host housekeeping (not in Argo) | Medium | Scheduled `image prune`; disk-usage warnings per host | 4 |
| Rolling/progressive delivery, PR preview envs | Low | Out of scope (compose recreates containers) | — |

---

## Architecture

One process (`composecd server`) runs these modules, each in its own package:

```
cmd/composecd/        cobra root: server, app {list,get,diff,sync,rollback,history,logs,pause,resume},
                      host {list,get}, window list, adopt, version, login
internal/config/      bootstrap config + object types (Host, Repository, Application, ApplicationSet,
                      SyncWindow, Notifier), loading/validation (JSON Schema), defaults
internal/git/         git CLI wrapper: ls-remote/fetch/checkout per commit into a content-addressed
                      cache (<data>/repos/<hash>/<sha>), creds (token, SSH key, known_hosts), webhooks
internal/source/      Application → rendered desired state at a sha: chart path or oci://, valueFiles
                      + inline values, `dependency build` for Chart.lock, SOPS decrypt, env injection;
                      calls pkg/action.Render + Merge + hooks.Extract
internal/hosts/       Host registry; per-host compose.CLI (Global/Env), storage.Volume, Docker SDK
                      client (connhelper for ssh) for events/logs/inspect; ping + capabilities + arch
internal/controller/  workqueue per app (rate-limited), refresh loop, compare, health, drift,
                      decides sync/heal/prune; respects windows, dependsOn, pause, host maintenance
internal/syncer/      runs operations through pkg/action (Upgrade{Install:true}, Rollback, Uninstall,
                      Repair); per-app mutex; retries; captures logs per operation; release labels
internal/health/      container rows → app health
internal/windows/     sync window evaluation (robfig/cron/v3, time zones)
internal/appset/      generators → Applications (phase 3)
internal/notify/      shoutrrr dispatch + templates
internal/store/       SQLite (modernc.org/sqlite, CGO-free): operations, events, users, tokens
internal/api/         REST/JSON + SSE live updates, auth middleware, webhook endpoints, /metrics
web/                  React + Vite + TypeScript SPA, embedded with go:embed
deploy/chart/         a composetpl chart that runs composecd (used for self-management)
examples/             a sample homelab GitOps repo
```

### Objects (Git root, `apiVersion: composecd/v1alpha1`)

```yaml
kind: Host
metadata: {name: nas, labels: {role: storage, arch: amd64}}
spec:
  url: unix:///var/run/docker.sock        # or ssh://deploy@pi.lan, tcp://gpu.lan:2376
  tlsSecretRef: gpu-tls                    # tcp only
  projectRoot: /opt/composecd              # remote ProjectDir base (see "Remote paths")
---
kind: Application
metadata: {name: media, labels: {tier: apps}}
spec:
  source:
    repoURL: https://git.lan/me/homelab.git
    targetRevision: main
    path: stacks/media                     # or chart: oci://ghcr.io/me/charts/media, version: ^1.2
    valueFiles: [values.yaml, values-nas.yaml, secrets.sops.yaml]
    values: {apps: {jellyfin: {enabled: true}}}
    env: {TZ: America/Chicago}             # ${VAR} interpolation and the env template function
  destination: {host: nas}                 # the release name = metadata.name (release.ValidateName)
  dependsOn: [traefik]
  syncPolicy:
    automated: {prune: true, selfHeal: true}
    options: {wait: true, timeout: 5m, pull: missing, atomic: true, historyMax: 10}
    retry: {limit: 5, backoff: {duration: 10s, factor: 2, maxDuration: 5m}}
---
kind: SyncWindow
metadata: {name: nightly}
spec: {kind: allow, schedule: "0 2 * * *", duration: 3h, timeZone: America/Chicago,
       selector: {apps: ["*"], hosts: [nas], labels: {tier: apps}}, manualSync: true}
```

The bootstrap file `composecd.yaml` (or env vars) holds the root repo, credential locations, the data dir, listen address and auth settings. Everything else lives in Git.

### Reconcile loop (per app)

1. **Resolve:** `targetRevision` → sha, via a poll or webhook and the git cache.
2. **Render** at the sha against the destination host's live `.Capabilities`/`lookup`. Call `action.DiffUpgrade` in dry-run mode (`internal/action/diff.go:27`) to get the desired manifest and `[]diff.Change` against the deployed revision. The cache key is (sha, app spec hash, host caps).
3. **Compare.** Sync status is `OutOfSync` if the manifest differs, the release is missing, or the latest revision failed. **Drift** is measured on the live host: `ConfigHash` vs. container labels, missing or stopped services, and orphans.
4. **Health**, from `compose.PS` (`pkg/compose` `Container`). Hook containers are excluded.
5. **Decide**, in this order: paused → host maintenance → sync windows → `dependsOn` healthy → auto-sync (Upgrade) or self-heal (Repair) → retry/backoff. Prune applies when an app disappears from Git and `prune: true`.
6. **Record** the operation in SQLite (sha, trigger, user, composetpl revision, hook results, logs). Set the release labels. Send notifications.

Triggers: poll timer, Git webhook, Docker `events` stream per host (`die`, `destroy`, `stop`, `start`, `health_status`), and manual actions from the API. One worker per app runs at a time. Host-level concurrency is capped.

### Remote paths and files (homelab gotchas the plan must handle)
- **Relative bind mounts.** Compose resolves `./data` on the *client*, so they'd point at the controller's checkout path, which doesn't exist on the remote host. For remote hosts, `ProjectDir = <host.projectRoot>/<app>`, so relative paths resolve on the remote host. Local hosts keep the chart dir. Lint warnings flag relative paths that point into the chart.
- **Config files from `files/`.** The recommended pattern is compose `configs: {x: {content: '{{ .Files.Get "…" }}'}}`, which compose ships through the API, so no file sync is needed. A spike verifies this works against remote engines (compose ≥2.23). Fall back to a documented `rsync` hook if it doesn't.
- **`build:` contexts** stream from the controller over the API, so they work remotely.
- **Registry credentials** come from the controller's docker config. Compose sends them to each daemon.
- **Secrets** end up in composetpl release records on each host's volume (already documented upstream). The UI masks SOPS-sourced keys.

### Packaging
- Image `FROM docker:cli` (which includes the compose plugin), plus git and openssh-client, plus the `composecd` binary. It mounts `/var/run/docker.sock`, `/data` and the SSH keys.
- GoReleaser and CI copied from composetpl's `.goreleaser.yaml`, `.github/workflows/{ci,release}.yml` and `.golangci.yml`. Multi-arch binaries and images go to GHCR. Apache-2.0 with a NOTICE, plus CONTRIBUTING, as in composetpl.
- `deploy/chart` is a composetpl chart. `composecd bootstrap --host nas --repo …` installs composecd with composetpl and registers it as its own Application.

---

## Phased roadmap

- **Phase 0: composetpl public API** (above). Ship as a composetpl tag. composecd pins it in `go.mod`.
  *Done in composetpl v0.2.0, except the optional `sync-fail` event and context-aware storage.*
- **Phase 1: MVP.** Config loader; git cache; source render (Git path + OCI); multiple hosts (unix/ssh/tcp); compare, diff and health; manual and automated sync; hooks via composetpl; rollback (pauses auto-sync); operation history in SQLite; REST API; CLI; a minimal read-only status page. Self-managed via app-of-apps.
  *Done. Self-management is not yet safe: composecd recreating its own container interrupts the upgrade, so install it with composetpl for now.*
- **Phase 2: Reconciliation quality.** Docker events, self-heal (Repair), prune, sync windows, host maintenance mode, `dependsOn`, retry/backoff, webhooks, notifications, `sync-fail`, Prometheus metrics.
  *Done, except `sync-fail`, which needs the new hook event in composetpl first. Notifications are native (ntfy, Gotify, Discord, Slack, webhook) rather than shoutrrr, and live in `composecd.yaml` because their URLs are secrets. Maintenance mode is toggled at runtime (`host maintenance`), not in Git.*
- **Phase 3: UI and scale-out.** **3a delivered:** local users, roles, named tokens, browser sessions, CSRF and OIDC. **3b delivered:** embedded Go templates and htmx, live panels and container logs via SSE, filters, diff, history/rollback, maintenance and account management. **3c delivered:** SOPS/age values via the CLI, in-memory decryption, output masking and a pinned image dependency. **3d delivered:** OCI charts with Git values, independent revisions, commit-aware auto-sync and values webhooks. **3e delivered:** Git-directory and Host-label ApplicationSets, shared validation/dependencies, guarded pruning and API/CLI/UI status. Remaining: adopting unmanaged projects; see [PHASE3.md](PHASE3.md).
- **Phase 4: Extras.** Image updater (write-back to Git and digest pinning), ignoreDifferences, GPG verification, the outbound agent, host housekeeping.

---

## Verification

- **composetpl Phase 0:** `go test ./...`, `golangci-lint run ./...`, `go test -tags integration -run Integration -v ./cmd/composetpl`. Plus new unit tests for `Env` injection (two parallel renders with different env), `ConfigHash`, `Repair` (no new revision, no hooks), release labels, and `sync-fail` ordering with `--atomic`.
- **composecd unit tests:** a fake `compose.Runner` (reuse `pkg/compose/composetest`), a fake git cache, table tests for windows (DST, overlapping allow/deny, `manualSync`), the health aggregation table, and the controller decision table (paused, maintenance, window closed, dependency unhealthy, retry exhausted).
- **Integration (`-tags integration`):** a local bare Git repo with `examples/homelab` charts and two engines (the local socket plus a `docker:dind` container reached by `tcp://`). Then assert:
  1. Commit → auto-sync → containers running on the right host → `composetpl list --context` shows a revision labeled `managed-by=composecd`.
  2. Changing values in Git → OutOfSync → upgrade → the `pre-upgrade` hook ran.
  3. `docker rm -f` on a service → self-heal recreates it with no new revision.
  4. A deny window blocks auto-sync, and manual sync works only with `manualSync: true`.
  5. Deleting the app's YAML → prune uninstalls it, and its volumes survive.
  6. A broken chart → sync failed → `sync-fail` hook and notification (a webhook test server).
- **Manual:** run `composecd server` from the image against the homelab, open the UI, and run `composecd app diff media` / `app sync media` / `app rollback media` from the CLI.
