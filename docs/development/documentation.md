# Working on the documentation

The documentation uses [Zensical](https://zensical.org/docs/). Canonical pages
live in `docs/`, with navigation and theme settings in `zensical.toml`.
The root README is the repository introduction; the other root guides keep
their old heading anchors and point readers to the corresponding site pages.

## Install

From the repository root, create a virtual environment with Python 3.10 or
newer and install the pinned documentation dependency:

=== "pip"

    ```sh
    python3 -m venv .venv-docs
    . .venv-docs/bin/activate
    python -m pip install -r requirements-docs.txt
    ```

    On Windows, activate it with `.venv-docs\Scripts\activate` instead.

=== "uv"

    ```sh
    uv venv .venv-docs
    uv pip install --python .venv-docs -r requirements-docs.txt
    . .venv-docs/bin/activate
    ```

The Python environment is only for documentation. Building Coldarr itself
still requires only Go.

## Preview

```sh
zensical serve
```

Open the local URL printed by Zensical, normally <http://localhost:8000>.
Edits rebuild automatically. Check the affected pages at desktop and mobile
widths, including light and dark themes.

## Build and validate

```sh
zensical build --clean --strict
```

The generated site goes into `site/`. Strict mode fails on warnings, including
broken page links and heading anchors. Both `site/` and Zensical's `.cache/`
are ignored by Git and excluded from the Docker build context.

## Editing pages

- Add new pages to the explicit `nav` in `zensical.toml`.
- Use relative Markdown links, such as `../configuration/tiers.md#mount-safety`.
  Zensical rewrites them to site URLs when building.
- Use fenced code blocks with a language, such as `sh`, `yaml`, or `text`.
- Use Zensical admonitions (`!!! note` or `!!! warning`) for callouts.
- Prefer definition lists to wide tables for reference entries, such as the
  environment variables; they stay readable on a phone.
- Keep published images under `docs/assets/`. The gallery harness writes to
  `docs/assets/screenshots/`; see the [gallery guide](index.md#screenshots-gallery-harness).
- Add `#gh-light-mode-only` and `#gh-dark-mode-only` to the light and dark
  copy of each screenshot. The site shows the one matching its theme toggle,
  and GitHub does the same when someone reads the page in the repository.
- Keep the example YAML, compose file, and license in their root files.
  Snippets include them in the site at build time, avoiding duplicate copies.
  Missing snippet files fail the build.
- Link to source code on GitHub when it is outside `docs/`. Repository-relative
  links to Go files do not exist in the generated site.

## Publishing

The workflow in
[`.github/workflows/docs.yml`](https://github.com/voc0der/Coldarr/blob/main/.github/workflows/docs.yml)
builds documentation pull requests and uploads a preview artifact. It deploys
successful builds from `main` using GitHub Pages. Pull requests cannot deploy.

Before the first deployment, set the repository's **Settings > Pages > Build
and deployment > Source** to **GitHub Actions**. This is the repository setup
required by [Zensical's publishing guide](https://zensical.org/docs/publish-your-site/).
Then merge the documentation changes into `main`, or run the Documentation
workflow manually on `main` once the workflow is present there.

The configured production URL is <https://voc0der.github.io/Coldarr/>. If a
custom domain is introduced, update `site_url` in `zensical.toml` and the
repository's Pages settings together.

The workflow installs `requirements-docs.txt`, runs the strict clean build,
and uploads `site/`. It does not cache Zensical's build output.

Renovate picks up the Zensical pin in `requirements-docs.txt` and folds its
updates into the usual minor/patch group. That PR changes the requirements
file, so the strict build runs on it, and a release that breaks the build
can't merge on its own.
