# Development

Notes for working on Coldarr itself - building, testing, and shipping a
release. If you just want to run Coldarr, see [README.md](README.md).

## Building

```
go build -o coldarr ./cmd/coldarr
```

Go 1.26+. No other build-time dependencies - the web GUI's assets
(templates, CSS, htmx, icons) are all embedded via `//go:embed`, and the
release Docker image is a standard multi-stage build (see `Dockerfile`).

## Testing

Before opening a PR, all of these should be clean (this is exactly what CI
runs - see below):

```
go build ./...
go vet ./...
gofmt -l .          # should print nothing
go test ./... -race
```

Unit tests cover the planner, scoring, mover, and secrets packages. There
are no automated tests for the web GUI (`internal/webui`) - it's a set of
Go `html/template` pages with no JS framework, so verifying a UI change
means actually running the server and looking at it:

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

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for branch/commit/PR naming. The
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
   feature, following semver off the latest tag (`git tag --sort=-v:refname
   | head -1` or `gh release list --limit 1`).
3. Cut the release:
   ```
   gh release create vX.Y.Z --title "vX.Y.Z - short summary" --notes "..."
   ```
   Title format: `vX.Y.Z - short summary`, joining multiple unrelated
   changes with " + " if a release bundles more than one (e.g. `v0.5.0 -
   safe concurrent apply + favicon`). Notes are hand-written markdown -
   `## Highlights` or `## Fix` sections, a bold lead-in per change, and
   *why* it matters, not just what changed. Look at past releases (`gh
   release view vX.Y.Z`) for the tone to match.
4. The image build/push happens automatically from there - no manual
   Docker steps.

## Roadmap (not yet implemented)

- Plex support alongside Jellyfin
- Jellyfin play-history/play-count and request-history (Jellyseerr/
  Overseerr) as scoring inputs (Favorites are already in)
- An automatic scheduled mode (report/plan/apply are all you get for now
  - on purpose, until the policy engine has proven itself)
- Torrent client / seeding-state awareness
- GUI authentication
- Editing policy thresholds (tags, cooldown, score threshold) through the
  GUI, not just tiers/connections
