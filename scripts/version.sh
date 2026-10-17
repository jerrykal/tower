#!/usr/bin/env bash
# Prints the version to stamp into a build: the tag when HEAD is exactly a
# clean v* tag, otherwise the last tag (or 0.0.0) with
# -dev.<commit time>.<commit count>+<commit>. The commit time (UTC,
# YYYYMMDDhhmmss), then the count (commits one rebase rewrote in the same
# second), put development builds in order under sort -V, which a host's
# current link goes by. A dirty tree adds the time of its newest changed
# file (-dev.<time>.<count>.<changed>+…), which orders rebuilds of one
# commit, and a hash of its changes (tracked and untracked)
# (+<commit>.dirty.<hash>), so each rebuild of edited code is a version of
# its own: install on connect puts a build on a host only when its version
# is missing there.
set -euo pipefail
if tag=$(git describe --tags --exact-match --match 'v*' 2>/dev/null) && git diff --quiet HEAD &&
  [ -z "$(git ls-files --others --exclude-standard)" ]; then
  echo "${tag#v}"
  exit
fi
utc() { date -u -d "@$1" +%Y%m%d%H%M%S 2>/dev/null || date -u -r "$1" +%Y%m%d%H%M%S; }
mtime() { stat -c %Y -- "$1" 2>/dev/null || stat -f %m -- "$1" 2>/dev/null || echo 0; }
last=$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo v0.0.0)
sha=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
# --no-show-signature: log.showSignature would print a signature first.
at=$(git log -1 --no-show-signature --format=%ct 2>/dev/null || echo 0)
at=$(utc "${at:-0}")
n=$(git rev-list --count HEAD 2>/dev/null || echo 0)
dev="$at.$n"
untracked=$(git ls-files --others --exclude-standard)
if ! git diff --quiet HEAD 2>/dev/null || [ -n "$untracked" ]; then
  changes=$({
    git diff HEAD
    if [ -n "$untracked" ]; then
      printf '%s\n' "$untracked" | while IFS= read -r f; do printf '%s\n' "$f"; cat -- "$f"; done
    fi
  } | shasum -a 256 | cut -c1-8)
  newest=0
  while IFS= read -r f; do
    [ -e "$f" ] || continue
    m=$(mtime "$f")
    [ "$m" -gt "$newest" ] && newest=$m
  done < <(git diff --name-only HEAD; printf '%s\n' "$untracked")
  [ "$newest" -gt 0 ] || newest=$(date +%s)
  dev="$dev.$(utc "$newest")"
  sha="$sha.dirty.$changes"
fi
echo "${last#v}-dev.$dev+$sha"
