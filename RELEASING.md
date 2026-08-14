# Releasing go-agc

`go-agc` is a library-only Go module. It does not need release executables,
archives, or a package-registry upload. A semantic-version Git tag is the Go
module release; the tag-triggered GitHub Actions workflow also creates a
source-only GitHub release with generated notes.

## Initial release

Use a `v0.x.y` version while the public API is still allowed to evolve. For
example, to release `v0.1.0`:

```bash
git switch main
git pull --ff-only
go test ./...
go test -race ./...
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

The pushed tag starts `.github/workflows/release.yml`. The workflow repeats
formatting, vet, normal, race, and cgo-disabled checks before creating the
GitHub release. It deliberately builds no executable and uploads no binary
assets.

Consumers can then select the version normally:

```bash
go get github.com/martinghunt/go-agc@v0.1.0
```

Go's module proxy will discover the version from the repository tag when it is
first requested. Creating the GitHub release is useful for release notes but is
not required by the Go module system.

## Later releases

For each release:

1. Ensure `main` is clean and its test workflow passes.
2. Choose the next semantic version and review the public API for compatibility.
3. Create and push an annotated tag as shown above.
4. Confirm the release workflow succeeds and the GitHub release appears.
5. Optionally verify resolution with `go list -m github.com/martinghunt/go-agc@vX.Y.Z`.

Starting a stable `v2` or later requires changing the module path and imports
to include the major-version suffix, such as `github.com/martinghunt/go-agc/v2`.

