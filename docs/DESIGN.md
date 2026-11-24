# Design

## Goals

1. Backups are deduplicated restic repositories on storage you own: one NFS export, the plain local
   restic backend. No backup service, no cloud, no object storage, no keys to escrow.
2. Recovery must not depend on this project, the cluster, or a key you might lose. A policy's backups
   are one restic repository, and restic is one free binary — the mover image already ships it.
3. Small blast radius by construction: a mover only ever mounts its own `<namespace>/<policy>` folder
   of the export, and the operator never touches the store.
4. Restores are tested automatically, not assumed.
5. Passwordless by design: the export's ACL is the security boundary, exactly as it was before.

## Decisions

**The controller schedules runs itself and creates Jobs; there are no CronJobs.** This is how VolSync and
k8up work, and it keeps all scheduling state in one visible place: `status.lastScheduleTime`,
`nextSyncTime`, the policy's `Ready` and `Healthy` conditions. It also lets the controller know about the
rest of the system: it starts nothing while a `Restore` for the policy is running, and a manual trigger
waits for a running backup.

The cost, stated plainly: **backups stop while the manager is down.** A rendered CronJob would have kept
firing. Mitigations: a missed slot runs once when the manager returns, the staleness condition
(`Healthy=False/Stale`) reports a policy that has gone too long without a success, and the manager is a
single small Deployment with leader election. The earlier CronJob-based design is in the git history.

**Movers mount NFS directly, the manager does not.** A mover mounts its own `<namespace>/<policy>`
directory of the export as a `subPath` mount, plus the PVC it backs up and `/scratch` — tenant isolation
by construction: a mover running a user-chosen postgres image can neither see nor modify another
namespace's backups. The manager can stay non-root with a read-only filesystem. Results travel back
through the pod termination message (`/dev/termination-log`), a small JSON document
(`internal/jobcfg.Result`).

**Every mover mounts `/scratch`.** restic keeps its chunk cache there, sqlite and postgres restores
stage their payload through it, and verify runs a throwaway postgres server on it.
`pod.scratchStorageClassName` sizes it from a storage class; the default emptyDir consumes node disk.

**The runner binary is injected by an init container.** An init container copies `/heirloom` to an
emptyDir, so postgres pods run in the user's own `postgres:N` image and `pg_dump` always matches the
server major version. The same binary is manager, installer and runner.

**Backups are restic repositories, exclusively.** One repository per policy at
`<store>/<namespace>/<policy>/restic/` on the export, plain local backend: no rest-server, no object
storage, nothing listening on a port. Files are added with `restic backup .` from the volume root;
sqlite copies and postgres dumps are staged under a transient `.staging` folder and committed. restic
commits a snapshot only at the end of a run, so an interrupted run publishes nothing, and cleanup is
sweeping `.staging` and letting the next `forget --prune` drop unreferenced packs.

**Dedup instead of browsable folders.** This is the headline trade. A changed 10 GiB file costs its
changed chunks, not a fresh copy; sqlite copies and pg_dump dumps dedup across runs, so retention stays
affordable on a home NAS, and status shows "N bytes added to repository" per run so you can watch it
work. The cost, stated plainly: **the store is not browsable** — no Finder, no `cp -a`, no `ls` of a
snapshot. Individual files come back with the restic binary (`ls`, `dump`, `restore` against the
policy's repository); a one-off pod with the operator image and the store mounted is the manual path.

**Unencrypted repositories (`--insecure-no-password`).** Deliberate. Backups were plaintext on the NAS
before and are plaintext now; the export's ACL was always the security boundary and still is. The pitch
was never "encrypted at rest", it was "your NAS, your files" — now "your NAS, your repositories". A
password that guards only the format, with the payload readable beside it, is one more thing to lose on
the worst day of the year.

**Snapshot identity is restic's hex id.** `status.lastSnapshot.name` shows the 8-character short id and
`Restore.spec.from` accepts `latest` or a hex id. `restore.delete` maps to `restic restore --delete`
with the backup's excludes passed along so excluded files survive; `delete` with `paths` is rejected,
because restic refuses to mix include and exclude.

**GFS counts instead of a max-age.** `keepHourly/Daily/Weekly/Monthly` map straight to
`restic forget --keep-* --prune`; the buckets are unchanged from the hardlink era. Predictable storage
and easy to explain. The newest snapshot is never pruned.

**Empty-source guard.** A recreated PVC is the most common way to destroy a backup chain by being
faithful to it. `minSourcePercent` compares file counts to the previous snapshot; both counts come from
restic run summaries.

**Verification is a first-class feature.** Every verify run does `restic check` plus a rotating
`restic check --read-data-subset=N/10` (N advances by ISO week, so the whole repository is read within
ten weeks), plus per-source content checks: sample-file checksums against `restic dump`,
`PRAGMA integrity_check` on every sqlite copy, and postgres dumps loaded into a throwaway server on
scratch where user-supplied `verify.sql` assertions run.

**Cluster-scoped BackupStore.** The NAS is infrastructure, shared by namespaces; policies pick it by
name. Isolation lives in the mover's mount, not in the store object.

## Rejected alternatives

**rest-server or an object-storage backend.** restic speaks both, and both are battle-tested, but one
is a service listening on a port and the other is S3 credentials and a vendor — the backup service this
project exists to avoid. The local backend gives the identical repository pointed at an export you
already run.

**Encrypted repositories.** Encryption is restic's headline feature and the old design rejected the tool
over it. The tool is no longer an objection — one free binary, shipped in the mover image — but a
password is precisely the thing you cannot recover backups without, and the export ACL is the boundary
here, so the repositories are passwordless. If you need encryption at rest, encrypt the volume on the
NAS; if you need encrypted off-site repositories, use VolSync or k8up against object storage.

**Plain hardlinked snapshot folders (the previous design).** Every snapshot a full browsable tree,
unchanged files hardlinked — cheap and restorable with `cp`. What it cost: a database dump re-copied in
full every run, shared inodes so damage to one file hit every snapshot, and slow pruning of
metadata-heavy trees. restic trades Finder browseability for true chunk-level dedup; dumps deduping
across runs won.

**Btrfs/reflink snapshots on the NAS.** Instant, space-efficient snapshots would be ideal, but it needs
a specific NAS filesystem and an API or SSH to trigger snapshots, and ties the design to one vendor.
Reflinks also do not cross NFS reliably. restic's local backend needs nothing but a writable export.

**rsync daemon on the NAS.** Avoids NFS client requirements on nodes and gives file-level transfers, but
means a second service, credentials in cleartext rsync, and a different security boundary. NFS export
ACLs are simple and already how the NAS is shared.

**CSI snapshots.** Not universally available (k3s local-path has none) and tie backups to the storage
driver. A restic run with a database-specific dump path for databases covers the target use.

**Operator-side store mount.** Would simplify listing snapshots but forces the manager to run as root
with an NFS mount and makes it a single point of failure.
