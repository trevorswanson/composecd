# Offline monitoring inventory

`composecd monitoring export composecd --repo-root .` exports desired state from
an existing local checkout. It does not add a controller, ApplicationSet generator
kind, API endpoint or long-running service. It is an offline build tool for a
repository-owned renderer (for example, Gatus YAML generation).

ApplicationSets use the same `internal/appset.Generate` implementation as the
controller: host selectors, Git directory globs, exclusions and Application
parameter substitution all retain their existing semantics. Explicit Applications
and generated Applications are validated together, including duplicate names.
The export refuses empty sets and skipped directories rather than treating a
partial discovery as an instruction to remove monitors. An intentionally empty
set must be removed from the input before exporting.

## Values and contract

Put public, literal monitoring declarations in chart `values.yaml`, ordered
`source.valueFiles`, or `source.values`. The exporter uses composetpl's
`values.Merge` with that precedence. Lists replace rather than append. The
`monitoring` subtree has no special meaning to normal chart deployment; the
consumer defines and validates its schema. Nothing is inferred from Traefik
labels, containers, host availability or chart templates.

Example values:

```yaml
monitoring:
  group: infrastructure
  endpoints:
    - name: http
      url: https://example.com/health
      conditions: ['[STATUS] == 200']
```

Output contract:

```json
{
  "version": 1,
  "applications": [
    {
      "name": "example",
      "host": "edge",
      "monitoring": {
        "group": "infrastructure",
        "endpoints": [
          {"name": "http", "url": "https://example.com/health", "conditions": ["[STATUS] == 200"]}
        ]
      }
    }
  ]
}
```

Applications are sorted by name. Undeclared monitoring is `null`; unrelated values,
source configuration, host environment and credentials are not included. Output
is buffered until the entire inventory succeeds. A downstream renderer should
validate version and schema, reject empty inventories by default, produce stable
IDs, detect collisions, write atomically and offer a non-writing check mode.
Preserve the last committed output when either export or rendering fails.

## Offline boundary

- Supports local Git-path charts in this checkout. Source and directory-generator
  `repoURL` and `targetRevision` must be empty. The caller selects the checkout;
  the exporter never fetches Git. External repositories, explicit revisions and
  OCI charts are rejected, even if they currently have no monitors.
- No Docker, running ComposeCD API, chart rendering, dependency vendoring,
  environment expansion, SOPS decryption or Infisical requests occur.
- Infisical declarations are permitted only with `into` scoped outside
  `monitoring`, such as `into: secrets`. Unscoped or overlapping injection is
  rejected because it could change the effective monitoring values.
- Encrypted values files are rejected rather than silently ignored. Chart,
  values-file and object paths must remain within the checkout. Object symlinks
  are rejected; chart/value-file symlinks may not escape the checkout.
- **Monitoring is a public configuration boundary, not a secret detector.**
  Never put credentials in it. A consumer should reject secret-bearing fields
  and interpolate nothing unless it has an explicit runtime secret contract.
- The command resolves declarations, not Gatus condition syntax, reachability,
  chart schemas, deployed state or downstream publication. Those need separate
  validation. Declared endpoints stay present when hosts/containers fail; removing
  an Application or endpoint from Git is an intentional monitoring deletion that
  should be reviewed in the generated diff.

Commit generated config with its source declarations. CI can repeat the export
and compare downstream renderer output without repository write credentials or
production secrets. Deploy the resulting chart through the ordinary reconciler.
