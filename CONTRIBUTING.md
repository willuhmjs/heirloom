# Contributing

Thanks for looking. heirloom is small on purpose; the fastest way to a merged change is a focused one.

## Ground rules

- **Plain files are the product.** A change that makes snapshots depend on a tool to read (a repository format,
  encryption, an index you need) is out of scope. Anything that keeps restore-by-copying working is welcome.
- **Safety beats features.** The code deletes snapshots and scales workloads. Prefer refusing and saying why over
  guessing. Every guard has a test that fails without it.
- **Say what you did not test.** If your change touches NFS behaviour, a particular NAS or a database version,
  tell us which one you ran it against.

## Setup

```sh
go version        # see go.mod for the minimum
make build test lint
```

`make manifests` regenerates the CRDs from `api/v1`. CI fails if the committed CRDs differ, so commit them.
The Helm chart ships its own copy of the CRDs, and `make manifests` keeps the two in sync.

## Tests

- `go test ./...` runs the unit tests. Retention, the runner and the controllers each have them; the runner tests
  call real `rsync` and `sqlite3` and skip when those are missing. Some tests need root (file ownership) and skip
  otherwise; run them in a Linux container if you can.
- `scripts/e2e.sh` builds a kind cluster and a real NFS server and exercises the whole thing. It needs Docker,
  kind, kubectl and Helm, and uses its own private kubeconfig. `KEEP=1 scripts/e2e.sh` leaves the cluster running.

## Pull requests

- One change per PR, with the reason in the description. A bug fix includes the test that reproduces it.
- Update [`docs/REFERENCE.md`](docs/REFERENCE.md) and the README when behaviour changes, and
  [`CHANGELOG.md`](CHANGELOG.md) for anything a user would notice.
- API changes: keep them additive while the API is `v1`, and note them in
  [`docs/UPGRADING.md`](docs/UPGRADING.md).

## Releases

Maintainers: see [`docs/RELEASING.md`](docs/RELEASING.md).

## Reporting security issues

Privately, see [SECURITY.md](SECURITY.md).
