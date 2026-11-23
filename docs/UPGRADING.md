# Upgrading

## The CRDs are not upgraded by Helm

Helm installs the files in a chart's `crds/` directory on the first `helm install` and never changes them again.
Before (or right after) `helm upgrade` to a release whose changelog mentions an API change, apply the new CRDs
from that release yourself:

```sh
helm pull backshots/backshots --version <new-version> --untar
kubectl apply --server-side --force-conflicts -f backshots/crds/
helm upgrade backshots backshots/backshots -n backshots-system --version <new-version>
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

## Rolling back

Install the previous chart version. Snapshots on the NAS are plain folders and are never rewritten by an upgrade.
