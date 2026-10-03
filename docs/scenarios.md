# Scenarios

The acceptance suite (`test/scenario`, run with `mise run scenarios`). A
package is done when its scenarios pass. Each row says what must hold;
the test holds the setup and the exact thresholds. Status: `pass`, `fail`
(with the entry in [progress.md](progress.md)), or `todo`.

Timings in the suite are shortened (keepalive 1s, the home gives up a
silent stream after 2s, backoff 200ms → 3s, stable period 3s) except where
a scenario measures what a user would see.

## Core edge cases

| ID | What must hold | Package | Status |
| --- | --- | --- | --- |
| S00 | Happy path: the picker attaches to B; the dashboard hands off B → A; a local switch on A; previous is A:apple | towerd, loop | pass |
| S01 | B is its own home while A's home includes B: one towerd on B plays both roles (one `_tower`, one control client); each client on B is tagged with its home and sees that home's view; A's hand-off relayed through B leaves B's own loop alone | towerd | todo |
| S02 | Two homes for one host, one crashes, one exits: one stream per home id; the crashed home dropped at once; its `_tower` gone within 0.5s and its server exits with its last session; B's towerd then idles out, no `_tower` left | towerd | pass |
| S03 | Two aliases of one server and another user on the machine: one linked, the other `dup: same towerd as …`; one control client and one stream on B | towerd | pass |
| S04 | A server restarts and reuses `$0`: prepare refuses ("restarted since it was listed"); an attach in flight exits 43; no client on the impostor | towerd, loop | pass |
| S05 | Three `-L` servers on one machine: one towerd each (three ids, three sockets in one run dir); hand-off between two | towerd | pass |
| S06 | Exit 42 from elsewhere: a bare exit 42, a stale dashboard (its switch refused before anything detaches), a planted switch for another generation, ssh exiting 42: each ignored with its reason | towerd, loop | pass |
| S07 | The dashboard dies after its switch was stored: the switch completes (committed once stored); with `TOWER_EAGER=0` a normal detach discards it and says so; a later exit 42 finds it too old | towerd, loop | pass |
| S08 | Three terminals on one session (two homes); one hands off: only the pressing terminal moves | towerd, loop | pass |
| S09 | Target killed or renamed between listing and ⏎: "selection is gone"; attach by id fails visibly; a renamed session is still reached | towerd, ui | todo |
| S10 | `prefix d` versus hand-off: tower exits like tmux, the session keeps running, the registration is dropped, the next `tower` reattaches | loop | pass |
| S11 | `tower` inside tmux is the dashboard, never a nested attach; dashboards in clients no loop owns (on the home and on a remote) see every host and switch locally but cannot hand off | ui, towerd | todo |
| S12 | base-index 0 and 1: windows reached by id | loop | pass |
| S13 | Sleep (every connection half-open), then back: detected within seconds; back fast; B replaces the stale stream (never two); B's control client unaffected; the loop is restored | towerd, loop | pass |
| S14 | A tunnel flapping 0.6s/0.6s for 12s: few ssh calls (backoff), up once steady, the local link untouched | towerd | pass |
| S15 | ssh would prompt (host key, password, locked key, unresolvable, Tailscale check, timeout): each down with its reason and fix, fast; every call `BatchMode=yes`; `tower host add` reports the same | transport, towerd | pass |
| S16 | 104-byte socket paths: the run dir falls back to a short one; one control path value in every call; a stale control socket removed, a busy one kept; no `ssh -O check` | config, transport | pass |
| S17 | No server: towerd polls and never starts one; `^n` starts one, waiting for a slow config with "starting tmux on N…"; `_tower` never keeps a server alive; towerd outlives its server | towerd, ui | pass (towerd part) |
| S18 | No binary (`tower is not installed on M`), protocol 0 only (`incompatible protocol … 0-0 … 1-1`), newer peers (1–2) and one sending unknown messages: the right outcome each; hand-off to the newer peer works | towerd | pass |
| S19 | 16 hostile names: created, relayed B → home → C, renamed, picked, handed off to, all exact; no name on any ssh command line | towerd, ui | todo |
| S20 | 300 windows; state and view padded to 2 MB: a new session on B reaches the view held on C fast; C's dashboard reads its rows fast | towerd, stream | pass |
| S21 | A remote's clock an hour ahead: the offset measured; ages right; a 1.5s-deadline request runs there | stream, towerd | pass |
| S22 | Control-mode side effects: `session_last_attached` untouched; attached 0; window sizes unaffected; little traffic while a pane prints megabytes; 400 concurrent commands matched; `_tower` ends with the last session | tmux, towerd | pass |
| S23 | Kill, rename, new on C from B's dashboard: fast; a duplicate id runs once; C down: an error fast, never run later; home frozen: a timeout, never run after thawing; home dead: refused fast | towerd | pass |
| S24 | Previous and current across hosts, a stream reset and a home crash: `-` ⏎ goes back; kept through the reset; relearned by the restarted home | towerd, loop | pass |
| S25 | Everyday habits: closing the last pane with another session free (tmux moves, tower follows) and without (the previous session on another host, with a note); nothing left anywhere: exit; `tower last` across hosts; `prefix d` returns fast | loop, towerd | pass |
| S26 | Hand-offs both ways without a flash: the frame held while the old client leaves and the new one enters, released after, nothing printed between, also when the dashboard cannot write its client's tty | loop, ui | pass |
| S27 | `prefix L` six times, 120–240ms apart, between two hosts: every client change held, released at the end | loop | pass |
| D01 | `tower dash` outside tmux: a second terminal starts at the picker and picks its own target while the first stays; `Esc` attaches to the last target, or exits with none | loop, ui | pass |

## Extras

| ID | What must hold | Package | Status |
| --- | --- | --- | --- |
| E01 | A second towerd for one server is refused | towerd | pass |
| E02 | A home with no loops idles out, then its remote; nothing left in tmux | towerd | pass |
| E03 | The last session ends while the home's stream is half-open: the server exits at once | towerd | pass |
| E04 | `tower host add / off / on / rm`: checks in order (ssh, tmux, OS, tower); the stream closed on off and rm | towerd | pass |

## towerd

| ID | What must hold | Package | Status |
| --- | --- | --- | --- |
| V01 | 8 concurrent first calls start one towerd; SIGKILL leaves a stale socket the next call replaces; stop leaves nothing | client, towerd | pass |
| V02 | Upgrading the home with a loop attached: replaced fast; the old loop re-registers by heartbeat; an older binary never downgrades it; hand-off from an old dashboard binary works | client, towerd | pass |
| V03 | Upgrading a remote with a loop's client attached there: the next bridge replaces its towerd, which restores the client from disk; hand-off works | towerd | pass |
| V04 | Two machines sharing one home directory: two towerds, separate run and state dirs; hand-off between them | config, towerd | pass (towerd part) |
| V05 | A remote whose own `hosts.toml` lists another host stays remote-only while only a bridge started it; an attach loop there makes it a home | towerd | pass |
| V06 | A dashboard on a remote: its rows fast; a session on C shows on B fast; a preview of C's pane from B fast | towerd, ui | pass |
| V07 | The home dies while a loop's client is on a remote: B keeps the last view, marked not connected; ⏎ to A refused before detaching; a local switch works; the restarted home relearns the loop and hand-off works | towerd | pass |
| V08 | Keys: towerd takes `M-o` and `prefix L` where free, leaves the user's `L`; in a plain terminal `prefix L` is tmux's own; in a tower terminal `M-o` opens the dashboard and `prefix L` goes back across hosts; stopping towerd puts the keys back | towerd | pass |

## Slow, stalled and wedged hosts

| ID | What must hold | Package | Status |
| --- | --- | --- | --- |
| LH01 | Baseline round trips with a slow host and a stalled host in the list | towerd | pass |
| LH02 | One stalled host: a change elsewhere still reaches every dashboard fast | towerd, stream | pass |
| LH03 | A switch to a stalled host is refused at once once marked; one that stalls just before is given up, back where it was with a note; recovery | towerd, loop | pass |
| LH04 | Two homes on one remote, one home's link stalled: the other home's view keeps flowing | towerd, stream | pass |
| LH05 | A wake with one wedged master: every other host back fast | towerd | pass |
| LH06 | A towerd wedged on a healthy link with a loop's client attached there: stalled, the stream alone given up, a new towerd started, the client never cut | towerd, client | pass |

## Connections and switches

| ID | What must hold | Package | Status |
| --- | --- | --- | --- |
| LC01 | Cold start, attach, wake, upgrade: the host never shows up empty; a reattach after a wake is fast | towerd, loop | pass |
| LC02 | A clean drop reconnects within 200ms of a stable link | towerd | pass |
| LC03 | A link half-open after a network change: the client back in seconds, faster with the interface watcher | towerd | pass |
| LC04 | A 20s blackout: the loop's reattach does not sleep through the link's return | loop, towerd | pass |
| LC05 | A slow but live link (200–800ms each way, 30s) is never dropped | stream, towerd | pass |
| LC06 | A switch to a host half-open 1s ago lands in seconds | towerd, loop | pass |
| LC07 | Spawn cost: tower runs tmux past version-manager shims, and passes the machine key down | tmux, config | todo |
| LC08 | A switch's critical path at 0/50/150/400ms round trips: the loop ends the old client itself; with standbys laptop → remote 0.5r, remote → remote 1r | loop, towerd | pass |

## Freshness and dashboards

| ID | What must hold | Package | Status |
| --- | --- | --- | --- |
| LV01 | A change on one remote reaches another's view in about one round trip; a bell in one round trip (alert hooks) | towerd | pass (towerd part) |
| LV02 | A burst of 40 switches and 40 new windows costs the home about 17 states | towerd | pass |
| LV04 | An open dashboard follows the view within a round trip plus its redraw | ui, towerd | todo |
| LD01 | Dashboard requests at 150 and 400ms: rows right after every answer; a kill's row gone before the answer; a preview's windows before the capture | ui, towerd | todo |
| LD02 | Deadlines at 400ms round trips: a request that failed never ran | towerd, stream | pass |
| LD03 | `^x` three times fast kills three sessions | ui | todo |

## Standbys and the relay

| ID | What must hold | Package | Status |
| --- | --- | --- | --- |
| LS01 | Hand-offs through standbys; the host left gets a new one; resize reaches the client; standbys are no clients; nothing outlives the loop | loop | pass |
| LS02 | A stuck standby is given up after its wait and a new session lands, the frame held throughout | loop | pass |
| LS03 | Wake, network change, towerd killed, stall: each replaces the standby; one made for an earlier link is never used | loop, towerd | pass |
| LS04 | `TOWER_STANDBY=0`, `standby = false`, a remote upgrade, a reload: standbys follow | loop, towerd | pass |
| LS05 | Job control the same through a standby, a new relayed session and ssh given the terminal | loop, relay | pass |
| LS06 | The relay is byte-exact both ways; frame writes land between sequences | relay | pass |
| LS07 | The relay adds microseconds to a keystroke's echo | relay | pass |
| LS08 | The relay under load: throughput, keys into a flood, backpressure, flat memory | relay | pass |
| LS09 | Every remote attach relayed: exits 255, 43, 42, hand-offs, a stall, `prefix d` the same as with ssh given the terminal | loop, relay | pass |

LS06 and LS07 are tests of `internal/relay` (`go test ./internal/relay`):
they need no host. LS08 is there too against a command given the terminal
(`mise run test:full` for its 200 MiB runs), and in this suite through the
fake ssh to a host with a pty, relayed against the fake ssh given the
terminal (`ls08_test.go`).

## Real hosts

Run by hand, never by an agent: `TOWER_REAL=pc,831 mise run scenarios -run R0`.

| ID | What must hold | Status |
| --- | --- | --- |
| R01 | laptop → `pc` → `831` over real ssh: bridge start, an unresolvable host, control paths, attach, hand-off, a kill relayed, a remote dashboard, a wake reset; teardown leaves nothing | todo |
| R02 | A hand-off from `pc` to the laptop and back, recorded at the terminal: held across the teardown and the new attach; back through `pc`'s standby | todo |
