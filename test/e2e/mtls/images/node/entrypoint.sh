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

# The run's PKI (pki service, volume at /e2e/pki), when mounted:
#   - the test root joins the system trust store, so this node's agent accepts
#     the edges' TLS (a home node dials wss://tunnel.odac.run);
#   - the server certificate becomes the node's fallback certificate, which the
#     proxy serves for every domain without one of its own (ACME has no route
#     out). SSL keeps a fallback whose expiry is ahead and whose files exist.
PKI=/e2e/pki
if [ -f "$PKI/done" ]; then
	cp "$PKI/root.pem" /usr/local/share/ca-certificates/mtls-e2e-root.crt
	update-ca-certificates >/dev/null 2>&1 || true
	if [ ! -f "$ODAC_BASE/config/ssl.json" ]; then
		mkdir -p "$ODAC_BASE/cert/ssl"
		chmod 700 "$ODAC_BASE/cert/ssl"
		cp "$PKI/server.pem" "$ODAC_BASE/cert/ssl/e2e.crt"
		cp "$PKI/server.key" "$ODAC_BASE/cert/ssl/e2e.key"
		chmod 600 "$ODAC_BASE/cert/ssl/e2e.key"
		expiry=$((($(date +%s) + 20 * 86400) * 1000))
		printf '{"ssl":{"cert":"%s","expiry":%s,"key":"%s"}}\n' \
			"$ODAC_BASE/cert/ssl/e2e.crt" "$expiry" "$ODAC_BASE/cert/ssl/e2e.key" >"$ODAC_BASE/config/ssl.json"
	fi
fi

# A home node's tunnel, the way the Cloud's proxy.tunnel command leaves it in
# config.tunnels. The token comes from the environment and only lands in the
# agent's own config, as it does in production.
if [ -n "${E2E_TUNNEL_TOKEN:-}" ] && [ -n "${E2E_TUNNEL_DOMAIN:-}" ] && [ ! -f "$ODAC_BASE/config/proxy.json" ]; then
	umask 077
	printf '{"tunnels":{"%s":{"container":"%s","token":"%s"}}}\n' \
		"$E2E_TUNNEL_DOMAIN" "${E2E_TUNNEL_CONTAINER:-home}" "$E2E_TUNNEL_TOKEN" >"$ODAC_BASE/config/proxy.json"
	umask 022
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
unset E2E_TUNNEL_TOKEN E2E_TUNNEL_DOMAIN E2E_TUNNEL_CONTAINER
exec /app/bin/odac-watchdog
