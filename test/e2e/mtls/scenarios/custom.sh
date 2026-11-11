# shellcheck shell=bash
# Custom domain scenarios (MTLS_PLAN.md Faz 10, the first six "Custom:" rows).
# Sourced by run.sh; suite_custom is the entry point.
#
# Topology on edge-a: app "origin" (recipe mtls-origin from the test hub) with
# the record mtls.test and its subdomains a.mtls.test and b.mtls.test. The
# record runs `optional`; a.mtls.test is later overridden to `required`.
# Everything is driven through the agent CLI (`odac domain mtls ...`), the
# same local API surface an operator uses.
#
# The scenarios build on each other in this order; a failed prerequisite turns
# the dependants into SKIP (which still fails the run).

C_NODE=edge-a
C_APP=origin
C_RECORD=mtls.test
C_HOST_A=a.mtls.test
C_HOST_B=b.mtls.test
C_FP=""          # fingerprint of the "alice" certificate once issued
C_READY=0        # app + domains are up
C_OPTIONAL=0     # record is optional and the app holds its signing key
C_ISSUED=0       # alice is issued and copied to the client
C_REQUIRED=0     # a.mtls.test is required

fp_of() { sed -nE 's/.*Fingerprint: ([0-9a-f]{64}).*/\1/p' <<<"$1" | head -n1; }

custom_setup() {
	local out d
	if ! out=$(odac "$C_NODE" app create mtls-origin); then
		fail custom.setup "app create refused: $(cli_safe "$out")"
		return 1
	fi
	info "app create: $(cli_safe "$out")"
	for d in "$C_RECORD" "$C_HOST_A" "$C_HOST_B"; do
		if ! out=$(odac "$C_NODE" domain add "$d" "$C_APP"); then
			fail custom.setup "domain add $d refused: $(cli_safe "$out")"
			return 1
		fi
		info "domain add $d: $(cli_safe "$out")"
	done
	if ! wait_probe 60 '[ "$P_BODY_OK" = 1 ]' "$C_NODE" "$C_HOST_A" /e2e/setup; then
		fail custom.setup "origin never answered through the proxy: $(probe_line)"
		return 1
	fi
	if [ -n "$P_VERIFY" ]; then
		fail custom.setup "verify header present before any mode was set: $(probe_line)"
		return 1
	fi
	C_READY=1
	pass custom.setup "app+3 hosts behind proxy, no policy: $(probe_line)"
}

# Optional mode: a visitor without a certificate reaches the app as `none`. The
# request also carries forged verdict headers, which the proxy must replace.
custom_optional_none() {
	local name=custom.optional.none out
	[ "$C_READY" = 1 ] || { skip "$name" "setup failed"; return; }
	if ! out=$(odac "$C_NODE" domain mtls set -d "$C_RECORD" --mode optional); then
		fail "$name" "set optional refused: $(cli_safe "$out")"
		return
	fi
	info "set optional: $(cli_safe "$out")"
	# ODAC_CLIENT_CERT_KEY reaches a container only at start (docs: restart after
	# the first mode), so until the restart every signature is unverifiable.
	if ! out=$(odac "$C_NODE" app restart "$C_APP"); then
		fail "$name" "app restart refused: $(cli_safe "$out")"
		return
	fi
	info "app restart: $(cli_safe "$out")"
	wait_probe 60 '[ "$P_BODY_OK" = 1 ] && [ "$P_KEY" = true ] && [ -n "$P_VERIFY" ]' \
		"$C_NODE" "$C_HOST_A" /e2e/optional "" \
		'-H "X-Odac-Client-Verify: success" -H "X-Odac-Client-Fingerprint: forged"' || true
	if [ "$P_CODE" = 200 ] && [ "$P_VERIFY" = none ] && [ "$P_SIG" = true ] && [ -z "$P_FP" ]; then
		C_OPTIONAL=1
		pass "$name" "no cert, forged headers sent: $(probe_line) key_present=$P_KEY forged_fp_dropped=yes"
	else
		fail "$name" "no cert: $(probe_line) key_present=${P_KEY:-?} fp=${P_FP:-<absent>}"
	fi
}

# A valid certificate is reported as `success`, the identity headers carry its
# fingerprint, and the app verifies the proxy's HMAC over all of them.
custom_cert_success() {
	local name=custom.cert.success out local_fp
	[ "$C_OPTIONAL" = 1 ] || { skip "$name" "optional mode not in place"; return; }
	make_csr alice || { fail "$name" "openssl could not make a CSR"; return; }
	copy_file client /work/alice.csr "$C_NODE" /tmp/e2e/alice.csr
	if ! out=$(odac "$C_NODE" domain mtls issue -d "$C_RECORD" --name alice --hosts "$C_HOST_A" \
		--csr /tmp/e2e/alice.csr --out /tmp/e2e/alice.crt); then
		fail "$name" "issue refused: $(cli_safe "$out")"
		return
	fi
	C_FP=$(fp_of "$out")
	[ -n "$C_FP" ] || { fail "$name" "issue printed no fingerprint: $(cli_safe "$out")"; return; }
	info "issue: $(cli_safe "$out")"
	copy_file "$C_NODE" /tmp/e2e/alice.crt client /work/alice.crt
	local_fp=$(cert_fp client /work/alice.crt)
	C_ISSUED=1
	wait_probe 15 '[ "$P_VERIFY" = success ]' "$C_NODE" "$C_HOST_A" /e2e/cert alice || true
	if [ "$P_CODE" = 200 ] && [ "$P_VERIFY" = success ] && [ "$P_SIG" = true ] &&
		[ "$P_FP" = "$C_FP" ] && [ "$local_fp" = "$C_FP" ]; then
		pass "$name" "$(probe_line) fp=${C_FP:0:16} (header = CLI = client-side DER hash)"
	else
		fail "$name" "$(probe_line) header_fp=${P_FP:0:16} cli_fp=${C_FP:0:16} der_fp=${local_fp:0:16}"
	fi
}

# K4: the same CSR sent again returns the certificate it already produced.
custom_same_csr() {
	local name=custom.issue.same-csr out fp2 file_fp same
	[ "$C_ISSUED" = 1 ] || { skip "$name" "no certificate issued"; return; }
	if ! out=$(odac "$C_NODE" domain mtls issue -d "$C_RECORD" --name alice --hosts "$C_HOST_A" \
		--csr /tmp/e2e/alice.csr --out /tmp/e2e/alice-retry.crt); then
		fail "$name" "retry refused: $(cli_safe "$out")"
		return
	fi
	fp2=$(fp_of "$out")
	file_fp=$(cert_fp "$C_NODE" /tmp/e2e/alice-retry.crt)
	same=no
	node_sh "$C_NODE" "cmp -s /tmp/e2e/alice.crt /tmp/e2e/alice-retry.crt" && same=yes
	if [ -n "$fp2" ] && [ "$fp2" = "$C_FP" ] && [ "$file_fp" = "$C_FP" ] && [ "$same" = yes ]; then
		pass "$name" "retry fp=${fp2:0:16} = first, files byte-identical; CLI said: $(cli_safe "$out" | cut -c1-80)"
	else
		fail "$name" "first=${C_FP:0:16} retry=${fp2:0:16} retry_file=${file_fp:0:16} identical=$same"
	fi
}

# Required mode: without a certificate nothing reaches the app (the handshake
# itself demands one); with alice's certificate the host still opens.
custom_required() {
	local name=custom.required.none out none_line
	[ "$C_ISSUED" = 1 ] || { skip "$name" "no certificate to keep the host open"; return; }
	if ! out=$(odac "$C_NODE" domain mtls set -d "$C_HOST_A" --mode required); then
		fail "$name" "set required refused: $(cli_safe "$out")"
		return
	fi
	info "set required: $(cli_safe "$out")"
	wait_probe 15 '[ "$P_BODY_OK" != 1 ]' "$C_NODE" "$C_HOST_A" /e2e/required || true
	none_line=$(probe_line)
	local none_ok=0
	[ "$P_BODY_OK" != 1 ] && { [ "$P_EXIT" != 0 ] || [ "$P_CODE" = 403 ]; } && none_ok=1
	probe "$C_NODE" "$C_HOST_A" /e2e/required alice
	if [ "$none_ok" = 1 ] && [ "$P_CODE" = 200 ] && [ "$P_VERIFY" = success ] && [ "$P_SIG" = true ]; then
		C_REQUIRED=1
		pass "$name" "no cert: $none_line | alice: $(probe_line)"
	else
		fail "$name" "no cert: $none_line | alice: $(probe_line)"
	fi
}

# Scope: alice opens a.mtls.test only, so on b.mtls.test (optional) she is
# `out_of_scope`. Widening the scope opens b on the next request with the same
# certificate, no reissue.
custom_scope() {
	local name=custom.scope out before
	[ "$C_ISSUED" = 1 ] || { skip "$name" "no certificate issued"; return; }
	# An optional host asks only once some certificate opens it (H6), so a
	# keeper certificate, never presented, makes b ask and alice show hers.
	make_csr keeper-b
	copy_file client /work/keeper-b.csr "$C_NODE" /tmp/e2e/keeper-b.csr
	out=$(odac "$C_NODE" domain mtls issue -d "$C_RECORD" --name keeper-b --hosts "$C_HOST_B" \
		--csr /tmp/e2e/keeper-b.csr --out /tmp/e2e/keeper-b.crt) || { fail "$name" "keeper issue: $(cli_safe "$out")"; return; }
	wait_probe 15 '[ "$P_VERIFY" = out_of_scope ]' "$C_NODE" "$C_HOST_B" /e2e/scope alice || true
	before=$(probe_line)
	if ! { [ "$P_CODE" = 200 ] && [ "$P_VERIFY" = out_of_scope ] && [ "$P_SIG" = true ]; }; then
		fail "$name" "before scope change on $C_HOST_B: $before (want verify=out_of_scope)"
		return
	fi
	if ! out=$(odac "$C_NODE" domain mtls scope --fingerprint "$C_FP" --hosts "$C_HOST_A,$C_HOST_B"); then
		fail "$name" "scope refused: $(cli_safe "$out")"
		return
	fi
	info "scope: $(cli_safe "$out")"
	wait_probe 15 '[ "$P_VERIFY" = success ]' "$C_NODE" "$C_HOST_B" /e2e/scope alice || true
	if [ "$P_CODE" = 200 ] && [ "$P_VERIFY" = success ] && [ "$P_SIG" = true ] && [ "$P_FP" = "$C_FP" ]; then
		pass "$name" "before: $before | after scope a+b, same cert: $(probe_line)"
	else
		fail "$name" "before: $before | after: $(probe_line) fp=${P_FP:0:16}"
	fi
}

# Revoke: an open WebSocket authenticated by alice is closed by the proxy, and
# the next request is refused on the required host and `revoked` on the
# optional one.
custom_revoke() {
	local name=custom.revoke out ip i ticks_before ticks_after closed_after="" done_line a_line b_line
	[ "$C_REQUIRED" = 1 ] || { skip "$name" "required host not in place"; return; }
	ip=$(node_ip "$C_NODE")
	# The wrapper records curl's exit, so "closed" never depends on ps output.
	client_sh "rm -f /work/ws.out /work/ws.err /work/ws.done; \
		setsid sh -c 'curl -sk -N --http1.1 --max-time 120 --cert /work/alice.crt --key /work/alice.key \
		--resolve $C_HOST_A:443:$ip wss://$C_HOST_A/ws >/work/ws.out 2>/work/ws.err; \
		echo \"exit=\$?\" >/work/ws.done' >/dev/null 2>&1 &"
	for i in $(seq 1 15); do
		client_sh "grep -q '^tick 2' /work/ws.out 2>/dev/null" && break
		sleep 1
	done
	if ! client_sh "grep -q '^hello verify=success sig_valid=true' /work/ws.out && grep -q '^tick 2' /work/ws.out"; then
		fail "$name" "WebSocket never opened as success: $(client_sh "head -c 200 /work/ws.out /work/ws.err 2>/dev/null | tr '\n' ' '")"
		return
	fi
	ticks_before=$(client_sh "grep -c '^tick' /work/ws.out")
	if ! out=$(odac "$C_NODE" domain mtls revoke --fingerprint "$C_FP"); then
		fail "$name" "revoke refused: $(cli_safe "$out")"
		return
	fi
	info "revoke: $(cli_safe "$out")"
	for i in $(seq 1 15); do
		if client_sh "test -f /work/ws.done"; then
			closed_after=$i
			break
		fi
		sleep 1
	done
	ticks_after=$(client_sh "grep -c '^tick' /work/ws.out")
	done_line=$(client_sh "cat /work/ws.done 2>/dev/null")
	wait_probe 15 '[ "$P_BODY_OK" != 1 ]' "$C_NODE" "$C_HOST_A" /e2e/revoked alice || true
	a_line=$(probe_line)
	local a_ok=0
	[ "$P_BODY_OK" != 1 ] && { [ "$P_EXIT" != 0 ] || [ "$P_CODE" = 403 ]; } && a_ok=1
	wait_probe 15 '[ "$P_VERIFY" = revoked ]' "$C_NODE" "$C_HOST_B" /e2e/revoked alice || true
	b_line=$(probe_line)
	if [ -n "$closed_after" ] && [ "$a_ok" = 1 ] && [ "$P_VERIFY" = revoked ] && [ "$P_SIG" = true ]; then
		pass "$name" "ws closed ${closed_after}s after revoke ($done_line, ticks $ticks_before->$ticks_after) | $C_HOST_A: $a_line | $C_HOST_B: $b_line"
	else
		client_sh "kill \$(pgrep -f 'wss://$C_HOST_A/ws') 2>/dev/null" || true
		fail "$name" "ws_closed=${closed_after:-no} ticks $ticks_before->$ticks_after | $C_HOST_A: $a_line | $C_HOST_B: $b_line"
	fi
}

suite_custom() {
	log "--- suite: custom domain (agent CLI on $C_NODE) ---"
	custom_setup || true
	custom_optional_none
	custom_cert_success
	custom_same_csr
	custom_required
	custom_scope
	custom_revoke
}

# Secrets and key material this suite produced, one per line, for the leak
# check. Values never leave this pipe.
leak_needles_custom() {
	# Every base64 body line, not just the first: a log that dumps part of a PEM
	# must still be caught. Lines under 32 characters are dropped by the caller.
	client_sh 'for f in /work/alice.csr /work/alice.crt /work/alice.key /work/keeper-b.csr /work/keeper-b.key; do [ -f "$f" ] && grep -v -- "-----" "$f"; done' 2>/dev/null || true
}
