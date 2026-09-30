# Screenshots

The web interface in light and dark mode. Images follow the documentation
site's color scheme.

## Dashboard

![Dashboard: library counts, Radarr/Sonarr/Jellyfin connection status, and every tier path's used and total space against its target and max](assets/screenshots/dashboard-light.png#gh-light-mode-only)
![Dashboard: library counts, Radarr/Sonarr/Jellyfin connection status, and every tier path's used and total space against its target and max](assets/screenshots/dashboard-dark.png#gh-dark-mode-only)

## Plan

A dry run of what would move, why, and where, with each drive's
usage before and after. Here a Jellyfin Favorite that had gone cold is
coming back to hot storage.

![Plan page: twelve moves with links, sizes, source and destination tiers, scores and reasons, then projected usage per path](assets/screenshots/plan-light.png#gh-light-mode-only)
![Plan page: twelve moves with links, sizes, source and destination tiers, scores and reasons, then projected usage per path](assets/screenshots/plan-dark.png#gh-dark-mode-only)

## Applying

One move at a time per destination drive, each confirmed
landed before the next one starts.

![Apply in progress: some moves done, some moving, the rest pending](assets/screenshots/applying-light.png#gh-light-mode-only)
![Apply in progress: some moves done, some moving, the rest pending](assets/screenshots/applying-dark.png#gh-dark-mode-only)

## History

Every move Coldarr has made, with links back into Radarr,
Sonarr and Jellyfin.

![History page: past moves with source and destination tier, path and size](assets/screenshots/history-light.png#gh-light-mode-only)
![History page: past moves with source and destination tier, path and size](assets/screenshots/history-dark.png#gh-dark-mode-only)

## Verify sizes

Re-checks each moved item's current size against Radarr/Sonarr, to catch
transfers left half-done by a crash.

![Verify sizes: every moved item's recorded size matches its current size in Radarr or Sonarr](assets/screenshots/verify-light.png#gh-light-mode-only)
![Verify sizes: every moved item's recorded size matches its current size in Radarr or Sonarr](assets/screenshots/verify-dark.png#gh-dark-mode-only)

## Connections

Radarr, Sonarr, and Jellyfin, each with a live connection test. The optional
external URL is only used to build the Links column.

![Connections settings: Radarr, Sonarr, and Jellyfin URLs and API keys, each connected, with optional external URLs](assets/screenshots/connections-light.png#gh-light-mode-only)
![Connections settings: Radarr, Sonarr, and Jellyfin URLs and API keys, each connected, with optional external URLs](assets/screenshots/connections-dark.png#gh-dark-mode-only)

## Storage tiers

Paths that turn out to be on the same disk are detected
and treated as sharing its capacity.

![Storage tiers settings: a hot tier whose two paths share a disk, and two cold tiers that require their own mounted drive](assets/screenshots/tiers-light.png#gh-light-mode-only)
![Storage tiers settings: a hot tier whose two paths share a disk, and two cold tiers that require their own mounted drive](assets/screenshots/tiers-dark.png#gh-dark-mode-only)

## Editing a storage tier

A tier's role, paths, media types, fill target and ceiling, and whether its
paths must be on their own mounted drive.

![Edit storage tier form: a cold tier with two satellite paths, movies only, a 92% target, a 95% maximum, and the own-drive check enabled](assets/screenshots/tier-edit-light.png#gh-light-mode-only)
![Edit storage tier form: a cold tier with two satellite paths, movies only, a 92% target, a 95% maximum, and the own-drive check enabled](assets/screenshots/tier-edit-dark.png#gh-dark-mode-only)

## Orphaned storage

Folders on a tier that no service tracks anymore,
including leftovers from an interrupted move.

![Orphaned storage page: tier writability, and three orphaned folders with their tier and size](assets/screenshots/orphans-light.png#gh-light-mode-only)
![Orphaned storage page: tier writability, and three orphaned folders with their tier and size](assets/screenshots/orphans-dark.png#gh-dark-mode-only)

## Scheduler

Every automatic task is off until you turn it on, and Weekly Omit Days pause
all of them on the days you pick.

![Scheduler settings: weekly omit days, then the Run the Plan, Rescan Cold Storage, Refresh Links Cache, Scan Quality Cutoffs, and Scan for Orphaned Storage tasks](assets/screenshots/scheduler-light.png#gh-light-mode-only)
![Scheduler settings: weekly omit days, then the Run the Plan, Rescan Cold Storage, Refresh Links Cache, Scan Quality Cutoffs, and Scan for Orphaned Storage tasks](assets/screenshots/scheduler-dark.png#gh-dark-mode-only)

## A drive goes missing

Coldarr flags its path and refuses every move
until it's back.

![Dashboard with one satellite path unavailable because it's on the system disk, not its own drive](assets/screenshots/dead-drive-light.png#gh-light-mode-only)
![Dashboard with one satellite path unavailable because it's on the system disk, not its own drive](assets/screenshots/dead-drive-dark.png#gh-dark-mode-only)

The Plan page says which path failed its check instead of offering a plan.

![Plan page refusing to move anything until every tier path checks healthy, naming the satellite path that is on the system disk](assets/screenshots/dead-drive-plan-light.png#gh-light-mode-only)
![Plan page refusing to move anything until every tier path checks healthy, naming the satellite path that is on the system disk](assets/screenshots/dead-drive-plan-dark.png#gh-dark-mode-only)

## Signing in

Without OIDC configured, a single password protects the web interface.

![Sign-in page: OIDC isn't configured, so Coldarr is protected by a password instead](assets/screenshots/login-light.png#gh-light-mode-only)
![Sign-in page: OIDC isn't configured, so Coldarr is protected by a password instead](assets/screenshots/login-dark.png#gh-dark-mode-only)
