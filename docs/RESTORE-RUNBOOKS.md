# Restore runbooks

Every procedure has two forms: the `Restore` CR, and by hand with only an NFS or SMB client.
Snapshot names are UTC timestamps like `2026-11-22T10-05-00Z`; layout is
`<store>/<namespace>/<policy>/<snapshot>/`. Ignore `.incomplete-*` directories.

Before any restore: if Flux or Argo manages the workload, suspend it so it does not scale the app back
up (`flux suspend kustomization <name>` or annotate the object
`kustomize.toolkit.fluxcd.io/reconcile: disabled`). Resume afterwards.

Mount the store for the by-hand procedures:

```sh
sudo mount -t nfs -o vers=4.1 192.168.1.5:/volume1/k8s-backup /mnt/backups
# or browse the same share over SMB from a laptop
```

## Single file

CR: a files `Restore` with `paths: [path/to/file]` (see `examples/restore-files-subpath.yaml`).

By hand:

```sh
cp -a /mnt/backups/NS/POLICY/SNAPSHOT/data/path/to/file /somewhere/safe/
```

Copy it into the live volume with `kubectl cp`, or from a pod that mounts the PVC.

## One directory

CR: `paths: [some/dir]`, optionally `delete: true` to remove extras (removes files under those paths that are not in the snapshot, so review first).

By hand, stop the app first if it writes there:

```sh
rsync -aHAX /mnt/backups/NS/POLICY/SNAPSHOT/data/some/dir/ /path/to/pvc/some/dir/
```

Run as root so ownership is restored.

## One SQLite database

Snapshot holds a consistent copy of each configured path.

CR: a sqlite `Restore`, with `scaleDown` for the app.

By hand:

```sh
kubectl -n NS scale deploy/APP --replicas=0
cp -a /mnt/backups/NS/POLICY/SNAPSHOT/sqlite/db.sqlite3 /path/to/pvc/db.sqlite3
rm -f /path/to/pvc/db.sqlite3-wal /path/to/pvc/db.sqlite3-shm
kubectl -n NS scale deploy/APP --replicas=1
```

Stale `-wal` and `-shm` files from the old database will corrupt the restored one, so remove them.

## One Postgres database

The snapshot contains globals and a dump per database (check the directory for exact file names).

CR: `databases: [name]` and `scaleDown` listing the apps that use it, not postgres itself.

By hand:

```sh
kubectl -n NS scale deploy/APP --replicas=0
# dumps are plain SQL made with --create --clean --if-exists: connect to the maintenance db, not to `name`
gunzip -c /mnt/backups/NS/POLICY/SNAPSHOT/postgres/name.sql.gz | psql -v ON_ERROR_STOP=1 -h HOST -U postgres -d postgres
kubectl -n NS scale deploy/APP --replicas=1
```

Roles come from `postgres/globals.sql.gz` (`gunzip -c ... | psql -h HOST -U postgres -d postgres`); apply it
first on a fresh server and ignore "already exists" errors. Use a client whose major version is at least the
server's.

## Whole PVC

1. Scale down every workload using the PVC.
2. If the PVC is gone, recreate it with the same name and size, and apply it before the restore.
3. CR: files `Restore` with `from: latest`, `delete: true`.
4. By hand: run a root pod mounting the PVC and the NFS export, then
   `rsync -aHAX --delete /store/NS/POLICY/SNAPSHOT/data/ /data/`.
5. Scale up and check the app.

For a PVC whose policy has `subPath`, the snapshot is the subpath's contents, so restore into that subpath.

## Whole-cluster rebuild (Flux + sops-age)

Assumes the git repo and the NAS survive.

1. Install the cluster and Flux. Before Flux reconciles encrypted secrets, recreate the key:
   `kubectl create secret generic sops-age -n flux-system --from-literal=age.agekey='AGE-SECRET-KEY-...'`
   (the age key must come from your password manager; it is not in git or on the NAS).
2. Bootstrap Flux. Let it install the backshots HelmRelease and the BackupStore.
3. Apps that need data come up with empty PVCs. Suspend their Kustomizations, scale them to zero.
4. Recreate the PVCs (these are often applied manually, not by Flux), then create `Restore` objects, or
   copy by hand. Restore databases before the apps that use them.
5. BackupPolicy objects live in git, so they come back with the apps. Policies are the index into the
   store: `<namespace>/<policy>` must match the old names, or restore by hand from the directory.
6. Resume Flux and run a manual backup (`spec.trigger.manual`) to confirm the chain works. The
   empty-source guard compares against the old snapshots, so a wrongly empty volume will be refused
   rather than becoming the newest snapshot.

If backshots itself is gone, none of this needs it: every step above works with `rsync`, `cp`, `psql`
and an NFS mount.
