# Docker and environment

See the [quick start](../getting-started.md) for the compose one-liner. The
default command is `serve` - a persistent GUI, not a one-shot job.
Everything Coldarr persists (`coldarr.yaml`, the encrypted connection
store + its key, move history) lives under one bind-mounted `/config`
directory. To run one-shot CLI commands (or a cron job) against that same
volume instead of the GUI, see the
[CLI guide](../usage/cli.md#running-the-cli-in-docker).

## Environment variables

The image understands these variables directly. **Everything else** (tiers,
tags, thresholds) goes through `coldarr.yaml`, which supports `${VAR}`
substitution against whatever environment variables you pass to the
container.

### Container

`PUID` / `PGID`
:   The uid/gid the process runs as, so it can read your bind mounts. Same
    convention as Radarr/Sonarr/Jellyfin images. Default `1000`/`1000`.
    Ignored if the container is already started as non-root
    (`docker run --user`, compose `user:`, a rootless runtime, or a
    Kubernetes securityContext) - the entrypoint skips straight to running
    as whatever user it was given.

`TZ`
:   Container timezone, mostly cosmetic for log timestamps.

`COLDARR_CONFIG`
:   Path to the config file. Default `/config/coldarr.yaml` (via the image's
    `/config` working directory).

### Web server

`COLDARR_LISTEN_ADDR`
:   Address `serve` listens on, in Go's `host:port` form - a bare port number
    is normalized to `:port` (all interfaces), but anything else must include
    the colon yourself, e.g. `0.0.0.0:8555` or `127.0.0.1:8555`. Default
    `:8478`.

`COLDARR_TLS_CERT_FILE` / `COLDARR_TLS_KEY_FILE`
:   Certificate and private-key paths. Set both to make `coldarr serve`
    listen with HTTPS directly.

`COLDARR_TRUSTED_REVERSE_PROXIES_CIDR` (`TRUSTED_REVERSE_PROXIES_CIDR`)
:   Comma-separated proxy CIDRs whose `Forwarded` / `X-Forwarded-Proto` /
    `X-Forwarded-Host` headers should be trusted. Headers from other remote
    IPs are ignored.

### Sign-in

`COLDARR_PASSWORD`
:   Password that gates the GUI whenever OIDC is disabled. If unset (and
    `COLDARR_PASSWORD_FILE` isn't either), a random 64-character password is
    generated on every start and printed to the container's console log,
    with a warning that it won't survive a restart - set this to keep it
    stable. Irrelevant once OIDC is enabled.

`COLDARR_PASSWORD_FILE`
:   Path to a file containing the password (its contents win over
    `COLDARR_PASSWORD` if both are set) - point this at a Docker secret or
    any file you bind-mount into the container.

`COLDARR_OIDC_ENABLED` / `COLDARR_OIDC_ISSUER_URL` / `COLDARR_OIDC_CLIENT_ID` / `COLDARR_OIDC_CLIENT_SECRET`
:   OIDC auth overrides. When any are set, env values win over GUI-saved
    values. Set `COLDARR_OIDC_ENABLED=false` to disable OIDC entirely for
    troubleshooting.

`COLDARR_OIDC_REDIRECT_URL` / `COLDARR_OIDC_REQUIRED_GROUP` / `COLDARR_OIDC_GROUPS_CLAIM`
:   Optional OIDC details. Required group defaults to `coldarr`; groups claim
    defaults to `groups`.

`COLDARR_OIDC_AUTO_LOGIN`
:   Set to `true`/`false` to skip straight to the IdP instead of showing a
    login button, overriding the GUI-saved **Settings > Auth** checkbox. The
    login page shown right after signing out stays manual, so logging out
    doesn't sign you straight back in.

`COLDARR_OIDC_CLIENT_SECRET_POST`
:   Set to `true` for providers whose client registration uses
    `token_endpoint_auth_method: client_secret_post` (common with Authelia).
    The GUI exposes this as a checkbox under **Settings > Auth**.

### Connections

`RADARR_URL` / `RADARR_API_KEY` (`SONARR_*`, `JELLYFIN_*`, `JELLYFIN_ENABLED`)
:   Connection overrides - see [Connections](connections.md).

### Move timing

`COLDARR_SETTLE_CHECK_INTERVAL` / `COLDARR_SETTLE_STABLE_CHECKS` / `COLDARR_SETTLE_MAX_WAIT`
:   Tune how long `apply` waits for a move to actually land on disk before
    starting the next one queued for the same volume (Go duration strings,
    e.g. `5s`/`6h`). Defaults suit typical local disks; raise `MAX_WAIT` for
    very large files on slow/network storage.

`COLDARR_JELLYFIN_RESOLVE_TIMEOUT` / `COLDARR_JELLYFIN_RESOLVE_INTERVAL`
:   How long `apply` waits, after the moves finish, for Jellyfin to index
    each item at its new path so its artwork can be refreshed (Go duration
    strings; defaults `15m` / `10s`, the interval backing off to a minute
    between polls). Raise the timeout if you see
    `no Jellyfin item appeared at ... within` in the logs - see
    [Jellyfin](jellyfin.md#refresh-timeouts).

## Media paths

Tier paths must be bind-mounted at the *same path Radarr/Sonarr use
internally* - Coldarr compares its own disk checks and the root folder
paths the Arr APIs report as literal strings, so a mismatched mount looks
like a misplaced or unavailable path.

## Example compose file

Download the
[`docker-compose.example.yml`](https://github.com/voc0der/Coldarr/blob/main/docker-compose.example.yml)
and edit the paths before starting Coldarr. The repository also includes an
[`.env.example`](https://github.com/voc0der/Coldarr/blob/main/.env.example)
for optional environment overrides.

??? example "Complete compose example"

    ```yaml title="docker-compose.example.yml"
    --8<-- "docker-compose.example.yml"
    ```
