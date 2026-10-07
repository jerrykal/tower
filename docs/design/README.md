# Design notes

One note per package: its responsibilities, types, interfaces and
concurrency model. The behaviour they implement is specified in
[../protocol.md](../protocol.md); [../scenarios.md](../scenarios.md) is the
acceptance suite.

```
cmd/tower            subcommand dispatch
internal/version     the stamped version
internal/proto       wire types: stream messages, local calls, the model
internal/config      paths, identities, hosts.toml, state files
internal/tmux        binary resolution, quoting, one-shot runs, the control client
internal/stream      one JSON-lines peer: queued writer, requests, ping, stall, clock
internal/transport   ssh: options, commands, failure classes, probe, masters, network watch
internal/client      calls to the local towerd, ensure (start, upgrade, replace)
internal/install     install on connect: platform, build source, upload
internal/dirs        git state of directories, zoxide directories, network mounts
internal/towerd      the daemon: watch, registrations, keys, remote and home roles
internal/relay       terminal modes, ptys, the relay, frame writes between sequences
internal/loop        the attach loop, the attach shim, standbys
internal/hosts       the host list's edits, naming rules, ssh aliases and checks
internal/ui          the dashboard
test/scenario        the acceptance harness, its host containers and sshds
```

Dependencies point down the list: `proto` and `config` depend on nothing
of tower's; `towerd` never imports `loop` or `ui`; `loop` and `ui` reach
towerd only through `client`.

| Note | Package |
| --- | --- |
| [proto.md](proto.md) | `proto` |
| [stream.md](stream.md) | `stream` |
| [config.md](config.md) | `config` |
| [tmux.md](tmux.md) | `tmux` |
| [transport.md](transport.md) | `transport` |
| [client.md](client.md) | `client` |
| [install.md](install.md) | `install` |
| [dirs.md](dirs.md) | `dirs` |
| [towerd.md](towerd.md) | `towerd` |
| [relay.md](relay.md) | `relay` |
| [loop.md](loop.md) | `loop` |
| [ui.md](ui.md) | `ui` |
| [harness.md](harness.md) | `test/scenario` |
