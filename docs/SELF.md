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

```go
// Prepare renders the chart, validates it with `compose config`, and saves the
// release with status pending-install or pending-upgrade. It runs no hooks and
// touches no containers.
func (u *Upgrade) Prepare(ctx context.Context, name, chartPath string, values map[string]interface{}) (*release.Release, error)

// Apply deploys a release saved by Prepare (or a pending rollback): it runs the
// pre hooks, compose up, and the post hooks, then marks the release deployed, or
// marks it failed and, if Atomic is set, rolls back to the last deployed revision.
// Applying a revision that is no longer pending is an error.
type Apply struct {
    DeployOptions            // Wait, Timeout, Pull, ...
    Atomic     bool
    Timeout    time.Duration // hooks
    HistoryMax int
}
func NewApply(cfg *Configuration) *Apply
func (a *Apply) Run(ctx context.Context, name string, revision int) (*release.Release, error)

// Abandon marks a pending release failed without touching containers, for
// recovery when the process applying it died.
func Abandon(cfg *Configuration, name string, revision int, reason string) error
```

`Upgrade.Run` becomes `Prepare` followed by `Apply`. `Install` and `Rollback` get the same `Prepare` and `Apply`
pair. `Repair` doesn't create a new revision, so it gets a separate mode instead: `Apply` with `ForceRecreate`
on the deployed revision, flagged as a repair so that it doesn't change status.

This ships as composetpl v0.3.0. The change is additive, and composecd raises its required version.

---

## 4. The helper

### `composecd self-apply`

A hidden subcommand that runs in the helper container:

```
composecd self-apply --release composecd --revision N \
  --project-root /opt/composecd --storage volume --wait-timeout 5m [--rollback-to M | --repair]
```

- It only talks to the local engine, through the mounted socket. It doesn't load `composecd.yaml`, open SQLite
  or start a server.
- It calls `action.Apply` with `Wait: true` and `Atomic: true`. With `--wait`, compose waits for the image's
  `HEALTHCHECK` (`/healthz`) to report healthy. A container that fails its health check, or never starts,
  counts as a failure and triggers the rollback.
- Logs go to stdout, which `docker logs` keeps. The exit code is 0 for deployed, 1 for failed and rolled back,
  and 2 for failed with the rollback also failed.

### Starting it

When the reconciler decides to sync, repair or roll back the self app:

1. **Drain.** Stop starting new operations, and wait (bounded, default 2m) for running ones to finish. Each
   app that's waiting shows `Waiting: composecd is updating itself`. If the drain times out, skip self this
   pass and try again on the next one.
2. **Prepare** the release (`Prepare`, or the matching rollback or repair setup). Record the operation as
   `Running` with `kind=sync`, as usual, plus a new `helper` column holding the helper's container name.
3. **Start the helper:**
   ```
   docker run -d --name composecd-self-apply-<opID> \
     --label composecd.self-apply=<opID> \
     -v /var/run/docker.sock:/var/run/docker.sock \
     <our current image ID> self-apply ...
   ```
   - The helper uses our **current image ID**, not the new tag. The old image is known to work and is already
     pulled, so a broken new image can't break the process that's meant to roll it back.
   - The helper gets the socket path and env from our own container's inspect data, so a non-default socket
     mount still works.
   - Don't pass `--rm`, because we need its exit code and logs.
4. **Follow the helper** by streaming `docker logs -f` into the operation log, so the UI shows progress. At some
   point the old composecd is stopped. That's expected.

### Boot recovery

On startup, before the first reconcile, composecd looks for helpers and pending self releases:

| State found | Action |
| --- | --- |
| A helper container is still running | Keep its operation `Running` (don't mark it interrupted). Leave self alone, follow the helper's logs, and wait for it to exit. |
| A helper container has exited | Read its exit code and logs, finish the operation as `Succeeded` or `Failed`, send a notification, and remove the helper. |
| The self release is pending and there's no helper | The helper died, for example because the host rebooted. Call `Abandon` on the release, mark the operation failed, and let the normal reconcile decide again. |
| A helper exists for an operation not in the DB | This can happen after a DB restore. Wait for it to exit, then remove it, and log a warning. |

To support this, `store.Open` no longer marks operations interrupted across the board. Operations with a
`helper` set are skipped and handled by the recovery step above.

### Cleanup

composecd removes exited helpers whose operation it has finished. As a backstop, it also removes any
`composecd.self-apply` container older than 24h.

---

## Config changes

The chart puts `composecd.yaml` into `configs.content`, so with self-management, composecd's own config is in
Git as well. A bad change to `root`, `repositories` or the auth settings can leave the new composecd unable to
read Git, and then it can't pull the fix.

- **Parse and validation errors are caught.** composecd validates the self app's rendered config with the same
  code as `composecd validate` before it starts the helper. If the config is invalid, the operation fails
  before anything restarts.
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
| B | composetpl `Prepare`, `Apply` and `Abandon`, released as v0.3.0 | none (separate repo) |
| C | `self-apply`, starting the helper, drain, boot recovery, `helper` column, reconciling self last | A, B |
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
