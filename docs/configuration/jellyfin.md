# Jellyfin

Jellyfin is optional, and only ever a consumer of the library - it never
moves anything. It is used to read Favorites (so favorited items are kept
on, or reclaimed to, hot storage) and to keep artwork intact across a move.

See [Connections](connections.md) to add Jellyfin and
[Scoring and Favorites](scoring.md#jellyfin-favorites) for the protection
rules.

## Library paths

**Every tier path must be inside a Jellyfin library folder**, mounted at
the same path Coldarr and Radarr/Sonarr use. Jellyfin can only index media
under a configured library location, so an item moved to a tier no library
covers doesn't merely lose its poster - it disappears from Jellyfin
altogether. Check **Dashboard > Libraries**, or:

```sh
curl -s -H "Authorization: MediaBrowser Token=\"$KEY\"" \
  "$JELLYFIN_URL/Library/VirtualFolders" \
  | jq -r '.[] | "\(.Name): \(.Locations|join(", "))"'
```

## Scanning after moves

**Jellyfin is slow to notice a move, by design.** Reporting a changed path
does not trigger an immediate scan: Jellyfin debounces it by
`LibraryMonitorDelay` (**Dashboard > Advanced**, 60 seconds out of the box),
and re-reporting a path it already has queued *restarts* that timer. Only
then does it act - and for a path it has never seen before, that means
re-validating the whole library root containing it, which on a large
library takes minutes per root.

This is why moves are reported individually as they land rather than in one
batch at the end: it gives that work the length of the run to happen in.
What can't be moved earlier is the refresh itself, since it needs the item's
*new* ID, which doesn't exist until Jellyfin has indexed it.

## Refresh timeouts

If you see

```text
could not refresh N item(s) in Jellyfin: "..." no Jellyfin item appeared at ... within 15m0s
```

Jellyfin hadn't finished scanning within the budget. The items are fine and
will appear once it does, but they keep the artwork records pointing at the
tier they left, because the whole-library fallback scan runs in Jellyfin's
"Default" refresh mode and only fills in artwork it considers *missing*. Fix
them by hand (select them, then **Refresh metadata > Replace existing
images**), and raise `COLDARR_JELLYFIN_RESOLVE_TIMEOUT` so the next run waits
long enough. Note that an apply run holds Coldarr's apply lock until this
finishes, so the timeout is also the longest a finished run can block the
next one.

## Watch state after a move

A moved item's new Jellyfin item ID also leaves its user data - played
status, resume position, and the like - behind on the old ID. The optional
[Restore User Data After Move](https://github.com/voc0der/jellyfin-plugin-restore-userdata-after-move)
plugin puts it back. With the plugin installed, the scheduled Run the Plan
task can start it once a run's moves have all landed and Jellyfin has
indexed them: turn on **On completion, start Restore user data after move**
under **Settings > Scheduler**. Applying a plan by hand never starts it.
