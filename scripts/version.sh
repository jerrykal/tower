#!/usr/bin/env bash
# Prints the version to stamp into a build: the tag when HEAD is exactly a
# clean v* tag, otherwise the last tag (or 0.0.0) with -dev+<commit>.
set -euo pipefail
if tag=$(git describe --tags --exact-match --match 'v*' 2>/dev/null) && git diff --quiet HEAD; then
  echo "${tag#v}"
  exit
fi
last=$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo v0.0.0)
sha=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
git diff --quiet HEAD 2>/dev/null || sha="$sha.dirty"
echo "${last#v}-dev+$sha"
