#!/bin/sh
# One ODAC node: a private Docker engine (dind) plus the agent built from
# source, running directly on that engine's host exactly as the production
# image runs on a real host (HOME=/app, data in /app/.odac, watchdog in front).
set -eu

ODAC_BASE=/app/.odac

# The node is privileged and shares the Docker Desktop VM kernel with the
# developer's own containers. Two agent features act on that kernel rather
# than on this container, so they are switched off before the server first
# reads its config:
#   - the elastic swap manager would mkswap/swapon a file here, which adds
#     swap to the whole VM and can outlive `compose down`;
#   - persist would rewrite /etc/fstab (harmless here, but pointless).
# BBR/qdisc tuning in odac-proxy needs modprobe with /lib/modules, which is
# deliberately not mounted, so it degrades to a log line.
mkdir -p "$ODAC_BASE/config"
if [ ! -f "$ODAC_BASE/config/system.json" ]; then
	cat >"$ODAC_BASE/config/system.json" <<'JSON'
{
    "swap": {
        "allowShrink": true,
        "autoManage": false,
        "maxDiskPct": 25,
        "maxIncrements": 8,
        "persist": false
    }
}
JSON
fi

# Engine on the unix socket only. The stock dind default also listens on
# tcp://0.0.0.0:2375 without TLS, which would hand the engine to anything on
# the test network.
dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock >/var/log/dockerd.log 2>&1 &

i=0
until docker info >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -gt 120 ]; then
		echo "mtls-e2e-node: dockerd did not come up" >&2
		tail -n 50 /var/log/dockerd.log >&2 || true
		exit 1
	fi
	sleep 0.5
done
echo "mtls-e2e-node: dockerd ready"

cd /app
exec /app/bin/odac-watchdog
