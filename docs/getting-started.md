# Quick start

The Docker image runs the web interface on port `8478`. You need Docker
Compose, a Radarr or Sonarr instance, and the storage paths you want to use
as hot and cold tiers. Jellyfin is optional.

## 1. Download the compose file

```sh
curl -o docker-compose.yml https://raw.githubusercontent.com/voc0der/Coldarr/main/docker-compose.example.yml
```

Edit `docker-compose.yml` before starting the container:

- Set the media bind mounts to your hot and cold storage paths.
- Set `PUID` and `PGID` to a user and group that can read those paths.
- Adjust the published port and timezone if needed.
- Optionally set `COLDARR_PASSWORD` or `COLDARR_PASSWORD_FILE` for a stable
  login password. Otherwise Coldarr generates a new password on each start.

!!! warning "Use the same paths in every service"

    Coldarr's media paths must match the paths **inside Radarr and Sonarr**.
    For example, if Radarr sees `/data/media/movies`, Coldarr must see that
    same path. Matching only the host directory is not enough.

    The example mounts media read-only in Coldarr. Radarr and Sonarr need
    their own writable mounts because they perform the actual moves.

The bind-mounted `/config` directory holds configuration, credentials, and
move history. Keep it across container upgrades. See the
[Docker reference](configuration/docker.md) for all environment variables.

## 2. Start and sign in

```sh
docker compose up -d
docker compose logs coldarr
```

Open <http://localhost:8478> (or your Docker host and chosen port). Sign in
with the password you configured, or find the generated password in the
container logs. You can configure OIDC under **Settings > Auth** later.

## 3. Connect your services

Under **Settings > Connections**, add your Radarr and/or Sonarr URL and API
key. Their API keys are under **Settings > General** in each app. Test each
connection, then save it.

Use addresses reachable from the Coldarr container. `localhost` inside the
container refers to Coldarr itself; service names such as `radarr` only
resolve when the containers share a Docker network.

If you use Jellyfin, add its connection too. Every hot and cold path must
be included in a Jellyfin library using the same paths. See
[Jellyfin setup](configuration/jellyfin.md).

## 4. Add storage tiers

Under **Settings > Storage tiers**, add your primary storage as a hot tier
and your overflow drives as cold tiers. Set the allowed media types and
cold-tier target and maximum usage. For removable or satellite drives, turn
on **Require these paths to be on their own mounted drive**
(`require_mount`) so Coldarr detects a missing drive before planning moves.

You can also start with the [example configuration](configuration/example.md).
The [storage tiers guide](configuration/tiers.md) explains capacity limits,
mount safety, and paths that share a physical drive.

## 5. Review your first plan

Check the **Dashboard** for connection and storage health, then open
**Plan**. Review the proposed items, destinations, and projected usage
before selecting **Apply this plan** and confirming. The page shows
progress; the **History** page records completed moves.

Nothing runs on a schedule until you enable it under
**Settings > Scheduler**. Set up **Settings > Notifications** if you want
run summaries. See
[scheduling and notifications](features.md#keeping-tabs-on-it-without-watching-it).

!!! note "Optional Jellyfin plugin"

    The Restore User Data After Move plugin restores Jellyfin watch state
    that a move leaves behind. Get it from its
    [GitHub repository](https://github.com/voc0der/jellyfin-plugin-restore-userdata-after-move),
    and see [Watch state after a move](configuration/jellyfin.md#watch-state-after-a-move)
    for how Coldarr can start it.

Prefer a binary or shell automation? Continue with the [CLI guide](usage/cli.md).
