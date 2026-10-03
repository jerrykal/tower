# Install on connect

How the tower binary reaches every host: the home installs its own
version on a host the first time it connects there with that version, over
the ssh master it already has. Remote hosts never need GitHub, a package
manager or the user's dotfiles.

## Layout on a host

```
~/.local/share/tower/
├── 0.0.1/tower
├── 0.0.2/tower
└── current -> 0.0.2      (what `tower` on that host's PATH should be)
```

- The home's stream and attaches run `<root>/<version>/tower` by its exact
  path, never `current`, so a host shared by two homes of different
  versions runs each home's own build and a half-done install never runs.
- `current` is swapped (a new symlink renamed over the old) after each
  install, so the user's `tower` there is the newest one installed; a
  dashboard opened on that host reaches the towerd the home started.
- `tower = "<path>"` on a host in `hosts.toml` pins a binary the user
  manages: no check, no install.
- `TOWER_INSTALL_DIR` overrides `<root>` (tests, and the real-host checks,
  which install under `~/.cache/tower-test/harness/`).

## The check costs nothing when the build is there

The stream's remote command execs the versioned path:

```
exec "$HOME"/.local/share/tower/0.0.2/tower towerd --stdio --tmux …
```

A host that has the build streams at once, as before. One that lacks it
fails with the shell's 127; for an unpinned host the link then installs
and reconnects at once, instead of marking the host `failed: tower is not
installed`. So the only cost of install on connect is paid once per host
and version.

## Installing

1. **Platform**: `ssh host -- uname -s -m` over the master → GOOS
   (`Darwin` → darwin, `Linux` → linux) and GOARCH (`x86_64` → amd64,
   `aarch64`/`arm64` → arm64). Anything else fails: `cannot install tower
   on <host>: unsupported platform <uname>`.
2. **The build**: this binary (`os.Executable`) when the host's platform is
   the home's; otherwise `tower_<version>_<os>_<arch>.tar.gz` from the dist
   cache, `~/.cache/tower/dist/<version>/` (`TOWER_DIST_DIR`). A missing
   archive is downloaded once from the GitHub release `v<version>` with
   its `checksums.txt` and verified before it is kept. A development build
   has no release: `mise run dist` fills the cache, and the failure says so
   (`no tower <version> build for linux/arm64: run mise run dist`).
3. **Upload**: one ssh session with the binary on stdin:
   `mkdir -p <root>/<v> && cat > <root>/<v>/.tower.<rand> && chmod 755 … &&
   mv -f … <root>/<v>/tower && ln -sfn <v> <root>/.current.<rand> && mv -f
   <root>/.current.<rand> <root>/current`. Renames make it atomic: two
   homes installing at once leave one whole binary.
4. **Reconnect** at once. The new bridge finds the older towerd running
   and replaces it (towerd's upgrade path), which keeps its registrations.

The link shows `installing tower <v>…` while it uploads. A failed install
is a `failed` host with the reason; it is retried at the backoff cap.

## Status and CLI

- The link's status carries `installed <v>` as a warning line after an
  install, and the failure reason otherwise.
- `tower host add` runs the same check: a missing build is installed as
  the last step of the checks.

## Scenarios

| ID | What must hold |
| --- | --- |
| I01 | A host without tower: the first connect installs this build under `TOWER_INSTALL_DIR`, swaps `current`, and the host is up; the second connect does not install again |
| I02 | The home upgraded: the new version is installed beside the old, `current` swapped, the older towerd replaced by the new bridge, a loop's client there kept |
| I03 | A pinned `tower =` host is never installed to; a missing pinned binary is `failed: tower is not installed` |
| I04 | Another platform: the build comes from the dist cache; missing there, it is downloaded from the release server with its checksum verified; a bad checksum is refused; a dev build with no cache fails with the `mise run dist` fix |
| I05 | Two homes installing on one host at once leave one whole binary and a valid `current` |
| R03 | `pc` and `831`: install into `~/.cache/tower-test/harness/install` over real ssh, stream, and clean up |
