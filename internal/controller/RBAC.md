# RBAC used by the controller

Cluster-scoped (ClusterRole):

| apiGroup | resources | verbs |
|---|---|---|
| backshots.io | backupstores | get, list, watch |
| backshots.io | backuppolicies, restores | get, list, watch |
| backshots.io | backuppolicies/status, restores/status | get, update |
| backshots.io | restores | update (the `backshots.io/restore` finalizer) |
| backshots.io | backuppolicies/finalizers, restores/finalizers | update (blockOwnerDeletion on owned Jobs) |
| (core) | persistentvolumeclaims | get (uncached: a restore checks its claim exists before stopping anything) |
| apps | deployments, statefulsets | get, patch |
| (core) | pods | get, list, watch |
| (core) | events | create, patch (health changes and restore phases) |

Pods: cached list/watch of mover pods (label `backshots.io/policy`), and uncached `list` of the pods of
workloads being scaled down by a Restore (any pod in the Restore's namespace).

Workloads are read uncached with `get` and changed with a merge `patch` (replicas and annotations
`kustomize.toolkit.fluxcd.io/reconcile`, `backshots.io/restore`). No watch/list on Deployments or StatefulSets.

In every namespace that holds BackupPolicies/Restores (Role or the ClusterRole above bound cluster-wide):

| apiGroup | resources | verbs |
|---|---|---|
| batch | jobs | get, list, watch, create, delete |

The controller creates every Job itself: scheduled backup and verify runs (`bs-<policy>-run-<slot>`,
`bs-<policy>-verify-<slot>`), manual runs and restore Jobs. It deletes only finished backup and verify Jobs
beyond the history limit (3 succeeded, 3 failed per role); the rest are removed by garbage collection when the
policy or restore is deleted (ownerReferences). There are no CronJobs.
Because Jobs and Pods are watched cluster-wide, the chart grants them via ClusterRole.

Leader election (when `--leader-elect`): `coordination.k8s.io` `leases` get/create/update in the manager namespace,
and `events` create/patch.

The mover pods run with the namespace's default ServiceAccount and need no API access.
