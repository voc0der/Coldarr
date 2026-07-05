# Coldarr

A policy-based storage-tiering balancer for Radarr/Sonarr libraries.

Coldarr looks at disk usage and library metadata (tags, quality profile,
monitored state, series ended/continuing, age, size), decides which movies
and series belong on hot vs. cold storage, and asks Radarr/Sonarr to
relocate them - so their databases stay the source of truth. Coldarr never
moves files on disk directly.

It has both a CLI and a web GUI, backed by the same config - use whichever
fits, or mix them (e.g. configure connections in the GUI, run `apply` from
cron).

## Status: first pass / MVP

Implemented:

- Radarr + Sonarr inventory (tags, quality profile, monitored/queue state)
- Disk usage per configured path, with mount-point safety checks
- A transparent, tunable scoring engine (protected / hot / cold)
- A planner that only moves cold items off hot paths that are actually
  over their configured threshold, packing cold destinations
  fullest-first
- Cooldown tracking so nothing gets moved twice in a short window
- Connections (Radarr/Sonarr/Jellyfin) stored encrypted at rest, with
  per-app environment-variable overrides that always take precedence
- A web GUI (`coldarr serve`) for connections, tiers, dashboard, plan
  preview, apply, and move history - and an equivalent CLI (`report` /
  `plan` / `apply` / `connections`)
- Moves executed through Radarr's/Sonarr's own bulk "move" API, so their
  databases stay correct
- Optional Jellyfin library refresh trigger after a successful apply

Deliberately out of scope for this pass (see the bottom of this file):
Plex, Seerr/Overseerr request history, watch-history scoring, a
fully-automatic scheduled mode, torrent client awareness, and GUI
authentication.

## How a run works

1. **Inventory** - check every configured tier path (exists? mounted, if
   `require_mount` is set? how full?), then pull every movie from Radarr
   and every series from Sonarr, including tags, quality profile, and
   whether an active download/import is in progress for it.
2. **Score** - each item is evaluated into one of three buckets:
   - `protected` - tagged `never-move`/`keep-hot`/etc, or has an active
     download/import. Never touched.
   - `hot` - should stay on primary storage (recently added, a
     currently-airing series, or just didn't score high enough to be
     cold).
   - `cold` - safe to relocate to overflow storage, with a score used to
     rank *which* cold items move first.
3. **Plan** - for each hot path over its configured `max_used_percent`,
   Coldarr picks cold-scored items stored on that exact path (coldest and
   largest first) and assigns each one to whichever cold-tier path has
   the most room to spare without crossing *its* `max_used_percent`. It
   prefers the fullest-but-not-full destination, so satellites get
   packed one at a time instead of spread thin. Items moved within
   `cooldown_days` are skipped.
4. **Apply** - moves are grouped by destination path and sent to
   Radarr/Sonarr's bulk editor endpoint (`moveFiles: true`), which
   relocates files on disk and keeps the folder name intact. Every
   successful move is logged to the history file immediately (used for
   cooldown on future runs), then Jellyfin is asked to refresh, if
   configured.

Nothing above ever happens without `report`/`plan` (or the GUI's dashboard
and plan page) being read-only, or without confirming before an apply
executes - a `--yes` flag on the CLI, a JS confirm dialog in the GUI.

## Setup

```
go build -o coldarr ./cmd/coldarr
./coldarr connections set radarr --url http://localhost:7878 --api-key <key>
./coldarr connections set sonarr --url http://localhost:8989 --api-key <key>
cp coldarr.example.yaml coldarr.yaml   # or just add tiers via `coldarr serve`
```

Radarr/Sonarr API keys can be found under Settings -> General in each app.

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

CLI:

```
coldarr report   # tier usage + scored inventory, read-only
coldarr plan     # builds and prints a move plan, makes no changes
coldarr apply    # builds a plan, prompts, then executes it
coldarr apply -y # skip the confirmation prompt (e.g. for cron/systemd)
coldarr connections list|set|test|delete <radarr|sonarr|jellyfin>
coldarr serve    # run the web GUI
```

All commands take `--config path/to/coldarr.yaml` (default
`./coldarr.yaml`, overridable via the `COLDARR_CONFIG` env var).

Web GUI (`coldarr serve`, default `:8478`, override with `--listen` or
`COLDARR_LISTEN_ADDR`):

- **Dashboard** - tier usage/space allotment, library item counts by
  decision (protected/hot/cold), connection status.
- **Connections** - configure and test Radarr/Sonarr/Jellyfin.
- **Tiers** - add/edit/delete hot and cold tiers, with live per-path disk
  usage shown right in the list.
- **Plan** - the same dry-run preview as the CLI's `plan`, with an Apply
  button (confirm dialog, then executes through Radarr/Sonarr).
- **History** - every move Coldarr has executed.

**The GUI has no built-in authentication.** It can view connection status
and trigger real moves, so put it behind your own reverse proxy/auth
(Authelia, Traefik forward-auth, etc.) or keep it off any network you
don't trust - don't publish its port directly to the internet.

## Docker

```
docker build -t coldarr .
cp docker-compose.example.yml docker-compose.yml   # edit tier paths, ports
cp .env.example .env                                # only if using env-var connections

docker compose up -d
# then open http://localhost:8478 and use the Connections/Tiers pages,
# or configure everything via env vars / a mounted coldarr.yaml instead
```

The default command is `serve` - a persistent GUI, not a one-shot job.
Everything Coldarr persists (`coldarr.yaml`, the encrypted connection
store + its key, move history) lives under one bind-mounted `/config`
directory.

Prefer the CLI / scripting instead of the GUI? Override `command`:

```
docker compose run --rm coldarr report
docker compose run --rm coldarr plan
docker compose run --rm coldarr apply --yes
```

```
# /etc/cron.d/coldarr - rebalance every night at 4am
0 4 * * * root docker compose -f /path/to/docker-compose.yml run --rm coldarr apply --yes
```

**Environment variables** the image understands directly:

| Variable               | Purpose                                                          |
| ---------------------- | ----------------------------------------------------------------- |
| `PUID` / `PGID`        | uid/gid the process runs as, so it can read your bind mounts. Same convention as Radarr/Sonarr/Jellyfin images. Default `1000`/`1000`. |
| `TZ`                   | container timezone, mostly cosmetic for log timestamps.           |
| `COLDARR_CONFIG`       | path to the config file. Default `/config/coldarr.yaml` (via the image's `/config` working directory). |
| `COLDARR_LISTEN_ADDR`  | address `serve` listens on. Default `:8478`.                      |
| `RADARR_URL` / `RADARR_API_KEY` (`SONARR_*`, `JELLYFIN_*`, `JELLYFIN_ENABLED`) | connection overrides - see [Connections](#connections). |

**Everything else** (tiers, tags, thresholds) goes through `coldarr.yaml`,
which supports `${VAR}` substitution against whatever environment
variables you pass to the container.

Tier paths must be bind-mounted at the *same path Radarr/Sonarr use
internally* - Coldarr compares its own disk checks and the root folder
paths the Arr APIs report as literal strings, so a mismatched mount looks
like a misplaced or unavailable path.

## Tiers

A tier is a named policy (target/max usage, allowed media types, whether
paths must be real mount points) applied to one or more physical paths.
Each path is checked independently - a tier is a shared policy across
drives, not a pooled volume. See `coldarr.example.yaml` for a worked
example with one hot tier (the primary NAS) and two cold tiers (movies
and TV split across satellite drives) - or just add them through the
GUI's Tiers page, which writes the same file.

`max_used_percent` has no built-in ceiling. The conventional advice for
overflow storage is 90-95%, so imports and metadata writes don't choke on
a completely full filesystem - but if you want a drive packed to 100%,
set it to 100. Coldarr won't stop you.

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

## Scoring

See [internal/scoring/scoring.go](internal/scoring/scoring.go) for the
full, small set of rules. In short: tags and active downloads can force
`protected` or `hot` outright; otherwise items accumulate a score from
age, size, a series having ended, time since last aired, a low-priority
quality profile, and unmonitored/missing state. Items at or above
`cold_score_threshold` are cold candidates, ranked by that score. (Policy
thresholds like these are still YAML-only for now - not yet exposed in
the GUI.)

## CI/CD

- `.github/workflows/ci.yml` - on every PR and push to `main`: `go build`,
  `go vet`, a `gofmt -l` check, `go test -race`, and a docker build (not
  pushed) to catch Dockerfile breakage early.
- `.github/workflows/release.yml` - on publishing a GitHub Release: builds
  a multi-arch (amd64/arm64) image and pushes it to
  `ghcr.io/voc0der/coldarr`, tagged with the release version, its
  `major.minor`, and `latest` (skipped for prereleases). Also runnable
  manually via `workflow_dispatch`. No extra secrets needed - it
  authenticates to GHCR with the repo's built-in `GITHUB_TOKEN`.

To ship a new version: bump behavior as needed, then cut a GitHub Release
with a semver tag (e.g. `v0.2.0`) - the image build/push happens
automatically from there.

## Roadmap (not in this pass)

- Plex support alongside Jellyfin
- Watch-history and request-history (Jellyseerr/Overseerr) as scoring
  inputs
- An automatic scheduled mode (report/plan/apply are all you get for now
  - on purpose, until the policy engine has proven itself)
- Torrent client / seeding-state awareness
- GUI authentication
- Editing policy thresholds (tags, cooldown, score threshold) through the
  GUI, not just tiers/connections
