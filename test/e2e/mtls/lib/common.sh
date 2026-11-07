# shellcheck shell=bash
# Shared helpers for the mTLS end-to-end harness. Sourced by run.sh.
#
# Conventions:
#   - Every docker call goes through dc(), pinned to the mtls-e2e project.
#   - Nothing here prints a key, CSR, PEM body or secret. Scenario evidence is
#     status codes, verdicts and fingerprints (public hashes) only; the leak
#     check at the end also greps this run's own output.

PROJECT=${PROJECT:-mtls-e2e}

RESULTS=()   # "PASS|FAIL<TAB>name<TAB>evidence"
FAILED=0

dc() {
	docker compose -p "$PROJECT" -f "$HERE/compose.yml" "$@"
}

log()  { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
info() { log "  $*"; }
die()  { log "FATAL: $*"; exit 2; }

pass() {
	RESULTS+=("PASS	$1	$2")
	log "PASS  $1 :: $2"
}

fail() {
	RESULTS+=("FAIL	$1	$2")
	FAILED=$((FAILED + 1))
	log "FAIL  $1 :: $2"
}

# skip records a scenario that could not run because an earlier step it
# depends on failed. It counts as a failure: a skipped check proves nothing.
skip() {
	RESULTS+=("SKIP	$1	$2")
	FAILED=$((FAILED + 1))
	log "SKIP  $1 :: $2"
}

summary() {
	log "==================== RESULTS ===================="
	local row
	# The +-guard keeps bash 3.2 (macOS) from calling an empty array unbound.
	for row in ${RESULTS[@]+"${RESULTS[@]}"}; do
		IFS=$'\t' read -r status name evidence <<<"$row"
		printf '%-5s %-28s %s\n' "$status" "$name" "$evidence"
	done
	log "================================================="
}

# --- container helpers ---

# node_sh NODE SCRIPT: run a shell snippet in a node container.
node_sh() {
	local node=$1
	shift
	dc exec -T "$node" sh -c "$*" </dev/null
}

# client_sh SCRIPT: run a shell snippet in the client container.
# stdin is closed on purpose: a dc exec that is not meant to read it would
# otherwise swallow what a pipeline was feeding the next command.
client_sh() {
	dc exec -T client sh -c "$*" </dev/null
}

# odac NODE ARGS...: the agent CLI inside a node, colour codes stripped.
# Output is captured by callers and only ever echoed after a check that it
# carries no PEM (see cli_safe).
odac() {
	local node=$1
	shift
	dc exec -T "$node" odac "$@" 2>&1 | sed -E $'s/\x1b\\[[0-9;]*m//g'
}

# cli_safe TEXT: print CLI output unless it looks like it carries key material.
cli_safe() {
	if grep -qE 'BEGIN [A-Z ]*(PRIVATE KEY|CERTIFICATE REQUEST|CERTIFICATE)' <<<"$1"; then
		info "(CLI output withheld: it contains PEM)"
	else
		# Drop the banner and blank lines, keep it to one line of evidence.
		grep -vE '^\s*(ODAC)?\s*$' <<<"$1" | tr '\n' ' ' | sed -E 's/ +/ /g'
	fi
}

# copy_file SRC_SERVICE SRC_PATH DST_SERVICE DST_PATH: stream a file between
# two containers without it touching the host disk or the log.
copy_file() {
	dc exec -T "$1" cat "$2" | dc exec -T "$3" sh -c "mkdir -p \"\$(dirname '$4')\" && cat > '$4'"
}

# wait_healthy SERVICE [TIMEOUT_S]
wait_healthy() {
	local svc=$1 timeout=${2:-180} i=0 cid state
	cid=$(dc ps -q "$svc")
	[ -n "$cid" ] || die "$svc is not running"
	while :; do
		state=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || echo gone)
		[ "$state" = healthy ] && return 0
		case $state in exited | dead | gone) die "$svc stopped ($state)" ;; esac
		i=$((i + 2))
		[ "$i" -ge "$timeout" ] && die "$svc not healthy after ${timeout}s (state: $state)"
		sleep 2
	done
}

# node_ip NODE: the node's address on the test network, as the client sees it.
node_ip() {
	client_sh "getent hosts $1 | awk '{print \$1; exit}'"
}

# --- HTTP probes (run in the client) ---

# probe NODE HOST PATH [CERT_BASENAME] [EXTRA_CURL_ARGS]
# Sets: P_CODE (HTTP status, 000 on transport failure), P_EXIT (curl exit
# code), P_BODY_OK (1 when the body is the origin's JSON), the verdict fields
# P_VERIFY, P_SIG, P_REASON, P_FP, P_KEY, and for any other answer P_LOC (the
# redirect target) and P_TEXT (the body's first printable characters).
probe() {
	local node=$1 host=$2 path=$3 cert=${4:-} extra=${5:-} ip certargs="" out
	ip=$(node_ip "$node")
	[ -n "$cert" ] && certargs="--cert /work/$cert.crt --key /work/$cert.key"
	# shellcheck disable=SC2016
	out=$(client_sh '
		rm -f /work/body
		meta=$(curl -sk --max-time 10 --resolve '"$host:443:$ip"' '"$certargs"' '"$extra"' \
			-o /work/body -w "%{http_code} %{redirect_url}" "https://'"$host$path"'") ; rc=$?
		code=${meta%% *}; loc=${meta#* }
		[ -n "$code" ] || code=000
		if jq -e ".app == \"mtls-e2e-origin\"" /work/body >/dev/null 2>&1; then
			jq -r --arg code "$code" --arg rc "$rc" \
				"[\$code, \$rc, \"1\", .verify, (.sig_valid|tostring), .sig_reason, .fingerprint, (.key_present|tostring), \"\", \"\"] | join(\"|\")" /work/body
		else
			text=$(head -c 120 /work/body 2>/dev/null | tr -c "[:print:]" " " | tr "|" "/" | sed "s/  */ /g")
			printf "%s|%s|0||||||%s|%s\n" "$code" "$rc" "$(echo "$loc" | tr "|" "/")" "$text"
		fi
	')
	# "|" rather than a tab: read collapses runs of whitespace separators, which
	# would shift every field after an empty one.
	IFS='|' read -r P_CODE P_EXIT P_BODY_OK P_VERIFY P_SIG P_REASON P_FP P_KEY P_LOC P_TEXT <<<"$out"
	P_VERIFY=${P_VERIFY:-}
	P_EXIT=${P_EXIT:-?}
	P_LOC=${P_LOC:-}
	P_TEXT=${P_TEXT:-}
}

# probe_line: one-line evidence for the last probe.
probe_line() {
	if [ "$P_BODY_OK" = 1 ]; then
		printf 'http=%s verify=%s sig_valid=%s(%s)' "$P_CODE" "${P_VERIFY:-<absent>}" "$P_SIG" "$P_REASON"
	else
		printf 'http=%s curl_exit=%s app_reached=no' "$P_CODE" "$P_EXIT"
		[ -n "$P_LOC" ] && printf ' location=%s' "$P_LOC"
		[ -n "$P_TEXT" ] && printf ' body="%s"' "$(printf '%s' "$P_TEXT" | sed 's/ *$//' | cut -c1-70)"
	fi
	return 0
}

# wait_probe TIMEOUT_S CONDITION NODE HOST PATH [CERT] [EXTRA]: re-probe until
# CONDITION (a shell expression over the P_* variables) holds. Config pushes
# are asynchronous, so a single probe right after a CLI call can race them.
wait_probe() {
	local timeout=$1 cond=$2 i=0
	shift 2
	while :; do
		probe "$@"
		eval "$cond" && return 0
		i=$((i + 1))
		[ "$i" -ge "$timeout" ] && return 1
		sleep 1
	done
}

# --- certificates (keys are made and stay in the client) ---

# make_csr NAME: ECDSA P-256 key + CSR in the client's tmpfs.
make_csr() {
	client_sh "openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
		-keyout /work/$1.key -out /work/$1.csr -subj /CN=$1 >/dev/null 2>&1 && chmod 600 /work/$1.key"
}

# cert_fp SERVICE PATH: SHA-256 of the first certificate's DER in a PEM file,
# the same fingerprint the agent uses.
cert_fp() {
	dc exec -T "$1" sh -c "openssl x509 -in '$2' -outform der 2>/dev/null | sha256sum | cut -d' ' -f1" </dev/null
}

# jqc FILTER: jq in the client, reading stdin, so the host needs no jq.
jqc() {
	dc exec -T client jq -r "$1"
}

# app_container NODE APP: the running container of an app on a node's engine.
app_container() {
	dc exec -T "$1" sh -c "docker ps --filter 'name=^$2' --format '{{.Names}}' | head -n1" </dev/null
}

# app_api NODE APP ACTION: call the agent's local API from inside the app's
# container with the app's own key (`origin api`, see images/origin). The data
# array comes on stdin. Prints the call's summary JSON, never a certificate.
app_api() {
	local c
	c=$(app_container "$1" "$2")
	dc exec -T "$1" docker exec -i "$c" /usr/local/bin/origin api "$3" || true
}

# hub_ctl METHOD PATH: the test hub's control API, from the client. A body, when
# there is one, comes on stdin.
hub_ctl() {
	dc exec -T client curl -s --max-time 40 -X "$1" -H 'Content-Type: application/json' \
		--data-binary @- "http://hub:8080/ctl/$2"
}

# wait_until TIMEOUT_S CMD...: poll a shell condition once a second.
wait_until() {
	local timeout=$1 i=0
	shift
	until "$@"; do
		i=$((i + 1))
		[ "$i" -ge "$timeout" ] && return 1
		sleep 1
	done
}
