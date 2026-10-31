# ApplicationSet example

With `examples/gitops` as a GitOps repository, replace its explicit Applications
with `stacks.yaml` under `composecd/`. Keep the Host and SyncWindow objects.
Do not append the set alongside explicit apps with the same names; collisions
are reported and the last valid object snapshot is retained.

The set scans `charts/*` for directories containing `Chart.yaml`, excluding the
optional encrypted-values demo. The default example remains unchanged until
you opt into this file. Inspect generated apps with `composecd appset list`,
`composecd app list`, or the ApplicationSets UI page, then enable automation in
the template when ready. Per-chart custom values can be added to the template
when every generated application can read them; inline values are templated too.

For a chart in a separate repository, configure `repoURL` / `targetRevision` on
`gitDirectories`. A generated Git source without an explicit repository inherits
that generator's repository and revision.

For one copy per selected host, use a `hosts` generator:

```yaml
apiVersion: composecd/v1alpha1
kind: ApplicationSet
metadata: {name: agents}
spec:
  generators:
    - hosts: {selector: {role: server}}
  template:
    metadata: {name: 'agent-{{ .host.name }}'}
    spec:
      source: {path: charts/agent}
      destination: {host: '{{ .host.name }}'}
```

Create the `charts/agent` chart first. The two generator types each produce
independent results; they are not a matrix. Missing template parameters fail the
set. String helpers such as `lower`, `replace` and `kebabcase` are available;
file/environment/network helpers are not.

Directory/host removals honor the release's recorded prune policy. Zero output
from a previously populated set is guarded, including after a controller restart.

To update the generated apps a few at a time, add a `RollingSync` strategy.
`rolling.yaml` updates DNS servers one host at a time, so one keeps serving
while the other updates, and a failed or unhealthy update halts the rollout.
Follow it with `composecd appset rollout unbound`. See
[Rolling sync](../../README.md#rolling-sync).
