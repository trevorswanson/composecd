# OCI chart, Git values

After publishing `examples/gitops/charts/whoami` to your own OCI registry, replace
the sample registry reference in `application.yaml` and copy the Application into
your GitOps repository's `composecd/` directory. Keep `values/whoami.yaml` from
`examples/gitops/values/` at that repository's root.

`valuesFrom: {}` uses that Git root's repository and revision. For a separate
values repository, use:

```yaml
valuesFrom:
  repoURL: https://git.example.lan/me/values.git
  targetRevision: main
valueFiles: [values/whoami.yaml]
```

Files are relative to the values repository root. Regular and SOPS-encrypted
files can be mixed in order, with inline `values` applied last. Encrypted files
need the server's `sopsAgeKeyFile` configuration.

Inspect `composecd app get whoami-oci` for the chart's pinned digest and the
values commit before running `composecd app sync whoami-oci`. Add automated sync
when ready. A new values commit triggers a new sync even if a changed value is
unused by the chart. Push webhooks for the values repository trigger refreshes.
