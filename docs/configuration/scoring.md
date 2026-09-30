# Scoring and Favorites

See [internal/scoring/scoring.go](https://github.com/voc0der/Coldarr/blob/main/internal/scoring/scoring.go)
for the full, small set of rules. In short: tags or an active download can
force `protected` outright, and a Jellyfin Favorite mark forces `hot`;
otherwise items accumulate a score from age, size, a series having ended,
time since last aired, a low-priority quality profile, and
unmonitored/missing state. Items at or above `cold_score_threshold` are cold
candidates, ranked by that score. (Policy thresholds like these are still
YAML-only for now - not yet exposed in the GUI.)

See the [example configuration](example.md) for every policy setting.

## Jellyfin Favorites

If Jellyfin is connected and enabled, Coldarr fetches every user's favorited
movies/series and matches them back to Radarr/Sonarr items by path -
anything favorited by anyone is kept on hot storage. That means it is never
moved to cold, and if it is already on cold (you favorited it *after*
Coldarr moved it), the next plan moves it back to hot, evicting
cold-eligible items from hot first if that's what it takes to make room.
Favoriting takes effect on the very next plan: unlike ordinary hot->cold
packing, the reclaim ignores `cooldown_days` and `min_move_size_gb`.

A `never-move`/`keep-hot` tag or an active download/import still outranks a
Favorite - those stay `protected` and are not moved in either direction.
Matching is by path, so this only works correctly if Jellyfin sees the same
paths Radarr/Sonarr do (see [Docker's path note](docker.md#media-paths)).

Coldarr snapshots these favorites before it builds a plan. If the fetch
fails for any reason, inventory and planning fail closed: no manual, CLI, or
scheduled apply can start without favorite protection. If Jellyfin goes
offline after a run starts, the run continues using the snapshot captured
before it began; only the post-move refresh may fail. That refresh is logged
per item (endpoint, resolved item ID, parameters, response), so a moved item
Jellyfin never picked up shows up in the log rather than silently losing its
artwork.
