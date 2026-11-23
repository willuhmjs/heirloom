# Releasing

A release is a tag on `main`. CI does the rest. The first release has a few one-time steps that GitHub does not
let a workflow do for itself.

## One time

1. **Create the repository** and push `main`:

   ```sh
   gh repo create willuhmjs/backshots --public --source . --push
   ```

2. **Allow the workflows to write.** Settings > Actions > General > Workflow permissions: "Read and write
   permissions" (the workflows still request only what each job needs).
3. **Tag the first release.** The `release` workflow verifies the tag is on `main` and the tests pass, builds and
   signs the multi-arch image, pushes the chart to `oci://ghcr.io/<owner>/charts`, creates the chart's GitHub
   release and creates the `gh-pages` branch with the landing page and `index.yaml`:

   ```sh
   git tag v0.1.0 && git push origin v0.1.0
   ```
4. **Turn on GitHub Pages**, once `gh-pages` exists (after the first release run):

   ```sh
   gh api -X POST repos/willuhmjs/backshots/pages -f 'source[branch]=gh-pages' -f 'source[path]=/'
   ```

   The Helm repository is then live at `https://willuhmjs.github.io/backshots` (`index.yaml`), next to the landing page.
5. **Make the packages public.** GitHub creates a container package private the first time CI pushes it, and
   there is no API for changing that. On the repository's Packages page, open `backshots` and `charts/backshots`
   and set the visibility to Public. Until you do, `helm install` fails with `ImagePullBackOff`.
6. **Enable private vulnerability reporting** (Settings > Code security), which `SECURITY.md` points to.

Check it works from a machine that is not logged in to anything:

```sh
helm repo add backshots https://willuhmjs.github.io/backshots && helm repo update
helm search repo backshots
helm install backshots backshots/backshots -n backshots-system --create-namespace --dry-run
cosign verify ghcr.io/willuhmjs/backshots:0.1.0 \
  --certificate-identity-regexp 'https://github.com/willuhmjs/backshots/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Every release

1. Move the `[Unreleased]` entries in `CHANGELOG.md` under the new version, and bump `version` and `appVersion` in
   `charts/backshots/Chart.yaml` (the workflow packages with the tag's version regardless, but the file should match).
2. If the API changed, say so in `CHANGELOG.md` and `docs/UPGRADING.md`.
3. Merge to `main`, then `git tag vX.Y.Z && git push origin vX.Y.Z`.

Releases are immutable: fix forward with a new tag, never move one.
