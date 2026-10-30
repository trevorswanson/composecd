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
    # valuesFrom: {}              # OCI only: values from the root repository
    # valueFiles: [values/media.yaml]  # relative to valuesFrom's repository root
    values:                       # applied last
      apps: {jellyfin: {enabled: true}}
    env: {PUID: "1000"}           # over the host's env
  destination:
    host: nas
  dependsOn: [traefik]            # auto-sync only once these are Synced and Healthy
  # self: true                    # this app runs composecd itself (normally detected; see below)
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
Releases that are left behind and not pruned show as orphaned releases on their host, where an admin can
delete one by hand: the same uninstall, recorded as a manual prune operation.

**composecd's own Application** is the one whose compose project composecd's container belongs to, on a Host
that reaches the engine it runs on. composecd finds it by itself; `self: true` marks it when it can't (for
example, when `/proc/self/mountinfo` doesn't show the container's ID). composecd shows it with a "self" badge
and never syncs, self-heals, rolls back, prunes or deletes it, because recreating its own container from
inside that container would stop composecd halfway through. Upgrade it with `composetpl upgrade` on the host.
See [docs/SELF.md](docs/SELF.md) for the plan to let composecd update itself.

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
| `app diff NAME` | Per-resource unified diff from the deployed release to Git (from nothing, before the first sync). Before the first sync it also lists containers already in the project that the sync would recreate or remove. `--exit-code` exits 1 when out of sync. |
| `app sync NAME` | Fetches and deploys now, even if paused. Waits by default; `--logs` prints compose and hook output. |
| `app rollback NAME [REVISION]` | Redeploys an earlier composetpl revision. Automated apps are then **paused**, so auto-sync doesn't undo the rollback. |
| `app pause NAME`, `app resume NAME` | Stops or restarts automatic sync. |
| `app history NAME` | composetpl revisions, with the Git commit each came from. |
| `app operations NAME`, `operation list`, `operation get ID` | Sync and rollback history, with each operation's output. |
| `app refresh NAME`, `refresh` | Fetch and reconcile now instead of waiting for the interval. |
| `host list` | Reachability, engine versions, maintenance and orphaned releases. |
| `host unmanaged NAME` | Unmanaged/orphaned releases and plain Compose projects, including config paths. `-o json\|yaml` |
| `adopt HOST RELEASE` | Export Application YAML with saved user values (admin); warnings go to stderr. |
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
`readonly` role. Everything that changes state needs `admin`; adoption exports also need admin because they include saved values in plaintext:

| Endpoint | |
| --- | --- |
| `GET /api/v1/apps`, `GET /api/v1/apps/{name}` | Status |
| `GET /api/v1/applicationsets` | Generator output, apps and errors (readonly) |
| `GET /api/v1/apps/{name}/containers/{container}/logs?follow=1&tail=200` | Container log SSE stream (readonly; tail 0–10000) |
| `GET /api/v1/apps/{name}/history`, `GET /api/v1/apps/{name}/operations` | History |
| `POST /api/v1/apps/{name}/sync`, `.../rollback` (`{"revision": N}`) | Start an operation (202, returns it) |
| `POST /api/v1/apps/{name}/pause` (`{"reason": "..."}`), `.../resume`, `.../refresh` | |
| `GET /api/v1/operations`, `GET /api/v1/operations/{id}` | Operations, with output |
| `GET /api/v1/hosts`, `POST /api/v1/hosts/{name}/maintenance` (`{"enabled": true, "reason": "..."}`) | |
| `GET /api/v1/hosts/{name}/unmanaged` | Release metadata and plain Compose config paths (readonly) |
| `GET /api/v1/hosts/{name}/adopt/{release}` | Application YAML and warnings as JSON (admin; contains plaintext user values) |
| `DELETE /api/v1/hosts/{name}/releases/{release}` | Uninstall an orphaned release, keeping its history and named volumes (admin; returns the operation) |
| `GET /api/v1/windows`, `GET /api/v1/root`, `POST /api/v1/refresh`, `GET /api/v1/version` | |
| `GET /api/v1/me` | Who you are, your role, and how you signed in |
| `GET/POST /api/v1/tokens` (`{"name", "role"}`), `DELETE /api/v1/tokens/{name}` | API tokens (admin) |
| `GET/POST /api/v1/users` (`{"name", "role", "password"}`), `PUT .../{name}/password`, `DELETE .../{name}` | Local users (admin, or yourself for your password) |

`/healthz` and `/metrics` need no token. `POST /api/v1/webhook` checks the Git server's signature instead.

## Adopt existing releases

Use the same host connection and release storage as your existing composetpl install. Volume storage
uses the engine's composetpl release volume. For `storage: local`, migrate the release records into
`<dataDir>/hosts/<host>/releases` first; composecd cannot discover another client's local state directory.

```bash
composecd host unmanaged nas
# From the GitOps repository root, assuming objects are under composecd/.
composecd adopt nas media > composecd/media.yaml
# Review the YAML and warnings and protect secrets before committing.
git add composecd/media.yaml && git commit -m "feat: manage media with composecd" && git push
composecd refresh
composecd app diff media
composecd app sync media
```

The admin-only export contains **plaintext saved user values**. This is an intentional exception to
normal API/UI masking. Encrypt secrets in a SOPS values file and reference it with `valueFiles` before
committing; remove the corresponding inline values so they do not override the encrypted file.
The UI Hosts page's **Adopt** link shows the same YAML and warnings. Discovery only exposes metadata.

Exporting changes neither Git nor Docker. The YAML keeps the release name and host, so the first sync
upgrades its existing history in place. Automation and prune are off until you explicitly configure them.
The diff reports that composecd will take ownership, even if the rendered resources already match.
An existing Application with the same name or a pending composetpl operation blocks export.
Orphaned composecd releases on configured hosts can also be exported.

A chart path is suggested only when its recorded location maps into the current root repository and
contains `Chart.yaml`. OCI references retain their recorded tag/digest (or recorded version for a bare
reference). Otherwise the export suggests `charts/<release>` and warns you to copy the original chart
there or replace the source with its OCI reference. Relative recorded paths are interpreted from the
repository root; review them before syncing. Also match `Host.spec.projectRoot` to the original release's
project directory, and review host environment overrides and the chart version. A name match preserves
the project identity, but a changed manifest or project directory can still recreate containers.

Plain Compose projects have no composetpl history to adopt. Their config paths come from their container
labels, and may be unavailable for projects without containers. Convert them to a chart with
`composetpl create`; automatic conversion is not supported. See [the adoption example](examples/adoption/README.md).

## Users and access

There are two roles. **readonly** sees apps, masked diffs, history, operations and hosts. **admin** can
also sync, roll back, pause, refresh, toggle maintenance, export adoption YAML, and manage users and tokens. Manual operations record
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

**The web UI.** It signs you in with a user name and password, through your identity provider, or with an API
token pasted as the password (leave the user name empty).
- A sign-in starts a server-side session. The cookie is `HttpOnly` and `SameSite=Lax`, and only its hash is
  stored. A session ends after 7 days idle, and after 30 days at most.
- Anything a session changes needs the session's CSRF token, which the page sends for you. API tokens in the
  `Authorization` header don't.

**The CLI.** `composecd login` saves the server and a token in `composecd/login.yaml`, in your user config
directory (`~/.config` on Linux), readable only by you. `--server`/`--token` and `$COMPOSECD_SERVER`/`$COMPOSECD_TOKEN` take precedence over it.

## Web UI

Open the server URL to sign in. The UI and its pinned htmx, SSE and dagre scripts are embedded in the
binary; there is no CDN or Node build step.

- **Applications** (`/apps`): filter by host, sync, health, or a label (`key` or `key=value`).
- **Application details** (`/apps/NAME`): a strip with the app's health, sync status (desired and deployed
  commits) and last operation, then tabs: a **graph** of its services, networks, volumes, configs, secrets and
  containers, each marked Synced, OutOfSync, Missing or Prune with its health (click one for its details, its
  live `docker inspect` state with engine bookkeeping folded away, a live-to-desired diff, and its desired and
  deployed YAML); containers and live log tails; diffs, either of the compose file against the deployed
  release (side by side, compact or every resource in full) or of each object's live state against Git; release history with rollback; and operations with
  their output. **Details** shows the source, automation policy and retries.
  Admins can sync, pause, resume and refresh; a running sync disables the button and streams its output.
- **ApplicationSets** (`/applicationsets`): generated apps, counts and expansion/validation errors.
- **Hosts** (`/hosts`): connectivity, Docker/Compose versions, orphaned releases and admin maintenance toggles.
  Lists unmanaged composetpl releases and plain Compose projects with config paths. Admins can use **Adopt** to preview Application YAML, or **Delete** an orphaned release.
- **Sync windows** (`/windows`): open/closed state, next change, and matching applications.
- **Operations** (`/operations`): the latest 50 operations across applications, including who acted and output.
- **Settings** (`/settings`): your identity; admins can create/revoke tokens and add/delete users or change
  passwords. A newly created token is shown once.

Pages are server-rendered and readable without JavaScript; actions also accept ordinary forms. With
JavaScript enabled, authenticated `GET /events` delivers `app`, `host` and `operation` SSE events containing
HTML panels swapped with `hx-swap-oob`. Reconnecting clients receive a fresh snapshot. Changes never block
reconciliation, even if a browser is slow. Log tails stay connected when other panels update.

Live object state (`GET /api/v1/apps/NAME/objects/SECTION/NAME`, e.g. `services/web`; `?all=true` adds keys Git
doesn't set) compares what composecd can read back from the engine (image, command, environment, ports,
volumes, networks, labels, restart, healthcheck and similar) with Git, leaving out what the image sets by
default. Inspect output is cached for 30 seconds. Readonly users see every environment value replaced by a
tag (`<redacted 1a2b3c4d>`), since live containers can hold values composecd never decrypted and so can't
mask; equal values get equal tags, so a changed value still shows in the diff.

The log endpoint checks a fresh compose project listing and its project label before streaming; a container
from another project is refused. Log lines, diffs and displayed operation output mask SOPS-decrypted values
of four characters or more (see [Encrypted values with SOPS](#encrypted-values-with-sops)); plain values are already in Git and
are shown as they are. Masking is best effort: short values and transformed values (for example base64) may
remain visible.
Readonly users can view these outputs. Light/dark styling follows your system preference, and tables scroll
inside their panels at phone widths.

For a reverse proxy, allow long-lived SSE responses and disable response buffering on `/events` and log
endpoints (`X-Accel-Buffering: no` is sent). Stream connections recheck credentials periodically so revoked
sessions stop receiving updates.

## ApplicationSets

An `ApplicationSet` generates Applications from chart directories or Host labels. Place it beside your
other GitOps objects:

```yaml
apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: stacks}
spec:
  generators:
    - gitDirectories:
        directories: ["charts/*"]
        exclude: ["charts/experimental-*"]
        # repoURL: https://git.example.lan/me/charts.git
        # targetRevision: main
  template:
    metadata:
      name: "{{ .path.basename }}"
      labels: {appset: stacks}
    spec:
      source: {path: "{{ .path.path }}"}
      destination: {host: nas}
      syncPolicy: {automated: {prune: true}}
```

`gitDirectories` uses Go `path.Match` globs: `*` matches one path segment, and `**` has no recursive-glob
meaning. Hidden and symlinked directories are skipped. A matched directory generates an app when that
app has a chart: its `source.path` needs a regular `Chart.yaml`. That path need not be the matched
directory, so directories holding only values can share one chart:

```yaml
  generators:
    - gitDirectories: {directories: ["envs/*"]}
  template:
    metadata: {name: "web-{{ .path.basename }}"}
    spec:
      source:
        path: charts/web
        valueFiles: ["../../{{ .path.path }}/values.yaml"]
      destination: {host: nas}
```

A matched directory whose app has no chart there (a `Chart.yml`, say) generates nothing, and the set
lists it in a warning, as it does a set that generates no applications at all. Charts in another
repository or revision, and OCI charts, aren't checked during generation; if one is missing, the app
shows the render error.
Overlapping include globs within one generator generate each directory once. Repository and revision
defaults follow the Git root, or `HEAD` for another repository. Generated Git sources with no explicit
repository inherit their Git generator's repository/revision.

Directory parameters are `.path.path`, `.path.basename` and `.path.segments`. A Host generator instead
matches exact labels (an empty selector selects all Hosts) and provides `.host.name` and `.host.labels`:

```yaml
spec:
  generators:
    - hosts: {selector: {role: server}}
  template:
    metadata: {name: "agent-{{ .host.name }}"}
    spec:
      source: {path: charts/agent}
      destination: {host: "{{ .host.name }}"}
      syncPolicy: {automated: {prune: true}}
```

Each generator produces its own applications; generators are not combined into a matrix. Every string
field and map key in the template is rendered with Go `text/template` and `missingkey=error`. Common,
deterministic Sprig string helpers are available (`lower`, `upper`, `replace`, `trim`, `kebabcase`,
`splitList`, `join`, and others); environment, file, network, random, date and crypto helpers are excluded.
Booleans, numbers and durations keep their declared types.

Generated apps carry `composecd/applicationset: NAME` and a definition such as
`applicationset stacks (charts/media)`. Explicit and generated apps are validated together: duplicate
names, invalid destinations, unresolved dependencies and cycles reject the snapshot. `dependsOn` works
in both directions. `validate` checks definitions locally; the server performs expansion and the final
reference checks. Generator repositories participate in push webhooks.

A removed directory or Host selection removes its app; its recorded prune policy controls release
removal. Changing set ownership or the template's prune flag updates release metadata through sync,
even without a manifest change. A set that fails to generate (a template error, an unreachable
repository) keeps the apps it generated before and reports the error on the set and the root; the rest of
Git still applies. A set dropping from N apps to zero reports `generated: 0, was: N`, keeps its apps and
protects existing releases. Release ownership labels restore that protection after restart; protected releases may appear
as host orphans until generation recovers. To intentionally retire the entire set, remove the set object
while retaining a valid nonempty root, as with the existing whole-root prune guard.

Generated apps whose charts fail to render (a template error, invalid `Chart.yaml` or values) report the
error on the app, and the ApplicationSets page marks their set as failing and lists each app's error.

A Host removed from Git is retained as a retired connection for pending prune/retries during the current
process. Its last connection is not persisted, so keep Host definitions until pruning completes if a
restart is possible. Retired hosts appear on the Hosts page, with maintenance controls unavailable.

Inspect sets with `composecd appset list` (`-o json` / `-o yaml`),
`GET /api/v1/applicationsets`, or the **ApplicationSets** UI page. These are available to readonly users;
configuration stays in Git. [The example](examples/applicationsets/README.md) generates apps over `charts/*`.

## OCI charts with values from Git

Keep a chart in an OCI registry and its per-host values in Git:

```yaml
apiVersion: composecd/v1alpha1
kind: Application
metadata: {name: media}
spec:
  source:
    chart: oci://ghcr.io/me/charts/media
    version: ^1.2
    valuesFrom: {}                  # the root Git repository at its root revision
    valueFiles: [values/media.yaml, secrets/media.sops.yaml]
    values: {replicas: 1}            # inline overrides are applied last
  destination: {host: nas}
  syncPolicy: {automated: {}}
```

`valuesFrom` is required when an OCI chart uses `valueFiles`; it only applies to OCI charts. Its `repoURL`
defaults to the root repository. An empty `targetRevision` uses the root's revision for that repository,
or `HEAD` for another repository. Set either field explicitly to select another values repository or pin
a branch, tag or commit. File paths are relative to the values repository root, and symlinks/paths cannot
escape it. SOPS files use the same mounted age identity and masking described below.

Refreshes and push webhooks include the values repository. Sync and app refresh fetch it immediately;
each preparation reads files from its resolved commit. The chart is pinned to a digest before rendering
and deployment, so a moving OCI tag cannot change it midway through a sync.

The API/UI and `composecd app get NAME` show the desired chart digest and desired/deployed values commits.
Host releases record the chart in `release.Chart.Ref` and values commit in the `composecd/values-revision`
label. The values commit and chart digest both participate in desired-state identity: a values-only commit
can auto-sync even when its changed fields aren't used in the rendered YAML. Unchanged revisions don't
continually resync. Rollbacks retain the selected revision's values label and pause automated sync as usual.

[The optional OCI example](examples/oci-values/README.md) shows a root-repository values file without
adding a registry dependency to the default GitOps applications.

## Encrypted values with SOPS

Set `sopsAgeKeyFile` in the server's `composecd.yaml` and mount your age identity read-only:

```yaml
sopsAgeKeyFile: /run/secrets/sops-age-key
```

The container image includes checksum-verified SOPS **3.13.3** for amd64 and arm64. Outside the image,
install the [SOPS CLI](https://github.com/getsops/sops/releases) on the server's `PATH` and generate your
own identity with [age-keygen](https://github.com/FiloSottile/age). Keep the identity outside Git. Encrypt
values with its public recipient, then list the encrypted file alongside regular values files:

```sh
sops --encrypt --age YOUR_AGE_RECIPIENT values.yaml > values.sops.yaml
```

```yaml
spec:
  source:
    path: charts/media
    valueFiles: [values-nas.yaml, values.sops.yaml]
```

Files named `*.sops.yaml` / `*.sops.yml`, or containing top-level `sops:` metadata, are decrypted through
`sops --decrypt --output-type yaml --input-type yaml FILE`. Stdout is parsed in memory; no decrypted
values file is created in the checkout. File order and inline overrides work as before. Missing keys,
missing SOPS, malformed metadata and decryption errors make the app's sync state `Unknown`, naming the
values file. Raw SOPS diagnostics are suppressed because they can contain sensitive data.

`composecd validate DIR` checks that encrypted filenames have SOPS metadata without decrypting them.
It also checks local referenced values in sibling chart directories when it can find the surrounding Git
root. External source repositories are checked when the controller prepares them. SOPS' own `.sops.yaml`
creation-rules file is left alone.

Decrypted scalar values of four characters or more are masked with `••••••` in diffs, API/UI output,
operation output and errors, container logs, and notifications. Plain values aren't masked. Old values
remain masked after rotation: composecd reads the values that retained releases saved at the encrypted
files' keys, which works after a restart and even without the age identity, since SOPS keeps keys in plaintext. Operation logs are masked before SQLite
storage. Application and container names remain usable as routing identifiers. Masking is best effort:
short values and transformed values (base64, URLs, escaping, etc.) can still be visible.

**Deployed values stay plain.** Composetpl stores user values and rendered manifests on each host; Docker
and the containers also receive the plain values. Protect the release volume/local release directory and
Docker access. Masking does not encrypt those records, and after old history is purged, a restarted
controller can only recover secrets from the values and history still available.

[The example](examples/sops/README.md) includes a disposable public test identity, a real encrypted values
file, and an optional demo Application. Use your own identity for real secrets.

## Secrets from Infisical

As an alternative to SOPS files, an Application can read secrets straight from
[Infisical](https://infisical.com). Create a machine identity with Universal Auth and read access to the
project, then give composecd its credentials in `composecd.yaml`:

```yaml
infisical:
  siteURL: https://app.infisical.com   # default; set it for a self-hosted server
  clientID: 3f1c...                    # the identity's client ID
  clientSecretFile: /run/secrets/infisical-client-secret
  # or, instead of clientID/clientSecretFile: tokenFile: /run/secrets/infisical-token
```

Then select the secrets in the Application:

```yaml
spec:
  source:
    path: charts/media
    valueFiles: [values-nas.yaml]
    infisical:
      projectId: 1b2c3d4e-...
      environment: prod
      path: /media          # default /
      recursive: false
      into: secrets         # optional dotted values path; default is the top level
```

Each secret key becomes a value key (`.Values.secrets.DB_PASSWORD` above). With `recursive: true`,
secrets in subfolders are nested by folder name: a secret `CF_DNS_API_TOKEN` in the folder `/media/traefik`
is `.Values.secrets.traefik.CF_DNS_API_TOKEN`, so a template can use
`{{- range $key, $value := index .Values.secrets "traefik" }}`. A secret and a folder with the same name
at one level collide, which is an error. Secrets are merged after
`valueFiles` and before inline `values`, so inline values still win. Secret references and imports are
resolved by Infisical. Values are fetched on every refresh, so rotating a secret in Infisical shows up as
a diff on the next refresh and, with automated sync, is deployed.

Fetched values are masked exactly like SOPS values (see above), including after rotation: composecd
remembers the keys it has seen and recovers older values from release history. Unlike SOPS, the key names
are only known after a successful fetch, so after a restart an old value is masked only once Infisical has
been reachable again. A failed fetch makes the app's sync state `Unknown`; the error never includes the
server's response. `siteURL` must be `https://` (plain `http://` is allowed for localhost only). As with
SOPS, deployed values are stored plain on the host. The same masking limits apply.

## Webhooks, notifications and metrics

**Webhooks.** Set `webhook.secretFile` in `composecd.yaml`, then add a push webhook in your Git server that
points at `https://<composecd>/api/v1/webhook`, using the same secret and the JSON content type. Gitea,
Forgejo and GitHub sign the payload with it; GitLab sends it as its token. A push to the root, or to any
Application's chart/values or ApplicationSet generator repository, triggers a refresh right away.

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
| Encrypted values | SOPS + age via a mounted identity; plaintext stays out of the checkout and composecd output |
| Web UI, roles, local sign-in and SSO | Embedded htmx UI with SSE, local users, API tokens and OIDC |
| OCI chart with Git values | `valuesFrom` repository, independent chart/values revisions, SOPS and automatic sync |
| ApplicationSets | Git-directory and Host-label generators, safe templates and zero-result protection |
| Adoption | Unmanaged discovery and admin Application YAML export; commit and sync to upgrade in place |
| Image updater | Planned; see [docs/PLAN.md](docs/PLAN.md) |

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
