# shellcheck shell=bash
# Log collection and the leak check (MTLS_PLAN.md Faz 10 "Log:" row, §7 first
# item). Sourced by run.sh.
#
# Two kinds of needle:
#   - shapes: PEM armour, a CSR/caPem field in a JSON dump, env dumps of the
#     two app secrets. These catch a leak of anything, including material this
#     run did not make.
#   - values: the node's root API key, each app's ODAC_CLIENT_CERT_KEY, and
#     one body line of every CSR, certificate and private key the scenarios
#     made. These catch a leak that dropped the armour.
# Values are read into this shell's memory and handed to grep on a pipe; they
# are never printed or written to disk, and the report names files and counts.

# Shapes, as label=ERE. Labels are what gets printed, so the report itself
# never contains a string that would match.
LEAK_SHAPES=(
	"pem-private-key=-----BEGIN [A-Z ]*PRIVATE KEY-----"
	"pem-csr=-----BEGIN (NEW )?CERTIFICATE REQUEST-----"
	"pem-certificate=-----BEGIN CERTIFICATE-----"
	"json-csr-field=\"csr\"[[:space:]]*:"
	"json-capem-field=\"caPem\"[[:space:]]*:"
	"env-client-cert-key=ODAC_CLIENT_CERT_KEY="
	"env-api-key=ODAC_API_KEY="
	"tunnel-secret=ODAC_TUNNEL_SECRET="
)

running_nodes() {
	dc ps --services --status running 2>/dev/null | grep -E '^(edge-|home$)' || true
}

# collect_logs DIR: everything a human or a log shipper would see.
collect_logs() {
	local dir=$1 node c
	mkdir -p "$dir"
	chmod 700 "$dir"
	dc logs --no-color --timestamps >"$dir/compose.log" 2>&1 || true
	for node in $(running_nodes); do
		mkdir -p "$dir/$node/odac-logs"
		dc exec -T "$node" sh -c 'tar -C /app/.odac -cf - logs 2>/dev/null' | tar -x -C "$dir/$node/odac-logs" 2>/dev/null || true
		dc exec -T "$node" cat /var/log/dockerd.log >"$dir/$node/dockerd.log" 2>/dev/null || true
		for c in $(dc exec -T "$node" docker ps -a --format '{{.Names}}' 2>/dev/null); do
			dc exec -T "$node" docker logs "$c" >"$dir/$node/container-$c.log" 2>&1 || true
		done
	done
}

# leak_values: secret values, one per line, on stdout (consumed by a pipe).
leak_values() {
	local node c
	for node in $(running_nodes); do
		dc exec -T "$node" sh -c "sed -nE 's/.*\"auth\"[[:space:]]*:[[:space:]]*\"([^\"]+)\".*/\\1/p' /app/.odac/config/api.json" 2>/dev/null || true
		for c in $(dc exec -T "$node" docker ps -a --format '{{.Names}}' 2>/dev/null); do
			dc exec -T "$node" docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$c" 2>/dev/null |
				sed -nE 's/^(ODAC_CLIENT_CERT_KEY|ODAC_API_KEY)=(.+)$/\2/p' || true
		done
	done
	# This run's own secrets, and the key material each suite made.
	printf '%s\n' "${E2E_TUNNEL_SECRET:-}" "${E2E_TUNNEL_TOKEN:-}"
	local fn
	for fn in $(declare -F | awk '$3 ~ /^leak_needles_/ {print $3}'); do
		"$fn"
	done
	# Short values would match by accident; real secrets here are >= 32 chars.
	return 0
}

# leak_check DIR RUNLOG: grep every collected log plus this run's own output.
leak_check() {
	local dir=$1 runlog=$2 name=log.leak-grep hits=0 entry label re n report="" f
	local -a files=()
	while IFS= read -r f; do files+=("$f"); done < <(find "$dir" -type f 2>/dev/null)
	[ -f "$runlog" ] && files+=("$runlog")
	[ "${#files[@]}" -gt 0 ] || { fail "$name" "no logs collected"; return; }

	# count_hits GREP_ARGS...: total matching lines over all files.
	# grep exits 1 on "no match", which pipefail would turn into a dead run.
	count_hits() { { grep -Ih -c "$@" "${files[@]}" 2>/dev/null || true; } | awk '{s+=$1} END {print s+0}'; }
	# hit_files GREP_ARGS...: basenames of the files that match.
	hit_files() { { grep -Il "$@" "${files[@]}" 2>/dev/null || true; } | xargs -n1 basename 2>/dev/null | sort -u | paste -sd, -; }

	for entry in "${LEAK_SHAPES[@]}"; do
		label=${entry%%=*}
		re=${entry#*=}
		n=$(count_hits -E -- "$re")
		if [ "$n" -gt 0 ]; then
			hits=$((hits + n))
			report+=" $label=$n[$(hit_files -E -- "$re")]"
		fi
	done

	# Values stay in this shell's memory and reach grep through a pipe.
	local values nvalues=0
	values=$(leak_values | awk 'length($0) >= 32' | sort -u)
	[ -n "$values" ] && nvalues=$(printf '%s\n' "$values" | wc -l | tr -d ' ')
	if [ "$nvalues" -gt 0 ]; then
		n=$(count_hits -F -f <(printf '%s\n' "$values"))
		if [ "$n" -gt 0 ]; then
			hits=$((hits + n))
			report+=" secret-value=$n[$(hit_files -F -f <(printf '%s\n' "$values"))]"
		fi
	fi
	values=""

	if [ "$hits" = 0 ]; then
		pass "$name" "0 hits: ${#LEAK_SHAPES[@]} shapes + $nvalues secret/PEM values over ${#files[@]} log files"
	else
		fail "$name" "$hits hits:$report (logs kept in $dir)"
	fi
}
