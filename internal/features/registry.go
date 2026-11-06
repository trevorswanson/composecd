package features

import (
	"strconv"

	"github.com/trevorswanson/composecd/internal/config"
)

// fromRoot is resolved for a repository URL that the root repository's fills in.
func fromRoot(ctx Context, v string) (string, bool) {
	return "the root repository", ctx.Boot != nil && v == stripCredentials(ctx.Boot.Root.RepoURL)
}

// fromRootRevision is resolved for a revision that the root's fills in.
func fromRootRevision(ctx Context, v string) (string, bool) {
	return "the root's revision", ctx.Boot != nil && v == ctx.Boot.Root.TargetRevision
}

// registry is every setting, by scope and group, in the order shown. A test
// checks it against the config types, so a new field needs an entry here.
var registry = []Feature{
	// composecd.yaml
	{Scope: Server, Group: "Server", Path: "listen", Title: "Listen address", Help: "The HTTP address the UI, API and webhook listen on.", Default: config.DefaultListen, Example: `":8080"`},
	{Scope: Server, Group: "Server", Path: "dataDir", Title: "Data directory", Help: "Holds the database, the Git cache, local release storage and the generated admin token.", Default: config.DefaultDataDir, Example: "/var/lib/composecd"},
	{Scope: Server, Group: "Server", Path: "refreshInterval", Title: "Refresh interval", Help: "How often Git and every Application are refreshed. A webhook refreshes sooner.", Default: config.DefaultRefreshInterval.String(), Example: "3m"},
	{Scope: Server, Group: "Server", Path: "concurrency", Title: "Concurrency", Help: "How many Applications are reconciled at once.", Default: strconv.Itoa(config.DefaultConcurrency), Example: "4"},

	{Scope: Server, Group: "GitOps root", Path: "root.repoURL", Title: "Repository", Help: "The Git repository the Hosts, Applications, ApplicationSets and SyncWindows are read from.", Example: "https://git.example.lan/me/homelab.git"},
	{Scope: Server, Group: "GitOps root", Path: "root.targetRevision", Title: "Revision", Help: "The branch, tag or commit to read. Applications in the root repository default to it.", Default: "HEAD", Example: "main"},
	{Scope: Server, Group: "GitOps root", Path: "root.path", Title: "Path", Help: "The directory the objects are read from, recursively.", Default: "the repository root", Example: "composecd"},
	{Scope: Server, Group: "GitOps root", Path: "repositories", Title: "Repository credentials", Help: "Credentials for Git repositories, matched by URL prefix. Secrets are read from files.", Example: "- url: https://git.example.lan/\n  username: composecd\n  passwordFile: /run/secrets/git-token"},
	{Scope: Server, Path: "repositories[].url", Title: "URL", Help: "Every repository URL starting with this uses these credentials."},
	{Scope: Server, Path: "repositories[].username", Title: "Username", Help: "The HTTPS user."},
	{Scope: Server, Path: "repositories[].passwordFile", Title: "Password file", Help: "A file holding the HTTPS password or token."},
	{Scope: Server, Path: "repositories[].sshKeyFile", Title: "SSH key file", Help: "A private key for SSH remotes."},
	{Scope: Server, Path: "repositories[].knownHostsFile", Title: "Known hosts file", Help: "SSH host keys to trust."},
	{Scope: Server, Path: "repositories[].insecureIgnoreHostKey", Title: "Ignore host key", Help: "Skips SSH host key checking."},

	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.tokenFile", Title: "Admin token file", Help: "Holds the admin API token.", Default: "generated in <dataDir>/admin-token", Example: "/run/secrets/composecd-admin-token"},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.disabled", Title: "Authentication off", Help: "Turns sign-in off, for a proxy that authenticates in front of composecd. Everyone is admin.", Default: "false", Example: "true"},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.oidc", Title: "OpenID Connect", Help: "Sign in with an identity provider (Authentik, Authelia, Keycloak, Pocket ID, ...), with groups mapped to roles.", Example: "issuer: https://auth.example.lan\nclientID: composecd\nredirectURL: https://composecd.example.lan/auth/callback"},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.oidc.issuer", Title: "Issuer", Help: "The provider's issuer URL."},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.oidc.clientID", Title: "Client ID"},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.oidc.clientSecretFile", Title: "Client secret file", Help: "Omit for a public client.", Default: "a public client"},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.oidc.redirectURL", Title: "Redirect URL", Help: "composecd's callback URL, registered with the provider."},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.oidc.scopes", Title: "Scopes", Default: "openid, profile, email, groups"},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.oidc.usernameClaim", Title: "Username claim", Default: "preferred_username, then email, then sub"},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.oidc.groupsClaim", Title: "Groups claim", Default: "groups"},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.oidc.adminGroups", Title: "Admin groups", Help: "Members get the admin role.", Example: "[homelab-admins]"},
	{Scope: Server, Group: "Sign-in", Doc: "users-and-access", Path: "auth.oidc.readonlyGroups", Title: "Readonly groups", Help: "Members get the readonly role. Listing groups stops everyone else from signing in.", Default: "every other user is readonly", Example: "[homelab]"},

	{Scope: Server, Group: "Webhooks and notifications", Doc: "webhooks-notifications-and-metrics", Path: "webhook.secretFile", Title: "Webhook secret file", Help: "Turns on POST /api/v1/webhook, so a push to Git refreshes at once instead of at the next interval.", Default: "webhook off", Example: "/run/secrets/composecd-webhook"},
	{Scope: Server, Group: "Webhooks and notifications", Doc: "webhooks-notifications-and-metrics", Path: "notifications", Title: "Notifications", Help: "Send events (failed syncs, degraded apps, hosts down, image updates, ...) to webhook, ntfy, Gotify, Discord or Slack.", Default: "none", Example: "- name: phone\n  type: ntfy\n  url: https://ntfy.sh/my-topic"},
	{Scope: Server, Path: "notifications[].name", Title: "Name"},
	{Scope: Server, Path: "notifications[].type", Title: "Type", Help: "webhook, ntfy, gotify, discord or slack."},
	{Scope: Server, Path: "notifications[].url", Title: "URL", Help: "The endpoint.", Secret: true},
	{Scope: Server, Path: "notifications[].urlFile", Title: "URL file", Help: "Reads the URL from a file, for URLs that are secrets."},
	{Scope: Server, Path: "notifications[].tokenFile", Title: "Token file", Help: "An ntfy access token or a Gotify application token."},
	{Scope: Server, Path: "notifications[].events", Title: "Events", Help: "Which events to send.", Default: "sync-failed, health-degraded, host-down, prune, host-disk, write-back-blocked, image-available"},
	{Scope: Server, Path: "notifications[].applications", Title: "Applications", Help: "Name globs of the apps whose events are sent. Host events are always sent.", Default: "every app"},

	{Scope: Server, Group: "Secrets", Doc: "encrypted-values-with-sops", Path: "sopsAgeKeyFile", Title: "SOPS age key file", Help: "Decrypts SOPS-encrypted value files (*.sops.yaml) for Applications.", Default: "SOPS off", Example: "/run/secrets/age-key.txt"},
	{Scope: Server, Group: "Secrets", Doc: "secrets-from-infisical", Path: "infisical", Title: "Infisical", Help: "Lets Applications read secrets from Infisical, with a machine identity or an access token.", Example: "clientID: 0f1e...\nclientSecretFile: /run/secrets/infisical-secret"},
	{Scope: Server, Group: "Secrets", Doc: "secrets-from-infisical", Path: "infisical.siteURL", Title: "Infisical server", Default: config.DefaultInfisicalSiteURL},
	{Scope: Server, Group: "Secrets", Doc: "secrets-from-infisical", Path: "infisical.clientID", Title: "Infisical client ID", Help: "A Universal Auth machine identity, with clientSecretFile."},
	{Scope: Server, Group: "Secrets", Doc: "secrets-from-infisical", Path: "infisical.clientSecretFile", Title: "Infisical client secret file"},
	{Scope: Server, Group: "Secrets", Doc: "secrets-from-infisical", Path: "infisical.tokenFile", Title: "Infisical token file", Help: "An access token, instead of a machine identity."},

	{Scope: Server, Group: "Image updates", Doc: "image-updates", Path: "imageUpdates.enabled", Title: "On for every Application", Help: "Pins moving tags to registry digests, so a new image shows as a change. Each Application and service can opt in or out.", Default: "false", Example: "true"},
	{Scope: Server, Group: "Image updates", Doc: "image-updates", Path: "imageUpdates.schedule", Title: "Check schedule", Help: "A cron expression for when registries are checked. It runs whenever any Application takes part.", Default: config.DefaultImageSchedule + " (hourly)", Example: `"0 */6 * * *"`},
	{Scope: Server, Group: "Image updates", Doc: "image-updates", Path: "imageUpdates.timeZone", Title: "Schedule time zone", Default: "UTC", Example: "Europe/Berlin"},
	{Scope: Server, Group: "Image updates", Doc: "write-back-to-git", Path: "imageUpdates.writeBack.branch", Title: "Write-back branch", Help: "The branch image update commits are pushed to. Another branch than the deployed one leaves merging, and so deploying, to you.", Default: "each source's targetRevision", Example: "image-updates"},
	{Scope: Server, Group: "Image updates", Doc: "write-back-to-git", Path: "imageUpdates.writeBack.author.name", Title: "Write-back author name", Default: config.DefaultWriteBackName, Example: "composecd"},
	{Scope: Server, Group: "Image updates", Doc: "write-back-to-git", Path: "imageUpdates.writeBack.author.email", Title: "Write-back author email", Default: config.DefaultWriteBackEmail, Example: "composecd@example.lan"},
	{Scope: Server, Group: "Image updates", Doc: "write-back-to-git", Path: "imageUpdates.writeBack.signingKeyFile", Title: "Write-back signing key", Help: "An SSH private key the commits are signed with.", Default: "unsigned", Example: "/run/secrets/composecd-signing-key"},

	// Host
	{Scope: Host, Group: "Engine", Doc: "host", Path: "metadata.labels", Title: "Labels", Help: "Select the host in ApplicationSet hosts generators and sync windows.", Example: "{role: dns}"},
	{Scope: Host, Group: "Engine", Doc: "remote-hosts-and-paths", Path: "spec.url", Title: "Engine URL", Help: "unix://, ssh:// or tcp://, as for docker -H.", Default: "the controller's own engine (DOCKER_HOST or the local socket)", Example: "ssh://deploy@nas.lan"},
	{Scope: Host, Group: "Engine", Doc: "remote-hosts-and-paths", Path: "spec.context", Title: "Docker context", Help: "A docker context name, instead of a URL.", Example: "nas"},
	{Scope: Host, Group: "Engine", Doc: "remote-hosts-and-paths", Path: "spec.tlsCertPath", Title: "TLS certificates", Help: "A directory on the controller with ca.pem, cert.pem and key.pem, for a tcp:// engine.", Example: "/etc/composecd/certs/nas"},
	{Scope: Host, Group: "Engine", Doc: "remote-hosts-and-paths", Path: "spec.env", Sensitive: true, Title: "Environment", Help: "Added to every docker call to this host, and visible to compose ${VAR} interpolation and the env template function.", Example: "{TZ: Europe/Berlin}"},
	{Scope: Host, Group: "Releases", Doc: "remote-hosts-and-paths", Path: "spec.projectRoot", Title: "Project root", Help: "Each app's compose project directory is created under it, on the engine's machine. Relative bind mounts resolve there.", Default: config.DefaultProjectRoot, Example: "/srv/stacks"},
	{Scope: Host, Group: "Releases", Doc: "release-storage-volume-or-local", Path: "spec.storage", Options: []string{config.StorageVolume, config.StorageLocal}, Title: "Release storage", Help: "Where composetpl keeps release history: volume (on the engine) or local (in composecd's data directory).", Default: config.StorageVolume, Example: config.StorageLocal},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping", Title: "Housekeeping", Help: "Measures the engine's disk and prunes it. Without it composecd never measures or prunes the host.", Default: "off", Example: "imagePrune: {schedule: \"0 4 * * *\"}\ndiskWarning: {percent: 85}"},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping.imagePrune", Title: "Image prune", Help: "Removes images no container uses: on a schedule, or on request from this page.", Example: "{schedule: \"0 4 * * *\", until: 168h}"},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping.imagePrune.schedule", Title: "Image prune schedule", Help: "A cron expression.", Default: "only on request", Example: `"0 4 * * *"`},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping.imagePrune.timeZone", Title: "Image prune time zone", Default: "UTC", Example: "Europe/Berlin"},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping.imagePrune.until", Title: "Spare images newer than", Default: "spare none", Example: "168h"},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping.imagePrune.all", Title: "Prune tagged images", Help: "Removes unused tagged images too, not only dangling ones.", Default: "false", Example: "true"},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping.builderPrune", Title: "Build cache prune", Help: "Removes build cache: on a schedule, or on request from this page.", Example: "{schedule: \"0 4 * * 0\", keepStorage: 10GB}"},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping.builderPrune.schedule", Title: "Build cache prune schedule", Help: "A cron expression.", Default: "only on request", Example: `"0 4 * * 0"`},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping.builderPrune.timeZone", Title: "Build cache prune time zone", Default: "UTC", Example: "Europe/Berlin"},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping.builderPrune.keepStorage", Title: "Build cache to keep", Default: "none", Example: "10GB"},
	{Scope: Host, Group: "Housekeeping", Doc: "housekeeping", Path: "spec.housekeeping.diskWarning.percent", Title: "Disk warning", Help: "Warns, here and in a host-disk notification, when the engine's data root is this full (percent).", Default: "no warning", Example: "85"},

	// Application
	{Scope: Application, Group: "General", Doc: "application", Path: "metadata.labels", Title: "Labels", Help: "Filter the applications list, and select apps in sync windows, rolling sync steps and notifications.", Example: "{tier: media}"},
	{Scope: Application, Group: "General", Doc: "application", Path: "spec.destination.host", Title: "Host", Help: "The Host the app runs on."},
	{Scope: Application, Group: "General", Doc: "application", Path: "spec.dependsOn", Title: "Depends on", Help: "Apps that must be Synced and Healthy before this one syncs automatically.", Default: "nothing", Example: "[traefik]"},
	{Scope: Application, Group: "General", Doc: "application", Path: "spec.self", Title: "Runs composecd", Help: "Marks the app that runs composecd itself, when composecd can't find its own container.", Default: "detected", Example: "true"},

	{Scope: Application, Group: "Source", Doc: "application", Path: "spec.source.repoURL", resolved: fromRoot, Title: "Repository", Help: "The Git repository holding the chart.", Default: "the root repository", Example: "https://git.example.lan/me/charts.git"},
	{Scope: Application, Group: "Source", Doc: "application", Path: "spec.source.targetRevision", resolved: fromRootRevision, Title: "Revision", Help: "A branch, tag or commit.", Default: "the root's revision (HEAD for another repository)", Example: "main"},
	{Scope: Application, Group: "Source", Doc: "application", Path: "spec.source.path", Title: "Chart path", Help: "The chart directory in the repository.", Example: "stacks/media"},
	{Scope: Application, Group: "Source", Doc: "oci-charts-with-values-from-git", Path: "spec.source.chart", Title: "OCI chart", Help: "An oci:// chart, instead of a repository path.", Example: "oci://ghcr.io/me/charts/media"},
	{Scope: Application, Group: "Source", Doc: "oci-charts-with-values-from-git", Path: "spec.source.version", Title: "Chart version", Help: "A semver constraint for the OCI chart.", Default: "the newest", Example: `"^1.2"`},
	{Scope: Application, Group: "Source", Doc: "oci-charts-with-values-from-git", Path: "spec.source.valuesFrom", Title: "Values from Git", Help: "A Git repository that valueFiles of an OCI chart are read from.", Example: "{}"},
	{Scope: Application, Group: "Source", Doc: "oci-charts-with-values-from-git", Path: "spec.source.valuesFrom.repoURL", resolved: fromRoot, Title: "Values repository", Default: "the root repository", Example: "https://git.example.lan/me/homelab.git"},
	{Scope: Application, Group: "Source", Doc: "oci-charts-with-values-from-git", Path: "spec.source.valuesFrom.targetRevision", resolved: fromRootRevision, Title: "Values revision", Default: "the root's revision (HEAD for another repository)", Example: "main"},

	{Scope: Application, Group: "Values", Doc: "application", Path: "spec.source.valueFiles", Title: "Value files", Help: "Applied in order over the chart's values.yaml. Files ending in .sops.yaml are decrypted with SOPS.", Example: "[values-nas.yaml]"},
	{Scope: Application, Group: "Values", Doc: "application", Path: "spec.source.values", Title: "Inline values", Help: "Applied last, over the value files.", Example: "image: {tag: \"1.27\"}"},
	{Scope: Application, Group: "Values", Doc: "application", Path: "spec.source.env", Sensitive: true, Title: "Environment", Help: "Visible to compose ${VAR} interpolation and the env template function, over the host's environment.", Example: "{PUID: \"1000\"}"},
	{Scope: Application, Group: "Values", Doc: "secrets-from-infisical", Path: "spec.source.infisical", Title: "Secrets from Infisical", Help: "Reads secrets from Infisical into the values, after the value files and before inline values. Needs infisical in composecd.yaml.", Example: "projectId: 6f1c...\nenvironment: prod"},
	{Scope: Application, Group: "Values", Doc: "secrets-from-infisical", Path: "spec.source.infisical.projectId", Title: "Infisical project"},
	{Scope: Application, Group: "Values", Doc: "secrets-from-infisical", Path: "spec.source.infisical.environment", Title: "Infisical environment"},
	{Scope: Application, Group: "Values", Doc: "secrets-from-infisical", Path: "spec.source.infisical.path", Title: "Infisical folder", Default: "/", Example: "/media"},
	{Scope: Application, Group: "Values", Doc: "secrets-from-infisical", Path: "spec.source.infisical.recursive", Title: "Infisical subfolders", Help: "Also reads secrets in subfolders.", Default: "false", Example: "true"},
	{Scope: Application, Group: "Values", Doc: "secrets-from-infisical", Path: "spec.source.infisical.into", Title: "Infisical values path", Help: "A dotted values path the secrets go under.", Default: "the top level of the values", Example: "secrets"},

	{Scope: Application, Group: "Sync", Doc: "application", Path: "spec.syncPolicy.automated", Title: "Automatic sync", Help: "Syncs whenever Git changes, within sync windows. Without it, the app syncs only on request.", Default: "manual", Example: "{}"},
	{Scope: Application, Group: "Sync", Doc: "application", Path: "spec.syncPolicy.automated.prune", Title: "Prune", Help: "Uninstalls the release when the app is removed from Git or moved to another host. Named volumes and history are kept.", Default: "false", Example: "true"},
	{Scope: Application, Group: "Sync", Doc: "application", Path: "spec.syncPolicy.automated.selfHeal", Title: "Self-heal", Help: "Repairs containers removed or changed by hand, from the deployed revision.", Default: "false", Example: "true"},
	{Scope: Application, Group: "Sync", Doc: "application", Path: "spec.syncPolicy.retry", Title: "Retry", Help: "Retries a failed automatic sync of the same desired state, with backoff.", Default: "no retries", Example: "{limit: 3}"},
	{Scope: Application, Group: "Sync", Doc: "application", Path: "spec.syncPolicy.retry.limit", Title: "Retry limit", Default: "0", Example: "3"},
	{Scope: Application, Group: "Sync", Doc: "application", Path: "spec.syncPolicy.retry.backoff.duration", Title: "First retry after", Default: config.DefaultBackoff.String(), Example: "30s"},
	{Scope: Application, Group: "Sync", Doc: "application", Path: "spec.syncPolicy.retry.backoff.factor", Title: "Backoff factor", Default: strconv.Itoa(config.DefaultFactor), Example: "2"},
	{Scope: Application, Group: "Sync", Doc: "application", Path: "spec.syncPolicy.retry.backoff.maxDuration", Title: "Longest wait", Default: config.DefaultMaxDuration.String(), Example: "10m"},

	{Scope: Application, Group: "Sync options", Doc: "application", Path: "spec.syncPolicy.options.wait", Title: "Wait for healthy", Help: "Waits until services are running and healthy (docker compose up --wait).", Default: "true", Example: "false"},
	{Scope: Application, Group: "Sync options", Doc: "application", Path: "spec.syncPolicy.options.timeout", Title: "Timeout", Help: "Bounds the wait and each hook.", Default: config.DefaultTimeout.String(), Example: "10m"},
	{Scope: Application, Group: "Sync options", Doc: "application", Path: "spec.syncPolicy.options.pull", Options: []string{"always", "missing", "never"}, Title: "Pull policy", Help: "always, missing or never.", Default: "compose's", Example: "always"},
	{Scope: Application, Group: "Sync options", Doc: "application", Path: "spec.syncPolicy.options.atomic", Title: "Atomic", Help: "Rolls a failed sync back to the last deployed revision.", Default: "false", Example: "true"},
	{Scope: Application, Group: "Sync options", Doc: "application", Path: "spec.syncPolicy.options.forceRecreate", Title: "Force recreate", Help: "Recreates containers even when their configuration is unchanged.", Default: "false", Example: "true"},
	{Scope: Application, Group: "Sync options", Doc: "application", Path: "spec.syncPolicy.options.historyMax", Title: "Revisions kept", Help: "How many release revisions composetpl keeps (0 keeps all).", Default: strconv.Itoa(config.DefaultHistoryMax), Example: "20"},

	{Scope: Application, Group: "Image updates", Doc: "image-updates", Path: "spec.syncPolicy.imageUpdates.enabled", Title: "Image updates", Help: "Pins moving tags to registry digests, so a new image shows as a change from Git.", Default: "false", Example: "true", inherit: func(ctx Context) (string, string, bool) {
		if ctx.Boot == nil {
			return "", "", false
		}
		return strconv.FormatBool(ctx.Boot.ImageUpdates.Enabled), "imageUpdates.enabled in composecd.yaml", true
	}},
	{Scope: Application, Group: "Image updates", Doc: "strategies", Path: "spec.syncPolicy.imageUpdates.strategy", Options: []string{"digest", "semver", "newest-build"}, Title: "Strategy", Help: "digest keeps Git's tag; semver and newest-build move to newer tags.", Default: "digest", Example: "semver"},
	{Scope: Application, Group: "Image updates", Doc: "strategies", Path: "spec.syncPolicy.imageUpdates.constraint", Title: "Semver constraint", Default: "the major version of Git's tag", Example: `"~1.27"`},
	{Scope: Application, Group: "Image updates", Doc: "strategies", Path: "spec.syncPolicy.imageUpdates.allowTags", Title: "Allowed tags", Help: "A regular expression tags must match (required for newest-build).", Default: "any", Example: `'^\d+\.\d+\.\d+$'`},
	{Scope: Application, Group: "Image updates", Doc: "strategies", Path: "spec.syncPolicy.imageUpdates.ignoreTags", Title: "Ignored tags", Help: "Globs of tags never picked.", Default: "none", Example: `["*-rc*"]`},
	{Scope: Application, Group: "Image updates", Doc: "write-back-to-git", Path: "spec.syncPolicy.imageUpdates.writeBack", Options: []string{config.WriteBackMemory, config.WriteBackGit}, Title: "Write-back", Help: "memory applies a picked tag in memory; git commits it, so Git stays the record of what runs. git also commits new digests for images Git pins (nginx:1.27@sha256:…), which are otherwise only reported.", Default: config.WriteBackMemory, Example: config.WriteBackGit},
	{Scope: Application, Group: "Image updates", Doc: "upgrade-offers", Path: "spec.syncPolicy.imageUpdates.offer", Title: "Upgrade offers", Help: "Looks for newer semver tags than the strategy moves to, and reports them as upgrades an admin approves. Nothing is applied until one is.", Default: "off", Example: "constraint: \">=3\""},
	{Scope: Application, Group: "Image updates", Doc: "upgrade-offers", Path: "spec.syncPolicy.imageUpdates.offer.strategy", Options: []string{"semver"}, Title: "Offer strategy", Default: "semver"},
	{Scope: Application, Group: "Image updates", Doc: "upgrade-offers", Path: "spec.syncPolicy.imageUpdates.offer.constraint", Title: "Offer constraint", Help: "A semver constraint the offered tags match.", Default: "any newer release, majors included", Example: `">=3"`},
	{Scope: Application, Group: "Image updates", Doc: "upgrade-offers", Path: "spec.syncPolicy.imageUpdates.offer.allowTags", Title: "Offer allowed tags", Help: "A regular expression offered tags must match.", Default: "any", Example: `'^\d+\.\d+\.\d+$'`},
	{Scope: Application, Group: "Image updates", Doc: "upgrade-offers", Path: "spec.syncPolicy.imageUpdates.offer.ignoreTags", Title: "Offer ignored tags", Help: "Globs of tags never offered.", Default: "none", Example: `["*-rc*"]`},

	{Scope: Application, Group: "Reading other Applications", Doc: "reading-other-applications", Path: "spec.permissions.env", Title: "Read environment", Help: "Apps (app or app/service globs) whose services' environment this chart may read with lookup, secrets masked.", Default: "identity and labels only", Example: `["*"]`},
	{Scope: Application, Group: "Reading other Applications", Doc: "reading-other-applications", Path: "spec.permissions.configs", Title: "Read configs", Help: "Apps whose mounted configs this chart may read.", Default: "none", Example: "[traefik]"},
	{Scope: Application, Group: "Reading other Applications", Doc: "reading-other-applications", Path: "spec.permissions.secrets", Title: "Read secrets", Help: "Apps whose unmasked environment and mounted secrets this chart may read.", Default: "none", Example: "[database/postgres]"},

	// Compose service labels
	{Scope: ServiceLabel, Group: "Service labels", Doc: "image-updates", Path: config.LabelImageUpdates, Title: "Image updates", Help: "\"true\" or \"false\": turns image updates on or off for one service, over the Application.", Example: "labels:\n  " + config.LabelImageUpdates + `: "false"`},
	{Scope: ServiceLabel, Group: "Service labels", Doc: "strategies", Path: config.LabelImageUpdates + ".strategy", Title: "Strategy", Help: "The service's own strategy. Another strategy than the Application's starts from its defaults.", Example: "labels:\n  " + config.LabelImageUpdates + ".strategy: semver"},
	{Scope: ServiceLabel, Group: "Service labels", Doc: "strategies", Path: config.LabelImageUpdates + ".constraint", Title: "Semver constraint", Example: "labels:\n  " + config.LabelImageUpdates + `.constraint: "~1.27"`},
	{Scope: ServiceLabel, Group: "Service labels", Doc: "strategies", Path: config.LabelImageUpdates + ".allow-tags", Title: "Allowed tags", Example: "labels:\n  " + config.LabelImageUpdates + `.allow-tags: '^\d+\.\d+\.\d+$'`},
	{Scope: ServiceLabel, Group: "Service labels", Doc: "write-back-to-git", Path: config.LabelImageUpdates + ".tag-key", Title: "Write-back tag key", Help: "The values key (or Chart.appVersion) write-back edits, when tracing can't tell.", Example: "labels:\n  " + config.LabelImageUpdates + ".tag-key: image.tag"},

	// ApplicationSet
	{Scope: ApplicationSet, Group: "General", Doc: "applicationsets", Path: "metadata.labels", Title: "Labels", Example: "{team: media}"},
	{Scope: ApplicationSet, Group: "Generators", Doc: "applicationsets", Path: "spec.generators", Title: "Generators", Help: "Each generator yields parameters; the template makes one Application of each.", Example: "- gitDirectories: {directories: [\"charts/*\"]}"},
	{Scope: ApplicationSet, Path: "spec.generators[].gitDirectories.repoURL", Title: "gitDirectories.repoURL", Help: "The repository to list.", Default: "the root repository"},
	{Scope: ApplicationSet, Path: "spec.generators[].gitDirectories.targetRevision", Title: "gitDirectories.targetRevision", Default: "the root's revision"},
	{Scope: ApplicationSet, Path: "spec.generators[].gitDirectories.directories", Title: "gitDirectories.directories", Help: "path.Match globs of chart directories: one Application per match."},
	{Scope: ApplicationSet, Path: "spec.generators[].gitDirectories.exclude", Title: "gitDirectories.exclude", Help: "Globs of directories to leave out."},
	{Scope: ApplicationSet, Path: "spec.generators[].hosts.selector", Title: "hosts.selector", Help: "Host labels: one Application per matching Host."},
	{Scope: ApplicationSet, Group: "Rollout", Doc: "rolling-sync", Path: "spec.strategy", Title: "Strategy", Help: "Rolls a change out across the generated Applications a few at a time, each synced and Healthy before the next.", Default: "all at once", Example: "type: RollingSync\nrollingSync: {maxUpdate: 1}"},
	{Scope: ApplicationSet, Group: "Rollout", Doc: "rolling-sync", Path: "spec.strategy.type", Options: []string{config.StrategyRollingSync}, Title: "Strategy type", Help: "RollingSync is the only one."},
	{Scope: ApplicationSet, Group: "Rollout", Doc: "rolling-sync", Path: "spec.strategy.rollingSync.steps", Title: "Steps", Help: "Label selectors matched in order; an app belongs to the first step that selects it. Apps no step selects only sync manually.", Default: "one step of every app", Example: "- matchExpressions: [{key: env, operator: In, values: [staging]}]\n- matchExpressions: [{key: env, operator: In, values: [prod]}]"},
	{Scope: ApplicationSet, Path: "spec.strategy.rollingSync.steps[].matchExpressions", Title: "matchExpressions", Help: "Label requirements: key, operator (In or NotIn) and values."},
	{Scope: ApplicationSet, Path: "spec.strategy.rollingSync.steps[].matchExpressions[].key", Title: "key"},
	{Scope: ApplicationSet, Path: "spec.strategy.rollingSync.steps[].matchExpressions[].operator", Title: "operator"},
	{Scope: ApplicationSet, Path: "spec.strategy.rollingSync.steps[].matchExpressions[].values", Title: "values"},
	{Scope: ApplicationSet, Path: "spec.strategy.rollingSync.steps[].maxUpdate", Title: "maxUpdate", Help: "The step's own limit; 0 holds the step."},
	{Scope: ApplicationSet, Group: "Rollout", Doc: "rolling-sync", Path: "spec.strategy.rollingSync.maxUpdate", Title: "Updating at once", Help: "How many apps of a step may update at once: a count, or a percentage.", Default: "100%", Example: "1"},
	{Scope: ApplicationSet, Group: "Rollout", Doc: "rolling-sync", Path: "spec.strategy.rollingSync.minHealthyTime", Title: "Healthy for", Help: "How long an app must stay Healthy after it syncs before the rollout moves on.", Default: "0s", Example: "30s"},
}
