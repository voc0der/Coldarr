# CLI

Most people will run Coldarr as the Docker web GUI - see the
[quick start](../getting-started.md) for that. This is for running it as a
binary or scripting it (cron, systemd, `docker compose run`) instead.

## Building from source

```sh
go build -o coldarr ./cmd/coldarr
```

Go 1.26+. No other build-time dependencies - see the
[development guide](../development/index.md) for details.

## Setting up connections

```sh
./coldarr connections set radarr --url http://localhost:7878 --api-key <key>
./coldarr connections set sonarr --url http://localhost:8989 --api-key <key>
cp coldarr.example.yaml coldarr.yaml   # or add tiers via `coldarr serve`
```

Radarr/Sonarr API keys are under **Settings > General** in each app. Jellyfin
is optional and set up the same way, with `jellyfin` as the app name. See
[Connections](../configuration/connections.md) for how connections are
stored and env-var overrides.

## Commands

```sh
coldarr report   # tier usage + scored inventory, read-only
coldarr plan     # builds and prints a move plan, makes no changes
coldarr apply    # builds a plan, prompts, then executes it
coldarr apply -y # skip the confirmation prompt (e.g. for cron/systemd)
coldarr connections list|set|test|delete <radarr|sonarr|jellyfin>
coldarr serve    # run the web GUI, default port 8478
coldarr version  # print the Coldarr version
```

All commands take `--config path/to/coldarr.yaml` (or `-c`; default
`./coldarr.yaml`, overridable via the `COLDARR_CONFIG` env var).

`serve` also takes `--listen`, `--tls-cert-file`, `--tls-key-file`, and
`--trusted-reverse-proxies-cidr`, each defaulting to its
[environment variable](../configuration/docker.md#web-server).

## Running the CLI in Docker

The image's default command is `serve`; override it to run one-shot CLI
commands against the same `/config` volume instead:

```sh
docker compose run --rm coldarr report
docker compose run --rm coldarr plan
docker compose run --rm coldarr apply --yes
```

```sh
# /etc/cron.d/coldarr - rebalance every night at 4am
0 4 * * * root docker compose -f /path/to/docker-compose.yml run --rm coldarr apply --yes
```
