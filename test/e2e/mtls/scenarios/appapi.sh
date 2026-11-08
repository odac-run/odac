# shellcheck shell=bash
# App token scenarios (MTLS_PLAN.md A2, A3, A5): an app granted the
# domain.mtls.cert.* actions reaches its own records and nothing else, and
# domain.mtls.set / cert.scope stay out of reach whatever the grant says.
#
# Runs on edge-a after the custom suite, whose record mtls.test (app origin,
# CA already created) it reuses. A second app, other, gets other.test and a
# full grant (--all), which still must not cover set or scope.
# Calls go through `origin api` inside the app's container: the real socket,
# the real ODAC_API_KEY, the summary printed without any certificate.

A_NODE=edge-a
A_READY=0
A_DAVE=""     # fingerprint origin issued through its own token
A_ERIN=""     # fingerprint on other's CA, issued by the CLI

# api_field JSON FILTER
api_field() { printf '%s' "$1" | jqc "$2"; }

appapi_setup() {
	local out
	[ "${C_READY:-0}" = 1 ] || { skip appapi.setup "custom suite setup did not run"; return 1; }
	out=$(odac "$A_NODE" app create mtls-other) || { fail appapi.setup "app create other: $(cli_safe "$out")"; return 1; }
	out=$(odac "$A_NODE" domain add other.test other) || { fail appapi.setup "domain add other.test: $(cli_safe "$out")"; return 1; }
	out=$(odac "$A_NODE" domain mtls set -d other.test --mode optional) || { fail appapi.setup "set other.test: $(cli_safe "$out")"; return 1; }
	make_csr erin
	copy_file client /work/erin.csr "$A_NODE" /tmp/e2e/erin.csr
	out=$(odac "$A_NODE" domain mtls issue -d other.test --name erin --hosts other.test --csr /tmp/e2e/erin.csr --out /tmp/e2e/erin.crt) ||
		{ fail appapi.setup "issue erin: $(cli_safe "$out")"; return 1; }
	A_ERIN=$(fp_of "$out")
	A_READY=1
	pass appapi.setup "app other + other.test (optional, own CA), erin=${A_ERIN:0:16} on other's CA"
}

# A3 at grant time: set and scope cannot be granted at all.
appapi_grant_refused() {
	local name=appapi.grant.refused out1 out2 rc1=0 rc2=0
	[ "$A_READY" = 1 ] || { skip "$name" "setup failed"; return; }
	out1=$(odac "$A_NODE" app api origin --allow domain.mtls.set) || rc1=$?
	out2=$(odac "$A_NODE" app api origin --allow domain.mtls.cert.scope) || rc2=$?
	if [ "$rc1" != 0 ] && [ "$rc2" != 0 ]; then
		pass "$name" "--allow domain.mtls.set: $(cli_safe "$out1" | cut -c1-90) | --allow cert.scope refused too"
	else
		fail "$name" "set grant rc=$rc1 ($(cli_safe "$out1" | cut -c1-80)) scope grant rc=$rc2"
	fi
}

appapi_grant() {
	local name=appapi.grant out
	[ "$A_READY" = 1 ] || { skip "$name" "setup failed"; return 1; }
	out=$(odac "$A_NODE" app api origin --allow domain.mtls.cert.issue,domain.mtls.cert.revoke,domain.mtls.cert.list,domain.mtls.cert.import) ||
		{ fail "$name" "grant origin: $(cli_safe "$out")"; return 1; }
	out=$(echo yes | odac "$A_NODE" app api other --all) || { fail "$name" "grant other --all: $(cli_safe "$out")"; return 1; }
	odac "$A_NODE" app restart origin >/dev/null || { fail "$name" "restart origin"; return 1; }
	odac "$A_NODE" app restart other >/dev/null || { fail "$name" "restart other"; return 1; }
	if ! wait_until 60 appapi_has_grant origin || ! wait_until 60 appapi_has_grant other; then
		fail "$name" "the API key never reached the containers"
		return 1
	fi
	pass "$name" "origin: issue,revoke,list,import; other: --all; both restarted and hold ODAC_API_KEY"
}

appapi_has_grant() {
	local out
	out=$(app_api "$A_NODE" "$1" domain.mtls.cert.list </dev/null 2>/dev/null) || return 1
	[ "$(api_field "$out" '.code // ""')" != no_grant ]
}

# A2: the app's own host works; another app's host, alone or in a list, does not.
appapi_issue_scope() {
	local name=appapi.issue.own-only own foreign mixed
	[ "$A_READY" = 1 ] || { skip "$name" "setup failed"; return; }
	make_csr dave
	own=$(client_sh "jq -cn --rawfile csr /work/dave.csr '[\"mtls.test\", {csr: \$csr, name: \"dave\", hosts: [\"b.mtls.test\"]}]'" |
		app_api "$A_NODE" origin domain.mtls.cert.issue)
	A_DAVE=$(api_field "$own" '.fingerprint // ""')
	make_csr dave2
	foreign=$(client_sh "jq -cn --rawfile csr /work/dave2.csr '[\"other.test\", {csr: \$csr, name: \"dave2\", hosts: [\"other.test\"]}]'" |
		app_api "$A_NODE" origin domain.mtls.cert.issue)
	mixed=$(client_sh "jq -cn --rawfile csr /work/dave2.csr '[\"mtls.test\", {csr: \$csr, name: \"dave2\", hosts: [\"b.mtls.test\", \"other.test\"]}]'" |
		app_api "$A_NODE" origin domain.mtls.cert.issue)
	if [ "$(api_field "$own" .result)" = true ] && [ -n "$A_DAVE" ] &&
		[ "$(api_field "$foreign" .result)" = false ] && [ "$(api_field "$mixed" .result)" = false ]; then
		pass "$name" "own b.mtls.test: ok fp=${A_DAVE:0:16} | other.test: \"$(api_field "$foreign" .message | cut -c1-50)\" | mixed: \"$(api_field "$mixed" .message | cut -c1-50)\""
	else
		fail "$name" "own=$(api_field "$own" '.result,.message' | tr '\n' ' ') foreign=$(api_field "$foreign" '.result,.message' | tr '\n' ' ') mixed=$(api_field "$mixed" '.result,.message' | tr '\n' ' ')"
	fi
}

# list reaches only the app's own CA.
appapi_list() {
	local name=appapi.list.own-only out has_dave has_erin
	[ -n "$A_DAVE" ] || { skip "$name" "no app-issued certificate"; return; }
	out=$(app_api "$A_NODE" origin domain.mtls.cert.list </dev/null)
	has_dave=$(api_field "$out" "[.fingerprints[]? | select(. == \"$A_DAVE\")] | length")
	has_erin=$(api_field "$out" "[.fingerprints[]? | select(. == \"$A_ERIN\")] | length")
	if [ "$(api_field "$out" .result)" = true ] && [ "$has_dave" = 1 ] && [ "$has_erin" = 0 ]; then
		pass "$name" "$(api_field "$out" '.fingerprints | length') rows, dave listed, erin (other's CA) absent"
	else
		fail "$name" "result=$(api_field "$out" .result) dave=$has_dave erin=$has_erin"
	fi
}

# A3 on the request path: even --all does not reach set or scope.
appapi_set_scope_denied() {
	local name=appapi.set-scope.denied set scope cai
	[ "$A_READY" = 1 ] || { skip "$name" "setup failed"; return; }
	set=$(echo '["other.test", "off", {}]' | app_api "$A_NODE" other domain.mtls.set)
	scope=$(echo "[\"$A_ERIN\", [\"other.test\"]]" | app_api "$A_NODE" other domain.mtls.cert.scope)
	cai=$(echo '["x", "y"]' | app_api "$A_NODE" other domain.mtls.ca.import)
	if [ "$(api_field "$set" .result)" = false ] && [ "$(api_field "$scope" .result)" = false ] && [ "$(api_field "$cai" .result)" = false ]; then
		pass "$name" "other (--all): set \"$(api_field "$set" .message | cut -c1-40)\", scope and ca.import refused too"
	else
		fail "$name" "set=$(api_field "$set" .result) scope=$(api_field "$scope" .result) ca.import=$(api_field "$cai" .result)"
	fi
}

# A2/A5 revoke: unknown and foreign fingerprints are not_found; the app's own
# revokes, and revoking it again still succeeds.
appapi_revoke() {
	local name=appapi.revoke unknown foreign first second
	[ -n "$A_DAVE" ] || { skip "$name" "no app-issued certificate"; return; }
	unknown=$(echo "[\"$(openssl rand -hex 32)\"]" | app_api "$A_NODE" origin domain.mtls.cert.revoke)
	foreign=$(echo "[\"$A_ERIN\"]" | app_api "$A_NODE" origin domain.mtls.cert.revoke)
	first=$(echo "[\"$A_DAVE\"]" | app_api "$A_NODE" origin domain.mtls.cert.revoke)
	second=$(echo "[\"$A_DAVE\"]" | app_api "$A_NODE" origin domain.mtls.cert.revoke)
	if [ "$(api_field "$unknown" .result)" = false ] && [ "$(api_field "$unknown" .code)" = not_found ] &&
		[ "$(api_field "$foreign" .result)" = false ] && [ "$(api_field "$foreign" .code)" = not_found ] &&
		[ "$(api_field "$first" .result)" = true ] && [ "$(api_field "$second" .result)" = true ]; then
		pass "$name" "unknown fp: not_found | erin (other's): not_found | dave: ok, again: ok"
	else
		fail "$name" "unknown=$(api_field "$unknown" '[.result,.code]|@csv') foreign=$(api_field "$foreign" '[.result,.code]|@csv') first=$(api_field "$first" .result) second=$(api_field "$second" .result)"
	fi
}

suite_appapi() {
	log "--- suite: app tokens (A2/A3/A5 on $A_NODE) ---"
	appapi_setup || true
	appapi_grant_refused
	if appapi_grant; then
		appapi_issue_scope
		appapi_list
		appapi_set_scope_denied
		appapi_revoke
	fi
}

leak_needles_appapi() {
	client_sh 'for f in /work/dave.csr /work/dave.key /work/dave2.csr /work/dave2.key /work/erin.csr /work/erin.key; do [ -f "$f" ] && grep -v -- "-----" "$f"; done' 2>/dev/null || true
}
