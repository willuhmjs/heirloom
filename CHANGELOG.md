# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). The API is `v1`: changes follow semver, and breaking
API changes only come with a new major version.

## [1.2.2] - 2026-11-26

### Added
- Richer `BackupPolicy` status for dashboards and `kubectl get`: `store`, `encrypted`, `repoSize` (repository size
  after the run), `lastAdded` (bytes added after dedup) and `lastMessage` (one-line result of the newest run,
  failed or not), with matching printer columns (Size, Added, Took, Store, Encrypted, and Result in wide output).

## [1.2.1] - 2026-11-26

### Fixed
- An SMB store's volume handle now includes a hash of its mount options. csi-driver-smb keeps one staged mount per
  handle, so recreating a PV with different options (for example adding `nobrl`) kept mounting with the old ones.
- `kubectl get backupstores` (and dashboards that read printer columns) show the SMB server and share, and the
  encryption secret, next to the NFS columns; before, an SMB store listed an empty server and path.
- Docs: SQLite sources stage their copy on the store and need the `nobrl` mount option on SMB (CIFS byte-range
  locks otherwise fail with "database is locked").

## [1.2.0] - 2026-11-26

### Added
- **Repository encryption.** `BackupStore.spec.encryption.passwordSecretRef` (chart: `store.encryption.*`) encrypts
  the restic repositories. The operator reads one master password from its own namespace and derives a separate
  password per policy (`HMAC-SHA256(master, "heirloom/v1/<ns>/<policy>")`), stored in a create-only Secret in the
  policy's namespace, so a tenant only holds its own repository's key. A run never falls back to a passwordless
  repository. Existing passwordless repositories do not convert. New RBAC: `create` on Secrets cluster-wide and
  `get` on Secrets in the operator's namespace only.

## [1.1.1] - 2026-11-26

### Fixed
- The release pipeline publishes the chart again (Helm 3 for the OCI push; Helm 4.3 fails against ghcr) and builds
  each image architecture on a native runner with a layer cache instead of one emulated build. No code changes
  since 1.1.0; 1.1.0's image exists but its OCI chart was never published, so use 1.1.1.

## [1.1.0] - 2026-11-26

### Added
- **SMB stores.** A `BackupStore` can set `spec.smb` (`server`, `share`, `credentialsSecretRef`, optional
  `mountOptions`) instead of `spec.nfs`; exactly one of the two is required. The share is mounted through
  [csi-driver-smb](https://github.com/kubernetes-csi/csi-driver-smb) (`smb.csi.k8s.io`) with a static PV/PVC per
  namespace, reclaim policy `Retain`. The chart gets `store.smb.*` values. NFS stores behave exactly as before.

## [1.0.0] - 2026-11-24

### Changed
- **Renamed: backshots is now heirloom.** The API group is `heirloom.io`, the Go module is
  `github.com/willuhmjs/heirloom`, the image is `ghcr.io/willuhmjs/heirloom`, the chart is published to
  `oci://ghcr.io/willuhmjs/charts/heirloom` and the Helm repository moved to
  `https://willuhmjs.github.io/heirloom`. Nothing about how backups work changed, and the repositories on your
  store are untouched — point policies at the same paths and the same restic repos keep deduplicating.
  Migrating the Kubernetes objects is a manual swap, documented in
  [`docs/UPGRADING.md`](docs/UPGRADING.md#upgrading-from-backshots-0x).
- **The API graduates from `v1alpha1` to `v1`** with no schema changes: the same fields, served and stored as
  `heirloom.io/v1`. Policies and stores only need their `apiVersion` line changed.

_Releases before 1.0.0 were published under the project's former name; their tags and GitHub releases were
not carried over. The code history is complete._

## 0.3.0 - 2026-11-23

### Added
- **`command` source type**: a dump command is `exec`'d in a running app pod selected by label
  (`podSelector`, optional `container`), and its stdout is gzipped into `command/<file>` inside the
  snapshot — the same staging contract SQLite and Postgres follow, so retention and verify paths are
  shared. Optional `preCommand`/`postCommand` quiesce hooks: the pre-command must exit 0 before the dump
  starts, the post-command runs no matter how the dump ended, so a lock is never left held. The mover runs
  as a scoped, controller-maintained `heirloom-executor` ServiceAccount whose Role grants nothing but
  pod lookup and `pods/exec`. A `Restore` of a command source requires `targetPVC`: the dumps are restored
  onto that volume and the app's own import tool loads them.
- **Scheduling jitter** (`spec.jitter`): every cron slot of a policy shifts by a fixed, per-policy hash
  offset in `[0, jitter)`, so a fleet of policies scheduled `0 * * * *` does not stampede the NAS at the
  same second. The offset is stable across reconciles and leader changes; Job names stay idempotent.
- **NFS `mountOptions` on `BackupStore`**: a pod's inline NFS volume cannot carry mount options, so a store
  that sets them gets a static PV + PVC pair provisioned per namespace (deterministic name, shared by the
  namespace's policies, reclaim `Delete` — only the PV pointer is deleted, never the export) and movers
  mount that claim. Changing the options of a bound PV surfaces as an actionable `Ready=False` message
  instead of being silently ignored.
- **HPA quiesce during restores**: a `Restore` pauses every `HorizontalPodAutoscaler` targeting its
  `scaleDown` workloads — bounds are recorded in `status.scaledHPAs` before the autoscaler is touched, set
  to 0, and restored on completion, including through the finalizer path when a Restore is deleted
  mid-flight. Clusters before 1.30 whose API rejects a 0 bound get a warning event and keep autoscaling,
  as before.
- Chart: optional `replicas` (leader election is now on by default, and >1 enables a rolling update), an
  optional `PodDisruptionBudget`, and **CRD hooks**: a pre-install/pre-upgrade hook Job running the
  operator's own image applies the CRDs embedded in it (`heirloom install-crds`), closing Helm's
  "crds/ are installed once, never upgraded" gap. `crdHooks.enabled=false` opts out.

### Changed
- **Data safety:** a snapshot that restic committed but the shrink guard rejected is now `restic forget`-ed.
  Before, it stayed the repository's newest, so `restore from: latest` after a guarded run would have
  handed back the shrunken tree — and the next run would have compared against it and stayed silent.
- **Data safety:** the restic repository is now a true mutex in both directions: a `Restore` waits for a
  running backup or verify to finish before stopping workloads (the policy already paused while a Restore
  ran, but a run already in flight used to race the restore, and `forget --prune` rewrites packs while the
  restore reads them). Restore and verify also `restic unlock` the repository first, so a stale lock from
  a killed run cannot hang a mover forever.
- Mover pods get default **resource requests** (`cpu: 100m`, `memory: 512Mi`): a best-effort mover was the
  first pod evicted under memory pressure, mid-backup. Limits stay unset unless you set them.
- Leader election now defaults to **on** (`--leader-elect=true`); the chart still starts one replica.

### Development
- CI: unit tests run with `-race` and publish coverage, a `golangci-lint` job (v2 config) guards the tree,
  and a new e2e workflow runs the full install against a kind cluster with an NFS server on every pull
  request.

## 0.2.0 - 2026-11-23

### Changed
- **Breaking:** backups are now restic repositories at `<namespace>/<policy>/restic` on the store, replacing the
  plain hardlinked snapshot folders. Deduplication makes large changing datasets cheap — a changed 10 GiB file
  costs only its changed chunks, and SQLite and Postgres dump sets deduplicate across runs. The repositories are
  plain local and unencrypted (`--insecure-no-password`): no rest-server, no object storage.
  - The store folder is no longer browsable. Recover with `restic --repo <ns>/<policy>/restic
    --insecure-no-password restore latest --target <dir>`, or browse with `ls` and `dump`. Snapshot folders left by
    earlier builds stay untouched on the store but are no longer read or written.
  - Retention now maps to `restic forget --keep-* --prune`.
  - Snapshots are restic hex ids. Status shows the 8-char short id, and `Restore`'s `spec.from` is `latest` or a
    hex id; timestamp snapshot names are gone.
  - Verify runs `restic check` plus a rotating `--read-data-subset=N/10` pack read — every byte of the repository
    within 10 weeks — alongside the per-source content checks: files restore a sample and compare its checksum
    against `restic dump`, SQLite runs `integrity_check` on every database restored to scratch, Postgres loads the
    dumps into a throwaway scratch cluster and runs the `spec.verify.sql` assertions.
  - Every mover pod now mounts `/scratch` (restic cache, staging for SQLite and Postgres restores, scratch Postgres
    data), so `pod.scratchStorageClassName` matters in all modes and large SQLite or Postgres databases need a real
    scratch size.
  - `restore.delete` uses `restic restore --delete` with the backup's excludes passed, so excluded files are never
    deleted; `delete` together with `paths` is rejected, a restic limitation.
  - Manual triggers, schedules, suspend and the restore drain/quiesce behaviour are unchanged.

## 0.1.0

First release.

### Added
- `BackupStore`, `BackupPolicy` and `Restore` custom resources (`heirloom.io/v1`).
- Sources: files (rsync, preserving ownership, ACLs, xattrs, symlinks), SQLite (online backup plus integrity
  check) and PostgreSQL (`pg_dump` per database plus roles, run in the user's own Postgres image).
- Plain-file, hardlinked snapshots on NFS, published atomically, with hourly/daily/weekly/monthly retention.
- A controller-side scheduler: one Job per due run, a missed slot runs once, no overlapping runs, paused while a
  restore of the policy runs. Optional `timeZone`.
- Verify jobs that restore the latest snapshot into a scratch database and run SQL assertions.
- Restores as a resource: validation before anything is stopped, queueing, scale-down and scale-up with Flux
  paused, a stuck scale-down times out, and deleting a restore mid-flight puts workloads back.
- Guards: refuse an empty or shrunken source, resume an interrupted first backup, tenant isolation on the share,
  symlink containment, redaction of database data from error messages.
- Health: `Healthy` condition including `Stale` for an overdue backup, Kubernetes Events, Prometheus metrics
  served over TLS to authenticated callers.
- Helm chart (repository on GitHub Pages and OCI), CRD admission validation, a scripted end-to-end test.

[Unreleased]: https://github.com/willuhmjs/heirloom/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/willuhmjs/heirloom/releases/tag/v1.0.0
