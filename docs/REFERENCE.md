# heirloom reference

The detailed documentation: architecture, every behaviour and its limits, failure modes, the security model.
For the overview and a five-minute start, see the [README](../README.md). Restore procedures are in
[RESTORE-RUNBOOKS.md](RESTORE-RUNBOOKS.md), design decisions in [DESIGN.md](DESIGN.md).

## How it works

```
 BackupPolicy (ns/app)          BackupStore (cluster)
        |                              |  nfs: server + path
        v                              v
  +-----------+  creates a Job per due run, itself         +------------------+
  | heirloom | -----------------------------------------> | mover Job pod    |
  | manager   |   (own scheduler: cron slots, no CronJob)  |  /source  PVC    |
  +-----------+                                            |  /store   NFS    |
        ^                                                  |  /scratch          |
        | status via pod termination message                +--------+---------+
        +-------------------------------------------------------------+
                                            restic / sqlite3 / pg_dump
                                                                        |
                                                                        v
                                                          NAS: <ns>/<policy>/restic/
```

The manager is the scheduler. Each reconcile it checks whether a cron slot of a policy has passed since the
last run and, if so, creates one Job (`bs-<policy>-run-<slot>`; verify runs are `bs-<policy>-verify-<slot>`).
Slots carry a policy's fixed `spec.jitter` offset (see CRD reference) so a fleet of policies on the same
cron expression does not stampede the NAS at the same second.
It never starts a second run while one is active, starts nothing while a `Restore` for the policy is running
— and a `Restore` waits for an already-running backup or verify to finish, so nothing shares the repository
while a restore rewrites a volume —
prunes old finished Jobs (keeps 3 succeeded and 3 failed per role), and reads results back from pod
termination messages. It never mounts the store. **If the manager is down, scheduled runs pause**; when it
returns it runs a missed slot once, not once per missed slot, and a policy turns `Healthy=False/Stale` if
too long passes without a success.

Mover pods mount the PVC and only their own `<namespace>/<policy>` directory of the NFS export (a `subPath`
mount, so one mover never sees another's backups) plus a scratch volume, and run the heirloom binary
(`heirloom run`), which is copied in by an init container so database images (for example `postgres:16`)
can be used unchanged. The runner image ships `restic`, which the Dockerfile probes at build time.

Backups are restic repositories, exclusively: each policy's backups are one restic repository at
`<store>/<namespace>/<policy>/restic/` on the export, plain local backend — no rest-server, no object
storage, no extra service, nothing listening on a port. Repositories are unencrypted and passwordless
(`--insecure-no-password`) by design; the NFS export's ACL is the security boundary (see Security).

Source types:

| type | method |
|---|---|
| `files` | `restic backup .` from the volume root (or the `subPath` root), excludes via restic `--exclude` |
| `sqlite` | consistent copy per database with the SQLite online backup API, staged under `.staging` then committed into the repository |
| `postgres` | `pg_dump` `.sql.gz` per database plus `globals.sql.gz`, staged under `.staging` then committed into the repository |
| `command` | a dump command `exec`'d in a running app pod (matched by `podSelector`); its stdout is gzipped to `command/<file>` in the snapshot. Any database with a CLI dump tool (mysql, mongodump, redis) fits without heirloom growing a driver for it |

## On-disk layout

```
/volume1/k8s-backup/
  gitea/                              namespace
    gitea-data/                       policy
      restic/                         the policy's restic repository (plain local backend)
        config                        repository config
        keys/                         the empty password, stored by design
        data/                         content-addressed packs; nearly all of the space
        index/                        snapshot indexes
        snapshots/                    snapshot metadata
      .staging/                       transient: staged sqlite copies and pg_dump dumps,
                                      swept at the start of the next run
```

The repository layout is restic's internal format: treat `data/`, `index/`, `keys/`, `snapshots/` and
`config` as read-only to humans, and expect the policy folder's size to be dominated by `data/`.

Benefit, stated plainly: content-addressed dedup with pack storage. A changed file costs only its changed
chunks, and sqlite copies and pg_dump dumps dedup across runs, so hourly retention of a large changing
volume stays affordable. Cost, equally plainly: the store folder is not browsable. Recovery of individual
files uses the restic binary (`snapshots`, `ls`, `dump`, `restore`, always with `--insecure-no-password
-r <ns>/<policy>/restic`).

### Upgrading from the old plain-snapshot layout

Old plain snapshot folders under a policy (timestamp-named copies with `_status.json` and
`.incomplete-`/`.deleting-` directories) are left in place and untouched; nothing reads them anymore. The
first backup of the policy creates the restic repository beside them. Delete the old folders yourself when
you no longer need them.

## Quickstart

1. Synology: Control Panel > Shared Folder > create `k8s-backup`. Under NFS Permissions allow your
   node IPs only, with Squash set to **No mapping** (root must be able to write and chown), and
   allow asynchronous off. Enable NFS and set the maximum protocol to NFSv4.1.
2. Install the operator from the Helm repository:

   ```sh
   helm repo add heirloom https://willuhmjs.github.io/heirloom
   helm repo update
   helm install heirloom heirloom/heirloom \
     --namespace heirloom-system --create-namespace \
     --set store.create=true \
     --set store.nfs.server=192.168.1.5 \
     --set store.nfs.path=/volume1/k8s-backup
   ```

   The chart is also published as an OCI artifact: `helm install heirloom oci://ghcr.io/willuhmjs/charts/heirloom`.
   Or apply `examples/store.yaml` yourself.
3. Apply a policy in the namespace of the PVC:

   ```sh
   kubectl apply -f examples/policy-files.yaml
   kubectl get bp -A
   ```

   Run one now with `spec.trigger.manual` (see `examples/trigger-manual.yaml`).

Nodes need an NFS client (`nfs-common` / `nfs-utils`).

### SMB instead of NFS

A `BackupStore` can point at an SMB/CIFS share instead (set exactly one of `spec.nfs` or `spec.smb`). The share is
mounted by [csi-driver-smb](https://github.com/kubernetes-csi/csi-driver-smb), which you install first
(`smb.csi.k8s.io`). Create a Secret with the keys
`username` and `password` (optionally `domain`) and reference it from the store; see `examples/store-smb.yaml`:

```sh
helm install heirloom heirloom/heirloom -n heirloom-system --create-namespace \
  --set store.create=true \
  --set store.smb.server=192.168.1.5 \
  --set store.smb.share=k8s-backup \
  --set store.smb.credentialsSecret=smb-creds
```

An SMB store always goes through the provisioned static PV/PVC pair (the driver takes its credentials from the PV),
with `persistentVolumeReclaimPolicy: Retain` because csi-driver-smb does not delete statically provisioned volumes;
heirloom deletes a released PV itself, and that never touches the share. Restic's local repository needs neither
ownership nor symlinks on the share, so SMB works as a backend; a share that holds nothing else is still best.
Recommended `mountOptions`: `vers=3.0`, `uid=0`, `gid=0`, `dir_mode=0700`, `file_mode=0600`. Unlike NFS, SMB
authenticates a user, so the credentials in the Secret are the security boundary: restrict who can read it.

## Retention

`spec.retention` (`keepHourly`/`keepDaily`/`keepWeekly`/`keepMonthly`) maps directly to
`restic forget --keep-hourly ... --prune` at the end of each successful backup. Zero or unset counts are
not passed. Defaults hourly 24, daily 30, weekly 12, monthly 24; a policy overrides the store default, and
the newest snapshot is always kept. Retention applies per policy repository only: a run never opens another
policy's folder.

Tradeoffs, stated plainly:

- Dedup is the model: content-addressed packs mean unchanged files cost no extra space, a changed file
  costs only its changed chunks, and sqlite copies and pg_dump dumps dedup across runs too. Retention
  counts can therefore be generous even for large, churning sources.
- The store folder is not browsable. Individual files come back with the restic binary, not with `ls` and
  `cp` on the NAS (see Restore).
- Restic checksums every pack. With `verify.schedule` set, each run reads a rotating tenth of the pack data
  (`restic check --read-data-subset=N/10`, N rotating by ISO week), so every byte in the repository has
  been read and verified within ten weeks. Without verify, only structural `restic check` runs happen.
  Bit rot detected is not a second copy: keep a copy of the NAS offsite anyway (Hyper Backup, rclone, a
  second NAS).
- A run publishes its snapshot only at the end, so a failed or killed run leaves no snapshot. Its staged
  `.staging` tree is swept at the start of the next run, and packs it had already uploaded are
  unreferenced and removed by the next `forget --prune`.

## Verify

A policy may set `verify.schedule`. Every verify run checks the policy's own repository and proves the
latest snapshot restores:

- always: `restic check`, plus `restic check --read-data-subset=N/10` where N rotates by ISO week, so the
  full pack data of the repository is read within ten weeks.
- `files` and `command`: additionally restores the smallest sample files (up to 5, <= 4 MiB each) into
  scratch and checksum-compares them against `restic dump` (for a command source that reads back the
  `command/<file>` dump itself).
- `sqlite`: additionally restores the snapshot to scratch and runs `PRAGMA integrity_check` on every
  database.
- `postgres`: additionally loads the restored dumps into a throwaway postgres instance on scratch (fsync
  off) and runs each `verify.sql` assertion (each must return `t`) against one database: `verify.database`,
  or the single database the policy lists. A policy that dumps several databases must name one, otherwise
  it is rejected (`Ready=False`, `InvalidSpec`) rather than guessing.

Scratch is an emptyDir, or a generic ephemeral volume of `pod.scratchSize` (default 10Gi) when
`pod.scratchStorageClassName` is set. The result lands in `status.lastVerify` and in metrics. A backup you
have never restored is a guess.

## Restore

By hand: use the restic binary. Mount the export on any machine that can reach the NAS, or `kubectl exec`
into a mover pod (the runner image ships `restic`, which the Dockerfile probes at build time), and address
the repository directly — always with `--insecure-no-password`:

```sh
restic -r gitea/gitea-data/restic --insecure-no-password snapshots
restic -r gitea/gitea-data/restic --insecure-no-password ls latest
restic -r gitea/gitea-data/restic --insecure-no-password dump latest data/app.db > app.db
restic -r gitea/gitea-data/restic --insecure-no-password restore latest --target /recovered
```

Snapshot identity is restic's hex id. Policy status shows the 8-character short id; `from` accepts either
the short id or the full id. Timestamp snapshot names no longer exist.

What each type does:

- `files`: `restic restore` onto the volume root or the `subPath` root. `paths` restricts the restore via
  anchored restic `--include` patterns. `delete: true` maps to `restic restore --delete` and passes the
  backup's exclude patterns as `--exclude`, so excluded paths are never deleted. `delete` with `paths` is
  rejected by validation: restic refuses include+exclude combinations.
- `sqlite`: databases are restored from the snapshot into scratch, then swapped onto the volume; stale
  `-wal`/`-shm`/`-journal` files are removed.
- `postgres`: `globals.sql.gz` plus the selected (or all) database dumps are loaded via `psql` — plain SQL
  made with `--create --clean --if-exists`; no `pg_restore`.
- `command`: there is no universal loader for arbitrary dumps, so `targetPVC` (the volume the dumps are
  written to) is required. The snapshot's `command/` entries are restored onto it via restic `--include`
  (`paths` restricts which); the app's own import tool (`mysql`, `mongorestore`, `redis-cli --pipe`) takes
  them from there. It does not `exec` into the app pod — a restore only writes files.

With the CR: create a `Restore` next to the policy (see `examples/restore-*.yaml`) and watch
`kubectl get restores -n <ns>`. It can scale Deployments and StatefulSets to zero first and back
afterwards, restore `from: "latest"` (the default) or a hex id (8-64 characters), and target a different
PVC.

What it does before it touches anything: it validates `from`, `paths`, `databases`, the name length and that
every PVC it needs exists, and fails with a message if not, with your workloads still running. Restores of
one policy run one at a time, oldest first (a later one shows `waiting for restore "<name>"`). While one runs
the policy starts no backup, and a Restore whose policy has a backup or verify already running **waits for it
to finish first** (it shows `waiting for "<job>" to finish before restoring`): a restore and a mover must
never touch the same repository at once, because `forget --prune` rewrites pack files a restore is reading.
If `scaleDown` names a workload that a `HorizontalPodAutoscaler` targets, that autoscaler is **quiesced** too:
its `min`/`max` are recorded into `status.scaledHPAs` and set to 0 so it cannot scale the workload back up
under the restore, then put back on completion. On clusters before 1.30 whose API rejects a 0 bound, the
autoscaler is left running with a warning on the Restore instead of failing it.
If the workloads do not stop within 5 minutes the restore fails and they are scaled back up, because nothing was restored. If the restore itself fails the workloads stay down unless
`scaleUpOnFailure: true`, since the data may be half written. A `Restore` with `scaleDown` carries the
finalizer `heirloom.io/restore`: deleting it mid-flight puts the workloads back first. A files restore of a
policy with `subPath` writes back into that subPath, and `delete: true` never removes paths the backup
excludes.

## CRD reference

| kind | scope | purpose |
|---|---|---|
| `BackupStore` | cluster | NFS server and path, or an SMB server, share and credentials Secret; optional `mountOptions`; default retention |
| `BackupPolicy` (`bp`) | namespaced | storeRef, schedule, jitter, one source, retention, verify, trigger, suspend, pod options |
| `Restore` (`brs`) | namespaced | policyRef, from, targetPVC, paths, databases, delete, scaleDown |

Notable `BackupPolicy` fields: `timeZone` (IANA zone the schedules run in, default UTC), `jitter` (a
duration like `15m`; shifts every slot of the policy by a fixed per-policy hash offset in `[0, jitter)`, so
many policies sharing `0 * * * *` do not all hit the NAS at :00. The offset is stable across runs and per
role, so `bs-<policy>-run-<slot>` names stay idempotent; keep it well under the schedule interval, max 6h),
`minSourcePercent`
(default 50, 0 disables; see Failure modes), `activeDeadlineSeconds` (default 3600), `trigger.manual`,
`pod.{resources,nodeSelector,tolerations,priorityClassName,imagePullSecrets,scratchStorageClassName,scratchSize}`.
`pod.resources` defaults its **requests** to `cpu: 100m, memory: 512Mi` (a best-effort mover is the first
pod evicted, and restic's index alone is a few hundred MiB); limits stay unset unless you set them. A
measured guide for `limits.memory`: backups of large file trees
peak around 300 Mi (restic's packer buffers grow with sustained throughput), restores and database dumps are
far cheaper (~70 Mi), and leaving the limit unset simply runs best-effort.
`pod.scratchStorageClassName` affects **every** mover pod, not just verify: restic keeps its chunk cache on
`/scratch` (falling back to `--no-cache` when there is no scratch dir), sqlite and postgres restores stage
their payloads through scratch, and the throwaway postgres data dir lives there. Its size is
`pod.scratchSize` (default 10Gi); the default (unset) is an emptyDir, which consumes node disk — for
sqlite/postgres policies whose dumps are large, give `/scratch` a storage class.
Source options: files `exclude` (passed to restic as `--exclude`). (`skipACLsAndXattrs` is gone: it was an
rsync workaround; restic stores ACLs and extended attributes only where the filesystem has them, so it never
failed the way rsync did); postgres `databases`, `excludeDatabases`, `globals` (`full`,
`no-role-passwords` for a least-privilege backup role, or `skip`); command `podSelector`, `container`,
`command` (its stdout becomes the dump), `file` (the name inside `command/` in the snapshot), and the
optional `preCommand`/`postCommand` quiesce hooks. A command mover runs as a scoped, namespace-local
`heirloom-executor` ServiceAccount the controller maintains only for command policies — its Role grants
nothing but reading pods and `pods/exec`, nothing else. `preCommand` must exit 0 before the dump starts (the
place to take a read lock); `postCommand` runs no matter how the dump ended, so a quiesce is always undone
even if the run is cancelled or expires. The dump command must select exactly one Running, ready pod; if
`podSelector` matches several, the run fails rather than guessing which to lock.
`spec.nfs.mountOptions` on a `BackupStore` (for example `["nfsvers=4.1", "hard", "timeo=50"]`) is how a store
pins the NFS mount knobs that decide NAS reliability. A pod's inline NFS volume cannot carry mount options,
so a store that sets them instead gets a static PV + PVC pair provisioned per namespace (deterministic name,
`persistentVolumeReclaimPolicy: Delete` — it deletes only the PV pointer, never the export), and movers mount
that claim. Because `spec.mountOptions` is immutable on a bound PV, changing a live store's options shows up
as `Ready=False/StoreClaimPending` telling you to `kubectl delete pv <name>` so the next run recreates it.
Full schema: `config/crd/` or
`kubectl explain backuppolicy.spec`. Specs are validated when applied (non-empty fields, `from` format —
`latest` or a hex id of 8-64 chars, one source block matching `type`, name length, `delete` not combined
with `paths`) and again by the controller.

A manual trigger value can be used again later: setting `trigger.manual` to a value you used before, after a
different one, runs again.

Status shows `lastSnapshot`, `lastSyncTime`, `lastSyncDuration`, `nextSyncTime`, `snapshotsKept`,
`lastVerify` and the `Ready` and `Healthy` conditions. `lastSnapshot.name` holds the 8-character short id of
the run's restic snapshot. On success the `Healthy` condition carries the run summary, e.g.
`restic snapshot abc12345: 1043 files, 1.5 GiB processed, 8.2 MiB added to repository; kept 36, pruned 1` —
the "added to repository" number is the dedup payoff. `Healthy` is `False` with reason `BackupFailed` when
the latest run failed, and with reason `Stale` when a scheduled run is overdue and no backup has succeeded:
the grace period is a quarter of the schedule interval, clamped to 5 minutes..6 hours. `Stale` is what
reports an unreachable NAS promptly, because the stuck pod never finishes and so never reports a failure.

## Events

`kubectl describe backuppolicy <name>` shows an Event when the policy's health turns bad (reason `BackupFailed` or
`Stale`) or changes reason, and a `Recovered` event when it is healthy again; steady states are not repeated.
`kubectl describe restore <name>` shows `ScalingDown`, `Started`, `ScalingUp` and `Succeeded` or `Failed`.

## Metrics

The manager serves Prometheus metrics on `:8443/metrics` over TLS (self-signed) and requires an authenticated,
authorized caller: a TokenReview of the bearer token and a SubjectAccessReview for `get /metrics`. Bind the
ClusterRole `<release>-metrics-reader` to whatever scrapes it. `--set serviceMonitor.enabled=true` renders a
ServiceMonitor that uses Prometheus's ServiceAccount token. For a closed network you can turn this off with
`--set metrics.secure=false` (plain HTTP, no authentication, on the same port). Alert on
`heirloom_policy_healthy == 0`, which also covers a policy that has never succeeded, and on
`heirloom_last_verify_ok == 0`. Gauges, each labelled `namespace` and `policy`:

| metric | meaning |
|---|---|
| `heirloom_last_success_timestamp_seconds` | time of the last successful backup |
| `heirloom_last_snapshot_bytes` | size of the newest snapshot |
| `heirloom_last_verify_ok` | 1 if the last verify passed, else 0 |
| `heirloom_snapshots_kept` | snapshots retained after the last run |
| `heirloom_policy_healthy` | 1 if `Healthy` is True, 0 if False (failed or stale); absent while Unknown |
| `heirloom_policy_created_timestamp_seconds` | when the policy was created |

## Failure modes

| situation | what happens |
|---|---|
| NAS down or unmountable | the mover pod waits in ContainerCreating (kubelet keeps retrying the mount, so it completes by itself once the NAS is back); the policy turns `Healthy=False/Stale` after one overdue run plus grace; nothing is lost on the NAS |
| NAS full | restic or the dump fails, no snapshot is published, previous snapshots untouched, run reported failed; `.staging` is swept at the start of the next run |
| source empty or recreated | run fails the guard: the run's snapshot file count is compared against the previous snapshot's (both from restic summaries), and 0 files or a shrink below `minSourcePercent` fails the run; restic itself also refuses to snapshot an empty tree. restic has committed the snapshot by the time the guard trips, so the runner `restic forget`s that rejected snapshot: history stays ending at the last good one and `from: latest` never hands back the shrunken tree (its freed packs are reclaimed by the next run's prune) |
| repo lock left by a killed run | every restic read path (`restore`, `check`) unlocks the repository before starting, so a stale lock from a run that died mid-flight cannot make the next mover hang retrying a lock save forever |
| run longer than the interval | no second run starts while one is active; the due slot runs as soon as it finishes (once), and `activeDeadlineSeconds` kills a stuck run |
| operator down | a run already in progress finishes, but no new scheduled run starts and status stops updating; on return a missed slot runs once. Alert on `Stale` and on a missing or old `heirloom_last_success_timestamp_seconds` |
| crash mid-run | no snapshot is published (restic commits it only at the end); the transient `.staging` tree is swept at the start of the next run and packs the killed run uploaded are unreferenced and removed by the next `forget --prune` |

## Security

- Backups are plaintext and the restic repositories are unencrypted and passwordless
  (`--insecure-no-password`) by design: there is no key to guard, because the NAS share ACL *is* the
  security boundary. Anyone who can read the export can read every secret in every backed-up volume and
  dump.
- Trust model: whoever can create `BackupPolicy` or `Restore` in a namespace is trusted like a namespace
  admin of *that* namespace. They can back up or restore into any PVC there, pass any Secret key of that
  namespace to a pod, run a postgres image of their choosing, and scale that namespace's workloads. Grant
  these CRs accordingly. They cannot reach other namespaces: every reference is resolved in the CR's own
  namespace.
- Tenant isolation on the store: each mover mounts only its own `<namespace>/<policy>` directory of the
  export (a `subPath` mount), so no mover, including one running a user-chosen image, can see or modify
  another namespace's backups. A run only ever opens its own policy's repository. User-supplied paths
  (`subPath`, sqlite `paths`, `Restore.paths`) are resolved with symlinks followed and refused if they
  leave their volume.
- **Write access to the NFS export is as powerful as cluster access.** A restore pipes
  `globals.sql.gz` and the per-database `.sql.gz` dumps straight into `psql` (a planted dump can run
  `COPY ... PROGRAM` or `\!` on the database server, or in the mover as root) and writes restored files,
  including setuid bits and ownership, into a PVC as root. Restic's content hashes address data inside the
  repository; they are not signatures: an attacker who can write the export can plant a whole, internally
  consistent repository. Restrict the export to the node IPs, give it no other writers, and consider
  per-cluster exports. Read access exposes every secret in every backed-up volume.
- Status and Job messages carry the tail of `psql` and `pg_dump` error output. The lines in which PostgreSQL quotes
  data (`DETAIL`, `CONTEXT`, `STATEMENT`, `QUERY`) are removed first, but the primary `ERROR` line is kept and can
  still name a value (for example an invalid input). The messages are visible to anyone who can read the policy in
  its namespace.
- The namespace-admin trust model also covers `pod.nodeSelector`, `tolerations` and `priorityClassName`: a policy
  author can place a mover on tainted nodes and raise its priority.
- Mover pods run as root (needed to preserve ownership and read arbitrary volumes), with all capabilities
  dropped except `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `KILL`, `SETUID` and `SETGID`, no privilege
  escalation, and the runtime default seccomp profile.
- The NFS export itself is still whole-export to the node: anything with root on the node, or any NFS
  client allowed by the export, can read every namespace's backups. Limit the export to your node IPs.
  NFS with `AUTH_SYS` trusts the client.
- The manager runs non-root, read-only root filesystem, no capabilities.

## Limitations

- One store (NFS or SMB) per policy. Any namespace can reference any `BackupStore`; each policy still only ever sees
  its own `<namespace>/<policy>` directory, and a policy's run only ever opens its own repository.
- The store is not browsable. Individual files come back with the restic binary (`restic -r <ns>/<policy>/restic
  --insecure-no-password dump/restore`), not with `ls` and `cp` on the NAS.
- **A run must fit in `activeDeadlineSeconds` (default one hour).** A killed run publishes no snapshot, and
  the next run reads the whole source again; raise the deadline for a first backup of a large volume.
  Database sources are dumps and have no partial state; make sure the deadline covers one dump.
- A mover mounts the app's PVC, which is ReadWriteOnce. On a multi-node cluster the mover must land on the node
  that has the volume attached: use `pod.nodeSelector` (or ReadWriteMany storage). Single-node k3s with
  local-path has no such problem.
- Deleting a `BackupPolicy` deletes its Jobs but **leaves `<namespace>/<policy>` — restic repository
  included — on the NAS**, deliberately: backups outlive their configuration. A policy later created with
  the same name in the same namespace continues that history, because the repository is still there.
  Delete the directory yourself to start fresh. Deleting a `BackupStore` leaves its policies unable to
  start new runs until it is back.
- Retention counts mean exactly what `restic forget --keep-hourly/--keep-daily/--keep-weekly/--keep-monthly`
  means, applied to the policy's own repository only. An all-zero `retention` is rejected when applied
  (`hourly: 1` keeps only the newest snapshot); an absent `retention` means the store's, then the defaults.
- The scheduler pauses while the manager is down (see How it works). Verify and restore resolve a snapshot
  by id while a backup may prune an older one: a verify or restore of an explicitly named old id can fail
  spuriously if it is pruned at that moment.
- Flux and other GitOps tools: for workloads listed in `scaleDown`, a restore sets
  `kustomize.toolkit.fluxcd.io/reconcile: disabled` while it works and removes it afterwards (only if it
  added it). Other GitOps tools are not told: suspend them yourself. Workloads you scale by hand are not
  protected either.
- The `subPath` mount makes kubelet create `<namespace>/<policy>` on the export the first time, as root, so
  the export needs `no_root_squash` (already required for ownership). A verify or restore for a policy that
  has never run a backup fails cleanly: its restic repository does not exist yet.
- CRDs in `charts/heirloom/crds` install on first `helm install` only. On upgrade, run
  `kubectl apply -f charts/heirloom/crds/`.
- Block-level or application-consistent snapshots of arbitrary workloads are out of scope. Files are
  copied live; use a database source type for databases.

## Compared to other tools

- **VolSync**: restic/rclone/rsync movers with CSI snapshot support; excellent for encrypted offsite
  copies and block-consistent replication. Heirloom trades that breadth for one NFS export, GFS
  retention, and a built-in dump-and-verify flow for sqlite and postgres.
- **k8up**: also restic-based with a similar operator model, but S3-centric with restic profiles and
  offsite sync. Heirloom is the plain local backend on NFS: no provider configuration, nothing listening
  on a port.
- **Velero**: cluster-wide object and volume backup with cloud snapshots; far broader scope, much
  heavier, and restores need Velero.

Use those if you need encryption at rest, offsite object storage or CSI snapshots. Use heirloom if you
have one NFS export and want deduplicated, verified backups with no extra services: no rest-server, no
object storage, nothing to operate besides the operator.

See `docs/DESIGN.md` for design decisions and `docs/RESTORE-RUNBOOKS.md` for recovery procedures.

## Development

```sh
make build test lint manifests helm-lint helm-template docker-build
```

License: Apache-2.0.
