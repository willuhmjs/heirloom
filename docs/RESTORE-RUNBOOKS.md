# Restore runbooks

Every procedure has two forms: the `Restore` CR, and by hand with an NFS client plus the restic binary.
Backups are restic repositories at `<store>/<namespace>/<policy>/restic/`, so every by-hand command
below needs `--insecure-no-password`, and snapshots are restic hex ids (find them with
`restic ... snapshots`; policy status shows the 8-character short id, and `from` accepts it or the full
id). Timestamp-named snapshot folders no longer exist.

Before any restore: if Flux or Argo manages the workload, suspend it so it does not scale the app back
up (`flux suspend kustomization <name>` or annotate the object
`kustomize.toolkit.fluxcd.io/reconcile: disabled`). Resume afterwards.

Mount the store for the by-hand procedures, on any machine that can reach the NAS:

```sh
sudo mount -t nfs -o vers=4.1 192.168.1.5:/volume1/k8s-backup /mnt/backups
alias rs='restic --repo /mnt/backups/NS/POLICY/restic --insecure-no-password'
rs snapshots            # ids of available snapshots
rs ls latest            # paths inside a snapshot (relative to the volume root)
```

No restic on that machine? The operator image ships it — run a one-off pod that mounts only the
policy's folder (same isolation the movers get):

```sh
kubectl -n NS run restic --rm -it --restart=Never --image=ghcr.io/willuhmjs/heirloom \
  --overrides='{"spec":{"containers":[{"name":"restic","image":"ghcr.io/willuhmjs/heirloom","stdin":true,"tty":true,"command":["sh"],"volumeMounts":[{"name":"store","mountPath":"/store","subPath":"NS/POLICY"}]}],"volumes":[{"name":"store","nfs":{"server":"192.168.1.5","path":"/volume1/k8s-backup"}}]}}'
# inside the pod: restic --repo /store/restic --insecure-no-password snapshots
```

## Single file

CR: a files `Restore` with `paths: [path/to/file]` (see `examples/restore-files-subpath.yaml`).

By hand (paths inside the snapshot are relative to the volume root; `rs ls latest` lists them):

```sh
rs dump latest path/to/file > /somewhere/safe/file
```

Copy it into the live volume with `kubectl cp`, or from a pod that mounts the PVC.

## One directory

CR: `paths: [some/dir]`. (`delete` cannot be combined with `paths`: restic refuses include+exclude
combinations, and the CR rejects it up front.)

By hand, stop the app first if it writes there:

```sh
rs restore latest --target /tmp/recovered
rsync -aHAX /tmp/recovered/some/dir/ /path/to/pvc/some/dir/
```

Run as root so ownership is restored (restic restores it, rsync preserves it).

## One SQLite database

The snapshot holds a consistent copy of each configured path under `sqlite/<path>`.

CR: a sqlite `Restore`, with `scaleDown` for the app.

By hand:

```sh
kubectl -n NS scale deploy/APP --replicas=0
rs restore latest --target /tmp/recovered
cp -a /tmp/recovered/sqlite/db.sqlite3 /path/to/pvc/db.sqlite3
rm -f /path/to/pvc/db.sqlite3-wal /path/to/pvc/db.sqlite3-shm
kubectl -n NS scale deploy/APP --replicas=1
```

Stale `-wal`, `-shm` and `-journal` files from the old database will corrupt the restored one, so
remove them (the CR does this for you).

## One Postgres database

The snapshot contains globals and a dump per database under `postgres/` (`rs ls latest` shows the exact
file names).

CR: `databases: [name]` and `scaleDown` listing the apps that use it, not postgres itself.

By hand:

```sh
kubectl -n NS scale deploy/APP --replicas=0
rs restore latest --target /tmp/recovered
# dumps are plain SQL made with --create --clean --if-exists: connect to the maintenance db, not to `name`
gunzip -c /tmp/recovered/postgres/name.sql.gz | psql -v ON_ERROR_STOP=1 -h HOST -U postgres -d postgres
kubectl -n NS scale deploy/APP --replicas=1
```

Roles come from `postgres/globals.sql.gz` (`gunzip -c ... | psql -h HOST -U postgres -d postgres`); apply it
first on a fresh server and ignore "already exists" errors. Use a client whose major version is at least the
server's.

## One command-source dump (MySQL, MongoDB, ...)

The snapshot holds the command's gzipped output at `command/<file>` (`rs ls latest` shows it). There is no
universal loader, so the CR restore needs `targetPVC` (it writes the dump there under `command/`) and the
app's own import tool loads it.

By hand:

```sh
kubectl -n NS scale sts/APP --replicas=0
rs restore latest --target /tmp/recovered
gunzip -c /tmp/recovered/command/FILE | mysql -h HOST ...   # or mongorestore / redis-cli --pipe from stdin
kubectl -n NS scale sts/APP --replicas=1
```

`rs` is `restic --repo NS/POLICY/restic --insecure-no-password` (see Single file).

## Whole PVC

1. Scale down every workload using the PVC.
2. If the PVC is gone, recreate it with the same name and size, and apply it before the restore.
3. CR: files `Restore` with `from: latest`, `delete: true` — that is `restic restore --delete`, and the
   backup's excludes are passed along so excluded files are never removed.
4. By hand: run a root pod mounting the PVC and the policy folder of the export, then
   `restic --repo /store/restic --insecure-no-password restore latest --target /data`.
5. Scale up and check the app.

For a PVC whose policy has `subPath`, the snapshot is the subpath's contents, so restore into that subpath.

## Whole-cluster rebuild (Flux + sops-age)

Assumes the git repo and the NAS survive.

1. Install the cluster and Flux. Before Flux reconciles encrypted secrets, recreate the key:
   `kubectl create secret generic sops-age -n flux-system --from-literal=age.agekey='AGE-SECRET-KEY-...'`
   (the age key must come from your password manager; it is not in git or on the NAS).
2. Bootstrap Flux. Let it install the heirloom HelmRelease and the BackupStore.
3. Apps that need data come up with empty PVCs. Suspend their Kustomizations, scale them to zero.
4. Recreate the PVCs (these are often applied manually, not by Flux), then create `Restore` objects, or
   restore by hand with restic as above. Restore databases before the apps that use them.
5. BackupPolicy objects live in git, so they come back with the apps. Policies are the index into the
   store: `<namespace>/<policy>` selects the restic repository, so it must match the old names, or
   restore by hand from the repository.
6. Resume Flux and run a manual backup (`spec.trigger.manual`) to confirm the chain works. The
   empty-source guard compares this run's file count against the previous snapshot's, so a wrongly empty
   volume will be refused rather than becoming the newest snapshot.

## Confirming a backup is sound

You cannot eyeball a restic repository, so don't try — let the operator's `verify` pipeline do it
(`restic check`, a rotating `restic check --read-data-subset=N/10` that reads the whole repository
within ten weeks, plus per-source content checks), and by hand run `rs check` on a repository and
`rs dump` a file you know to spot-check.

If heirloom itself is gone, none of this needs the operator: every step above works with any restic
binary (the mover image ships one), `rsync`, `psql` and an NFS mount. What it does need is restic: the
repository format is the price of dedup, and there is nothing beside the repository to `cp -a`.
