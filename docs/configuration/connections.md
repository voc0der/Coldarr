# Connections

Radarr/Sonarr/Jellyfin connection info (URL + API key) is **not** part of
`coldarr.yaml`. It's stored encrypted at rest, alongside the config file,
in `connections.enc.json` (with a random key auto-generated on first run
at `.coldarr.key` next to it). You can set/inspect it three ways, in order
of precedence:

1. **Environment variables** - `RADARR_URL` / `RADARR_API_KEY` (and
   `SONARR_*`, `JELLYFIN_*`, plus `JELLYFIN_ENABLED`). If set, these
   always win, regardless of what's stored - useful for infra-as-code
   deployments that don't want to touch the GUI at all.
2. **The web GUI's Connections page** (**Settings > Connections**) - fill in
   URL + API key, hit "Test connection" to confirm it actually works, then
   Save. If an app's env vars are set, its fields show locked with a note
   explaining why.
3. **The CLI**: `coldarr connections set/list/test/delete` - see the
   [CLI guide](../usage/cli.md#setting-up-connections).

*Threat model:* this protects against the connection file leaking through
casual exposure (pasted into a support thread, an accidentally-committed
volume backup) - not against an attacker who already has read access to
the container/filesystem, since the key lives right next to the
ciphertext on the same volume.

## External URLs

Each connection on the Connections page also takes an optional external URL.
It's used only to build the clickable links in the Plan and History pages'
Links column - e.g. a public hostname your reverse proxy exposes instead of
the connection URL. Left blank, links fall back to the connection URL. The
external URL doesn't need to be reachable from Coldarr itself, only from
your browser.
