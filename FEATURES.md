# What Coldarr does

You've got fast storage and slow storage. Coldarr figures out what's safe to
move from the fast drive to the slow one, and does it - without you babysitting
it.

## The gist

- Tell it which drives are "hot" (fast, primary) and which are "cold"
  (overflow - satellite drives, a slower array, whatever you've got).
- It pulls your whole library from Radarr/Sonarr - age, size, tags, quality
  profile, whether a series has ended, whether anything's actively
  downloading right now.
- Anything that looks safe to move gets ranked coldest-and-biggest-first, and
  cold drives get packed toward a usage target you set.
- It never touches a file directly. It asks Radarr/Sonarr to do the move
  (their own bulk-move API), so their database never drifts out of sync with
  what's actually on disk.
- Moves happen one at a time per physical drive, and Coldarr waits for each
  one to actually land before starting the next. Firing a dozen simultaneous
  multi-GB transfers at one NAS is a great way to hang it - this happened
  during testing, hence the rule.
- Everything's dry-run by default. `report` and `plan` (or the dashboard/plan
  page in the GUI) never touch anything. `apply` asks you to confirm first,
  or skip the prompt with `-y` for cron.

## Why bother splitting hot/cold at all

Your hot storage is probably a real NAS - RAID or similar, actually
redundant, survives a drive dying. Your cold/satellite drives are probably
just bare USB drives sitting in a closet - zero redundancy, one drive, one
point of failure.

So keeping your newest and most-watched stuff on the protected NAS, and only
pushing the old, cold, easily-replaceable stuff out to bare drives, means
that when - not if - one of those cheap satellite drives eventually dies,
you lose the cold stuff. Annoying, not catastrophic. At least until you
refactor or throw more infrastructure at it.

## What it protects, automatically

- Anything tagged `never-move` (or whatever you call it) in your config
- Anything actively downloading or importing right now
- Anything any Jellyfin user has favorited
- Anything added recently (a grace period, before it's even considered)
- Currently-airing TV, if you want that on
- Anything not released/premiered yet (an unreleased movie, a show that
  hasn't aired its first episode) - it's about to start actively growing,
  so it stays off cold storage. If one of these turns up already sitting on
  a cold drive (imported straight there, or left behind from an earlier
  season), Coldarr moves it back to hot on its own - the one case where a
  move runs the other direction.

## How you run it

CLI or a small web GUI (`coldarr serve`) - same config file either way. Cron
the CLI, click around in the GUI, or both.

## Keeping tabs on it without watching it

Settings > Notifications takes a webhook URL for your own
[Apprise](https://github.com/caronc/apprise) endpoint. Point it there and
every run - scheduled or manual - sends you a one-line summary. Flip on
Verbose if you also want a ping per item, not just the summary; it's off by
default so a big move doesn't spam you.

Settings > Scheduler adds two jobs, both off until you turn them on:

- **Run the Plan** - does exactly what clicking Apply does, just on a
  schedule (every N hours, or daily at a time you pick). No cron entry to
  write, no `-y` flag to remember.
- **Rescan Cold Storage** - a read-only health check on your cold tiers
  (usage, reachability), nothing gets moved. Handy for those bare satellite
  drives - you find out one's gone missing or full from a notification, not
  from a failed transfer at 3am.

## What it's not (yet)

- No Plex - Jellyfin only, for now
- No watch-history/play-count scoring - Favorites are in, that's it
- No torrent-client awareness
