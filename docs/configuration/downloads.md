# Downloads and seeding

With qBittorrent connected, Coldarr can run your download drive as a
revolving door. Downloads land on fast storage. Once a download has finished
and settled, it moves to a cheap, large drive whose only job is seeding. When
your trackers' seeding requirements are met, it can be cleared out. The fast
drive keeps room for the next batch, and the seed drive never fills with
downloads that have done their time.

Like library tiering, Coldarr never moves a file itself. It asks qBittorrent
to move each download, so qBittorrent always knows where its data is.

See the [Downloads screenshots](../screenshots.md#downloads) for a plan,
move progress, recent activity, and the label and seeding settings.

## How a download moves through

1. **It downloads.** qBittorrent saves it to the download drive. Coldarr
   leaves it alone while it downloads and doesn't touch its label, which
   usually starts as `movies`, `tv`, or whatever your *arr apps use.
2. **It settles.** Once it has finished, it sits on the download drive for
   the settle time (3 days by default). If its label is imported by Radarr,
   Sonarr, or Lidarr, Coldarr also waits until that app's history shows it
   imported. Moving it any earlier would break the import.
3. **It moves.** Coldarr asks qBittorrent to move it to seed storage, one
   download at a time. Right before each move, Coldarr asks qBittorrent
   again whether it's still finished. A move counts as done once qBittorrent
   reports it at the new location and Coldarr can see it there too. Only
   then does it get the seed label (`seed` by default), so a move that fails
   never leaves a download labeled as moved while its data is still on the
   download drive.
4. **It's done.** Once its trackers' seeding requirement is met, Coldarr can
   remove it from qBittorrent along with its files. This is off unless you
   turn it on.

## Setting it up

1. **Connect qBittorrent** under **Settings > Connections**. Use an API key
   (qBittorrent 5.2+), a WebUI username and password, or nothing at all if
   qBittorrent's WebUI skips authentication for Coldarr's address. Connect
   Lidarr too if music downloads go through it. See
   [Connections](connections.md).
2. **Mount the download folders into Coldarr at the same paths qBittorrent
   uses.** If qBittorrent saves to `/downloads` and seeds from `/seed`,
   Coldarr needs `/downloads` and `/seed` too, from the same drives.
   Read-only mounts are fine unless you want to delete orphaned files (see
   [Orphans in download folders](#orphans-in-download-folders)).
3. **Add two downloads tiers** under **Settings > Storage tiers**, choosing
   **Used for: Downloads (qBittorrent)**:
    - a **hot** tier for the download drive, such as `/downloads`.
      Downloads saved in a folder below it, like a category folder, count
      too.
    - a **cold** tier for seed storage, such as `/seed`, with a target and
      a max like any cold tier. If it's a separate disk, tick the box
      requiring its paths to be on their own mounted drive (see
      [Mount safety](tiers.md#mount-safety)).
4. **Set up labels** under **Settings > Downloads**. Every category and tag
   qBittorrent has is listed. Set the ones whose downloads should move to
   **Move to seed storage**, and pick the app that imports them.
5. **Open the Downloads page** to see what would move and why the rest
   won't, then click **Move these downloads**. Once you're happy with it,
   schedule it (see [Scheduled tasks](#scheduled-tasks)).

## Labels

A label is a qBittorrent category or tag. Each one can be set to:

- **Move to seed storage.** Downloads with this label are Coldarr's to move
  and, later, to remove.
- **Never touch.** Downloads with this label are left alone, even if another
  of their labels says to move them. Their space still counts toward each
  drive's usage.
- **No rule.** Left alone. Any label you add in qBittorrent later starts this
  way, so nothing new is acted on until you choose.

**Imported by** names the app that imports a label's downloads. Coldarr
then waits until that app has imported each download before moving it, by
looking the download up in the app's history. Leave it at **Nothing** for
labels no app imports, such as downloads you add by hand. If the app isn't
connected, those downloads wait, and the Downloads page says why.

Rules you've saved are kept even when a label disappears from qBittorrent.
The table flags them, and they stay until you remove them.

## What holds a download back

The Downloads page lists every download that has settled but isn't moving,
and why. The usual reasons:

- **Its importer hasn't imported it**, or isn't connected.
- **Automatic Torrent Management is on.** qBittorrent keeps such a download
  in its category's folder and would undo the move. Turn it off for that
  download, or let Coldarr switch it off for the downloads it moves (the
  checkbox under **Settings > Downloads > Moving**).
- **Coldarr can't see it.** qBittorrent lists a download that isn't there
  from Coldarr's side of the mount, which usually means the folders aren't
  mounted at the same paths. If Coldarr can't see any finished download
  qBittorrent lists on a path, nothing moves to or from that path until
  it's fixed.
- **Something already exists at its destination.** qBittorrent would keep
  the existing files in place of its own, so the download stays put. These
  are often left over from an earlier move; the Orphaned Storage page lists
  them.
- **Cross-seeds aren't all ready.** See below.

## Cross-seeds

Downloads that share the same files on disk move together, in a single
request, or not at all. If one of them isn't ready, none of them move. A
download whose files sit inside another download's folder never moves,
because moving either one would pull the files out from under the other.
Move those in qBittorrent yourself.

## Seeding rules

Under **Settings > Downloads > Seeding rules**:

- **Default for every tracker.** How many days a download must seed. 0
  means forever, which is the default. Nothing counts as done seeding until
  you set a requirement.
- **Grace margin.** Extra days added to every requirement (7 by default).
- **Tracker overrides.** A different requirement for particular trackers.
  Pick a tracker your downloads use, or type its host. An override for
  `example.org` also covers `tracker.example.org`, and the most specific
  override wins. If you paste a whole announce URL, only its host is kept.
  The passkey never reaches your config.

A download that announces to several trackers has to satisfy the strictest.
Coldarr can't see a tracker's own count of seeding time. It uses the
smaller of qBittorrent's seeding time and the time since the download
finished, and the grace margin covers the rest.

Which trackers each download uses is found by the **Scan Torrent Trackers**
task. qBittorrent answers one download per request, so the scan never runs
on a page view, and it only looks up downloads it hasn't seen before. Until
a download's trackers have been scanned, it never counts as done seeding.

## Removing finished seeds

Under **Settings > Downloads > Removing finished seeds**, choose a mode:

- **Off** (the default). Nothing is ever removed.
- **Report only.** The Downloads page and the **Remove Finished Seeds**
  task list what would be removed, and nothing is.
- **Remove.** Downloads that are done seeding are removed from
  qBittorrent, along with their files.

Coldarr removes a download only if every one of these holds:

- Its seeding requirement is known and met.
- The app that imports it has confirmed importing it, and it isn't in any
  app's queue.
- It doesn't share a folder with anything in Radarr, Sonarr, or Lidarr.
  If one of them can't be reached, nothing is removed.
- Files another download still uses are never deleted. If a download that
  isn't done shares the files, only the finished one is removed from
  qBittorrent and the files stay.
- Right before removing it, qBittorrent still says it's finished and done
  seeding.

Each run removes at most **Most to remove per run** downloads (25 by
default), longest-seeded first, so a mistaken rule costs a few downloads
rather than the drive. Every run that removes anything sends a
notification naming what went, whether or not notifications are verbose.
The Downloads page lists every removal under **Recent activity**.

**Remove these now** on the Downloads page removes exactly the list the
page shows. If that list has changed since the page loaded, nothing is
removed, and the page shows the list as it is now to check again.

With removal set to Remove, a download that's done seeding while still on
the download drive is removed from there instead of being moved to seed
storage first. Downloads can be removed from either drive.

## Orphans in download folders

With qBittorrent connected, **Settings > Orphaned Storage** gets a
**Download folders** panel. The orphan scan compares everything in each
download folder against qBittorrent's downloads, loose files included. It
lists anything qBittorrent doesn't know about, such as a download removed
without its files or what's left of a move that didn't finish. The scan's
notification calls these out.

To delete them from that page, mark the downloads tier as **not
read-only** under **Settings > Storage tiers**, and mount the folder
read-write into Coldarr. A tier stays read-only until you choose otherwise.
Coldarr deletes only what you click Delete on, only entries the last scan
listed, and only after checking with every service that nothing has started
using them since. This is the only place Coldarr deletes files itself.

## Staying safe

- **One move at a time.** Each move is confirmed finished before the next
  starts, and a library apply and a downloads apply never run at once.
- **Restarts.** qBittorrent keeps moving a download even if Coldarr stops.
  While qBittorrent is still moving or checking anything, every plan and
  apply waits, library ones included, since the disk numbers aren't
  reliable until it's done. A download whose relabel was cut short gets its
  seed label on the next run.
- **qBittorrent unreachable.** Nothing moves or is removed, and plans say
  why. If qBittorrent can't be reached to check for moves in progress,
  library plans wait too.
- **Missing drives.** As with library tiers, a downloads tier path failing
  its checks blocks every move until it's back.
- **Downloads in progress.** The space they're still due to write is held
  back, so neither planner counts on room that's about to be used.
- **Full seed storage.** If downloads Coldarr leaves alone, or files that
  aren't downloads at all, fill seed storage past its max by themselves,
  the dashboard flags it. Nothing will move there until some of it is
  cleared or the max is raised.

## Scheduled tasks

Under **Settings > Scheduler**, with qBittorrent connected. All are off
until you turn them on.

| Task | Default time | What it does |
| --- | --- | --- |
| Scan Torrent Trackers | 04:00 | Finds each download's trackers, for its seeding requirement |
| Remove Finished Seeds | 04:15 | Scans trackers, then reports or removes what's done seeding |
| Move Settled Downloads | 04:30 | Moves settled downloads to seed storage, one at a time |

## Example

```yaml
tiers:
  - name: download-ssd
    domain: downloads
    role: hot
    paths: [/downloads]

  - name: seed
    domain: downloads
    role: cold
    paths: [/seed]
    target_used_percent: 92
    max_used_percent: 97
    require_mount: true
    allow_deletes: false      # true lets Orphaned Storage delete here

torrents:
  settle_days: 3
  seed_label: seed
  allow_disabling_auto_tmm: false
  labels:
    - {label: movies, action: move, imported_by: radarr}
    - {label: tv, action: move, imported_by: sonarr}
    - {label: music, action: move, imported_by: lidarr}
    - {label: manual, action: move}
    - {label: transfer, action: ignore}
  seeding:
    min_seed_days: 0          # 0 = forever: nothing is done until you say so
    grace_days: 7
    overrides:
      - {host: tracker.example.org, min_seed_days: 14}
  removal:
    mode: off                 # off, report, or remove
    max_per_run: 25
```
