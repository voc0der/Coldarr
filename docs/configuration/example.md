# Example configuration

Copy the repository's
[`coldarr.example.yaml`](https://github.com/voc0der/Coldarr/blob/main/coldarr.example.yaml)
to `coldarr.yaml` and adjust the paths and policy for your setup. In Docker,
place it in the host directory bound to `/config`. Alternatively, add tiers
through the web interface and let Coldarr create the file.

Connection credentials are configured separately; see [Connections](connections.md).
See [Storage tiers](tiers.md) and [Scoring and Favorites](scoring.md) for how
the policy is used.

```yaml title="coldarr.example.yaml"
--8<-- "coldarr.example.yaml"
```
