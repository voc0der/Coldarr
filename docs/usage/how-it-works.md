# How a run works

Coldarr works in four steps. `report` stops after scoring, `plan` stops after
planning, and only `apply` (or Apply in the web interface) goes on to move
anything.

## 1. Inventory

Check every configured tier path (exists? mounted, if
`require_mount` is set? how full?), then pull every movie from Radarr
and every series from Sonarr, including tags, quality profile, and
whether an active download/import is in progress for it.

## 2. Score

Each item is evaluated into one of three buckets:

- `protected` - tagged `never-move`/`keep-hot`/etc, or has an active
  download/import. Never touched.
- `hot` - should stay on primary storage (marked Favorite by any
  Jellyfin user, recently added, a currently-airing series, or just
  didn't score high enough to be cold). Never moved to cold, and
  reclaimed back from cold when it's a Favorite.
- `cold` - safe to relocate to overflow storage, with a score used to
  rank *which* cold items move first.

See [Scoring and Favorites](../configuration/scoring.md) for the rules.

## 3. Plan

Coldarr does not steer hot storage toward any usage level -
it's runoff, not a control variable, and it's fine for it to sit
however full it ends up. Every cold-scored item currently on a hot
path is a move candidate (coldest and largest first), assigned to
whichever cold-tier path has room under its `target_used_percent` (the
fill goal); if nothing has target room, it falls back to whatever has
room under `max_used_percent` (the hard ceiling, never crossed either
way). Among viable destinations it prefers the fullest-but-not-full
one, so satellites get packed one at a time instead of spread thin.
Items moved within `cooldown_days` are skipped.

## 4. Apply

Apply runs in the background and returns a live-updating status
page/log immediately; the moves themselves are serialized **one at a
time per destination physical volume** (never more than one write in
flight against the same disk), while different volumes proceed in
parallel. Hot storage, a read source, isn't throttled.

After asking Radarr/Sonarr to relocate an item (`moveFiles: true`), Coldarr
watches the destination's disk usage until the transfer has actually landed
before starting the next item queued for that same volume - the move
API returns once the operation is queued, not once the bytes are on
disk, so trusting it alone isn't enough. Only one apply can run at a
time, system-wide, enforced by a crash-safe lock. Every completed move
is logged to the history file with its real completion time.

Each move is reported to Jellyfin the moment it lands - naming the vacated
and new paths - so Jellyfin's rescan of those folders runs against the rest
of the run instead of starting from cold once the last move finishes. Each
moved item is then refreshed individually, in the background while the next
move runs: Coldarr waits for Jellyfin to surface the item at its new path and
forces a full metadata and image refresh on it. A moved item gets a brand-new Jellyfin
item ID (Jellyfin derives IDs from the file path), and a plain library
scan only fills in artwork it considers *missing* - so without this an
item can land in the new tier with no poster at all. Just before that
refresh, Coldarr puts back the date added each movie and episode had
before the move, which it notes as the run starts - Jellyfin dates a moved
file as newly added, so otherwise the title shows up under Recently Added
again. A whole-library
scan is still used as a fallback if an item can't be found at its new
path. See [Jellyfin](../configuration/jellyfin.md).

## Confirmation

`report` and `plan` (or the web interface's dashboard and Plan page) are
read-only. The CLI prompts before applying unless you pass `--yes`; the
web interface asks for confirmation. Enabling the scheduled Run the Plan
job authorizes it to apply automatically at the configured times.

## Why one destination volume at a time

Firing every move in a plan at
once - many large simultaneous writes across several files/drives - is a
very different load profile than steady, one-at-a-time movement, and can
saturate a storage subsystem badly enough to make a whole host
unresponsive. This isn't hypothetical; it happened during testing. The
serialization is keyed by physical device, not tier name, so it also
catches two differently-named tier paths that turn out to be the same
disk (see [Shared volumes](../configuration/tiers.md#shared-volumes)).
