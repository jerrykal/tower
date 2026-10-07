#!/usr/bin/env bash
# Runs one shard of the scenario suite in CI, as `mise run scenarios` does:
# SHARD is lc or rest, HOSTS fake, container or sshd. The run's
# directories are kept for the workflow's upload; each scenario that
# failed is run again alone, for a note, and the shard's own exit status
# is the step's. LS08's shortfalls reported (TT_LS08_REPORT) become
# warnings.
set -uo pipefail
case $HOSTS in container | sshd) export TOWER_HOSTS=$HOSTS ;; esac
if [ "$SHARD" = lc ]; then sel=(-run '^TestLC'); else sel=(-skip '^TestLC'); fi
base=/tmp/tsc-ci
mkdir -p "$base/tmux" "$base/tmp"
export TMUX_TMPDIR=$base/tmux TMPDIR=$base/tmp TOWER_SCENARIOS=1
unset TMUX TMUX_PANE
start=$(date +%s)
TOWER_TEST_DIR=$base/d mise exec -- go test -count=1 -timeout 40m ./test/scenario/ "${sel[@]}" -v >scenarios.log 2>&1
code=$?
echo "exit $code in $(($(date +%s) - start))s"
grep -E '^--- (PASS|FAIL|SKIP)' scenarios.log | awk '{print $2}' | sort | uniq -c
grep -E '^(--- FAIL|FAIL|ok)' scenarios.log
echo "::group::full log"
cat scenarios.log
echo "::endgroup::"
for t in $(grep -E '^--- FAIL: Test' scenarios.log | awk '{print $3}' | sort -u); do
	TOWER_TEST_DIR=$base/r mise exec -- go test -count=1 -timeout 10m ./test/scenario/ -run "^$t\$" -v >"rerun-$t.log" 2>&1
	alone=$(grep -E "^--- (PASS|FAIL|SKIP): $t " "rerun-$t.log" | awk '{sub(":", "", $2); print $2}')
	echo "::warning title=$t::failed in the shard; alone: ${alone:-no result}"
	echo "::group::$t alone"
	cat "rerun-$t.log"
	echo "::endgroup::"
done
if [ -s "${TT_LS08_REPORT:-}" ]; then
	while IFS= read -r l; do echo "::warning title=LS08, reported::$l"; done <"$TT_LS08_REPORT"
fi
echo "left behind:"
ps -axo pid,ppid,command | grep -E 'tsc-|sshd -D -e|tmux -L tt-' | grep -v grep || echo "no process"
if [ "$HOSTS" = container ]; then docker ps -a --filter label=tower-test --format '{{.Names}}'; fi
if [ "$HOSTS" = sshd ]; then sudo -n pfctl -a com.apple -s Anchors; sudo -n dnctl show; fi
tar czf worlds.tgz -C "$base" --exclude=d/bin --exclude=r/bin d r 2>/dev/null
exit $code
