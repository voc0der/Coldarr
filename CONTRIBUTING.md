# Contributing

## Rules

- Keep branches, commits, and PRs focused. Do not mix unrelated local changes into the same PR.
- Use semantic names by default.

## Naming

- Branches: `fix/<scope>-<summary>`, `feat/<scope>-<summary>`, `refactor/<scope>-<summary>`
- Commits: `fix(scope): summary`, `feat(scope): summary`, `refactor(scope): summary`
- PR titles: `fix(scope): summary`, `feat(scope): summary`, `refactor(scope): summary`

## Before opening a PR

Run the local check list in [DEVELOPMENT.md](DEVELOPMENT.md#testing) - `go
build`, `go vet`, `gofmt -l`, `go test -race`, and `golangci-lint`. CI runs
exactly those, so a clean local run means a green PR.

Don't skip `golangci-lint`. It is the one check with no `go vet` overlap -
it's what catches unused functions left behind by a refactor and errors
formatted with `%v` instead of `%w`, neither of which fails a build or a
test. The pinned command in DEVELOPMENT.md needs no install and matches the
version CI uses; a distro package may drift from it in either direction.
