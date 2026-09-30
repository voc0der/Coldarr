# Development

Notes for working on Coldarr itself - building, testing, and shipping a
release. If you just want to run Coldarr, see the [quick start](../getting-started.md).

## Building

```sh
go build -o coldarr ./cmd/coldarr
```

Go 1.26+. No other build-time dependencies - the web GUI's assets
(templates, CSS, htmx, icons) are all embedded via `//go:embed`, and the
release Docker image is a standard multi-stage build (see `Dockerfile`).

## Testing

For Go changes, all of these should be clean before opening a PR. CI runs
these checks as well:

```sh
go build ./...
go vet ./...
gofmt -l .          # should print nothing
go test ./... -race
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run ./...
```

Coverage is not part of that list and is not computed by CI - the README
badge is static, refreshed locally with `scripts/coverage.sh` when a change
moves the number. See [Contributing](contributing.md#coverage).

Unit tests cover every `internal/` package. `cmd/coldarr` (cobra command
wiring) and most of `internal/webui` are the exception - `internal/webui`
is a set of Go `html/template` pages with no JS framework, so verifying a
UI change means actually running the server and looking at it:

1. Build a throwaway `coldarr.yaml` pointing tiers at temp directories, and
   (if the change needs library data) seed a `history.json` and/or stand up
   minimal fake Radarr/Sonarr HTTP servers - just enough of the Servarr v3
   API surface Coldarr actually calls (`GET /api/v3/movie`, `/tag`,
   `/qualityprofile`, `/queue`, `/movie/{id}`, `PUT /movie/editor`, and the
   `/series` equivalents) to exercise inventory/scoring/planning/apply
   end-to-end without touching a real Radarr/Sonarr instance.
2. Run `go run ./cmd/coldarr --config <path> serve --listen :<port>` in the
   background.
3. Drive it with a headless browser (Playwright works well - `chromium-cli`
   if available, otherwise the Python/Node `playwright` package directly)
   and take real screenshots, in both light and dark
   (`prefers-color-scheme`) - don't just grep the HTML. Check
   `console`/`pageerror` events too; a page can render its shell while a
   fetch silently fails.
4. For anything involving the background apply flow, override
   `COLDARR_SETTLE_CHECK_INTERVAL` / `COLDARR_SETTLE_STABLE_CHECKS` /
   `COLDARR_SETTLE_MAX_WAIT` (short durations, e.g. `1s`/`1`/`3s`) so a test
   run settles in seconds instead of waiting on real disk growth.

`tools/gallery/run.sh --serve` does steps 1, 2 and 4 for you - see below.

## Screenshots (gallery harness)

`tools/gallery/` runs Coldarr, built from your checkout, against a fake
world in one container: FUSE "drives" whose `statfs` reports whatever
capacity and usage `fixture.yaml` asks for (a 16TB satellite at 89% costs a
few KB of sparse files), and fake Radarr/Sonarr/Jellyfin serving the
fixture's library. Moves really copy between those drives over a few
seconds, so apply, settle, and landing confirmation all run for real. Each
drive is its own mount, so `require_mount`, shared-volume detection, and
the dead-drive check behave as they do in production. It's a separate Go
module (go-fuse never touches Coldarr's own `go.mod`) and needs Docker
with FUSE on the host, plus Playwright for capturing.

```sh
tools/gallery/run.sh                      # every scene, light + dark, into docs/assets/screenshots/
tools/gallery/run.sh --only plan history  # just these scenes
tools/gallery/run.sh --serve              # leave it running at http://127.0.0.1:18478 (password: gallery)
```

Scenes run in order, since some change state (`applying` really applies
the plan, and `dead-drive` unmounts a satellite). To change what the
screenshots show, edit `fixture.yaml` (drives, library, orphans, history)
and `tools/gallery/coldarr.yaml` (tiers, policy, schedules).

## Documentation

The site uses Zensical. See [Working on the documentation](documentation.md)
for installation, local previews, strict builds, and GitHub Pages setup.

## CI/CD

- `.github/workflows/ci.yml` - on every PR and push to `main`: `go build`,
  `go vet`, a `gofmt -l` check, `go test -race`, `golangci-lint` (config in
  `.golangci.yml` - standard linters plus `bodyclose`, `errorlint`,
  `gosec`, `misspell`, `unconvert`, `unparam`), `govulncheck` against the
  module and its dependencies, and a docker build (not pushed) to catch
  Dockerfile breakage early.
- `.github/workflows/docs.yml` - builds the Zensical site in strict mode on
  documentation PRs and pushes to `main`, and deploys successful builds from
  `main` to GitHub Pages. See [publishing setup](documentation.md#publishing).
- `.github/workflows/renovate.yml` - runs Renovate (config in
  `.github/renovate.json5`) hourly, after every push to `main`, and when CI
  finishes on a `renovate/*` branch. It keeps Go modules, GitHub Actions,
  the Dockerfiles' base images, and the documentation dependency up to date.
  Minor and patch updates share one PR, which merges on its own once CI passes and the release is 3 days
  old; majors wait for approval on the Dependency Dashboard issue. Security
  fixes skip the wait: Renovate opens them from the repo's Dependabot alerts
  and from osv.dev, and Dependabot's own security-update PRs are off so each
  fix arrives once. The alerts and code scanning are configured separately
  under repo Settings > Security (no workflow file needed for those).
- `.github/workflows/release.yml` - on publishing a GitHub Release: builds
  a multi-arch (amd64/arm64) image and pushes it to both
  `ghcr.io/voc0der/coldarr` and `docker.io/voc0der/coldarr`, tagged with
  the release version, its `major.minor`, and `latest` (skipped for
  prereleases). Also runnable manually via `workflow_dispatch`. GHCR
  authenticates with the repo's built-in `GITHUB_TOKEN` - no setup needed.
  Docker Hub needs two repo-level settings under Settings > Secrets and
  variables > Actions:

    - Variable `DOCKERHUB_USERNAME` - the Docker Hub username (`voc0der`)
    - Secret `DOCKERHUB_TOKEN` - a Docker Hub access token (Account
      Settings > Security > Personal access tokens on hub.docker.com; scope
      it to "Read & Write" on the `coldarr` repo, not a full account
      password)

## Contributing

See [Contributing](contributing.md) for branch/commit/PR naming. The
short version: one focused change per branch/PR - don't bundle unrelated
fixes/features together, even if they happened to be worked on in the same
sitting.

## Releasing

"Releasing" means more than merging to `main` - it means cutting an actual
versioned GitHub Release, because that's what `release.yml` listens for to
build and publish the Docker image. To ship a new version:

1. Make sure everything landing in the release is merged to `main` as
   separate, focused PRs (per Contributing above).
2. Decide the version bump: patch for pure fixes, minor for any new
   feature, following semver off the latest tag
   (`git tag --sort=-v:refname | head -1` or `gh release list --limit 1`).
3. Cut the release:

    ```sh
    gh release create vX.Y.Z --title "vX.Y.Z - short summary" --notes "..."
    ```

    Title format: `vX.Y.Z - short summary`, joining multiple unrelated
    changes with " + " if a release bundles more than one (e.g.
    `v0.5.0 - safe concurrent apply + favicon`). Notes are hand-written
    markdown - `## Highlights` or `## Fix` sections, a bold lead-in per
    change, and *why* it matters, not just what changed. Look at past
    releases (`gh release view vX.Y.Z`) for the tone to match.
4. The image build/push happens automatically from there - no manual
   Docker steps.

## Roadmap (not yet implemented)

- Plex support alongside Jellyfin
- Jellyfin play-history/play-count and request-history (Jellyseerr/
  Overseerr) as scoring inputs (Favorites are already in)
- Torrent client / seeding-state awareness
- Editing policy thresholds (tags, cooldown, score threshold) through the
  GUI, not just tiers/connections/notifications/scheduler
- Translations for the web GUI - it's English-only today, with strings
  written directly into the `html/template` pages rather than pulled from
  a message catalog. Planned approach is Weblate (hosted, free for open
  source), which would need: extracting GUI strings into a catalog format
  it can translate (e.g. go-i18n/gotext), a Weblate component pointed at
  this repo, and a language switcher in the GUI. Not started - no catalog,
  no Weblate project, no switcher yet.
