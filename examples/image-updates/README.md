# Image updates with a semver strategy

Copy `application.yaml` into your GitOps repository's `composecd/` directory,
next to the [gitops example](../gitops)'s `charts/whoami`. Its image is
`traefik/whoami:v1.10` in Git; with `strategy: semver`, composecd lists the
repository's tags and deploys the highest that matches the constraint, the
`allowTags` regexp and none of the `ignoreTags` globs, pinned to its digest:

```
$ composecd app get whoami-semver
...
IMAGES:
SERVICE  IMAGE                 STRATEGY      TAG                PINNED BY  DEPLOYED             LATEST               UPDATE
web      traefik/whoami:v1.10  semver ~1.10  v1.10.1 → v1.10.3  composecd  sha256:3f1c9a0b2d4e  sha256:9ab07c1e5f20  available: sync to apply
```

Tags are listed on `imageUpdates.schedule` in `composecd.yaml`, or when you run
`composecd app check-images whoami-semver`. A new tag makes the app OutOfSync
with a reason like `image traefik/whoami (web): v1.10.1 → v1.10.3`, and the
automated sync deploys it. Nothing is written to Git: the pick lives in
composecd's database, and rolling back pauses auto-sync so it isn't deployed
again.

## Writing the tag back to Git

`writeback.yaml` runs the same chart with `writeBack: git`: each new tag is
committed to Git, and the commit is what deploys. Copy it into `composecd/`
and `values/whoami-writeback.yaml` into the repository's `values/`. The tag is
set in `charts/whoami/values.yaml`, which the other whoami Applications read
too, so composecd adds it to this app's own values file instead:

```
$ git log -1 -p
    composecd: update images

    whoami-writeback/web: traefik/whoami v1.10 → v1.10.3 (values/whoami-writeback.yaml tag)

    Composecd-Image-Update: true
...
 port: 8083
+tag: v1.10.3
```

composecd's Git credentials need write access to the branch. To review the
updates before they deploy, set `imageUpdates.writeBack.branch` in
`composecd.yaml` to another branch and merge from it.

When a tag can't be written (say the values file is SOPS-encrypted), the update
is held: nothing new deploys for the service, and an admin decides:

```
$ composecd app image-decision whoami-writeback web --dismiss
Dismissed v1.10.3 for whoami-writeback/web until a newer tag. To update from Git, set tag in values/whoami-writeback.yaml.
$ composecd app image-decision whoami-writeback web --in-memory
Applying v1.10.3 to whoami-writeback/web in memory; the deployment diverges from Git.
```

One service can use another policy with compose labels, which beat the
Application's settings:

```yaml
services:
  worker:
    image: ghcr.io/me/worker:main-1042
    labels:
      composecd.image-updates.strategy: newest-build
      composecd.image-updates.allow-tags: "^main-"
```

`newest-build` deploys the allowed tag whose image was built last. It reads one
image config per candidate, so it needs `allow-tags` and looks at the 50 newest
tags by name.
