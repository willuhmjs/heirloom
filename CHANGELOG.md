# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). The API is `v1alpha1` and may change between releases.

## [Unreleased]

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
