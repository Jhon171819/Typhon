# CI and release integrity

## Pull request gates

- Go tests and Python bridge tests cover Linux and Windows with Python
  3.11, 3.12 and 3.14. The Linux 3.12 job also runs vet, the full race
  detector, OpenAPI validation and the short native JSON fuzz target.
- A native Linux job runs PostgreSQL 16 and Redis 7 integration and race
  tests, including SIGTERM and the disposable `docker stop` fixture.
  The Compose job builds the image, exercises CRUD and volume persistence,
  and uploads its SPDX inventory.
- The supply-chain job gates on formatted Go, verified go.sum checksums,
  no changes from `go mod tidy -diff`, no reachable Go vulnerabilities
  reported by govulncheck, high-confidence and high-severity Gosec reports,
  and a fully redacted Gitleaks scan of Git history.
- The artifact job builds Linux amd64, Linux arm64 and Windows amd64,
  including SHA-256 checksums, Go build metadata and an SPDX SBOM for
  each binary. It runs the Linux amd64 binary on the runner.

The module still targets Go 1.26. The `toolchain` directive and Dockerfile
pin the patched Go version used to build artifacts. The Dockerfile pins
the builder and runtime image indexes by digest. Dependency caching
keys off go.sum. Review changes to the toolchain, module lock and
commit-pinned GitHub Actions together when updating them.

Pull request jobs run under `pull_request` with read-only repository
access, no checkout credential persistence and disposable database
fixtures. They use no release credentials or OIDC attestation permission.

## Run gates locally

```sh
gofmt -l ./cmd ./internal ./tests/load
go mod verify
go mod tidy -diff
go test ./...
go vet ./...
go test -race ./...
go test ./internal/typhon -run '^$' -fuzz '^FuzzNativeJSON$' -fuzztime 5s -parallel 2
go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...
go run github.com/securego/gosec/v2/cmd/gosec@v2.30.0 -severity high -confidence high ./...
go run github.com/zricethezav/gitleaks/v8@v8.30.1 git --no-banner --redact=100 .
```

Set `TYPHON_TEST_POSTGRES_URL` and `TYPHON_TEST_REDIS_URL` to disposable
instances for native integrations. Tests create temporary schemas, roles
and databases. The PostgreSQL fixture needs privileges to do so. Use
`go test -race ./internal/typhon -run TestNative -v` on a runner with
a compatible C toolchain. See the [Compose guide](docker_compose.md) for
container integration and [performance guide](performance.md) for soak.

## Released assets

When a maintainer publishes an editable GitHub release, the artifacts
workflow builds from that tag and attaches the binaries, Docker image
archive, SBOMs and SHA-256 files. Release-only jobs verify the checksums,
request GitHub build-provenance and SBOM attestations and upload the files
to the existing release. The PR version uploads workflow artifacts only.

```sh
sha256sum --check typhon-linux-amd64.sha256
gh attestation verify typhon-linux-amd64 --repo Jhon171819/Typhon
gh attestation verify typhon-linux-amd64 --repo Jhon171819/Typhon \
  --predicate-type https://spdx.dev/Document/v2.3
```

Apply the same verification to the image archive before `docker load`.
[GitHub artifact attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations)
