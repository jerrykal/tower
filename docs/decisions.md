# Decisions

Design decisions taken while building tower, one line each: what, why, and
the alternatives. Newest last.

| # | Decision | Why | Alternatives |
| --- | --- | --- | --- |
| 1 | Packages `config`, `tmux`, `stream` and `client` beside `proto`, `towerd`, `transport`, `relay`, `loop` and `ui` | each is a boundary with its own tests (the control client against a real tmux, the stream against pipes); `towerd`, `loop` and `ui` stay about behaviour | fold them into `towerd` and `transport`, which would mix the daemon with its parts |
| 2 | The dashboard is tower's own Bubble Tea UI from step 2 on; no fzf | the step-4 dashboard is a Bubble Tea column UI anyway; a native list makes live updates, a kept cursor and kill-without-waiting plain model updates instead of fzf reload round trips, helper processes and hide files, and drops fzf as a requirement on every host | an fzf picker in step 2 replaced in step 4 (two front ends, the first thrown away) |
| 3 | The fake ssh is its own test binary (`test/scenario/fakessh`), chosen with `TOWER_SSH` | test code stays out of the shipped binary | a hidden `tower _fakessh` subcommand |
| 4 | towerd's local socket serves one call per connection: a request line, a reply line | a call is its own unit of cancellation (the caller closing the connection cancels a long call); a unix connect costs microseconds | multiplexed calls on a persistent connection |
| 5 | `TOWER_HOME` puts config in `$TOWER_HOME/config` and state in `$TOWER_HOME/state` | tests and demos keep everything under one directory | separate overrides per directory |
