# shellcheck shell=bash
# Per-host issuer scenarios (MTLS_PLAN.md Faz 11, K21, H8). The record ph.test
# with a wildcard runs optional with --issuer per-host on edge-a and edge-b,
# both trusting the shared CA (the one the shared suite imports; imported again
# here, which changes nothing, so the suite also runs alone). phpub.test is a
# record of the same app with no mode, the HTTP/3 baseline.
#
# Evidence is the handshake itself: `openssl s_client` prints the CA names a
# certificate request carries ("Acceptable client certificate CA names"), which
# is exactly what a browser filters its certificates by.

PH_RECORD=ph.test
PH_A=a.ph.test
PH_B=b.ph.test
PH_C=c.ph.test
PH_PUB=phpub.test
PH_READY=0
PH_FP1=""   # g1: a.ph.test, issued on edge-a
PH_FP2=""   # g2: b.ph.test, issued on edge-a
PH_IMPORTED=0

# ca_names NODE HOST: the CA names the handshake for HOST gives, each followed by
# ";", "EMPTY" for a request naming none, or "NOREQ" when no certificate request
# was sent at all. s_client prints "No client certificate CA names sent" with or
# without a request, so the request itself is read from the -msg trace.
ca_names() {
	local ip
	ip=$(node_ip "$1")
	# shellcheck disable=SC2016
	client_sh 'echo | timeout 10 openssl s_client -msg -connect '"$ip"':443 -servername '"$2"' 2>/dev/null | awk '"'"'
		/^<<< .*CertificateRequest/ { req = 1 }
		/^Acceptable client certificate CA names/ { on = 1; next }
		on && (/^(Requested Signature|Client Certificate Types|Shared Requested|Peer signing|Server Temp Key|Negotiated|---)/ || /^$/) { on = 0 }
		on { sub(/^[ \t]+/, ""); names = names $0 ";" }
		END { if (!req) printf "NOREQ"; else if (names == "") printf "EMPTY"; else printf "%s", names }'"'"
}

# norm_dn TEXT: drop the spaces around "=", which openssl versions print differently.
norm_dn() { sed -E 's/ *= */=/g' <<<"$1"; }

# alt_svc NODE HOST: the Alt-Svc header of a response, or empty.
alt_svc() {
	local ip
	ip=$(node_ip "$1")
	client_sh "curl -sk --max-time 10 --resolve $2:443:$ip -D - -o /dev/null https://$2/e2e/altsvc | tr -d '\r' | sed -n 's/^[Aa]lt-[Ss]vc: *//p' | head -n1"
}

# issuer_fp SERVICE PATH: fingerprint of the second certificate in a PEM file,
# the per-host issuer in a CLI-issued .crt (leaf, issuer, CA).
issuer_fp() {
	dc exec -T "$1" sh -c "awk '/BEGIN CERTIFICATE/{n++} n==2' '$2' | openssl x509 -outform der 2>/dev/null | sha256sum | cut -d' ' -f1" </dev/null
}

# issuer_subject SERVICE PATH: subject of that issuer, one line.
issuer_subject() {
	dc exec -T "$1" sh -c "awk '/BEGIN CERTIFICATE/{n++} n==2' '$2' | openssl x509 -noout -subject -nameopt RFC2253 2>/dev/null" </dev/null
}

# ph_issue NODE NAME HOST: issue NAME for HOST on NODE from a client-made CSR,
# leave NAME.crt (leaf + chain) in the client and on the node, print the CLI output.
ph_issue() {
	local node=$1 cert=$2 host=$3 out
	make_csr "$cert"
	copy_file client "/work/$cert.csr" "$node" "/tmp/e2e/$cert.csr"
	out=$(odac "$node" domain mtls issue -d "$PH_RECORD" --name "$cert" --hosts "$host" --csr "/tmp/e2e/$cert.csr" --out "/tmp/e2e/$cert.crt") || {
		printf '%s' "$out"
		return 1
	}
	copy_file "$node" "/tmp/e2e/$cert.crt" client "/work/$cert.crt"
	printf '%s' "$out"
}

perhost_node() {
	local node=$1 out d ca
	if [ -z "$(app_container "$node" origin)" ]; then
		out=$(odac "$node" app create mtls-origin) || { info "$node app create: $(cli_safe "$out")"; return 1; }
	fi
	for d in "$PH_RECORD" "*.$PH_RECORD" "$PH_PUB"; do
		out=$(odac "$node" domain add "$d" origin) || { info "$node domain add $d: $(cli_safe "$out")"; return 1; }
	done
	out=$(odac "$node" domain mtls ca import --cert /e2e/pki/mtls-ca.pem --key /e2e/pki/mtls-ca.key) || { info "$node ca import: $(cli_safe "$out")"; return 1; }
	ca=$(ca_id_of "$out")
	out=$(odac "$node" domain mtls set -d "$PH_RECORD" --mode optional --ca "$ca" --issuer per-host) ||
		{ info "$node set --issuer per-host: $(cli_safe "$out")"; return 1; }
	info "$node: $(cli_safe "$out" | cut -c1-120)"
}

perhost_setup() {
	local name=perhost.setup out
	perhost_node edge-a && perhost_node edge-b || { fail "$name" "node setup failed (see the lines above)"; return; }
	# Refused shapes: on a subdomain, and an unknown value.
	local sub_rc=0 bad_rc=0 sub bad
	sub=$(odac edge-a domain mtls set -d "$PH_A" --mode optional --issuer per-host) || sub_rc=$?
	bad=$(odac edge-a domain mtls set -d "$PH_RECORD" --mode optional --issuer chain) || bad_rc=$?
	if ! wait_probe 60 '[ "$P_BODY_OK" = 1 ]' edge-a "$PH_A" /e2e/perhost; then
		fail "$name" "origin never answered on $PH_A: $(probe_line)"
		return
	fi
	if [ "$sub_rc" = 0 ] || [ "$bad_rc" = 0 ]; then
		fail "$name" "accepted: subdomain rc=$sub_rc ($(cli_safe "$sub" | cut -c1-60)) | bad value rc=$bad_rc ($(cli_safe "$bad" | cut -c1-60))"
		return
	fi
	PH_READY=1
	pass "$name" "$PH_RECORD optional --issuer per-host on edge-a+edge-b; refused on a subdomain: \"$(cli_safe "$sub" | cut -c1-55)\"; bad value: \"$(cli_safe "$bad" | cut -c1-40)\""
}

# H6: no certificate opens the host, so nothing is asked and HTTP/3 stays.
perhost_silent() {
	local name=perhost.silent names alt pub
	[ "$PH_READY" = 1 ] || { skip "$name" "setup failed"; return; }
	names=$(ca_names edge-a "$PH_A")
	alt=$(alt_svc edge-a "$PH_A")
	pub=$(alt_svc edge-a "$PH_PUB")
	if [ "$names" = NOREQ ] && [ -n "$alt" ] && [ -n "$pub" ]; then
		pass "$name" "$PH_A before any certificate: certificate request=none, Alt-Svc=\"$alt\" (public baseline \"$pub\")"
	else
		fail "$name" "$PH_A: names=$names alt-svc=\"$alt\" baseline=\"$pub\""
	fi
}

# H4: a host names its own issuer only; a host with no certificate asks nothing.
perhost_advertise() {
	local name=perhost.advertise out names_a names_b names_c alt_a alt_c subj
	[ "$PH_READY" = 1 ] || { skip "$name" "setup failed"; return; }
	out=$(ph_issue edge-a g1 "$PH_A") || { fail "$name" "issue g1: $(cli_safe "$out")"; return; }
	PH_FP1=$(fp_of "$out")
	out=$(ph_issue edge-a g2 "$PH_B") || { fail "$name" "issue g2: $(cli_safe "$out")"; return; }
	PH_FP2=$(fp_of "$out")
	local i=0
	while :; do
		names_a=$(ca_names edge-a "$PH_A")
		[ "$names_a" != NOREQ ] && break
		i=$((i + 1))
		[ "$i" -ge 15 ] && break
		sleep 1
	done
	names_b=$(ca_names edge-a "$PH_B")
	names_c=$(ca_names edge-a "$PH_C")
	alt_a=$(alt_svc edge-a "$PH_A")
	alt_c=$(alt_svc edge-a "$PH_C")
	subj=$(issuer_subject client /work/g1.crt)
	probe edge-a "$PH_A" /e2e/perhost g1
	local want_a want_b
	want_a=$(norm_dn "O = mtls-e2e edge client CA, CN = $PH_A;")
	want_b=$(norm_dn "O = mtls-e2e edge client CA, CN = $PH_B;")
	if [ "$(norm_dn "$names_a")" = "$want_a" ] && [ "$(norm_dn "$names_b")" = "$want_b" ] &&
		[ "$names_c" = NOREQ ] && [ -z "$alt_a" ] && [ -n "$alt_c" ] &&
		[ "$P_VERIFY" = success ] && [ "$P_FP" = "$PH_FP1" ]; then
		pass "$name" "$PH_A names [$names_a] | $PH_B names [$names_b] | $PH_C: $names_c, Alt-Svc back | g1 chain issuer: $subj | g1 on $PH_A: $(probe_line)"
	else
		fail "$name" "a=[$names_a] b=[$names_b] c=$names_c alt_a=\"$alt_a\" alt_c=\"$alt_c\" issuer=$subj g1: $(probe_line)"
	fi
}

# H5: x's certificate on y is refused although it chains to the same CA, and its
# scope cannot be moved there.
perhost_cross_host() {
	local name=perhost.cross-host on_b on_a out rc=0
	[ -n "$PH_FP1" ] && [ -n "$PH_FP2" ] || { skip "$name" "no per-host certificates"; return; }
	probe edge-a "$PH_B" /e2e/cross g1
	on_b=$(probe_line)
	local b_verify=$P_VERIFY
	probe edge-a "$PH_A" /e2e/cross g2
	on_a=$(probe_line)
	local a_verify=$P_VERIFY
	out=$(odac edge-a domain mtls scope --fingerprint "$PH_FP1" --hosts "$PH_B") || rc=$?
	if [ "$b_verify" = untrusted ] && [ "$a_verify" = untrusted ] && [ "$rc" != 0 ]; then
		pass "$name" "g1 (a) on $PH_B: $on_b | g2 (b) on $PH_A: $on_a | rescope g1 to $PH_B refused: \"$(cli_safe "$out" | cut -c1-70)\""
	else
		fail "$name" "g1 on b: $on_b | g2 on a: $on_a | rescope rc=$rc ($(cli_safe "$out" | cut -c1-70))"
	fi
}

# K18: edge-b has its own issuer under the same name; the import needs the chain.
perhost_import() {
	local name=perhost.import out leaf_out leaf_rc=0 before names_a names_b
	[ -n "$PH_FP1" ] || { skip "$name" "no per-host certificate"; return; }
	copy_file client /work/g1.crt edge-b /tmp/e2e/g1.crt
	client_sh "openssl x509 -in /work/g1.crt -out /work/g1-leaf.pem"
	copy_file client /work/g1-leaf.pem edge-b /tmp/e2e/g1-leaf.pem
	probe edge-b "$PH_A" /e2e/import g1
	before=$(probe_line)
	local before_verify=$P_VERIFY
	leaf_out=$(odac edge-b domain mtls import --cert /tmp/e2e/g1-leaf.pem --hosts "$PH_A") || leaf_rc=$?
	out=$(odac edge-b domain mtls import --cert /tmp/e2e/g1.crt --hosts "$PH_A") ||
		{ fail "$name" "import with chain refused: $(cli_safe "$out")"; return; }
	wait_probe 15 '[ "$P_VERIFY" = success ]' edge-b "$PH_A" /e2e/import g1 || true
	names_a=$(ca_names edge-a "$PH_A")
	names_b=$(ca_names edge-b "$PH_A")
	if [ "$before_verify" != success ] && [ "$leaf_rc" != 0 ] && [ "$P_VERIFY" = success ] && [ "$P_FP" = "$PH_FP1" ] &&
		[ "$names_b" = "$names_a" ] && [[ "$names_b" == *"CN"*"$PH_A"* ]]; then
		PH_IMPORTED=1
		pass "$name" "edge-b before: $before | leaf alone refused: \"$(cli_safe "$leaf_out" | cut -c1-60)\" | with chain: $(probe_line) | edge-b names [$names_b] = edge-a"
	else
		fail "$name" "before: $before | leaf rc=$leaf_rc ($(cli_safe "$leaf_out" | cut -c1-60)) | after: $(probe_line) | names a=[$names_a] b=[$names_b]"
	fi
}

# Renewed on edge-b with edge-b's issuer, imported back: both open both nodes.
perhost_renew() {
	local name=perhost.renew out fp_r ia ib sa sb res="" bad=0 n c
	[ "$PH_IMPORTED" = 1 ] || { skip "$name" "no imported certificate"; return; }
	make_csr g1r
	copy_file client /work/g1r.csr edge-b /tmp/e2e/g1r.csr
	out=$(odac edge-b domain mtls issue -d "$PH_RECORD" --renews "$PH_FP1" --csr /tmp/e2e/g1r.csr --out /tmp/e2e/g1r.crt) ||
		{ fail "$name" "renew on edge-b: $(cli_safe "$out")"; return; }
	fp_r=$(fp_of "$out")
	copy_file edge-b /tmp/e2e/g1r.crt client /work/g1r.crt
	copy_file client /work/g1r.crt edge-a /tmp/e2e/g1r.crt
	out=$(odac edge-a domain mtls import --cert /tmp/e2e/g1r.crt --hosts "$PH_A") ||
		{ fail "$name" "import of the renewal on edge-a: $(cli_safe "$out")"; return; }
	ia=$(issuer_fp client /work/g1.crt)
	ib=$(issuer_fp client /work/g1r.crt)
	sa=$(issuer_subject client /work/g1.crt)
	sb=$(issuer_subject client /work/g1r.crt)
	for n in edge-a edge-b; do
		for c in g1 g1r; do
			wait_probe 15 '[ "$P_VERIFY" = success ]' "$n" "$PH_A" /e2e/renew "$c" || bad=1
			res+=" $n/$c=$P_VERIFY"
		done
	done
	if [ "$bad" = 0 ] && [ -n "$ia" ] && [ "$ia" != "$ib" ] && [ "$sa" = "$sb" ] && [ -n "$fp_r" ]; then
		pass "$name" "renewal ${fp_r:0:16} signed by edge-b's issuer (${ib:0:12} != edge-a's ${ia:0:12}, same subject $sb);$res"
	else
		fail "$name" "issuers ${ia:0:12}/${ib:0:12} subjects [$sa]/[$sb];$res"
	fi
}

suite_perhost() {
	log "--- suite: per-host issuers on edge-a and edge-b (K21/H8) ---"
	perhost_setup
	perhost_silent
	perhost_advertise
	perhost_cross_host
	perhost_import
	perhost_renew
}

# The run's per-host key material, plus every issuer key the nodes made: those
# never leave their node, so a body line of one in any log is a leak.
leak_needles_perhost() {
	client_sh 'for f in /work/g1.csr /work/g1.key /work/g1.crt /work/g2.csr /work/g2.key /work/g2.crt /work/g1r.csr /work/g1r.key /work/g1r.crt; do [ -f "$f" ] && grep -v -- "-----" "$f"; done' 2>/dev/null || true
	local n
	for n in $(running_nodes); do
		dc exec -T "$n" sh -c 'cat /app/.odac/cert/mtls/issuers/*.key 2>/dev/null | grep -v -- "-----"' </dev/null 2>/dev/null || true
	done
}
