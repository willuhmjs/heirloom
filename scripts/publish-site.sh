#!/usr/bin/env bash
# Publishes the landing page (docs/site) to the gh-pages branch, next to the Helm repository index.yaml that
# chart-releaser keeps there. Creates the branch on first use. Safe to run repeatedly: no change, no commit.
#
# Run from the repository root inside a clone that can push to origin (CI does; GITHUB_SHA is the commit to return to).
set -euo pipefail

msg=${1:-"Update site"}
back=${GITHUB_SHA:-$(git rev-parse HEAD)}

tmp=$(mktemp -d)
cp docs/site/index.html docs/assets/logo.svg "$tmp"
touch "$tmp/.nojekyll" # serve the files as they are, no Jekyll processing

git fetch origin gh-pages >/dev/null 2>&1 || true
if git rev-parse --verify --quiet origin/gh-pages >/dev/null; then
  git switch -q -C gh-pages origin/gh-pages
else
  git switch -q --orphan gh-pages
  git rm -rfq . >/dev/null 2>&1 || true
fi

cp "$tmp/index.html" "$tmp/logo.svg" "$tmp/.nojekyll" .
git add -A
if git diff --cached --quiet; then
  echo "site unchanged"
else
  git commit -qm "$msg"
  git push -q origin gh-pages
  echo "site published"
fi
git switch -q --detach "$back"
