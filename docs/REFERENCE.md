# backshots reference

The detailed documentation: architecture, every behaviour and its limits, failure modes, the security model.
For the overview and a five-minute start, see the [README](../README.md). Restore procedures are in
[RESTORE-RUNBOOKS.md](RESTORE-RUNBOOKS.md), design decisions in [DESIGN.md](DESIGN.md).

## How it works

```
 BackupPolicy (ns/app)          BackupStore (cluster)
        |                              |  nfs: server + path
        v                              v
  +-----------+  creates a Job per due run, itself         +------------------+
  | backshots | -----------------------------------------> | mover Job pod    |
  | manager   |   (own scheduler: cron slots, no CronJob)  |  /source  PVC    |
  +-----------+                                            |  /store   NFS    |
        ^                                                  +--------+---------+
        | status via pod termination message                          |
        +-------------------------------------------------------------+
                                                       rsync / sqlite3 / pg_dump
                                                                       |
                                                                       v
                                                         NAS: <ns>/<policy>/<snapshot>/
```

The manager is the scheduler. Each reconcile it checks whether a cron slot of a policy has passed since the
last run and, if so, creates one Job (`bs-<policy>-run-<slot>`; verify runs are `bs-<policy>-verify-<slot>`).
It never starts a second run while one is active, starts nothing while a `Restore` for the policy is running,
prunes old finished Jobs (keeps 3 succeeded and 3 failed per role), and reads results back from pod
termination messages. It never mounts the store. **If the manager is down, scheduled runs pause**; when it
returns it runs a missed slot once, not once per missed slot, and a policy turns `Healthy=False/Stale` if
too long passes without a success.
Mover pods mount the NFS export and the PVC directly and run the backshots binary (`backshots run`),
which is copied in by an init container so database images (for example `postgres:16`) can be used
unchanged.

Source types:

| type | method |
|---|---|
| `files` | `rsync -aHAX` of the PVC, hardlinked against the previous snapshot |
| `sqlite` | consistent copy per database with the SQLite online backup API |
| `postgres` | `pg_dump` per database plus globals, run inside the image you name |

## On-disk layout

```
/volume1/k8s-backup/
  gitea/                              namespace
    gitea-data/                       policy
      2026-11-20T02-30-00Z/           complete snapshot (plain copy of the data)
      2026-11-21T02-30-00Z/
      2026-11-22T02-30-00Z/
        _status.json
      .incomplete-2026-11-23T02-30-00Z/   run in progress or crashed; ignored and cleaned up
```

A snapshot directory appears only through an atomic rename of a finished `.incomplete-` directory, so
every visible folder is complete.

## Quickstart

1. Synology: Control Panel > Shared Folder > create `k8s-backup`. Under NFS Permissions allow your
   node IPs only, with Squash set to **No mapping** (root must be able to write and chown), and
   allow asynchronous off. Enable NFS and set the maximum protocol to NFSv4.1.
2. Install the operator from the Helm repository:

   ```sh
   helm repo add backshots https://willuhmjs.github.io/backshots
   helm repo update
   helm install backshots backshots/backshots \
     --namespace backshots-system --create-namespace \
     --set store.create=true \
     --set store.nfs.server=192.168.1.5 \
     --set store.nfs.path=/volume1/k8s-backup
   ```

   The chart is also published as an OCI artifact: `helm install backshots oci://ghcr.io/willuhmjs/charts/backshots`.
   Or apply `examples/store.yaml` yourself.
3. Apply a policy in the namespace of the PVC:

   ```sh
   kubectl apply -f examples/policy-files.yaml
   kubectl get bp -A
   ```

   Run one now with `spec.trigger.manual` (see `examples/trigger-manual.yaml`).

Nodes need an NFS client (`nfs-common` / `nfs-utils`).

## Retention

Retention is GFS: keep the newest snapshot of each of the last N hours, days, ISO weeks and months
(defaults hourly 24, daily 30, weekly 12, monthly 24). The newest snapshot is always kept. A policy
overrides the store default. Pruning runs at the end of each successful backup.

Tradeoffs, stated plainly:

- `files` snapshots use hardlinks to the previous snapshot, so unchanged files cost no extra space and
  each kept snapshot looks like a full copy.
- `sqlite` and `postgres` dumps are rewritten every run. There is no cross-snapshot dedup, so each
  kept snapshot costs a full copy of the dump. Pick retention counts with that in mind.
- Hardlinks mean one physical file is shared by many snapshots. Bit rot or an accidental edit to that
  file changes it in every snapshot that shares it. Backshots does not checksum your store. Keep a
  second copy of the NAS offsite (Hyper Backup, rclone, a second NAS).
- A failed run leaves only a `.incomplete-` directory, never a half-written snapshot.

## Verify

A policy may set `verify.schedule`. The verify job reads the latest snapshot from the store
(read-only) and proves it restores:

- `postgres`: initdb a scratch cluster, restore the dump, run each `verify.sql` assertion (each must
  return `t`) against one database: `verify.database`, or the single database the policy lists. A policy
  that dumps several databases must name one, otherwise it is rejected (`Ready=False`, `InvalidSpec`)
  rather than guessing. The scratch volume is an emptyDir, or a generic ephemeral volume of
  `pod.scratchSize` (default 10Gi) when `pod.scratchStorageClassName` is set.
- `sqlite`: open each copy and run `PRAGMA integrity_check`.
- `files`: walk the snapshot and compare against recorded counts.

The result lands in `status.lastVerify` and in metrics. A backup you have never restored is a guess.

## Restore

By hand: copy the files back. That is the point. See `docs/RESTORE-RUNBOOKS.md`.

- files: copy the snapshot directory (or a subpath) into the PVC, preserving ownership (`cp -a`, `rsync -aHAX`).
- sqlite: stop the app, copy the database file over the original, delete any `-wal`/`-shm` files.
- postgres: `postgres/` in the snapshot holds `globals.sql.gz` and one `<db>.sql.gz` per database, plain SQL
  made with `--create --clean --if-exists`; load with `gunzip -c <db>.sql.gz | psql -d postgres` (no `pg_restore`).
- files live under `data/` and sqlite copies under `sqlite/` inside the snapshot directory.

With the CR: create a `Restore` next to the policy (see `examples/restore-*.yaml`) and watch
`kubectl get restores -n <ns>`. It can scale Deployments and StatefulSets to zero first and back
afterwards, restore a named snapshot or a subpath, and target a different PVC.

What it does before it touches anything: it validates `from`, `paths`, `databases`, the name length and that
every PVC it needs exists, and fails with a message if not, with your workloads still running. Restores of
one policy run one at a time, oldest first (a later one shows `waiting for restore "<name>"`). While one runs
the policy starts no backup. If the workloads do not stop within 5 minutes the restore fails and they are
scaled back up, because nothing was restored. If the restore itself fails the workloads stay down unless
`scaleUpOnFailure: true`, since the data may be half written. A `Restore` with `scaleDown` carries the
finalizer `backshots.io/restore`: deleting it mid-flight puts the workloads back first. A files restore of a
policy with `subPath` writes back into that subPath, and `delete: true` never removes paths the backup
excludes.

## CRD reference

| kind | scope | purpose |
|---|---|---|
| `BackupStore` | cluster | NFS server and path, default retention |
| `BackupPolicy` (`bp`) | namespaced | storeRef, schedule, one source, retention, verify, trigger, suspend, pod options |
| `Restore` (`brs`) | namespaced | policyRef, from, targetPVC, paths, databases, delete, scaleDown |

Notable `BackupPolicy` fields: `timeZone` (IANA zone the schedules run in, default UTC; snapshot names stay
UTC), `minSourcePercent` (default 50), `activeDeadlineSeconds` (default 3600), `trigger.manual`,
`pod.{resources,nodeSelector,tolerations,priorityClassName,imagePullSecrets,scratchStorageClassName,scratchSize}`.
Source options: files `exclude` and `skipACLsAndXattrs` (use when the NFS export does not support them);
postgres `databases`, `excludeDatabases`, `globals` (`full`, `no-role-passwords` for a least-privilege backup
role, or `skip`). Full schema: `config/crd/` or `kubectl explain backuppolicy.spec`. Specs are validated when
applied (non-empty fields, `from` format, one source block matching `type`, name length) and again by the
controller.

A manual trigger value can be used again later: setting `trigger.manual` to a value you used before, after a
different one, runs again.

Status shows `lastSnapshot`, `lastSyncTime`, `lastSyncDuration`, `nextSyncTime`, `snapshotsKept`,
`lastVerify` and the `Ready` and `Healthy` conditions. `Healthy` is `False` with reason `BackupFailed` when
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
`backshots_policy_healthy == 0`, which also covers a policy that has never succeeded, and on
`backshots_last_verify_ok == 0`. Gauges, each labelled `namespace` and `policy`:

| metric | meaning |
|---|---|
| `backshots_last_success_timestamp_seconds` | time of the last successful backup |
| `backshots_last_snapshot_bytes` | size of the newest snapshot |
| `backshots_last_verify_ok` | 1 if the last verify passed, else 0 |
| `backshots_snapshots_kept` | snapshots retained after the last run |
| `backshots_policy_healthy` | 1 if `Healthy` is True, 0 if False (failed or stale); absent while Unknown |
| `backshots_policy_created_timestamp_seconds` | when the policy was created |

## Failure modes

| situation | what happens |
|---|---|
| NAS down or unmountable | the mover pod waits in ContainerCreating (kubelet keeps retrying the mount, so it completes by itself once the NAS is back); the policy turns `Healthy=False/Stale` after one overdue run plus grace; nothing is lost on the NAS |
| NAS full | rsync or dump fails, the `.incomplete-` directory is removed, previous snapshots untouched, run reported failed |
| source empty or recreated | run aborts when the file count is under `minSourcePercent` of the previous snapshot, so an empty PVC does not become the newest snapshot |
| run longer than the interval | no second run starts while one is active; the due slot runs as soon as it finishes (once), and `activeDeadlineSeconds` kills a stuck run |
| operator down | a run already in progress finishes, but no new scheduled run starts and status stops updating; on return a missed slot runs once. Alert on `Stale` and on a missing or old `backshots_last_success_timestamp_seconds` |
| crash mid-run | leftover `.incomplete-` directory is ignored and cleaned on the next run |

## Security

- Backups are plaintext. The NAS share ACL is the security boundary. Anyone who can read the export
  can read every secret in every backed-up volume and dump.
- Trust model: whoever can create `BackupPolicy` or `Restore` in a namespace is trusted like a namespace
  admin of *that* namespace. They can back up or restore into any PVC there, pass any Secret key of that
  namespace to a pod, run a postgres image of their choosing, and scale that namespace's workloads. Grant
  these CRs accordingly. They cannot reach other namespaces: every reference is resolved in the CR's own
  namespace.
- Tenant isolation on the store: each mover mounts only its own `<namespace>/<policy>` directory of the
  export (a `subPath` mount), so no mover, including one running a user-chosen image, can see or modify
  another namespace's backups. User-supplied paths (`subPath`, sqlite `paths`, `Restore.paths`) are
  resolved with symlinks followed and refused if they leave their volume.
- **Write access to the NFS export is as powerful as cluster access.** A restore pipes `postgres/*.sql.gz`
  straight into `psql` (a planted dump can run `COPY ... PROGRAM` or `\!` on the database server, or in the
  mover as root) and writes snapshot files, including setuid bits and ownership, into a PVC as root. Backshots
  does not sign or checksum snapshots. Restrict the export to the node IPs, give it no other writers, and
  consider per-cluster exports. Read access exposes every secret in every backed-up volume.
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

- One NFS store per policy. Any namespace can reference any `BackupStore`; each policy still only ever sees
  its own `<namespace>/<policy>` directory.
- No dedup across snapshots for database dumps.
- **A first backup of a large volume can take several runs.** A files run killed at `activeDeadlineSeconds`
  (default one hour) leaves its partial tree, and the next run adopts it: rsync skips what is already there. Raise
  the deadline to finish in fewer runs. A run that failed the empty-source check does not leave a tree to resume
  from. Database sources are dumps and have no partial state; make sure the deadline covers one dump.
- A mover mounts the app's PVC, which is ReadWriteOnce. On a multi-node cluster the mover must land on the node
  that has the volume attached: use `pod.nodeSelector` (or ReadWriteMany storage). Single-node k3s with
  local-path has no such problem.
- Deleting a `BackupPolicy` deletes its Jobs but **leaves `<namespace>/<policy>` on the NAS**, deliberately:
  backups outlive their configuration. A policy later created with the same name in the same namespace will
  continue that history. Delete the directory yourself to start fresh. Deleting a `BackupStore` leaves its
  policies unable to start new runs until it is back.
- Retention keeps the newest snapshot of each of the last N hour, day, week and month *buckets that contain a
  snapshot*, not N time windows. An all-zero `retention` is rejected when applied (`hourly: 1` keeps only the
  newest snapshot); an absent `retention` means the store's, then the defaults. Snapshot names come from the
  node clock, so a node whose clock jumped into the future publishes a snapshot that sorts as newest until
  newer ones pass it; fix the clock and delete that folder.
- The scheduler pauses while the manager is down (see How it works). Verify and restore read a snapshot by
  name while a backup may prune an older one: a verify of an explicitly named old snapshot can fail
  spuriously if it is pruned at that moment.
- Flux and other GitOps tools: for workloads listed in `scaleDown`, a restore sets
  `kustomize.toolkit.fluxcd.io/reconcile: disabled` while it works and removes it afterwards (only if it
  added it). Other GitOps tools are not told: suspend them yourself. Workloads you scale by hand are not
  protected either.
- The `subPath` mount makes kubelet create `<namespace>/<policy>` on the export the first time, as root, so
  the export needs `no_root_squash` (already required for ownership). A verify or restore for a policy that
  has never run a backup fails to start, because the read-only mount cannot create the directory.
- CRDs in `charts/backshots/crds` install on first `helm install` only. On upgrade, run
  `kubectl apply -f charts/backshots/crds/`.
- Block-level or application-consistent snapshots of arbitrary workloads are out of scope. Files are
  copied live; use a database source type for databases.

## Compared to other tools

- **VolSync**: restic/rclone/rsync movers with CSI snapshot support; excellent for encrypted offsite
  and block-consistent copies. Backshots trades that for a repository you can browse without any tool.
- **k8up**: restic-based, S3-centric, with its own repository format. Similar operator model.
- **Velero**: cluster-wide object and volume backup with cloud snapshots; far broader scope, much
  heavier, and restores need Velero.

Use those if you need encryption at rest, deduplicated offsite storage or CSI snapshots. Use
backshots if you have a NAS and want ordinary folders.

See `docs/DESIGN.md` for design decisions and `docs/RESTORE-RUNBOOKS.md` for recovery procedures.

## Development

```sh
make build test lint manifests helm-lint helm-template docker-build
```

License: Apache-2.0.
