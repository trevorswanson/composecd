# Self-management: composecd deploys composecd

## Context

[PLAN.md](PLAN.md) says that composecd can manage itself: you put an Application for
[deploy/chart](../deploy/chart) in Git, and composecd keeps its own container up to date. No code handles that
case today. You can already add the Application and see it as `Synced` or `OutOfSync`. Syncing it breaks
composecd, as described below.

This document covers the design for review. It doesn't include any code.

**Goal:** a commit that changes composecd's own Application, such as a Renovate PR that bumps `tag` in the values
file, is synced like any other app. If the new version doesn't come up healthy, composecd rolls back to the
previous one by itself, with no one on hand to do it.

**Non-goals:**
- **Watching registries for new tags.** Updates come from Git. Bump the tag with Renovate or Dependabot.
- **Managing a composecd on another engine.** "Self" is only the copy running on the engine composecd is
  attached to. When one composecd deploys another composecd to a different host, that's a normal app.
- **High availability.** composecd is down while its container is recreated, usually for a few seconds.

---

## Why a self-sync breaks today

composetpl's `action.Upgrade.Run` (v0.2.0) runs these steps in one call:

1. Render the chart, and save the new release with status `pending-upgrade`.
2. Run the `pre-upgrade` hooks.
3. Run `docker compose up --detach --remove-orphans` as a child process (`compose.DockerExec`).
4. Run the `post-upgrade` hooks, mark the release `deployed`, and prune old revisions.

In step 3, the child process runs inside the composecd container. To recreate the `composecd` service, compose
first stops the old container. That kills tini, composecd and the `docker compose` process that is doing the
recreate. What's left:

| Problem | Why |
| --- | --- |
| composecd stays down | The new container is never created or started. The old one was stopped through the API, so `restart: unless-stopped` doesn't start it again. |
| The release is stuck | The release stays `pending-upgrade`, so every later `Upgrade.Run` fails with `errPending`. |
| Other operations are lost | Any other app's operation still running is marked "interrupted" at the next start ([store.go:140](../internal/store/store.go)). This part works as intended. |

The same happens for Repair (self-heal) and Rollback, since both also run `compose up`.

---

## Design overview

**The process that recreates composecd's container must not run inside that container.** composecd prepares
the release itself, then hands step 3 onward to a short-lived **helper container**. The helper is a separate
container from the same image, so it survives the recreate, waits for the new container to be healthy and
records the result. If the new version is unhealthy, the helper rolls it back.

```
composecd (old)                    helper (old image)                 composecd (new)
───────────────                    ──────────────────                 ───────────────
reconcile: self is OutOfSync
drain other operations
Prepare: render, save rev N
  as pending-upgrade
docker run -d helper ───────────▶  Apply rev N:
                                     pre-upgrade hooks
                                     compose up --wait  ── stops ──▶ (old exits)
                                                        ── starts ─▶ boots, sees helper
                                                                     running, leaves
                                                                     self alone
                                     healthy? post-upgrade hooks,
                                       mark deployed
                                     unhealthy? mark failed,
                                       roll back to rev N-1
                                     exit 0 / 1
                                                                     sees helper exited:
                                                                     read exit code and logs,
                                                                     finish operation, remove
                                                                     helper, resume self
```

This is the same approach Portainer uses to update itself. It needs four pieces:

1. **Detecting self:** composecd knows which Application is its own.
2. **Guards:** prune, uninstall, events and the like never act on self as they would on a normal app.
3. **A composetpl split:** "prepare a release" and "apply a release" become separate calls, so the helper only
   applies a manifest that's already stored.
4. **The helper:** a `composecd self-apply` subcommand, the code that starts it, and recovery on boot.

---

## 1. Detecting self

At startup and on every refresh, composecd works out which Application is self:

1. **Find our own container ID.** Read `/proc/self/mountinfo` and find a bind mount whose source path
   contains `containers/<64-hex id>/`, such as the one for `/etc/hostname` (Docker and Podman). Don't rely on
   `os.Hostname()`, because a chart can set `hostname:`. If no ID is found, composecd isn't running in a
   container, so there's no self. (`controller.ContainerID`, called once at startup.)
2. **Look for that container on every Host** that is up, with `docker container inspect <id>`. Container IDs
   are random 256-bit values, so finding ours on a Host means that Host reaches our engine, whatever its URL
   (`ssh://nas.lan` pointing at the same machine still matches). Record its `com.docker.compose.project`
   label for that Host. This replaced the planned comparison of engine IDs from `docker info`, because it
   needs no new backend call. A Host that's down, or that couldn't be asked, keeps its last result.
3. **Match an Application.** Self is the Application whose name equals the project label on its Host.

An explicit override is available for unusual setups:

```yaml
kind: Application
metadata: {name: composecd}
spec:
  self: true          # optional; an error on the app if it contradicts detection
```

An app marked `self: true` is remembered for the life of the process, so prune keeps protecting it after it
leaves Git. If detection found composecd under a different project on that Host, the app shows an error, and
both apps get the guards.

**If our container has no compose project label** (for example after `docker run`, as the README quick start
shows), there's no self. An Application for the composecd chart would then start a second composecd that
fights the first one over the port and the data volume. The planned `SelfUnmanaged` condition was dropped:
nothing reliably tells such an Application apart from any other. [Bootstrap](#bootstrap) covers switching to
the chart first.

The result is exposed as `AppStatus.Self`, shown as a "self" badge in the UI and as `self` in the AUTO column
of `composecd app list`. `HostStatus.Self` and `UnmanagedRelease.Self` name the self release on each host.

---

## 2. Guards

These guards apply even before the helper exists. With only these in place, you can safely keep composecd's
Application in Git, and get diffs and drift for it. (Delivered in step A.)

| Area | Guard | Where |
| --- | --- | --- |
| Prune | Never uninstall the self release, whether its Application left Git or moved to another host. It's listed as an orphan on its host instead. No notification: one would repeat on every refresh. | `prune` in [prune.go](../internal/controller/prune.go) |
| Delete orphan | `DeleteRelease` refuses the self release with `ErrSelf` (409). The hosts page shows "composecd itself" in place of the Delete button. | `DeleteRelease` in [prune.go](../internal/controller/prune.go) |
| Volumes | Covered by the two guards above: prune and delete are the only paths that uninstall, and they keep volumes anyway (`--keep-history`, no `-v`). | none |
| Docker events | Ignore events from the self project. Our own stop and start during an update must not start a reconcile. | `onEvent` in [controller.go](../internal/controller/controller.go) |
| Sync, Repair, Rollback | Until the helper ships, the self app can't sync automatically or self-heal: it shows as blocked. A manual sync or rollback returns `ErrSelf` (409). The UI disables Sync and hides Roll back. Once the helper ships, these go through it (section 4). | `gate` in [automation.go](../internal/controller/automation.go), `manualAllowed` in [operations.go](../internal/controller/operations.go) |
| Pause and maintenance | Work as usual. Pausing self, or putting its host in maintenance, holds back self-updates. | none |

Reconciling self last in each pass moved to step C: while self never syncs, the order doesn't matter.

---

## 3. composetpl split: Prepare and Apply

The helper shouldn't render charts. Rendering needs Git credentials, SOPS keys, Infisical access, `lookup`
against the host and the right chart version. All of that lives in composecd's config and secrets. Instead,
composecd renders and stores the release, and the helper only applies what's stored.

composetpl gains a two-step API. The existing `Run` methods become wrappers around it, so the CLI and current
callers don't change.

Delivered in [composetpl#4](https://github.com/trevorswanson/composetpl/pull/4):

```go
// Install.Prepare and Upgrade.Prepare render the chart, check it with `compose
// config`, and save the revision as pending-install or pending-upgrade. They run
// no hooks and touch no containers.
func (u *Upgrade) Prepare(ctx context.Context, name, chartPath string, values map[string]interface{}) (*release.Release, error)
func (i *Install) Prepare(ctx context.Context, name, chartPath string, values map[string]interface{}) (*release.Release, error)

// Apply deploys the latest revision, which must be pending install or upgrade:
// pre hooks, compose up, post hooks, then deployed; or failed, and with Atomic
// torn down (install) or rolled back to the last deployed revision (upgrade).
type Apply struct {
    DeployOptions // Wait, Timeout (also for hooks), Pull, ...
    Atomic      bool
    HistoryMax  int
    Description string
}
func NewApply(cfg *Configuration) *Apply
func (a *Apply) Run(ctx context.Context, name string, revision int) (*release.Release, error)

// Abandon marks a pending revision failed without touching containers.
func Abandon(cfg *Configuration, name string, revision int, reason string) error
```

`Install.Run` and `Upgrade.Run` are now `Prepare` followed by `Apply`, so the CLI's behavior doesn't change.
**Change from the plan:** Rollback and Repair aren't split. They render nothing and only read stored
revisions, so the helper runs `Rollback.Run` and `Repair.Run` whole.

Until composetpl v0.3.0 is tagged, composecd's `go.mod` requires the PR's commit as a pseudo-version.

---

## 4. The helper

### `composecd self-apply`

A hidden subcommand that runs in the helper container ([selfapply.go](../internal/selfapply/selfapply.go)):

```
composecd self-apply --release composecd --storage volume --wait=true --timeout 5m --history-max 10 \
  (--revision N --description "composecd sync to abc1234" | --rollback-to M --label k=v | --repair) \
  [--env KEY=VALUE ...] [--local-dir DIR] [--pull P] [--force-recreate]
```

- It doesn't load `composecd.yaml`, open SQLite or start a server. It builds a composetpl configuration for
  one engine and its release storage, and runs `Apply` (with `Atomic: true`), `Rollback` or `Repair`.
- `--wait` is always on for the self app, whatever the Application's `options.wait`. compose then waits for the
  image's `HEALTHCHECK` (`/healthz`), so a new version that never turns healthy fails and is rolled back.
- `--env` carries the Host's and Application's `env` for compose's `${VAR}` interpolation, as a normal sync
  passes them.
- Output goes to stdout, which `docker logs` keeps. The exit code is 0 on success and 1 on failure, whether or
  not the rollback worked. The operation's message says which, from the helper's last line of output.

### Starting it

When automation or a user syncs, self-heals or rolls back the self app
([selfupdate.go](../internal/controller/selfupdate.go)):

1. **Drain.** No new operation starts on any other app (`ErrBusy`: "composecd is updating itself"). composecd
   waits up to 2 minutes for running ones to finish. If they don't, the self operation fails, and automation
   tries again later.
2. **Prepare** the revision, for a sync. A self-heal or rollback has nothing to prepare.
3. **Start the helper,** after recording its name in the operation's new `helper` column:
   ```
   docker run --detach --name composecd-self-apply-<opID> --label composecd.self-apply=<opID> \
     --volumes-from <our container> [--env DOCKER_HOST=... | --env DOCKER_CONTEXT=...] \
     -- <our image ID> self-apply ...
   ```
   - The helper uses our **current image ID**, not the new tag. The old image is known to work and is already
     present, so a broken new image can't break the process that's meant to roll it back.
   - **Change from the plan:** instead of mounting the socket by itself, `--volumes-from` gives the helper every
     volume composecd has: the socket wherever it's mounted, the data directory (for `storage: local`), and any
     TLS certificates or SSH keys. The engine is selected as composecd selects it for that Host: its `url` or
     `context`, or composecd's own `DOCKER_HOST`.
   - No `--rm`, because composecd needs the exit code and logs.
4. **Follow the helper:** `docker logs --follow` streams into the operation log, and `docker wait` gets the exit
   code. Usually the old composecd is stopped before this finishes. If it isn't (for example, when the change
   didn't touch the composecd service, or failed before `up`), it finishes the operation, reads the release
   revision the helper left, and removes the helper.

### Boot recovery

At the first refresh after startup, once self is detected and its host is up, composecd finishes the
operations helpers ran (`recoverSelf`):

| State found | Action |
| --- | --- |
| A helper container is still running | Keep its operation `Running`, hold other operations, follow the helper's logs, and wait for it to exit. |
| A helper container has exited | The same as above, but it's over at once: read its exit code and logs, finish the operation (which sends the usual notification), and remove the helper. |
| The helper container is gone | Fail the operation, and `Abandon` the self release if its latest revision is still pending. |
| The self release is pending and no operation has a helper | `Abandon` it, and let the normal reconcile decide again. |

`store.Open` no longer marks operations with a `helper` interrupted; `HelperOperations` lists them. After a
successful rollback through a helper, an automated self app is paused, as for any rollback.

### Cleanup

Every refresh, after boot recovery, composecd lists the `composecd.self-apply` containers on each host its own
container runs on (`sweepHelpers`). It leaves alone the helpers its operations are waiting for, and handles
the rest:

| Helper found | Action |
| --- | --- |
| Running, and no operation is waiting for it (a restored DB, or an operation whose `docker wait` failed) | It may be replacing composecd right now, so no operation starts until it exits. Then it's removed. |
| Exited, and no operation is waiting for it | Removed, with a warning if its operation isn't in the DB. |
| Older than 24 hours, running or not | Removed, by force. A helper finishes within its timeout, so it's stuck or forgotten. An operation waiting for it then fails. |

Helpers carry a second label, `composecd.self-apply.app`, naming the app they update.

---

## Config changes

The chart puts `composecd.yaml` into `configs.content`, so with self-management, composecd's own config is in
Git as well. A bad change to `root`, `repositories` or the auth settings can leave the new composecd unable to
read Git, and then it can't pull the fix.

- **Parse and validation errors are caught.** After preparing a sync of the self app, composecd looks in the
  rendered manifest for a compose `config` mounted at the path it read its own configuration from (`--config`,
  `/etc/composecd/composecd.yaml` in the chart), and parses its `content` as the new composecd will. If that
  fails, composecd abandons the prepared revision and fails the operation, so nothing restarts. A config that
  comes from a `file:`, or that's mounted anywhere else, isn't checked. Compose `${VAR}` interpolation in the
  content isn't applied first (only `$$` is unescaped).
- **Errors at runtime aren't caught.** A wrong repo URL or an expired token produces a valid config, the new
  version comes up healthy, and composecd then can't fetch Git. Recovering means editing the config on the host
  and restarting. This is the same as running composecd without self-management, and it's documented.
- **`/healthz` stays as it is.** It doesn't fail on Git errors. If it did, a normal Git outage during an update
  would roll back a good version, and the rollback would then fail its health check as well.

---

## Bootstrap

1. Install the chart once by hand, with the same `projectRoot` and `storage` as the Host composecd will use:
   `composetpl install composecd deploy/chart -f values.yaml`.
2. Commit an Application named `composecd` that targets that Host, with `source.path` pointing at a copy of the
   chart or at the OCI chart, and the same values. The release already exists, so composecd takes it over
   without adopting it, and shows `Synced` if the values match.
3. Turn on `syncPolicy.automated`.

Anyone who started with `docker run` has to switch to the chart once (step 1), stopping the old container
first. The README quick start gets a "Managing composecd with composecd" section covering this.

---

## Order and parallelism

| Step | Content | Depends on |
| --- | --- | --- |
| A (delivered) | Detecting self, the guards, `AppStatus.Self`, UI badge. Sync of the self app is refused. | none |
| B (in review) | composetpl `Prepare`, `Apply` and `Abandon`, to be released as v0.3.0 | none (separate repo) |
| C (in review) | `self-apply`, starting the helper, drain, boot recovery, `helper` column, reconciling self last | A, B |
| D | README bootstrap section, example Application in `examples/gitops` | C |

A and B can run in parallel. A ships on its own and is useful without the rest.

---

## Verification

These integration tests use real Docker, alongside the existing `*_integration_test.go`:

1. **Detection.** Run composecd from the chart and assert that `Self` is true for its Application, and false
   for the same composecd when started with `docker run`.
2. **Guards.** Remove the self Application from Git with `prune: true`. The release and the `data` volume stay,
   and the condition is set. `DeleteRelease` returns 409.
3. **Successful update.** Build two tags of a test image, deploy tag 1, then commit tag 2. Assert that the
   container runs tag 2, the release is `deployed` at revision N, the operation `Succeeded`, the helper was
   removed, and an operation on another app that was running when the update started completed rather than
   being interrupted.
4. **Bad image.** Commit a tag whose health check fails. The helper rolls back. Assert that tag 1 is running,
   revision N is `failed`, N+1 is a rollback to N-1, and a notification was sent.
5. **Helper killed.** Kill the helper while `compose up` is running, then restart composecd by hand. Boot
   recovery abandons the pending release, and the next reconcile syncs again.
6. **Invalid config.** Commit a self config that doesn't validate. The operation fails before any container
   restarts.

---

## Risks and open points

- **The helper needs the Docker socket.** It's the same image and the same access composecd already has, so
  this adds no new permissions.
- **Overlap on the data volume.** compose stops the old container before it starts the new one, so two
  composecd servers never share SQLite. The helper doesn't open SQLite. If a chart ever sets
  `deploy.update_config.order: start-first`, there would be two servers at once. Validation should reject that
  for self.
- **The helper's image is cleaned up.** When the image is pruned while the helper uses it, `docker run` fails.
  composecd then fails the operation and retries on the next pass.
- **Hooks in the self chart** run in the helper, not in composecd. The chart doesn't have any hooks today.
  Document that hooks in the self chart can't call the composecd API during an update.
- **Self is detected automatically (decided).** `spec.self` is an optional override for unusual setups, not a
  requirement. Requiring it would be more explicit, but it adds one more field to get right, and forgetting it
  is exactly how a self-sync breaks.
- **Drain timeout.** A long-running sync on another app (a big image pull) delays the self-update. Two
  minutes, then retry on the next pass, seems right. It could be made configurable later.
