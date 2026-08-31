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

## Coverage

The coverage badge in [README.md](README.md) is a static shields.io badge -
there is deliberately no CI job computing it. That means the number is only
as current as the last person who refreshed it, so refresh it in the same PR
as any change that moves coverage: new tests, a new package, or a chunk of
untested code.

```
scripts/coverage.sh
```

That runs `go test ./... -covermode=atomic -coverprofile=coverage.out`, reads
the total out of `go tool cover -func`, picks the shields.io colour band for
it, and rewrites the badge URL in `README.md` in place. Commit the README
change along with the rest of your work. `coverage.out` is gitignored - it
stays behind so you can dig into what's actually uncovered:

```
go tool cover -html=coverage.out
```

To find out whether the badge is stale without touching the file - worth a
run before you open a PR:

```
scripts/coverage.sh --check
```

It exits non-zero and prints both the current and the correct badge URL if
they differ.

The number is whole-module statement coverage from each package's own tests,
which is why it sits well below the per-package numbers you see scroll past:
`cmd/coldarr` has no tests at all and `internal/webui` is mostly templates
(see the testing notes in [DEVELOPMENT.md](DEVELOPMENT.md#testing) for why
those two are exercised by hand instead). Don't switch the script to
`-coverpkg=./...` to make the badge look better - that counts code merely
executed by an unrelated package's test as covered, and reports ~4 points
higher for no extra testing.
