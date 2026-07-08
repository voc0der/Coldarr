# Configuration

Reference for running Coldarr day to day: how a run actually works,
connections, tiers, the CLI/GUI surface, Docker, and scoring. For the pitch
see [README.md](README.md)/[FEATURES.md](FEATURES.md); for building, testing,
and releasing see [DEVELOPMENT.md](DEVELOPMENT.md).

## How a run works

1. **Inventory** - check every configured tier path (exists? mounted, if
   `require_mount` is set? how full?), then pull every movie from Radarr
   and every series from Sonarr, including tags, quality profile, and
   whether an active download/import is in progress for it.
2. **Score** - each item is evaluated into one of three buckets:
   - `protected` - tagged `never-move`/`keep-hot`/etc, marked Favorite by
     any Jellyfin user, or has an active download/import. Never touched.
   - `hot` - should stay on primary storage (recently added, a
     currently-airing series, or just didn't score high enough to be
     cold).
   - `cold` - safe to relocate to overflow storage, with a score used to
     rank *which* cold items move first.
3. **Plan** - Coldarr does not steer hot storage toward any usage level -
   it's runoff, not a control variable, and it's fine for it to sit
   however full it ends up. Every cold-scored item currently on a hot
   path is a move candidate (coldest and largest first), assigned to
   whichever cold-tier path has room under its `target_used_percent` (the
   fill goal); if nothing has target room, it falls back to whatever has
   room under `max_used_percent` (the hard ceiling, never crossed either
   way). Among viable destinations it prefers the fullest-but-not-full
   one, so satellites get packed one at a time instead of spread thin.
   Items moved within `cooldown_days` are skipped.
4. **Apply** - runs in the background and returns a live-updating status
   page/log immediately; the moves themselves are serialized **one at a
   time per destination physical volume** (never more than one write in
   flight against the same disk), while different volumes proceed in
   parallel. Hot storage, a read source, isn't throttled. After asking
   Radarr/Sonarr to relocate an item (`moveFiles: true`), Coldarr watches
   the destination's disk usage until the transfer has actually landed
   before starting the next item queued for that same volume - the move
   API returns once the operation is queued, not once the bytes are on
   disk, so trusting it alone isn't enough. Only one apply can run at a
   time, system-wide, enforced by a crash-safe lock. Every completed move
   is logged to the history file with its real completion time, then
   Jellyfin is asked to refresh once the whole run finishes.

Nothing above ever happens without `report`/`plan` (or the GUI's dashboard
and plan page) being read-only, or without confirming before an apply
executes - a `--yes` flag on the CLI, a JS confirm dialog in the GUI.

**Why one destination volume at a time:** firing every move in a plan at
once - many large simultaneous writes across several files/drives - is a
very different load profile than steady, one-at-a-time movement, and can
saturate a storage subsystem badly enough to make a whole host
unresponsive. This isn't hypothetical; it happened during testing. The
serialization is keyed by physical device, not tier name, so it also
catches two differently-named tier paths that turn out to be the same
disk (see [Shared volumes](#shared-volumes)).

## Connections

Radarr/Sonarr/Jellyfin connection info (URL + API key) is **not** part of
`coldarr.yaml`. It's stored encrypted at rest, alongside the config file,
in `connections.enc.json` (with a random key auto-generated on first run
at `.coldarr.key` next to it). You can set/inspect it three ways, in order
of precedence:

1. **Environment variables** - `RADARR_URL` / `RADARR_API_KEY` (and
   `SONARR_*`, `JELLYFIN_*`, plus `JELLYFIN_ENABLED`). If set, these
   always win, regardless of what's stored - useful for infra-as-code
   deployments that don't want to touch the GUI at all.
2. **The web GUI's Connections page** - fill in URL + API key, hit "Test
   connection" to confirm it actually works, then Save. If an app's env
   vars are set, its fields show locked with a note explaining why.
3. **The CLI**: `coldarr connections set/list/test/delete`.

*Threat model:* this protects against the connection file leaking through
casual exposure (pasted into a support thread, an accidentally-committed
volume backup) - not against an attacker who already has read access to
the container/filesystem, since the key lives right next to the
ciphertext on the same volume.

## Usage

Prefer the CLI, or scripting Coldarr instead of clicking through the GUI?
See [CLI.md](CLI.md) for the full command reference.

Web GUI (`coldarr serve`, default `:8478`, override with `--listen` or
`COLDARR_LISTEN_ADDR`):

- **Dashboard** - tier usage/space allotment, library item counts by
  decision (protected/hot/cold), connection status.
- **Plan** - the same dry-run preview as the CLI's `plan`, with an Apply
  button (confirm dialog, then executes through Radarr/Sonarr) and live
  progress of the current (or most recent) apply run, auto-refreshing
  until it finishes.
- **History** - every move Coldarr has executed, with a size-verification
  check to catch a transfer a crash left half-done.
- **Settings** - Connections (configure/test Radarr/Sonarr/Jellyfin),
  Tiers (add/edit/delete hot and cold tiers, live per-path disk usage),
  Auth (optional OIDC login and group access), Notifications (an Apprise
  webhook for run summaries), and Scheduler (optionally run the plan or a
  cold-storage health check on a schedule - see [FEATURES.md](FEATURES.md)
  for details).

OIDC auth is disabled by default. The GUI can view connection status and
trigger real moves, so enable Settings -> Auth, keep it behind your own
trusted network boundary, or avoid publishing its port directly to the
internet.

## Docker

See [README.md](README.md) for the quick-start compose one-liner. The
default command is `serve` - a persistent GUI, not a one-shot job.
Everything Coldarr persists (`coldarr.yaml`, the encrypted connection
store + its key, move history) lives under one bind-mounted `/config`
directory. To run one-shot CLI commands (or a cron job) against that same
volume instead of the GUI, see [CLI.md](CLI.md#running-the-cli-in-docker).

**Environment variables** the image understands directly:

| Variable               | Purpose                                                          |
| ---------------------- | ----------------------------------------------------------------- |
| `PUID` / `PGID`        | uid/gid the process runs as, so it can read your bind mounts. Same convention as Radarr/Sonarr/Jellyfin images. Default `1000`/`1000`. Ignored if the container is already started as non-root (`docker run --user`, compose `user:`, a rootless runtime, or a Kubernetes securityContext) - the entrypoint skips straight to running as whatever user it was given. |
| `TZ`                   | container timezone, mostly cosmetic for log timestamps.           |
| `COLDARR_CONFIG`       | path to the config file. Default `/config/coldarr.yaml` (via the image's `/config` working directory). |
| `COLDARR_LISTEN_ADDR`  | address `serve` listens on. Default `:8478`.                      |
| `COLDARR_TLS_CERT_FILE` / `COLDARR_TLS_KEY_FILE` | certificate and private-key paths. Set both to make `coldarr serve` listen with HTTPS directly. |
| `COLDARR_TRUSTED_REVERSE_PROXIES_CIDR` (`TRUSTED_REVERSE_PROXIES_CIDR`) | comma-separated proxy CIDRs whose `Forwarded` / `X-Forwarded-Proto` / `X-Forwarded-Host` headers should be trusted. Headers from other remote IPs are ignored. |
| `COLDARR_OIDC_ENABLED` / `COLDARR_OIDC_ISSUER_URL` / `COLDARR_OIDC_CLIENT_ID` / `COLDARR_OIDC_CLIENT_SECRET` | OIDC auth overrides. When any are set, env values win over GUI-saved values. Set `COLDARR_OIDC_ENABLED=false` to disable OIDC entirely for troubleshooting. |
| `COLDARR_OIDC_REDIRECT_URL` / `COLDARR_OIDC_REQUIRED_GROUP` / `COLDARR_OIDC_GROUPS_CLAIM` | Optional OIDC details. Required group defaults to `coldarr`; groups claim defaults to `groups`. |
| `COLDARR_OIDC_AUTO_LOGIN` | set to `true`/`false` to skip straight to the IdP instead of showing a login button, overriding the GUI-saved Settings -> Auth checkbox. |
| `COLDARR_OIDC_CLIENT_SECRET_POST` | set to `true` for providers whose client registration uses `token_endpoint_auth_method: client_secret_post` (common with Authelia). The GUI exposes this as a checkbox under Settings -> Auth. |
| `RADARR_URL` / `RADARR_API_KEY` (`SONARR_*`, `JELLYFIN_*`, `JELLYFIN_ENABLED`) | connection overrides - see [Connections](#connections). |
| `COLDARR_SETTLE_CHECK_INTERVAL` / `COLDARR_SETTLE_STABLE_CHECKS` / `COLDARR_SETTLE_MAX_WAIT` | tune how long `apply` waits for a move to actually land on disk before starting the next one queued for the same volume (Go duration strings, e.g. `5s`/`6h`). Defaults suit typical local disks; raise `MAX_WAIT` for very large files on slow/network storage. |

**Everything else** (tiers, tags, thresholds) goes through `coldarr.yaml`,
which supports `${VAR}` substitution against whatever environment
variables you pass to the container.

Tier paths must be bind-mounted at the *same path Radarr/Sonarr use
internally* - Coldarr compares its own disk checks and the root folder
paths the Arr APIs report as literal strings, so a mismatched mount looks
like a misplaced or unavailable path.

## Tiers

A tier is a named policy (allowed media types, whether paths must be real
mount points, and for cold tiers, target/max usage) applied to one or more
physical paths. Each path is checked independently - a tier is a shared
policy across drives, not a pooled volume. See `coldarr.example.yaml` for
a worked example with one hot tier (the primary NAS) and two cold tiers
(movies and TV split across satellite drives) - or just add them through
the GUI's Tiers page, which writes the same file.

**Hot tiers have no `target_used_percent`/`max_used_percent`.** Coldarr
doesn't steer primary storage toward any usage level - it's runoff. In
the ideal case your cold drives sit at 99% and hot sits at whatever's left
over, and that's fine. If you want to know how full hot currently is,
the dashboard/`report` still show it - it just isn't a control variable.

**Cold tiers use both fields as a two-step packing goal:**
`target_used_percent` is what Coldarr actively packs toward; if nothing
has room under target, it falls back to `max_used_percent` - the hard
ceiling, never crossed. `max_used_percent` has no built-in cap - if you
want a satellite drive packed to 100%, set it to 100. Coldarr will
respect that.

Note: saving tiers through the GUI rewrites the whole `coldarr.yaml` file,
so hand-added comments won't survive a GUI save.

### Mount safety

Setting `require_mount: true` on a tier makes Coldarr verify that each of
its paths is a genuine mount point (a distinct filesystem from its parent
directory) before treating it as usable. This exists specifically to
catch the case where a satellite drive is unplugged or fails to mount:
without this check, Coldarr could otherwise "successfully" write into the
empty directory left behind on the root filesystem. A path that fails
this check is reported as unavailable and excluded from planning entirely
- Coldarr never falls back to using it anyway.

### Shared volumes

Sometimes two configured paths - even across different tiers, like
`Movies (Hot)` and `TV (Hot)` - are actually the same physical volume or
cluster storage, just different subdirectories. Optimizing between them
independently would be nonsense: filling one eats into the exact same
free space the other reports having.

Coldarr detects this **automatically**, by comparing each path's device
ID (the same technique `du -x`/`find -xdev` use to detect filesystem
boundaries) - there's nothing to configure, and it can't drift out of
sync the way a manually-declared grouping could if a mount changes later.
Paths sharing a volume show up flagged as such in `report` and the
dashboard/Tiers page, and the planner treats their capacity as one shared
pool: moving into one is reflected on the other, so it can never
double-commit the same disk across two differently-named destinations.

## Scoring

See [internal/scoring/scoring.go](internal/scoring/scoring.go) for the
full, small set of rules. In short: tags, an active download, or a
Jellyfin Favorite mark can force `protected` or `hot` outright; otherwise
items accumulate a score from age, size, a series having ended, time
since last aired, a low-priority quality profile, and unmonitored/missing
state. Items at or above `cold_score_threshold` are cold candidates,
ranked by that score. (Policy thresholds like these are still YAML-only
for now - not yet exposed in the GUI.)

**Jellyfin Favorites:** if Jellyfin is connected and enabled, Coldarr
fetches every user's favorited movies/series and matches them back to
Radarr/Sonarr items by path - anything favorited by anyone is protected,
same as a `never-move` tag. Matching is by path, so this only works
correctly if Jellyfin sees the same paths Radarr/Sonarr do (see
[Docker's path note](#docker) above). If the favorites fetch fails for
any reason, Coldarr proceeds without that protection for the run and
surfaces a warning in `report`/`plan`/the dashboard rather than failing
outright or silently skipping it.
