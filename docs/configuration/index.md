# Configuration

The CLI and web interface use the same `coldarr.yaml`. In Docker it lives
under `/config`; a local binary defaults to `./coldarr.yaml`. Override the
location with `--config` or `COLDARR_CONFIG`.

Connection URLs and API keys are stored separately in an encrypted file.
Environment variables override saved connections and authentication settings.
Values inside the YAML file can also use `${VAR}` substitution.

| Configure | Reference |
| --- | --- |
| Radarr, Sonarr, and Jellyfin URLs and API keys | [Connections](connections.md) |
| Container mounts, password, OIDC, HTTPS, and environment variables | [Docker and environment](docker.md) |
| Hot and cold storage, fill limits, and mount checks | [Storage tiers](tiers.md) |
| Library paths, Favorites, and artwork and date added after a move | [Jellyfin](jellyfin.md) |
| Tags, thresholds, and cold eligibility | [Scoring and Favorites](scoring.md) |
| Run summaries and scheduled jobs | [Notifications and scheduling](../features.md#keeping-tabs-on-it-without-watching-it) |
| A complete starting YAML file | [Example configuration](example.md) |

!!! note "Editing configuration through the web interface"

    Saving tiers, notifications, auth, or scheduler settings through the web
    interface rewrites `coldarr.yaml`, so hand-written comments do not
    survive a save. Policy thresholds are configured in YAML; the web
    interface does not yet expose them.

For daily operation, see [How a run works](../usage/how-it-works.md), the
[web interface](../usage/web-interface.md), or the [CLI](../usage/cli.md).
For a new installation, begin with the [quick start](../getting-started.md).
