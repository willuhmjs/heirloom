# heirloom

Kubernetes-native backups that land as deduplicated restic repositories on your NAS.

```sh
helm repo add heirloom https://willuhmjs.github.io/heirloom
helm repo update
helm install heirloom heirloom/heirloom -n heirloom-system --create-namespace \
  --set store.create=true --set store.nfs.server=192.168.1.5 --set store.nfs.path=/volume1/k8s-backup
```

That installs the operator and one `BackupStore` pointing at your NFS export. Then create a `BackupPolicy`
next to each app you want to back up. The project README, examples and runbooks are at
<https://github.com/willuhmjs/heirloom>.

Requires Kubernetes 1.29 or newer (the CRDs use CEL validation) and an NFS export that allows the nodes to
write as root (`no_root_squash`), because the movers preserve file ownership.

## Values

| value | default | meaning |
|---|---|---|
| `image.repository` / `image.tag` | `ghcr.io/willuhmjs/heirloom` / chart appVersion | the operator and mover image |
| `imagePullSecrets` | `[]` | for a private mirror of the image |
| `leaderElection` | `true` | leader election for the manager |
| `replicas` | `1` | manager replicas; >1 makes updates rolling (leader election on) |
| `podDisruptionBudget.enabled` / `.minAvailable` / `.maxUnavailable` | `false` / `1` / `1` | optional PDB for the manager |
| `crdHooks.enabled` | `true` | apply the image-embedded CRDs on every install/upgrade (Helm only installs `crds/` once) |
| `metrics.secure` | `true` | serve metrics over TLS and require an authenticated, authorized caller |
| `serviceMonitor.enabled` | `false` | render a Prometheus Operator ServiceMonitor |
| `store.create` | `false` | also render a default cluster-scoped `BackupStore` |
| `store.smb.server`, `store.smb.share`, `store.smb.credentialsSecret`, `store.smb.credentialsSecretNamespace`, `store.smb.mountOptions` | | use an SMB share instead of NFS (needs [csi-driver-smb](https://github.com/kubernetes-csi/csi-driver-smb)); leave `store.nfs.server` empty |
| `store.encryption.passwordSecret`, `store.encryption.passwordKey` | | encrypt the restic repositories with a master password from this Secret (release namespace); each policy gets its own derived password |
| `store.name`, `store.nfs.server`, `store.nfs.path`, `store.nfs.mountOptions`, `store.retention` | | the default store (`mountOptions` make movers mount it through a provisioned static PV/PVC, because inline NFS cannot carry options) |
| `resources`, `nodeSelector`, `tolerations`, `podAnnotations` | small defaults | for the manager pod |

## CRDs

The CRDs ship in `crds/`, which Helm installs on the first `helm install` and never upgrades. After upgrading to
a release that changes the API, apply them yourself:

```sh
kubectl apply --server-side -f https://raw.githubusercontent.com/willuhmjs/heirloom/main/charts/heirloom/crds/heirloom.io_backuppolicies.yaml
```

(or the whole `charts/heirloom/crds/` directory from the release you installed).

## Metrics

Metrics are on `:8443/metrics` over TLS and need a token that may `get /metrics`. Bind the ClusterRole
`<release>-metrics-reader` to your scraper, or use `serviceMonitor.enabled=true` with Prometheus Operator.
