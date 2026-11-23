# Design

## Goals

1. Backups are plain files on a NAS you control, readable with any NFS/SMB client.
2. Recovery must not depend on this project, the cluster, or a key.
3. Small blast radius: the operator never touches the store; scheduled work survives operator outages.
4. Restores are tested automatically, not assumed.

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

**Movers mount NFS directly, the manager does not.** The manager can stay non-root with a read-only
filesystem. Results travel back through the pod termination message (`/dev/termination-log`), a small
JSON document (`internal/jobcfg.Result`).

**The runner binary is injected by an init container.** An init container copies `/backshots` to an
emptyDir, so postgres pods run in the user's own `postgres:N` image and `pg_dump` always matches the
server major version. The same binary is manager, installer and runner.

**Snapshots are directories published by atomic rename.** Work happens in `.incomplete-<name>` and is
renamed to `<name>` at the end. Rename within one NFS directory is atomic, so anything visible is
complete, and crash cleanup is deleting `.incomplete-*`.

**Hardlinks for dedup of files** (`rsync --link-dest` against the previous snapshot). Works on any
filesystem and any NAS, and every snapshot is a full browsable tree. Cost: shared inodes mean damage
to one file hits all snapshots that share it, and metadata-heavy trees are slow to prune.

**GFS counts instead of a max-age.** Predictable storage and easy to explain. The newest snapshot is
never pruned.

**Empty-source guard.** A recreated PVC is the most common way to destroy a backup chain by being
faithful to it. `minSourcePercent` compares file counts to the previous snapshot.

**Verification is a first-class feature.** The verify job restores into scratch space and asserts
user-supplied SQL for postgres.

**Cluster-scoped BackupStore.** The NAS is infrastructure, shared by namespaces; policies pick it by name.

## Rejected alternatives

**Btrfs/reflink snapshots on the NAS.** Instant, space-efficient snapshots would be ideal, but it needs
a specific NAS filesystem and an API or SSH to trigger snapshots, and ties the design to one vendor.
Reflinks also do not cross NFS reliably. Hardlinks need nothing but a writable export.

**rsync daemon on the NAS.** Avoids NFS client requirements on nodes and gives file-level transfers, but
means a second service, credentials in cleartext rsync, and a different security boundary. NFS export
ACLs are simple and already how the NAS is shared.

**Encrypted repository (restic/borg/kopia).** Excellent dedup and encryption, but recovery then needs
the tool and the key. Those are the properties backshots deliberately gives up. Use VolSync or k8up if
you want them.

**CSI snapshots.** Not universally available (k3s local-path has none) and tie backups to the storage
driver. Live rsync with a database-specific path for databases covers the target use.

**Operator-side store mount.** Would simplify listing snapshots but forces the manager to run as root
with an NFS mount and makes it a single point of failure.

**Dedup for database dumps.** Dumps change everywhere on each run, so file-level hardlinks do not help.
Chunk-level dedup would need a repository format, contradicting goal 1. Accepted cost: a full copy
per kept snapshot, controlled by retention.
