# SOPS example

The identity in `test-age-key.txt` is **public test data**. Never use its recipient
for real secrets. The encrypted fixture at `../gitops/secrets/demo.sops.yaml`
contains only `sops-example-message` and `example-only-password`.

From the repository root, with SOPS installed:

```sh
SOPS_AGE_KEY_FILE="$PWD/examples/sops/test-age-key.txt" \
  sops --decrypt --output-type yaml examples/gitops/secrets/demo.sops.yaml
```

To deploy the sample, copy `examples/gitops` into a GitOps repository, copy
`application.yaml` into that repository's `composecd/` directory, and configure:

```yaml
# composecd.yaml, outside the GitOps repository
sopsAgeKeyFile: /run/secrets/sops-age-key
```

Mount `test-age-key.txt` read-only at that path. Refresh, inspect the app's diff,
and run `composecd app sync secret-demo`. The deployed environment contains the
plain values; composecd output masks them. The regular examples don't enable
this app, so they still work without SOPS or an age identity.

For real secrets, generate your own identity with `age-keygen`, keep it outside
Git, and encrypt YAML using its public recipient:

```sh
sops --encrypt --age YOUR_AGE_RECIPIENT values.yaml > values.sops.yaml
```

`recipient.txt` holds the public recipient for this disposable fixture. The
standard `.sops.yaml` creation-rules file is configuration, not an encrypted
values file; composecd's filename validation leaves it alone.
