# CLI

Most people will run Coldarr as the Docker web GUI - see
[README.md](README.md) for that quick start. This is for running it as a
binary or scripting it (cron, systemd, `docker compose run`) instead.

## Building from source

```
go build -o coldarr ./cmd/coldarr
```

Go 1.26+. No other build-time dependencies - see
[DEVELOPMENT.md](DEVELOPMENT.md) for details.

## Setting up connections

```
./coldarr connections set radarr --url http://localhost:7878 --api-key <key>
./coldarr connections set sonarr --url http://localhost:8989 --api-key <key>
cp coldarr.example.yaml coldarr.yaml   # or add tiers via `coldarr serve`
```

Radarr/Sonarr API keys are under Settings -> General in each app. See
[CONFIGURATION.md](CONFIGURATION.md#connections) for how connections are
stored and env-var overrides.

## Commands

```
coldarr report   # tier usage + scored inventory, read-only
coldarr plan     # builds and prints a move plan, makes no changes
coldarr apply    # builds a plan, prompts, then executes it
coldarr apply -y # skip the confirmation prompt (e.g. for cron/systemd)
coldarr connections list|set|test|delete <radarr|sonarr|jellyfin>
coldarr serve    # run the web GUI, default port 8478
```

All commands take `--config path/to/coldarr.yaml` (default
`./coldarr.yaml`, overridable via the `COLDARR_CONFIG` env var).

## Running the CLI in Docker

The image's default command is `serve`; override it to run one-shot CLI
commands against the same `/config` volume instead:

```
docker compose run --rm coldarr report
docker compose run --rm coldarr plan
docker compose run --rm coldarr apply --yes
```

```
# /etc/cron.d/coldarr - rebalance every night at 4am
0 4 * * * root docker compose -f /path/to/docker-compose.yml run --rm coldarr apply --yes
```
