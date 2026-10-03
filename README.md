# tower

tower brings the tmux servers on several machines together: switch
between, manage and watch sessions across hosts as if they were on one tmux
server.

## Install

With mise (the repository is private, so mise needs a GitHub token; with
`github.credential_command = "gh auth token"` it uses gh's):

```fish
mise use -g github:jerrykal/tower
```

## Develop

```fish
mise run build    # .dev/bin/tower, stamped with the version
mise run test
mise run dev      # this machine's mise runs .dev/bin/tower from now on
mise run dev:off  # back to the released tower
mise run dist     # release archives for darwin and linux, amd64 and arm64
```

`mise run dev` links `.dev` as the version `dev` of `github:jerrykal/tower`
and pins it in `~/.config/mise/config.local.toml`, which is per machine.
After that `mise run build` updates the tower on your PATH in place.

Versions come from git tags: a build at a clean `v*` tag is that version;
anything else is the last tag with `-dev+<commit>`.
