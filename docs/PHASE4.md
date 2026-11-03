# Phase 4 plan: image updater, ignoreDifferences, signatures, housekeeping, the agent

**Status:** 4a delivered (composetpl v0.5.0: post-render hook and `sync-fail`), 4b delivered (semver and
newest-build strategies), 4c delivered (write-back to Git with held updates), 4f delivered (host housekeeping).
4d, 4e and 4g are not started.

## Context

Phases 0–3 of [PLAN.md](PLAN.md) give composecd everything needed to run a homelab from Git: reconcile,
self-heal, prune, windows, dependencies, a web UI with roles, ApplicationSets, SOPS and Infisical secrets,
OCI charts with Git values, adoption and rolling ApplicationSet syncs. Phase 4 adds the extras from the
feature table:
- **the rest of the image updater:** new tags (semver and newest build), not just new digests, and writing
  what it picks back to Git;
- **ignoreDifferences**, for fields another tool manages;
- **commit signature verification** for the repositories composecd deploys from;
- **host housekeeping:** scheduled image pruning and disk-usage warnings;
- **an outbound agent**, for hosts the controller can't reach (behind NAT, a VPS without an open engine port).

Digest pinning (`imageUpdates`) is already done: moving tags are pinned at render time to digests stored in
SQLite, and digests pinned in Git are only reported (`internal/controller/images.go`, `internal/images`).

**Decisions:**
- **Write-back is opt-in.** New tags are applied in memory by default, as digests are today. Writing to Git is
  turned on per Application.
- **Write-back edits the chart's values in place**, in the file that sets the tag, rather than a separate
  override file. composecd traces which values key produces each `image:` by rendering with sentinels, and
  edits only that scalar's bytes. composecd never writes a separate override file.
- **Write-back never silently falls back to memory.** When an update can't be written to Git, it is held, and an
  admin decides: apply it in memory (with a warning that the deployment will diverge from Git), or dismiss it
  and fix the source by hand. Until then nothing is deployed for that service.
- **Signatures:** both GPG and SSH signatures (`git verify-commit` with `gpg.format`), with keys from files
  mounted next to `composecd.yaml`. Only the commit composecd deploys is verified, as in Argo CD. A repository
  whose commits are signed but not verified is flagged with a hint to turn verification on.
- **The agent tunnels the Docker API** and nothing else. The controller exposes each connected agent as a unix
  socket, so the docker CLI, compose, events, logs, helper containers and release volumes all work unchanged.
- **New dependency for the agent:** `github.com/hashicorp/yamux` (MPL-2.0, noted in NOTICE) to multiplex
  streams over one upgraded HTTP connection.

**Starting condition:** none. composetpl work (4a) can start now; nothing else waits on it except the cleanup
in 4b.

---

## Workstreams (one PR each)

### 4a. composetpl prerequisites (in `docker-helm-template`, tag v0.5.0)
- **Post-render hook.** `action.Upgrade.PostRender` and `action.Install.PostRender`, a
  `func(manifest []byte) ([]byte, error)` run after the merge and before the manifest is validated, saved as
  pending and applied. `DiffUpgrade` and `Render` take the same option so a dry run matches.
  - composecd moves image pinning there, replacing `pinRelease` (which edits the pending release between
    `Upgrade.Prepare` and `Apply` and runs `compose config` itself). 4b's tag overrides and 4d's
    `respectIgnoreDifferences` use the same hook.
- **`sync-fail` hook event** (left over from Phase 2): `hooks.SyncFail`, run when install, upgrade or rollback
  fails, before `--atomic` recovery, from the `markFailed` paths. composecd maps it to Argo's SyncFail and shows
  its output in the operation.

**Tests (composetpl):** a post-render that rewrites an image lands in the saved revision and in `compose up`;
an error from it fails the upgrade before anything runs; `sync-fail` ordering with and without `--atomic`.
**composecd:** bump to v0.5.0, delete `pinRelease`, keep `images_test.go` green.

### 4b. Image update strategies
Today each service is pinned to the digest of the tag it already has. 4b lets a service move to a new tag.

```yaml
syncPolicy:
  imageUpdates:
    enabled: true
    strategy: semver          # digest (default) | semver | newest-build
    constraint: "~1.27"       # semver only; default: same major as the current tag
    allowTags: '^\d+\.\d+\.\d+$'   # regexp tags must match (semver and newest-build)
    ignoreTags: ["*-rc*"]     # path.Match globs
```
- **Per service** with compose labels, as `composecd.image-updates` works today:
  `composecd.image-updates.strategy`, `.constraint`, `.allow-tags`. A label beats the Application.
- **semver:** list the repository's tags (oras `Repository.Tags`), keep those that parse as semver, match
  `allowTags`/`ignoreTags` and the constraint (Masterminds/semver, already an indirect dependency), and pick the
  highest. Prereleases only when the constraint names one. The chosen tag is then pinned to its digest, as now.
- **newest-build:** among tags matching `allowTags` (required, so a repository with thousands of tags isn't
  walked), the one whose image config has the newest `created`. This reads one config blob per candidate tag,
  so results are cached by digest and the candidate list is capped (default 50, newest tags by name first).
- **Where the choice lives.** A new `image_tags` table keyed by (app, service) holds the chosen tag, its
  digest, when it was picked and the tag list's check time. `image_digests` keeps caching tag → digest.
- **Applying it in memory.** `images.PinManifest` grows into `images.Override`, which rewrites `image:` to
  `repo:newtag@digest`. The sync reason reads `image nginx (web): 1.27.1 → 1.27.3`, and the diff shows it.
  Rolling back restores the old revision's image, and pauses auto-sync as today, so the update isn't
  re-applied on the next reconcile.
- **Not updated:** images pinned to a digest in Git (reported, as today), images using `${VAR}`, `build:`
  services and hooks. A semver strategy on a tag that isn't semver (`latest`) is an image warning, not an error.
- **Status and UI.** `ImageStatus` gains `Strategy`, `Tag` (current) and `Candidate` (what the strategy picked).
  The images tab lists them; notifications say `nginx (web): 1.27.3 available (semver ~1.27)`.
- **Limits.** Tag lists are fetched on `imageUpdates.schedule` and on **Check images**, never on a refresh.
  Docker Hub's tag list is paginated and rate-limited, so the plan caps pages and reports a truncated list.

**Tests:** a fake `Resolver` that also lists tags; semver picking (constraints, prereleases, allow and ignore);
newest-build with a capped candidate list; labels beating the Application; a Git digest pin left alone;
rollback not re-applying the update; masking unaffected.

### 4c. Write-back to Git
```yaml
syncPolicy:
  imageUpdates:
    strategy: semver
    writeBack: git            # memory (default) | git
```
```yaml
# composecd.yaml: who composecd commits as
imageUpdates:
  writeBack:
    branch: ""                # default: each source's targetRevision, which must then be a branch
    author: {name: composecd, email: composecd@localhost}
    signingKeyFile: /etc/composecd/signing-key   # optional SSH key, needed with verifySignatures (4e)
```
The tag is written into the chart's values where it already lives, so Git keeps one source of truth and a
`git diff` of the update reads like a change you'd make by hand. The digest stays pinned in memory, as today.

- **Finding the key (sentinel tracing).** A chart only shows composecd its output, so composecd works out which
  values key feeds each `image:` by experiment, using the offline render `validate --render` already does:
  1. Take the merged values' scalar leaves whose string appears in the service's image (`1.27.1`, `nginx`,
     `ghcr.io/me/app`), plus `.Chart.AppVersion`.
  2. For each, render with that leaf replaced by a unique sentinel and look for the sentinel in the service's
     `image:`. Leaves that land in the tag part are the tag key. This costs a few renders per service, only
     when an update is about to be written, and the result is cached by (chart commit, values commit).
  3. A label overrides the tracing when it guesses wrong or finds more than one key:
     `composecd.image-updates.tag-key: apps.web.tag` (or `Chart.appVersion`).
- **Choosing the file.** Values are merged in layers (chart `values.yaml`, each `valueFiles` entry, inline
  `values:`), and the last layer that sets the key wins. composecd edits that layer when it is a plain YAML file
  in Git that only this Application reads, or the Application's own inline `values:` in its YAML in the root.
  - **A shared file** (a chart `values.yaml` behind several apps, as with ApplicationSets over values-only
    directories) is edited only when every app reading it gets the same update. Otherwise the key is added to
    the Application's last app-specific Git values file, which overrides the shared value for this app only.
  - **A shared key** (`global.tag` feeding several services) is edited only when every service it feeds picks
    the same tag.
  - **`.Chart.AppVersion`** edits `appVersion` in `Chart.yaml` of a Git chart.
- **Editing without reformatting.** The file is parsed with `yaml.v3` only to find the scalar node; composecd
  then replaces exactly that scalar's bytes (by line and column), keeping its quoting style. Comments, ordering
  and indentation are untouched. Adding a missing key to an app's values file appends the shortest path that
  doesn't clash with existing keys.
- **When it can't write back**, the update is **held**, never applied on its own: the tag comes from a SOPS
  file, from Infisical, from text in a template, from an ApplicationSet template's inline values, from an OCI
  chart's packaged defaults with no Git values file to override it in, or tracing was ambiguous. Giving up on
  Git as the record defeats the point of composecd, so only a person can choose to.
  - The image shows `Held: <reason>` and a `write-back-blocked` notification fires (once per candidate). The app
    stays Synced on its current image; automation does nothing for that service.
  - An admin chooses, in the images tab (`POST /api/v1/apps/{name}/images/{service}/decision`,
    `composecd app image-decision NAME SERVICE --in-memory|--dismiss`):
    - **Apply in memory.** A confirmation states that the deployment will diverge from Git (Git says `1.27.1`,
      the host will run `1.27.3`) and that a re-render from Git alone won't reproduce it. The choice covers this
      service until its source changes (a new chart or values commit for the traced file, or the label), and is
      recorded with the user in the operation log. The image and app show a persistent `Diverged from Git`
      badge while it is in force.
    - **Dismiss and fix manually.** The candidate is ignored until a newer one appears. The UI shows what to
      change (the file and key when tracing found one, or why it couldn't) so the fix can be made in Git; once
      Git renders the new image, the hold clears itself.
  - Decisions live in a new `image_decisions` table keyed by (app, service), with the candidate, the choice,
    who made it and the source fingerprint it applies to. A changed fingerprint drops the decision, so a
    remediation in Git is never overridden by an old in-memory choice.
  - Editing SOPS files with `sops set` is a possible follow-up that would remove one of the hold reasons.
- **After writing**, the next render must produce the chosen image. If it doesn't (the trace was wrong), the
  commit is still the record, but composecd reports an error on the image and stops writing for that service
  until the label or the chart changes.
- **Committing without a work tree.** In the bare mirror: build the new tree with a private index
  (`read-tree` the remote tip, `update-index --cacheinfo` each changed file), `commit-tree` (with `-S` and
  `gpg.format=ssh` when a signing key is set), then `push origin <sha>:refs/heads/<branch>`. A rejected push
  (someone pushed meanwhile) fetches, re-traces against the new tip and retries, three times, then reports an
  error on the image. This goes in `gitcache` as `Cache.CommitFiles(ctx, url, branch, files, msg, author)`.
- **One commit per repository per check**, covering every service of every app that moved, with a message such
  as `composecd: update images` and one line per change. Charts in other repositories get their own commits.
  No commit when nothing changed.
- **Loops.** composecd's own commit arrives back through the webhook or poll and renders the same images, so
  nothing moves again. The commit's trailer `Composecd-Image-Update: true` lets webhooks skip the extra refresh.
- **Sync windows.** Commits happen whenever a check finds an update; deploying them still waits for windows,
  `dependsOn` and rollouts like any other Git change. With `branch:` set to something other than the deployed
  branch, nothing deploys until you merge, which gives a review step without composecd opening pull requests.
- **Credentials.** The `repositories` entry for each written repository needs write access. A failing push is
  an image error and a notification (`image-update` with the error), never a sync failure.
- **`adopt`** can use the same commit path later; out of scope here.

**Tests:**
- tracing: a direct `{{ .Values.tag }}`, `repo:tag` split across two keys, `printf`, `.Chart.AppVersion`, a
  literal in a template (not traceable), a `global.tag` feeding two services, and the label override;
- file choice: winning layer edited; shared chart `values.yaml` left alone and the key added to the app's file;
  inline `values:` in the Application YAML; SOPS and Infisical refused with a reason;
- holds: nothing deployed until a decision; apply-in-memory deploys and shows the divergence; dismiss ignores the
  candidate until a newer one; a Git change to the traced source drops the decision; readonly users can't decide;
- byte-exact edits: comments, quotes and indentation preserved (golden files);
- a local bare repository: a concurrent push forces a re-trace and retry; a signed commit verifies with 4e's
  checker; a webhook for composecd's own commit doesn't re-check registries.

### 4d. ignoreDifferences
```yaml
spec:
  ignoreDifferences:
    - services: ["web", "worker-*"]   # path.Match globs; default every service
      paths: [environment.LOG_LEVEL, labels.traefik.*, deploy.replicas]
    - services: ["watchtowered"]
      imageDigest: true               # ignore only @sha256:… on image:
  syncPolicy:
    options: {respectIgnoreDifferences: true}
```
- **Paths** are dotted paths into a compose service, with `*` matching one key. They are matched after the
  normalization the live view already does (`normalizeDesired` in `live.go`), so list and mapping forms of
  `environment` and `labels` are the same path. `networks/NAME` and `volumes/NAME` resources accept paths too.
- **Git vs. deployed.** Ignored paths are removed from both manifests before `diff.Manifests`, so a change only
  in ignored fields keeps the app Synced, and the desired-state key leaves them out, so it doesn't auto-sync.
- **Live drift.** Repair's drift is per service, by config hash, so it can't say which field changed. For each
  `changed` service, the controller compares the live container's projection (`projectContainer`) with the
  desired service, field by field, and drops the drift when every difference is ignored. Self-heal then leaves
  it alone. The live view marks ignored fields as ignored rather than as differences.
- **Syncing anyway.** A sync for another reason deploys Git's values for ignored fields, as in Argo CD. With
  `respectIgnoreDifferences`, the post-render hook (4a) copies ignored fields from the deployed release
  instead. It can't copy what another tool changed on a live container; the README says so.
- **Validation:** unknown top-level keys, empty `paths` without `imageDigest`, and paths that can't match.

**Tests:** table tests for path matching over both compose forms; Synced with only ignored changes; a drifted
container whose only difference is ignored isn't healed; `respectIgnoreDifferences` keeping the deployed value.

### 4e. Commit signature verification
```yaml
# composecd.yaml
repositories:
  - url: https://git.example.lan/me/homelab.git
    verifySignatures: true
    allowedSignersFile: /etc/composecd/allowed_signers   # SSH signatures
    gpgKeyringFile: /etc/composecd/trusted.gpg           # GPG signatures (armored or binary)
```
- **What is verified:** the commit composecd resolved for each source (root, chart repositories, `valuesFrom`),
  every time it resolves a new SHA. Results are cached by SHA. Tags are verified as the commit they point to.
- **How:** `git verify-commit --raw` in the mirror, with a throwaway `GNUPGHOME` that imports
  `gpgKeyringFile`, and `gpg.ssh.allowedSignersFile` for SSH signatures. The image adds `gnupg`; SSH
  verification uses the existing `openssh-client`.
- **Failing verification:**
  - an app's source → an app error (sync `Unknown`) naming the commit and why; nothing is synced;
  - the root → composecd keeps the last verified objects and reports the error on every app, like a root that
    fails to load. An ApplicationSet's Git directories use the root's result.
- **Status.** The app shows the signer (key fingerprint or SSH principal). Signing information is in
  `GET /api/v1/apps/{name}` and the Summary tab.
- **Write-back (4c)** must sign its commits when the root verifies signatures; validation says so if
  `signingKeyFile` is missing, and the docs show adding composecd's key to `allowed_signers`.
- **`validate`** gets `--verify-signatures` for CI, using the same checker.
- **Signed but not verified.** For a repository without `verifySignatures`, composecd looks at the last 20
  commits of each branch it deploys from whenever it resolves a new SHA, checking for a `gpgsig` header
  (`git cat-file commit`, no keys needed). When most of them are signed, it shows a hint:
  `18 of 20 recent commits on main are signed; set verifySignatures on this repository in composecd.yaml`.
  - Shown on the app Summary, on a new **Repositories** page (`GET /api/v1/repositories`,
    `composecd repo list`) listing each repository's verification state, signed ratio and last signer, and as a
    `validate` warning.
  - A hint, not an error or a notification, and nothing changes until you turn verification on. Its threshold
    (default 80%) is `signatureHint.threshold` in `composecd.yaml`, and `0` turns the hint off.
  - With verification on, the same page shows which allowed key or principal signed the deployed commit, and
    flags signed commits whose key isn't in the allowed list, a common mistake when turning it on.

**Tests:** a temporary repository with an SSH-signed, a GPG-signed, an unsigned and a wrongly signed commit;
root fallback to the last verified objects; the cache not re-running gpg for a known SHA; the hint at, above
and below the threshold, and absent once verification is on.

### 4f. Host housekeeping
```yaml
kind: Host
metadata: {name: nas}
spec:
  url: ssh://deploy@nas.lan
  housekeeping:
    imagePrune:
      schedule: "0 4 * * 0"    # cron; off when unset
      timeZone: Europe/Berlin
      until: 168h              # only images unused for a week
      all: false               # true also removes unused tagged images, not just dangling ones
    builderPrune: {schedule: "0 4 * * 0", keepStorage: 10GB}
    diskWarning: {percent: 85}
```
- **Pruning** runs `docker image prune --force --filter until=…` (with `--all` when set) and
  `docker builder prune`. It waits for the host's running operations (`Host.Use` held for writing, as
  `Invalidate` does), skips hosts in maintenance and down hosts, and records an operation.
  - `operations` gains a `host` column; housekeeping operations have kind `image-prune` or `builder-prune` and
    no app. The host page lists them, and so does `composecd host operations NAME`.
  - **Rollbacks after `all: true`** need to pull the old image again; the README says so. Images of stopped
    containers, including composetpl's volume accessor, are never removed by `image prune`.
- **Disk usage.** On each host ping (throttled to every 10 minutes), `docker system df --format json` gives
  images, containers, volumes and build cache with reclaimable sizes. Free space on the engine's data root comes
  from a short-lived helper container (the existing `StartHelper` path) running `df -P` on `DockerRootDir`
  read-only, using the image composetpl's volume accessor already pulled.
  - Above `diskWarning.percent`, the host shows a warning and a new `host-disk` notification event fires once
    per crossing.
  - Metrics: `composecd_host_disk_used_ratio`, `composecd_host_reclaimable_bytes{kind}`.
- **Manual:** `composecd host prune NAME [--all]`, `POST /api/v1/hosts/{name}/prune` and a button on the host
  page, admin only.

**Tests:** schedule evaluation reused from windows; prune skipped under maintenance and while an operation
runs; `system df` and `df -P` parsing; the warning firing once per crossing.

### 4g. Outbound agent
```bash
# on the remote machine
docker run -d --restart unless-stopped -v /var/run/docker.sock:/var/run/docker.sock \
  -e COMPOSECD_AGENT_TOKEN_FILE=/run/secrets/agent-token ghcr.io/trevorswanson/composecd \
  agent --server https://composecd.example.lan --host vps
```
```yaml
kind: Host
metadata: {name: vps}
spec: {url: "agent://", projectRoot: /opt/composecd}
```
- **Tunnel.** The agent sends `GET /api/v1/agent/connect` with `Upgrade: composecd-agent/1`, then runs a yamux
  session over the connection. The controller listens on `<dataDir>/agents/<host>.sock`; each connection the
  docker CLI makes to it opens a yamux stream, and the agent copies that stream to its local Docker socket.
  `Host.Global()` turns `agent://` into `--host unix://<dataDir>/agents/<host>.sock`, so `hosts.CLI` is
  unchanged.
- **Auth.** A new token role `agent`, bound to one host: `composecd token create vps-agent --role agent
  --host vps`. It can open only that host's tunnel and call nothing else. One session per host: a new
  connection replaces the old one, and the replaced agent backs off.
- **Liveness.** No session → the host is `Down` with "agent not connected", like a failed ping. yamux
  keepalives detect dead connections; the agent reconnects with jittered backoff up to a minute.
- **Behind proxies.** The upgrade goes through the same HTTPS listener as the UI, so a reverse proxy has to
  pass `Upgrade`; the README shows Caddy and Traefik. Plain `ws://` isn't needed.
- **What the agent can do:** nothing on its own. It never accepts commands, only byte streams to its Docker
  socket, and it can't reach any other address. The controller can do anything the socket allows, exactly as
  with `ssh://`.
- **Updating the agent.** Run it with composetpl or plain compose. Managing it as a composecd Application would
  cut the tunnel mid-operation, the same problem self-update solved with a helper container; that is out of
  scope and the docs say so.
- **UI and API.** The host page shows the agent's version, remote address and connected-since time.

**Tests:** an in-process agent and controller over `httptest` with a fake Docker socket echoing HTTP; a docker
CLI call through the unix socket; token scoping (a readonly token, another host's agent token); replacement of
a second session; reconnect after the server restarts. Integration: the CI `docker:dind` engine reached through
an agent, running the existing sync and self-heal tests.

---

## Order and parallelism

1. **4a first**, in composetpl. It is small and unblocks the cleanup in 4b and `respectIgnoreDifferences` in 4d.
2. **Then, in separate sessions and worktrees:**
   - 4b image strategies (touches `internal/images`, `controller/images.go`, `store`);
   - 4d ignoreDifferences (touches `config`, `controller/reconcile.go`, `live.go`, `objects.go`);
   - 4e signatures (touches `gitcache`, bootstrap `Repository`, `Dockerfile`);
   - 4f housekeeping (touches `hosts`, `store`, the host page).

   4b and 4d both edit `assess` in `reconcile.go`, and 4b and 4f both add to `store`; merge one before rebasing
   the other.
3. **4c write-back after 4b and 4e**: it builds on 4b's tag choice and must sign commits 4e will verify.
4. **4g agent last**, or alongside 4c once 4f has merged (both touch `hosts` and the host page). It is the
   largest piece and the most security-sensitive, so it gets its own review.

Each PR updates the README, the `docs/PLAN.md` status line and `examples/` (a semver image policy, a chart whose values
tag composecd writes back, an `allowed_signers`, a host with housekeeping, an agent host).

## Verification
- **Unit and end-to-end tests** in each PR, as listed above. `go test -race ./...` and
  `golangci-lint run ./...` must pass.
- **Integration tests** (`-tags integration`):
  - a local registry (`registry:2`) with tags `1.0.0`, `1.1.0`, `2.0.0`: semver `^1` deploys `1.1.0`, and
    write-back commits it to a local bare repository;
  - an unsigned commit to the root blocks the next sync, and a signed one goes through;
  - a dangling image is removed by a manual prune, and an image in use is not;
  - the agent path: the dind engine reached only through `composecd agent`.
- **Manually:** in the browser, the images tab shows strategy and candidate, the host page shows disk usage
  and prune history, and an agent host shows connected and then Down when the agent stops. Check phone width and
  both themes.

## Risks and open points
- **Registry tag lists are slow and rate-limited**, Docker Hub's most of all. Strategies other than `digest`
  only run on the schedule, and `allowTags` is required for `newest-build`.
- **Held updates can pile up** when a chart's tag isn't traceable. The images tab and the `write-back-blocked`
  notification make them visible, and the fix (a `tag-key` label or moving the tag into a values file) is
  shown with each hold.
- **Write-back changes your repository.** It only edits the one scalar that holds a tag, it never force-pushes,
  and it is off unless an Application asks. Branch protection that rejects composecd's pushes shows as an
  image error.
- **Sentinel tracing can be fooled** by charts that transform values (`upper`, `trimPrefix`) or pick images
  conditionally. It refuses rather than guesses when a trace is ambiguous, verifies the next render, and the
  `tag-key` label is the escape hatch.
- **ignoreDifferences on live containers is best effort**: it depends on how faithfully `live.go` projects a
  container back to compose fields. Fields it doesn't project can't be ignored, and the docs list them.
- **The agent widens the attack surface of the composecd server**: an agent token lets its holder present a
  Docker socket to the controller. A malicious agent can feed the controller false state but can't run anything
  on the controller; the threat model goes in the Security section of the README.
- **Signature verification depends on `gpg` behaving the same everywhere.** Tests pin the outputs composecd
  parses (`--raw` status lines) rather than human-readable messages.
