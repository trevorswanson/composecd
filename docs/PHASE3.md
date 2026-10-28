# Phase 3 plan: UI, auth, ApplicationSets, SOPS, multi-source, adoption

## Context

Phases 0–2 gave composecd a working controller with a CLI, a REST API and a read-only status page. Its
automation covers sync, self-heal, prune, windows, dependencies, retries, events, notifications, webhooks and
metrics. Auth is one admin token. Phase 3 of [PLAN.md](PLAN.md) makes it something you run day to day:
- a real web UI with roles, so you can operate from a browser and give read-only access to others;
- ApplicationSets, so a new stack is just a new directory;
- encrypted secrets in Git;
- values for OCI charts kept in Git;
- a path to adopt the composetpl releases you already have.

**Decisions:**
- **UI:** server-rendered Go templates plus htmx, embedded in the binary, with Server-Sent Events (SSE) for
  live updates. No Node toolchain.
- **Auth:** API tokens with roles, OIDC, and local users with passwords. No forward-auth header: an
  authenticating proxy can keep using `auth.disabled: true`.
- **ApplicationSet generators:** Git directories, and Hosts by label. No list, matrix or Git-files generators.
- **SOPS:** an age identity file (`sopsAgeKeyFile`) mounted into the container. Values are decrypted in memory
  and masked in the UI and API.

**Starting condition:** no Phase 3 code until the real-Docker integration tests (a separate session) report
and any fixes from them are merged. Phase 3 builds on the reconcile path those tests exercise.

---

## Roles and auth model (cross-cutting)

Two roles:

| | `readonly` | `admin` |
| --- | --- | --- |
| Apps, hosts, windows, operations, history, diffs (secrets masked) | ✓ | ✓ |
| Container logs | ✓ | ✓ |
| Sync, rollback, pause, resume, refresh | | ✓ |
| Host maintenance | | ✓ |
| Manage users and tokens | | ✓ |
| Export adoption YAML with saved values | | ✓ |

Ways to authenticate:
- **Bearer tokens.**
  - The bootstrap admin token (`auth.tokenFile` or the generated `admin-token`) keeps working as a break-glass
    admin.
  - New named tokens are created with `composecd token create NAME --role readonly|admin`. They look like
    `ccd_<id>_<secret>`, are shown once, and are stored as SHA-256 hashes. `last_used` is recorded.
  - Tokens are revoked with `token revoke NAME`.
- **Browser sessions.**
  - Signing in with a local password or OIDC creates a server-side session: a random ID stored in SQLite,
    sent as a `HttpOnly`, `SameSite=Lax` cookie, with a 7-day idle expiry.
  - The current "token in a cookie, GET only" login is replaced.
- **CSRF.**
  - Cookie-authenticated requests that change anything must send `X-CSRF-Token`. The token is per session and
    rendered into a `<meta>` tag; htmx sends it through `hx-headers`.
  - Bearer-token requests are exempt.
  - `SameSite=Lax` (not `Strict`) is needed for the OIDC redirect back to composecd.
- **Local users.**
  - Managed with `composecd user add NAME --role` (prompts for the password) and `user passwd` / `user delete`.
  - Passwords are hashed with bcrypt (`golang.org/x/crypto`).
  - Failed logins are rate-limited per user and per IP, with a short exponential lockout.
- **OIDC.** Configured in `composecd.yaml`:

  ```yaml
  auth:
    oidc:
      issuer: https://auth.example.lan/application/o/composecd/
      clientID: composecd
      clientSecretFile: /run/secrets/oidc-secret
      redirectURL: https://composecd.example.lan/auth/callback
      scopes: [openid, profile, email, groups]
      groupsClaim: groups
      adminGroups: [homelab-admins]
      readonlyGroups: [homelab-users]   # empty = any other signed-in user is readonly; list them to restrict
  ```

  - Uses `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`, with the authorization-code flow and PKCE.
  - State and nonce are kept in a short-lived signed cookie.
  - Users from OIDC aren't stored, apart from their sessions.

**Code:**
- A new `internal/auth` package holds `Principal{Name, Role, Via}`, `Authenticator.Identify(r)`, the session
  store, password hashing, token minting and checking, the OIDC flow and rate limiting.
- `internal/api` swaps `authorized()` for middleware that requires a role per route.
- New tables in `internal/store`: `users`, `tokens` and `sessions`.
- API: `GET/POST/DELETE /api/v1/tokens` and `/api/v1/users` (admin only), and `GET /api/v1/me`.
- The audit trail records who acted: `store.Operation` gets a `User` column, filled from the principal for
  manual operations, and the UI and `app operations` show it.

---

## Workstreams (one PR each, in this order)

### 3a. Auth foundation (delivered)
Covers everything in the roles and auth model above, plus the CLI (`token create|list|revoke`,
`user add|passwd|delete|list`, and `login`, which saves the server and token to
`~/.config/composecd/config.yaml` so `--token` isn't needed every time).

The existing admin token and `auth.disabled` behave as before. The current status page keeps working: it signs
in with a local user, OIDC, or the admin token entered as a password.

**Tests:**
- role enforcement per route;
- CSRF rejection;
- token hashing, revocation and `last_used`;
- bcrypt plus lockout;
- OIDC against an in-process fake issuer (`httptest`) that signs ID tokens;
- session expiry.

### 3b. Web UI (htmx) (delivered)
- The host page shows orphaned releases now; unmanaged discovery and **Adopt** are added in 3f.
- `Subscribe` uses `Update` for its invalidations because `controller.Change` already names a resource diff.
- **Files.**
  - Pages and partials as `html/template` files under `internal/web/templates`, loaded with `go:embed`.
  - htmx and its SSE extension vendored under `internal/web/static` at pinned versions (0BSD license, noted in
    NOTICE). No CDN.
  - The current `page.go` styles move into one stylesheet, keeping light and dark themes.
- **Pages.**
  - **Applications:** a filterable list, by host, sync, health and label, with live updates.
  - **Application detail**, in tabs:
    - Summary: source, revisions, why automation is waiting, retry state.
    - Containers: state, health, image, and a live log tail.
    - Diff: per-resource unified diff, colorized and masked.
    - History: composetpl revisions, each with a rollback button.
    - Operations: each with its output.
    - Action buttons (sync, pause and resume, refresh) for admins.
  - **Hosts:** state, versions, a maintenance toggle, and orphaned and unmanaged releases with **Adopt** (3f).
  - **Sync windows:** open and closed, and which apps they match.
  - **Operations:** recent activity across apps.
  - **Settings:** tokens and users (admin only), and who you're signed in as.
- **Live updates.**
  - `GET /events` (SSE) pushes `app`, `host` and `operation` events.
  - The controller gains a small pub/sub (`Subscribe() (<-chan Change, cancel)`), fed from `setStatus`,
    `finishOperation` and host pings.
  - Pages swap in the changed row or panel with `hx-swap-oob`.
- **Logs.**
  - A new `hosts.Backend.Logs(ctx, h, container, tail, follow, w)` wraps `docker logs`.
  - It's exposed as SSE at `GET /api/v1/apps/{name}/containers/{container}/logs?follow=1&tail=200`.
  - The container must belong to the app's compose project.
  - Lines are masked like diffs.
- **Accessibility and layout.** Real buttons and links, so it works without JavaScript for reading. Usable at
  phone width.

**Tests:**
- template rendering for each page from a stub controller;
- SSE framing and the fan-out from pub/sub;
- the logs endpoint refusing containers from other projects;
- htmx actions sending CSRF tokens.

### 3c. SOPS values files (delivered)
- Pinned SOPS 3.13.3 with per-architecture checksum verification; no Go SOPS/KMS dependency.
- Retained release user values restore masking after restart and across rotations/rollbacks.
- **Decrypting.**
  - Set `sopsAgeKeyFile` in `composecd.yaml`.
  - A values file is decrypted when its name matches `*.sops.yaml` / `*.sops.yml`, or when it has top-level
    `sops:` metadata. Decryption runs the **`sops` CLI** (`sops --decrypt --output-type yaml FILE` with
    `SOPS_AGE_KEY_FILE`).
  - The image adds a pinned `sops` release, with its checksum verified in the Dockerfile. For runs outside the
    container, the docs say to install it.
  - The CLI is used for the same reasons composecd already drives `git` and `docker`. The Go `decrypt` package
    pulls in every cloud KMS SDK, and a hand-written SOPS parser would be security-sensitive code to own.
- **Masking.**
  - `source.Prepared` gains `Secrets []string`, the decrypted leaf values of four characters or more.
  - A per-app masker replaces them with `••••••` in diffs, manifests, operation logs, container logs and
    notification messages.
  - Release records on the hosts still hold the plain values, as composetpl documents.
- **Status.** A missing key or a decryption failure is an app error (`Unknown` sync), with the file named.
- **Validation.** `validate` checks that a `.sops.yaml` file actually has SOPS metadata.

**Tests:**
- a fake `sops` on `PATH` (like the events tests);
- masking of diffs and logs;
- the not-encrypted and wrong-key errors.

### 3d. Values from Git for OCI charts (delivered)
```yaml
source:
  chart: oci://ghcr.io/me/charts/media
  version: ^1.2
  valuesFrom: {repoURL: "", targetRevision: ""}   # default: the root repository at the root's revision
  valueFiles: [values/media.yaml]                 # relative to valuesFrom's repository root
```
- `config` validation: `valueFiles` with `chart` now needs `valuesFrom`. Paths are checked as they are today.
- `RefreshAll` fetches the `valuesFrom` repositories. `prepare()` checks the values repository out at its
  resolved commit. SOPS (3c) applies to these files too.
- The sync status shows both revisions:
  - the chart: the pinned OCI reference and digest, already in `release.Chart.Ref`;
  - the values: a new `composecd/values-revision` label.

  The desired-state key includes the values commit, so a values-only change auto-syncs.
- Webhooks match `valuesFrom` repositories too.

### 3e. ApplicationSets (delivered)
```yaml
kind: ApplicationSet
metadata: {name: stacks}
spec:
  generators:
    - gitDirectories:
        repoURL: ""                 # default: the root repository
        targetRevision: ""          # default: the root's revision
        directories: ["stacks/*"]   # path.Match globs; one app per matching directory with a Chart.yaml
        exclude: ["stacks/old-*"]
    - hosts:
        selector: {role: server}    # every Host with these labels
  template:                         # an Application; strings are Go templates over the generator's params
    metadata:
      name: "{{ .path.basename }}"
      labels: {appset: stacks}
    spec:
      source: {path: "{{ .path.path }}"}
      destination: {host: nas}
      syncPolicy: {automated: {prune: true}}
```
- **Parameters.**
  - `gitDirectories` gives `.path.path`, `.path.basename` and `.path.segments`.
  - `hosts` gives `.host.name` and `.host.labels`.
  - Each generator produces its own set of Applications; there's no matrix. So "node-exporter on every server"
    is a `hosts` generator with `destination: {host: "{{ .host.name }}"}` and
    `name: "node-exporter-{{ .host.name }}"`.
- **Rendering.**
  - `text/template` with `missingkey=error`, applied to every string in the template. Sprig is limited to
    string functions, so a template can't read files or the environment.
- **Where it runs.**
  - `config.Objects` gains `ApplicationSets`. Expansion happens in the controller after loading the root,
    because Git directories need a checkout and the hosts generator needs the Hosts.
  - Generated Applications are then validated with the explicit ones: name collisions are errors, and
    `dependsOn` works across both.
  - Generated apps get `Origin: "applicationset stacks (stacks/media)"` and the label
    `composecd/applicationset: stacks`.
- **Removal.** When a directory or host disappears, its app disappears, and Phase 2 prune applies if the
  template set `prune`.
  - As with an empty root, an ApplicationSet that suddenly generates nothing is reported, not pruned.
  - This guard needs a new `generated: 0, was: N` check.
- **Interfaces.** `GET /api/v1/applicationsets`, `composecd appset list` and a UI page list each set with the
  apps it generates and any rendering errors.

**Tests:**
- generation from a temporary Git repo;
- host selectors;
- template errors reported per set;
- name collisions;
- the empty-generator guard;
- a generated app being pruned when its directory is removed.

### 3f. Adopt existing releases (delivered)
- Admin-only YAML export contains saved user values; inventory remains readonly and omits values.
- Export is a suggestion, with automation off and warnings for secrets, missing charts and project directory changes.
- Orphaned releases can be exported; conflicting Application names and pending operations are blocked.
- Plain project config paths are read from container labels because the pinned Runner.Ls omits that field.
- **Listing.** Per host, list composetpl releases that composecd doesn't manage, and compose projects that
  aren't composetpl releases at all. This uses `Store().List()` and `compose.Runner.Ls`.
  - Exposed as `GET /api/v1/hosts/{name}/unmanaged`, `composecd host unmanaged NAME`, and on the UI hosts page.
  - `GET /api/v1/hosts/{name}/adopt/{release}` returns YAML and warnings as JSON for admins; the UI renders the same preview.
- **Adopting a composetpl release.**
  - `composecd adopt HOST RELEASE` (and **Adopt** in the UI) prints an `Application` YAML.
  - The YAML has the same name, `destination.host`, the chart from `release.Chart.Ref` (a path under the root
    is suggested if it's in the repository, otherwise the OCI ref), and the release's user values as `values`.
  - composecd doesn't write to Git (that arrives with the Phase 4 image updater). You commit the YAML, and the
    next sync upgrades the same release in place, since the release name is the app name.
  - `app diff` beforehand shows exactly what will change.
- **Plain compose projects.** These are listed with their config file paths, and a hint to convert them to a
  chart with `composetpl create`. There's no automatic conversion.

---

## Order and parallelism

1. **3a auth comes first:** the UI's actions and user tracking depend on it.
2. **Then 3b UI, alongside 3c, 3d and 3e** in separate sessions and worktrees.
   - 3c, 3d and 3e touch `config` and `source`, while 3b mostly touches `internal/web` and `internal/api`.
   - 3d and 3e both edit `config/types.go` and `controller.RefreshAll`, so run them one after the other, or
     merge one before starting the other.
3. **3f last:** it's small and adds to both the API and the UI.

The README, `docs/PLAN.md` status lines and `examples/` (an ApplicationSet over `charts/*`, a `.sops.yaml`
values file with a test age key, an OIDC config block) are updated in each PR.

## Verification
- **Unit and end-to-end tests** in each PR, as listed above. `go test -race ./...` and
  `golangci-lint run ./...` must pass.
- **Integration tests** (from the integration-tests session) extended:
  - SOPS-encrypted values deploying for real;
  - an ApplicationSet generating apps on the CI engine;
  - the logs endpoint streaming a real container;
  - adopting a release installed with `composetpl install`.
- **Manually, in a browser** (the in-app preview):
  - sign in with a local user and with OIDC (Authentik or Pocket ID in a container);
  - check read-only users can't act;
  - sync, roll back and toggle maintenance from the UI and watch live updates;
  - view masked diffs and logs at desktop and phone widths, in light and dark mode.
- **Release:** the image builds with `sops` included (CI's `image` job), and `composecd version` and
  `sops --version` run inside it.

## Risks and open points
- **Integration findings may reshape the core paths** that the UI and ApplicationSets rely on. That's why
  nothing starts until the tests report.
- **Masking is best effort.** Very short secrets (under four characters), and values transformed by templates
  (base64, URLs), aren't caught. The docs will say so.
- **Logs and diffs are visible to readonly users** (masked). If that's too much exposure for your setup, a
  later option can make them admin-only.
