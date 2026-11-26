# Security policy

## Reporting a vulnerability

Please report security problems privately through GitHub: **Security tab, "Report a vulnerability"**. Do not open
a public issue. You will get an acknowledgement within a few days. heirloom is a one-maintainer alpha project, so
there is no bounty and no SLA, but real issues are taken seriously and fixed first.

## Supported versions

Only the latest release is supported. The API is `v1`.

## What the security model is

The full text is in [`docs/REFERENCE.md`](docs/REFERENCE.md#security). In short:

- **Backups are plaintext unless the store sets `spec.encryption`** (restic encryption, a per-policy key derived from one master password you must keep outside the cluster). The NFS export is the security boundary. Limit it to your node IPs and
  give it no other writers. Anyone who can read the export can read every secret in every backed-up volume;
  anyone who can write to it can plant files and database dumps that a restore will load as root.
- **Namespace isolation.** Whoever can create `BackupPolicy` or `Restore` objects in a namespace is trusted like an
  admin of that namespace and can reach nothing outside it. Each mover pod mounts only its own
  `<namespace>/<policy>` folder of the share, and user-supplied paths are refused if a symlink would take them out
  of their volume.
- **Movers run as root** (they must preserve ownership) with all capabilities dropped except the few they need,
  no privilege escalation, a read-only install step and no ServiceAccount token.
- **The manager** runs non-root with a read-only root filesystem and serves metrics over TLS to authenticated,
  authorized callers only.
- **Releases** are built by CI from a tag on `main` after the tests pass, with pinned actions, SBOM and provenance
  attached, and the image signed keylessly with cosign:
  `cosign verify ghcr.io/willuhmjs/heirloom:<version> --certificate-identity-regexp 'https://github.com/willuhmjs/heirloom/' --certificate-oidc-issuer https://token.actions.githubusercontent.com`

## Not covered

There is no encryption at rest and no integrity protection of snapshots (no signatures or checksums). If your threat
model includes someone with write access to the NAS, heirloom is not enough on its own.
