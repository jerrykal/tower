#!/usr/bin/env bash
# Prints the version to stamp into a build: the tag when HEAD is exactly a
# clean v* tag, otherwise the last tag (or 0.0.0) with -dev+<commit>. A
# dirty tree adds a hash of its changes (tracked and untracked), so each
# rebuild of edited code is a version of its own: install on connect puts
# a build on a host only when its version is missing there.
set -euo pipefail
if tag=$(git describe --tags --exact-match --match 'v*' 2>/dev/null) && git diff --quiet HEAD &&
  [ -z "$(git ls-files --others --exclude-standard)" ]; then
  echo "${tag#v}"
  exit
fi
last=$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo v0.0.0)
sha=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
untracked=$(git ls-files --others --exclude-standard)
if ! git diff --quiet HEAD 2>/dev/null || [ -n "$untracked" ]; then
  changes=$({
    git diff HEAD
    if [ -n "$untracked" ]; then
      printf '%s\n' "$untracked" | while IFS= read -r f; do printf '%s\n' "$f"; cat -- "$f"; done
    fi
  } | shasum -a 256 | cut -c1-8)
  sha="$sha.dirty.$changes"
fi
echo "${last#v}-dev+$sha"
