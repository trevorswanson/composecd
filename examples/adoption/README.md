# Bring an existing composetpl release into Git

Start with `composecd host unmanaged nas`. The host must reach the same Docker engine and release storage
as the composetpl CLI used. With local storage, copy existing records into
`<dataDir>/hosts/nas/releases` before discovery.

Run `composecd adopt nas media > media.yaml` as admin, or open **Adopt** on the Hosts page.
The export includes saved user values in plaintext; move secrets to a SOPS values file before committing.
Warnings go to stderr. No Git or Docker state changes when exporting.

The adjacent `media.yaml` illustrates a reviewed export after copying an external chart into `charts/media`.
Replace its source and host to match your release. Preserve its name, match the host's `projectRoot` to the
release's original project directory, and review environment differences. Do not add this example unchanged.

After committing, refresh composecd, inspect `composecd app diff media`, and run `composecd app sync media`.
The same release advances by one revision. Automation and prune stay off until explicitly enabled.

A plain Compose project must first be converted to a chart with `composetpl create`. Adoption does not
convert Compose files or write to Git.
