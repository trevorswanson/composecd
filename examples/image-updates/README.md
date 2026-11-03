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
