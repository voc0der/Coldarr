# Web interface

`coldarr serve` runs the web GUI, and is the Docker image's default command.
It listens on port `8478` by default; override that with `--listen` or
`COLDARR_LISTEN_ADDR`.

## Signing in

The web GUI can view connection status and trigger real moves, so it's never
reachable unauthenticated: without OIDC configured, it falls back to a single
shared password instead - see [Docker](../configuration/docker.md#sign-in)
for how that password is set (`COLDARR_PASSWORD`/`COLDARR_PASSWORD_FILE`), or
configure real identity-provider-backed login and group-based access via
**Settings > Auth** (or `COLDARR_OIDC_*` env vars).

## Pages

Once signed in:

- **Dashboard** - tier usage/space allotment, library item counts by
  decision (protected/hot/cold), connection status.
- **Plan** - the same dry-run preview as the CLI's `plan`, with an Apply
  button (confirm dialog, then executes through Radarr/Sonarr) and live
  progress of the current (or most recent) apply run, auto-refreshing
  until it finishes.
- **History** - every move Coldarr has executed, with a size-verification
  check to catch a transfer a crash left half-done.
- **Settings**:
    - **Connections** - configure/test Radarr/Sonarr/Jellyfin.
    - **Storage tiers** - add/edit/delete hot and cold tiers, live per-path
      disk usage.
    - **Orphaned Storage** - folders on a tier path that Radarr, Sonarr, and
      Jellyfin no longer track, including leftovers from an interrupted move,
      and whether each tier path is writable.
    - **Auth** - optional OIDC login and group access.
    - **Notifications** - an Apprise webhook for run summaries.
    - **Scheduler** - optionally run the plan, a cold-storage health check,
      or background scans on a schedule - see
      [features](../features.md#keeping-tabs-on-it-without-watching-it) for
      details.

The [screenshot gallery](../screenshots.md) shows each of these pages.

Prefer the CLI, or scripting Coldarr instead of clicking through the GUI?
See the [CLI guide](cli.md) for the full command reference.
