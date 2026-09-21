<p align="center">
  <img src="assets/icon-512.png" width="120" alt="Coldarr icon">
</p>

<h1 align="center">Coldarr</h1>

<p align="center">
  <a href="LICENSE.md"><img src="https://img.shields.io/github/license/voc0der/Coldarr" alt="License"></a>
  <a href="https://github.com/voc0der/Coldarr/releases/latest"><img src="https://img.shields.io/github/v/release/voc0der/Coldarr" alt="Latest release"></a>
  <a href="https://github.com/voc0der/Coldarr/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/voc0der/Coldarr/ci.yml?branch=main&label=CI" alt="CI status"></a>
  <a href="CONTRIBUTING.md#coverage"><img src="https://img.shields.io/badge/coverage-63.0%25-yellow" alt="Test coverage"></a>
  <a href="https://hub.docker.com/r/voc0der/coldarr"><img src="https://img.shields.io/docker/pulls/voc0der/coldarr" alt="Docker pulls"></a>
</p>

<p align="center">A policy-based storage-tiering balancer for Radarr/Sonarr libraries.</p>

Your hot storage is expensive, redundant, and always running out of
space; your cold/satellite drives are cheap and built to absorb the
overflow. Coldarr looks at your library (age, size, tags, quality
profile, monitored state, Jellyfin Favorites) and your disk usage, decides
what's safe to push to overflow storage, and asks Radarr/Sonarr to move it -
so their databases stay the source of truth. Coldarr never touches files on
disk directly, and nothing moves without a dry-run `report`/`plan` first.

CLI and web GUI, same config either way - mix them (e.g. configure
connections in the GUI, then automate with cron or the GUI's own
Settings > Scheduler).

<p align="center">
  <img src="assets/hot-cold-example.svg" alt="Example layout: primary NAS at 76%, satellite drives packed to 99%">
</p>

## Screenshots

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/screenshots/dashboard-dark.png">
  <img src="assets/screenshots/dashboard-light.png" alt="Dashboard: library counts, Radarr/Sonarr/Jellyfin connection status, and every tier path's used and total space against its target and max">
</picture>

<details>
<summary>More screenshots</summary>

**Plan** - a dry run of what would move, why, and where, with each drive's
usage before and after. Here a Jellyfin Favorite that had gone cold is
coming back to hot storage.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/screenshots/plan-dark.png">
  <img src="assets/screenshots/plan-light.png" alt="Plan page: twelve moves with links, sizes, source and destination tiers, scores and reasons, then projected usage per path">
</picture>

**Applying** - one move at a time per destination drive, each confirmed
landed before the next one starts.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/screenshots/applying-dark.png">
  <img src="assets/screenshots/applying-light.png" alt="Apply in progress: some moves done, some moving, the rest pending">
</picture>

**History** - every move Coldarr has made, with links back into Radarr and
Sonarr.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/screenshots/history-dark.png">
  <img src="assets/screenshots/history-light.png" alt="History page: past moves with source and destination tier, path and size">
</picture>

**Storage tiers** - paths that turn out to be on the same disk are detected
and treated as sharing its capacity.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/screenshots/tiers-dark.png">
  <img src="assets/screenshots/tiers-light.png" alt="Storage tiers settings: a hot tier whose two paths share a disk, and two cold tiers that require their own mounted drive">
</picture>

**Orphaned storage** - folders on a tier that no service tracks anymore,
including leftovers from an interrupted move.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/screenshots/orphans-dark.png">
  <img src="assets/screenshots/orphans-light.png" alt="Orphaned storage page: tier writability, and three orphaned folders with their tier and size">
</picture>

**A drive goes missing** - Coldarr flags its path and refuses every move
until it's back.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/screenshots/dead-drive-dark.png">
  <img src="assets/screenshots/dead-drive-light.png" alt="Dashboard with one satellite path unavailable because it's on the system disk, not its own drive">
</picture>

</details>

## Quick start

```
curl -o docker-compose.yml https://raw.githubusercontent.com/voc0der/Coldarr/main/docker-compose.example.yml
# edit tier paths, ports in docker-compose.yml, then:
docker compose up -d
```

Open `http://localhost:8478`, add your Radarr/Sonarr/Jellyfin connections
and tiers under Settings, then use the Plan page to preview a move and
Apply it.

> [!NOTE]
> Get the optional Restore User Data After Move plugin from its
> [GitHub repository](https://github.com/voc0der/jellyfin-plugin-restore-userdata-after-move).

Prefer the CLI, or building from source? See [CLI.md](CLI.md). Tuning
tiers, notifications/scheduling, or the full Docker env var reference? See
[CONFIGURATION.md](CONFIGURATION.md).

## Learn more

- [FEATURES.md](FEATURES.md) - what Coldarr does and why, in plain English
- [CLI.md](CLI.md) - building from source and the full CLI command reference
- [CONFIGURATION.md](CONFIGURATION.md) - connections, tiers, Docker, scoring,
  and the web GUI reference
- [DEVELOPMENT.md](DEVELOPMENT.md) - building, testing, CI/CD, releasing,
  and the roadmap
- [CONTRIBUTING.md](CONTRIBUTING.md) - branch/commit/PR conventions
- Licensed under [MIT](LICENSE.md)
- Radarr/Sonarr/Jellyfin logos in the web GUI's Links column are vendored
  from [selfh.st/icons](https://github.com/selfhst/icons), licensed
  [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/)
