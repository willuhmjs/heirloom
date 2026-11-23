# Upgrading

## The CRDs are upgraded by the chart itself

Helm installs the files in a chart's `crds/` directory on the first `helm install` and never changes them
again. The chart closes that gap: by default it runs a pre-install and pre-upgrade hook that applies the
CRDs embedded in the operator image (`backshots install-crds`), so an upgrade can never leave the cluster on
an old schema. It needs permission to manage `customresourcedefinitions`, which its own hook
ClusterRole grants. Set `crdHooks.enabled=false` only if you apply CRDs yourself, and then apply the
chart's `crds/` yourself before `helm upgrade` when the changelog mentions an API change:

```sh
helm pull backshots/backshots --version <new-version> --untar
kubectl apply --server-side --force-conflicts -f backshots/crds/
```

`--server-side` is needed because the CRDs are large for client-side apply.

## While the API is v1alpha1

Changes are kept additive where possible: new optional fields, new enum values. A breaking change is called out in
the changelog with the manual step to take. There is no conversion webhook; if a field is renamed, edit your
policies.

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
