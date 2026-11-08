# shellcheck shell=bash
# Shared CA scenarios (MTLS_PLAN.md A7, A8, K18): one client CA, imported with
# its key on edge-a and edge-b, gets the same id on both; a certificate signed
# on edge-a opens edge-b only once edge-b holds a record of it; a certificate
# revoked on edge-b cannot be imported back to life.
#
# The CA is the pki step's mtls-ca (mounted read-only at /e2e/pki). Each
# node routes shared.test to its own origin app.

S_CA=""        # CA id, identical on both nodes
S_READY=0
S_FP=""        # frank: issued on edge-a

ca_id_of() { sed -nE 's/.*(Imported client CA|Client CA) ([0-9a-f]{16}).*/\2/p' <<<"$1" | head -n1; }

# shared_node_setup NODE: an origin app and shared.test on a node.
shared_node_setup() {
	local node=$1 out
	if [ -z "$(app_container "$node" origin)" ]; then
		out=$(odac "$node" app create mtls-origin) || { info "$node app create: $(cli_safe "$out")"; return 1; }
	fi
	out=$(odac "$node" domain add shared.test origin) || { info "$node domain add: $(cli_safe "$out")"; return 1; }
}

# A7: same CA, same id, and a second import changes nothing.
shared_ca_import() {
	local name=shared.ca-import out_a out_b again id_a id_b id_again
	shared_node_setup edge-a && shared_node_setup edge-b || { fail "$name" "node setup failed"; return; }
	out_a=$(odac edge-a domain mtls ca import --cert /e2e/pki/mtls-ca.pem --key /e2e/pki/mtls-ca.key) || true
	out_b=$(odac edge-b domain mtls ca import --cert /e2e/pki/mtls-ca.pem --key /e2e/pki/mtls-ca.key) || true
	again=$(odac edge-a domain mtls ca import --cert /e2e/pki/mtls-ca.pem --key /e2e/pki/mtls-ca.key) || true
	id_a=$(ca_id_of "$out_a")
	id_b=$(ca_id_of "$out_b")
	id_again=$(ca_id_of "$again")
	if [ -z "$id_a" ] || [ "$id_a" != "$id_b" ] || [ "$id_again" != "$id_a" ]; then
		fail "$name" "edge-a: $(cli_safe "$out_a" | cut -c1-60) | edge-b: $(cli_safe "$out_b" | cut -c1-60) | again: $(cli_safe "$again" | cut -c1-60)"
		return
	fi
	S_CA=$id_a
	local n
	for n in edge-a edge-b; do
		out_a=$(odac "$n" domain mtls set -d shared.test --mode optional --ca "$S_CA") ||
			{ fail "$name" "$n set --ca: $(cli_safe "$out_a")"; return; }
	done
	# An optional host asks only once some certificate opens it (H6). A keeper
	# certificate on edge-b, never presented, keeps shared.test asking there, so
	# frank's verdict on edge-b is what this suite has always checked.
	make_csr keeper-s
	copy_file client /work/keeper-s.csr edge-b /tmp/e2e/keeper-s.csr
	out_a=$(odac edge-b domain mtls issue -d shared.test --name keeper-s --hosts shared.test --csr /tmp/e2e/keeper-s.csr --out /tmp/e2e/keeper-s.crt) ||
		{ fail "$name" "edge-b keeper issue: $(cli_safe "$out_a")"; return; }
	S_READY=1
	pass "$name" "CA id $S_CA on edge-a and edge-b; re-import: \"$(cli_safe "$again" | cut -c1-50)\"; shared.test optional --ca on both"
}

# A8: signed on a, refused on b, recorded on b by import, then accepted.
shared_cert_import() {
	local name=shared.cert-import out a_line before after
	[ "$S_READY" = 1 ] || { skip "$name" "shared CA not in place"; return; }
	make_csr frank
	copy_file client /work/frank.csr edge-a /tmp/e2e/frank.csr
	out=$(odac edge-a domain mtls issue -d shared.test --name frank --hosts shared.test --csr /tmp/e2e/frank.csr --out /tmp/e2e/frank.crt) ||
		{ fail "$name" "issue on edge-a: $(cli_safe "$out")"; return; }
	S_FP=$(fp_of "$out")
	copy_file edge-a /tmp/e2e/frank.crt client /work/frank.crt
	# The import takes the certificate alone, the way another node would get it.
	client_sh "openssl x509 -in /work/frank.crt -out /work/frank-leaf.pem"
	copy_file client /work/frank-leaf.pem edge-b /tmp/e2e/frank-leaf.pem
	wait_probe 15 '[ "$P_VERIFY" = success ]' edge-a shared.test /e2e/shared frank || true
	a_line=$(probe_line)
	probe edge-b shared.test /e2e/shared frank
	before=$(probe_line)
	local before_ok=0
	[ "$P_VERIFY" = untrusted ] && before_ok=1
	out=$(odac edge-b domain mtls import --cert /tmp/e2e/frank-leaf.pem --hosts shared.test) ||
		{ fail "$name" "import on edge-b refused: $(cli_safe "$out") | before: $before"; return; }
	wait_probe 15 '[ "$P_VERIFY" = success ]' edge-b shared.test /e2e/shared frank || true
	after=$(probe_line)
	if [ "$before_ok" = 1 ] && [ "$P_VERIFY" = success ] && [ "$P_FP" = "$S_FP" ]; then
		pass "$name" "edge-a: $a_line | edge-b before import: $before | after import: $after"
	else
		fail "$name" "edge-a: $a_line | edge-b before: $before | after: $after fp=${P_FP:0:16}/${S_FP:0:16}"
	fi
}

# Revocation is per node, and an import cannot undo it.
shared_revoked_import() {
	local name=shared.revoked-import out b_line a_line again rc=0
	[ -n "$S_FP" ] || { skip "$name" "no shared certificate"; return; }
	out=$(odac edge-b domain mtls revoke --fingerprint "$S_FP") || { fail "$name" "revoke on edge-b: $(cli_safe "$out")"; return; }
	wait_probe 15 '[ "$P_VERIFY" = revoked ]' edge-b shared.test /e2e/shared frank || true
	b_line=$(probe_line)
	local b_ok=0
	[ "$P_VERIFY" = revoked ] && b_ok=1
	again=$(odac edge-b domain mtls import --cert /tmp/e2e/frank-leaf.pem --hosts shared.test) || rc=$?
	probe edge-b shared.test /e2e/shared frank
	local still=$P_VERIFY
	probe edge-a shared.test /e2e/shared frank
	a_line=$(probe_line)
	if [ "$b_ok" = 1 ] && [ "$rc" != 0 ] && [ "$still" = revoked ] && [ "$P_VERIFY" = success ]; then
		pass "$name" "edge-b after revoke: $b_line | re-import refused: \"$(cli_safe "$again" | cut -c1-60)\" | edge-a untouched: $a_line"
	else
		fail "$name" "edge-b: $b_line | re-import rc=$rc ($(cli_safe "$again" | cut -c1-60)) still=$still | edge-a: $a_line"
	fi
}

suite_shared() {
	log "--- suite: shared CA across edge-a and edge-b (A7/A8) ---"
	shared_ca_import
	shared_cert_import
	shared_revoked_import
}

leak_needles_shared() {
	client_sh 'for f in /work/keeper-s.csr /work/keeper-s.key /work/frank.csr /work/frank.key /work/frank.crt /e2e/pki/mtls-ca.key /e2e/pki/root.key /e2e/pki/server.key; do [ -f "$f" ] && grep -v -- "-----" "$f"; done' 2>/dev/null || true
}
