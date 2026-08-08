#!/usr/bin/env bash
# End-to-end demo: boot vm0 on host-a, wait for the guest to serve traffic,
# then live-migrate it to host-b while fcprobe watches from the client. Prints
# the blackout verdict; exits non-zero if the guest reports memory-integrity
# errors (a correctness failure) — the blackout budget itself is reported by
# fcprobe, which is lenient about the nested-virt jitter floor on laptops.
set -euo pipefail

COMPOSE="docker compose -f $(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/docker-compose.yml"
dc() { $COMPOSE exec -T "$@"; }

# Restart the agents for a clean slate, so the demo is repeatable regardless of
# any VM left over from a previous run. The entrypoint is idempotent about the
# already-configured bridge, so restart re-runs it safely.
echo ">> Resetting host agents"
$COMPOSE restart host-a host-b >/dev/null
sleep 3

echo ">> Booting vm0 on host-a"
dc client curl -fsS -X POST http://host-a:8500/vms \
  -H 'Content-Type: application/json' \
  -d '{"id":"vm0","vcpus":1,"mem_mib":256,"scrib_buf_mb":32,"dirty_mbps":5}' >/dev/null

echo ">> Guest status:"
dc client curl -fsS http://172.30.0.50:7780/status; echo

echo ">> Live-migrating vm0 host-a → host-b (probing at 2kHz)"
dc client /opt/fcmig/fcprobe migrate

echo ">> Guest now served by host-b:"
dc client curl -fsS http://host-b:8500/vms/vm0; echo
