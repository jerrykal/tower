# Install on connect

How the tower binary reaches every host: the home installs its own
version on a host the first time it connects there with that version, over
the ssh master it already has. Remote hosts never need GitHub, a package
manager or the user's dotfiles. The package is `internal/install`; the
link (`internal/towerd/link.go`) and `tower host add` use it.

## Layout on a host

```
~/.local/share/tower/
├── 0.0.1/Linux-x86_64/tower
├── 0.0.2/Linux-x86_64/tower
└── current -> 0.0.2/Linux-x86_64   (what `tower` on that host's PATH should be)
```

The platform directory is the host's `uname -sm`: hosts of two
architectures sharing one home directory (NFS) each get their own build.

- The home's stream, attaches and standbys run
  `<root>/<version>/<platform>/tower` by its exact path, never `current`, so a host shared by two homes of
  different versions runs each home's own build and a half-done install
  never runs.
- `current` is swapped (a new symlink renamed over the old) after an
  install of a newer version than it points to (`sort -V`), so the user's
  `tower` there is the newest one installed and a home of an older version
  never downgrades it; a dashboard opened on that host reaches the towerd
  the home started.
- `tower = "<path>"` on a host in `hosts.toml` pins a binary the user
  manages: no check, no install.
- **The root** is set by the remote line itself: `r=${TOWER_INSTALL_DIR:-};
  [ -n "$r" ] || r=<the home's root>`, where the home's root is its own
  `TOWER_INSTALL_DIR`, else `~/.local/share/tower` (`~/` written as
  `"$HOME"/`, the rest quoted, so no character in it means anything to the
  shell). So `TOWER_INSTALL_DIR` set on the home moves the root on every
  host (the real-host checks install under `~/.cache/tower-test/harness/`),
  and set in a host's own environment moves it there (the scenario suite
  gives every simulated machine its own root in the world's directory).

## The check costs nothing when the build is there

The stream's remote command runs the versioned path:

```
sh -c 'r=${TOWER_INSTALL_DIR:-}; [ -n "$r" ] || r="$HOME"/.local/share/tower;
       u=$(uname -sm); p="${u% *}-${u#* }"; b="$r"/0.0.2/"$p"/tower;
       exec "$b" towerd --stdio --tmux …'
```

Every remote line goes through `sh -c` (`transport.Sh`): ssh hands it to
the user's login shell, which may be fish or csh, and tower's lines are
POSIX. Quoting never uses a backslash (`'"'"'` for a quote), since fish
reads `\'` as an escape even inside single quotes.

A host that has the build streams at once, as before. One that lacks it
exits with the shell's 127; for an unpinned host the link then installs
and reconnects at once, instead of marking the host `failed: tower is not
installed`. So the only cost of install on connect is paid once per host
and version. A 127 again after a successful install (the build did not
take: a full disk, a noexec root) fails the host with that reason until
it comes up or its entry changes; a failed install is simply tried again
at the backoff cap.

## Installing (`Installer.Install`)

1. **Platform**: `uname -s -m` over the master → GOOS (`Darwin` → darwin,
   `Linux` → linux) and GOARCH (`x86_64` → amd64, `aarch64`/`arm64` →
   arm64). Anything else fails: `cannot install tower on <host>:
   unsupported platform <uname>`.
2. **The build** (`Source.Binary`): this binary (`os.Executable`) when the
   host's platform is the home's; otherwise `tower_<version>_<os>_<arch>.tar.gz`
   from the dist cache, `$TOWER_DIST_DIR/<version>/` (default
   `~/.cache/tower/dist`), checked against its own `<archive>.sha256` (a
   download) or the cache's `checksums.txt` (`mise run dist`) — an archive
   with neither is refused — and its `tower` extracted afresh (a rebuilt development
   archive keeps its name). A missing archive is downloaded from the
   release `v<version>` (`$TOWER_RELEASE_URL`, default
   `https://github.com/jerrykal/tower/releases/download`) with its
   `checksums.txt`, and kept in the cache, with its own `.sha256`, only if
   its sha256 matches (`checksum mismatch …: refused` otherwise); the
   cache's `checksums.txt` is left alone. A development build
   (`-dev` in its version) has no release: `mise run dist` fills the cache,
   and the failure says so (`no tower <version> build for linux/arm64: run
   mise run dist`).
3. **Upload** (`UploadScript`): one ssh session with the binary on stdin
   into `<dir>/.tower.$$`. It goes in place only if every byte arrived
   (the size the home sent) and it says it is `<v>` (`version`): an upload
   cut short, or a binary replaced on the home since towerd started, is
   refused and removed, never installed. Then `mv -f` to `<dir>/tower`,
   and for a newer version `ln -sfn <v>/<platform> "$r/.current.$$"` and
   the link renamed over `current` without following it (`mv -T` on GNU,
   `mv -h` on BSD). Renames make it atomic: two homes installing at once leave one
   whole binary and a valid `current` (I05).
4. **Reconnect** at once. The new bridge finds the older towerd running
   and replaces it (towerd's upgrade path), which keeps its registrations
   (I02).

The link's status is `installing` with the reason `installing tower
<v>…` while it uploads. After an install the link's warning line says
`installed <v>`. A failed install is a `failed` host with the reason,
retried at the backoff cap.

## `tower host add`

The same check: the tower step runs this build's path (or the pinned
`--tower` one); a missing build is installed as the last step (`✓ tower:
installed <v>`), or the step shows why it could not be.

## `mise run dist`

Cross-compiles the four archives and `checksums.txt` into `dist/` (the
release assets) and copies them into the dist cache
(`${TOWER_DIST_DIR:-~/.cache/tower/dist}/<version>/`), so a development
home can install on other platforms.

## Tests

`install_test.go`: platforms, the root word through a real shell (odd
characters, a host's own `TOWER_INSTALL_DIR`), four concurrent uploads
leaving one whole binary and `current` swapped, the source from this
binary, the cache (and a tampered cache refused), a verified download
from an httptest release server (a bad checksum refused and not kept, the
archive kept for the next install), a development build with no cache,
and an install end to end through a local runner.

## Scenarios

| ID | What must hold |
| --- | --- |
| I01 | A host without tower: the first connect installs this build under `TOWER_INSTALL_DIR`, swaps `current`, and the host is up; the second connect does not install again; `tower host add` installs as its last check |
| I02 | The home upgraded: the new version is installed beside the old, `current` swapped, the older towerd replaced by the new bridge, a loop's client there kept |
| I03 | A pinned `tower =` host is never installed to; a missing pinned binary is `failed: tower is not installed` |
| I04 | Another platform: the build comes from the dist cache; missing there, it is downloaded from the release server with its checksum verified; a bad checksum is refused; a dev build with no cache fails with the `mise run dist` fix |
| I05 | Two homes installing on one host at once leave one whole binary and a valid `current` |
| R03 | `alpha` and `bravo`: install into `~/.cache/tower-test/harness/install` over real ssh, stream, and clean up |

The suite simulates another platform with a `uname` shim first on that
host's PATH (`Platform("Linux x86_64")`); the archives it serves hold the
real build, since the simulated host is this machine.
