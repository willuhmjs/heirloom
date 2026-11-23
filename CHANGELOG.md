# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). The API is `v1alpha1` and may change between releases.

## [Unreleased]

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

## [0.1.0]

First release.

### Added
- `BackupStore`, `BackupPolicy` and `Restore` custom resources (`backshots.io/v1alpha1`).
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

[Unreleased]: https://github.com/willuhmjs/backshots/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/willuhmjs/backshots/releases/tag/v0.1.0
