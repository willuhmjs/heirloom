# Upgrading

## The CRDs are upgraded by the chart itself

Helm installs the files in a chart's `crds/` directory on the first `helm install` and never changes them
again. The chart closes that gap: by default it runs a pre-install and pre-upgrade hook that applies the
CRDs embedded in the operator image (`heirloom install-crds`), so an upgrade can never leave the cluster on
an old schema. It needs permission to manage `customresourcedefinitions`, which its own hook
ClusterRole grants. Set `crdHooks.enabled=false` only if you apply CRDs yourself, and then apply the
chart's `crds/` yourself before `helm upgrade` when the changelog mentions an API change:

```sh
helm pull heirloom/heirloom --version <new-version> --untar
kubectl apply --server-side --force-conflicts -f heirloom/crds/
```

`--server-side` is needed because the CRDs are large for client-side apply.

## Upgrading from backshots 0.x

heirloom is backshots with a better name: the same operator, the same fields, the same restic repositories on
your NAS. Only the Kubernetes-side names changed, and there is nothing to migrate on the data side — point
policies at the same store paths and the existing repositories keep deduplicating into them.

The objects cannot be renamed in place, so the swap is manual:

```sh
# 1. Let any running backup and restore finish, then remove the old release. This deletes the operator and
#    the objects Helm owns (its stores and policies); it never touches data on the store.
helm uninstall backshots --namespace backshots
# 2. Drop the old types. Delete this chart's policies/stores/restores first if you manage them outside Helm.
kubectl delete crd backuppolicies.backshots.io backupstores.backshots.io restores.backshots.io
# 3. Install heirloom (README quickstart) and re-apply your policies with apiVersion: heirloom.io/v1.
```

Saved YAML needs exactly one line changed per object: `apiVersion: backshots.io/v1alpha1` becomes
`apiVersion: heirloom.io/v1`.

## Once on v1

The API is `v1`: fields are added in minor releases and are never removed or redefined outside a major one, and
every major bump gets a section here with its manual steps. There is no conversion webhook; within a major
version all changes are additive.

## What an upgrade does to running work

The manager is a single Deployment with leader election. Restarting it:

- does not touch snapshots or any Job already running (a running backup finishes);
- pauses scheduling for the few seconds it takes to come back, after which a missed slot runs once;
- resumes any `Restore` from its recorded phase (everything is derived from `status` and the restore Job, so a
  restart mid-restore is safe).

## Upgrading to the restic build

This release changes how data lands on the store: every policy's backups become a restic repository under
`<namespace>/<policy>/restic/`. Nothing is migrated or deleted. The first backup after the upgrade creates the
repository; snapshot folders left by earlier builds are no longer read or written, so anything you might still
want from them should be copied off (they are still plain folders, and only the old build can restore them).
Rolling back afterwards means the old build ignores the repository and its retention; old-style folders it does
see behave as before.

## Rolling back

Install the previous chart version. The store is only ever written by mover pods, so an upgrade or rollback
never rewrites existing data; new writes simply take the new (or old) form.
