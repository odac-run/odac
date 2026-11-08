# shellcheck shell=bash
# Edge and multi-edge scenarios (MTLS_PLAN.md Faz 10 "Edge:" and "Çoklu edge:",
# E1/E5/E8, K9, K17, K18, T8). The hub here is the test hub (images/hub), which
# speaks §6 to odac-tunnel; the dashboard's handler does not exist yet.
#
# Topology:
#   edge-a, edge-b  odac-tunnel as an app; tun.mtls.test and *.tun.mtls.test
#                   routed to it, optional, trusting the shared client CA;
#                   odac-tunnel granted the domain.mtls.cert.* actions
#   home            a server whose agent dials wss://tunnel.odac.run (a network
#                   alias of edge-a) for t1.tun.mtls.test, owned by user U1
#   hub             assigns request ids, queues for offline edges, flushes on
#                   reconnect, and can cut an edge off (/ctl/block)
# Requests and replies travel client -> hub -> edge inside the client
# container, so certificates and CSRs never pass through the host.

E_TUNNEL=t1.tun.mtls.test
E_U1=e2eOwnerU1x7lL6dR7Hggfxj
E_U2=e2eOwnerU2xddpq5Beo2XpMw
E_READY=0
E_FP1=""     # t1a: issued on edge-a for U1, imported to edge-b
E_FP2=""     # t1b: issued on edge-a for U1 only

# digest OWNER: tunnel.OwnerDigest, the first 32 hex of SHA-256 of the id.
digest() { client_sh "printf '%s' '$1' | sha256sum | cut -c1-32"; }

# hub_request EDGE OUT [QUEUE] < jq-program: build a request in the client with
# jq -n (its --rawfile/--arg come from the program's own text), send it, keep
# the reply in /work/OUT.reply, print "action|ok|code|edge|fingerprint|http".
hub_request() {
	local edge=$1 out=$2 queue=${3:-0} prog
	prog=$(cat)
	client_sh "jq -cn $prog | curl -s --max-time 40 -o /work/$out.reply -w '%{http_code}' -X POST \
		-H 'Content-Type: application/json' --data-binary @- 'http://hub:8080/ctl/request?edge=$edge&queue=$queue' >/work/$out.http; \
		jq -r '[.action // (if .queued then \"queued\" else \"\" end), (.ok|tostring), .code // .error // \"\", .edge // \"\", .fingerprint // \"\"] | join(\"|\")' /work/$out.reply 2>/dev/null | tr -d '\n'; \
		printf '|%s\n' \"\$(cat /work/$out.http)\""
}

# R_* from the last hub_request line.
parse_reply() { IFS='|' read -r R_ACTION R_OK R_CODE R_EDGE R_FP R_HTTP <<<"$1"; }

reply_line() { printf '%s ok=%s%s edge=%s' "${R_ACTION:-?}" "${R_OK:-?}" "${R_CODE:+ code=$R_CODE}" "${R_EDGE:-?}"; }

hub_edges() { hub_ctl GET edges </dev/null | jqc '.edges | sort | join(",")'; }

edge_online() { [[ ",$(hub_edges)," == *",$1,"* ]]; }

edge_offline() { ! edge_online "$1"; }

# tunnel_logs NODE: odac-tunnel's log on a node.
tunnel_logs() { dc exec -T "$1" sh -c 'docker logs odac-tunnel 2>&1'; }

tunnel_connected() { tunnel_logs edge-a | grep -q "agent connected.*domain=$E_TUNNEL"; }

# edge_node NODE LETTER: odac-tunnel on one edge node.
edge_node() {
	local node=$1 letter=$2 out d
	out=$(odac "$node" app create "odac-tunnel-$letter") || { info "$node create odac-tunnel: $(cli_safe "$out")"; return 1; }
	for d in tun.mtls.test '*.tun.mtls.test' tunnel.odac.run; do
		out=$(odac "$node" domain add "$d" odac-tunnel) || { info "$node domain add $d: $(cli_safe "$out")"; return 1; }
	done
	out=$(odac "$node" domain mtls ca import --cert /e2e/pki/mtls-ca.pem --key /e2e/pki/mtls-ca.key) || { info "$node ca import: $(cli_safe "$out")"; return 1; }
	local ca
	ca=$(ca_id_of "$out")
	out=$(odac "$node" domain mtls set -d tun.mtls.test --mode optional --ca "$ca") || { info "$node set: $(cli_safe "$out")"; return 1; }
	out=$(odac "$node" app api odac-tunnel --allow domain.mtls.cert.issue,domain.mtls.cert.revoke,domain.mtls.cert.list,domain.mtls.cert.import) ||
		{ info "$node grant: $(cli_safe "$out")"; return 1; }
	odac "$node" app restart odac-tunnel >/dev/null || { info "$node restart odac-tunnel failed"; return 1; }
}

edge_setup() {
	local name=edge.setup
	if [ "${TUNNEL_IMAGE:-0}" != 1 ]; then
		skip "$name" "no odac-tunnel source (TUNNEL_SRC or TUNNEL_REPO+TUNNEL_REF)"
		return 1
	fi
	edge_node edge-a a && edge_node edge-b b || { fail "$name" "edge node setup failed (see the lines above)"; return 1; }
	if ! wait_until 60 edge_online edge-a || ! wait_until 60 edge_online edge-b; then
		fail "$name" "odac-tunnel never reached the hub: online=[$(hub_edges)]; edge-a log: $(tunnel_logs edge-a | grep -iE 'refusing|hub|error' | tail -n 2 | tr '\n' ' ' | cut -c1-200)"
		return 1
	fi
	echo "{\"domain\":\"$E_TUNNEL\",\"user\":\"$E_U1\"}" | hub_ctl POST tunnel >/dev/null
	local out
	out=$(odac home app create mtls-home) || { fail "$name" "home app create: $(cli_safe "$out")"; return 1; }
	if ! wait_until 60 tunnel_connected; then
		fail "$name" "home never connected its tunnel; home proxy log: $(dc exec -T home sh -c 'grep -h Tunnel /app/.odac/logs/proxy.log 2>/dev/null | tail -n 2' | tr '\n' ' ' | cut -c1-200)"
		return 1
	fi
	probe edge-a "$E_TUNNEL" /e2e/edge
	if [ "$P_CODE" = 307 ] && [[ "$P_LOC" == *app.odac.run/tunnel/* ]]; then
		E_READY=1
		pass "$name" "hub sees [$(hub_edges)], home's tunnel is up on edge-a, no cert: $(probe_line)"
	else
		fail "$name" "no cert on $E_TUNNEL: $(probe_line) (want 307 to the sign-in flow)"
	fi
}

# E1/E8: odac-tunnel refuses to start without a usable secret, edge id or hub URL.
edge_fail_closed() {
	local name=edge.fail-closed res="" bad=0 c
	[ "${TUNNEL_IMAGE:-0}" = 1 ] || { skip "$name" "no odac-tunnel image"; return; }
	for c in \
		"no-secret|-e ODAC_EDGE_ID=edge-x" \
		"placeholder|-e ODAC_TUNNEL_SECRET=DEV_SECRET_CHANGE_IN_PROD -e ODAC_EDGE_ID=edge-x" \
		"no-edge-id|-e ODAC_TUNNEL_SECRET=@S@" \
		"bad-edge-id|-e ODAC_TUNNEL_SECRET=@S@ -e ODAC_EDGE_ID=Edge_X" \
		"plain-hub|-e ODAC_TUNNEL_SECRET=@S@ -e ODAC_EDGE_ID=edge-x -e ODAC_HUB_URL=ws://hub:8443/tunnel"; do
		local label=${c%%|*} env=${c#*|} verdict
		# @S@ becomes a throwaway made in the node; the check is that it never
		# shows up in what the process prints.
		verdict=$(dc exec -T edge-a sh -c '
			s=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d " \n")
			env=$(echo "'"$env"'" | sed "s/@S@/$s/g")
			cid=$(docker run -d $env mtls-e2e/odac-tunnel:local)
			i=0; while [ "$(docker inspect -f "{{.State.Running}}" $cid)" = true ] && [ $i -lt 10 ]; do sleep 1; i=$((i+1)); done
			running=$(docker inspect -f "{{.State.Running}}" $cid); code=$(docker inspect -f "{{.State.ExitCode}}" $cid)
			logs=$(docker logs $cid 2>&1); docker rm -f $cid >/dev/null
			refused=no; echo "$logs" | grep -q "refusing to start" && refused=yes
			leaked=no; echo "$logs" | grep -qF "$s" && leaked=yes
			port=no; echo "$logs" | grep -q "Booting" && port=yes
			echo "running=$running exit=$code refused=$refused listened=$port leaked=$leaked"')
		res+=" $label:[$verdict]"
		case $verdict in *"running=false"*"refused=yes listened=no leaked=no"*) ;; *) bad=1 ;; esac
		case $verdict in *"exit=0 "*) bad=1 ;; esac
	done
	if [ "$bad" = 0 ]; then
		pass "$name" "all 5 exit non-zero before listening, secret never echoed:$(sed 's/ running=false//g; s/ refused=yes listened=no leaked=no//g' <<<"$res")"
	else
		fail "$name" "$res"
	fi
}

# §6 round trip on edge-a, then the certificate opens the tunnel with no cookie.
edge_issue() {
	local name=edge.issue line cn
	[ "$E_READY" = 1 ] || { skip "$name" "edge setup failed"; return; }
	make_csr t1a
	line=$(hub_request edge-a t1a <<EOF
--rawfile csr /work/t1a.csr --arg name "$(digest "$E_U1")/t1a" '{action: "cert.issue", domain: "$E_TUNNEL", name: \$name, csr: \$csr, days: 30}'
EOF
)
	parse_reply "$line"
	E_FP1=$R_FP
	client_sh "jq -r '.cert // empty' /work/t1a.reply > /work/t1a.crt; jq -r '.chain // empty' /work/t1a.reply >> /work/t1a.crt"
	cn=$(client_sh "openssl x509 -in /work/t1a.crt -noout -subject -nameopt RFC2253 2>/dev/null | sed 's/^subject=CN=//'")
	local der_fp
	der_fp=$(cert_fp client /work/t1a.crt)
	if ! { [ "$R_ACTION" = cert.result ] && [ "$R_OK" = true ] && [ "$R_EDGE" = edge-a ] && [ -n "$E_FP1" ] && [ "$der_fp" = "$E_FP1" ] && [ "$cn" = "$(digest "$E_U1")/t1a" ]; }; then
		fail "$name" "reply: $(reply_line) fp=${R_FP:0:16} der=${der_fp:0:16} cn=$cn"
		return
	fi
	wait_probe 15 '[ "$P_BODY_OK" = 1 ]' edge-a "$E_TUNNEL" /e2e/edge t1a || true
	if [ "$P_BODY_OK" = 1 ] && [ "$P_CODE" = 200 ]; then
		pass "$name" "$(reply_line) fp=${E_FP1:0:16} cn=<digest(U1)>/t1a | with cert, no cookie: http=200 from home's app through the tunnel"
	else
		fail "$name" "$(reply_line) ok, but with the cert: $(probe_line)"
	fi
}

# E5/K9: odac-tunnel checks owner and host itself and does not forward.
edge_issue_refused() {
	local name=edge.issue.refused foreign wide unknown
	[ "$E_READY" = 1 ] || { skip "$name" "edge setup failed"; return; }
	make_csr t1x
	parse_reply "$(hub_request edge-a t1x-foreign <<EOF
--rawfile csr /work/t1x.csr --arg name "$(digest "$E_U2")/t1x" '{action: "cert.issue", domain: "$E_TUNNEL", name: \$name, csr: \$csr}'
EOF
)"
	foreign="$R_OK/$R_CODE"
	parse_reply "$(hub_request edge-a t1x-wide <<EOF
--rawfile csr /work/t1x.csr --arg name "$(digest "$E_U1")/t1x" '{action: "cert.issue", domain: "$E_TUNNEL", hosts: ["$E_TUNNEL", "t2.tun.mtls.test"], name: \$name, csr: \$csr}'
EOF
)"
	wide="$R_OK/$R_CODE"
	parse_reply "$(hub_request edge-a t1x-unknown <<EOF
--rawfile csr /work/t1x.csr --arg name "$(digest "$E_U1")/t1x" '{action: "cert.issue", domain: "t9.tun.mtls.test", name: \$name, csr: \$csr}'
EOF
)"
	unknown="$R_OK/$R_CODE"
	local signed
	signed=$(odac edge-a domain mtls list -d tun.mtls.test | grep -c t1x || true)
	if [ "$foreign" = false/owner_mismatch ] && [ "$wide" = false/bad_request ] && [ "$unknown" = false/owner_mismatch ] && [ "$signed" = 0 ]; then
		pass "$name" "U2's digest on U1's tunnel: owner_mismatch | two hosts: bad_request | no such tunnel: owner_mismatch | agent signed none of them"
	else
		fail "$name" "foreign=$foreign wide=$wide unknown=$unknown signed_t1x=$signed"
	fi
}

# K18: edge-b refuses edge-a's certificate until cert.import gives it a record.
edge_multi_import() {
	local name=multi.import before after
	[ -n "$E_FP1" ] || { skip "$name" "no tunnel certificate"; return; }
	# An optional host asks only once some certificate opens it (H6). A keeper
	# certificate, never presented, keeps the tunnel host asking on edge-b,
	# so t1a is shown and refused rather than never requested.
	# edge-a gets one too, for multi.revoke-offline, where t1a is revoked there.
	local out n
	make_csr keeper-t
	for n in edge-a edge-b; do
		copy_file client /work/keeper-t.csr "$n" /tmp/e2e/keeper-t.csr
		out=$(odac "$n" domain mtls issue -d "$E_TUNNEL" --name keeper-t --hosts "$E_TUNNEL" --csr /tmp/e2e/keeper-t.csr --out /tmp/e2e/keeper-t.crt) ||
			{ fail "$name" "$n keeper issue: $(cli_safe "$out")"; return; }
	done
	wait_probe 15 '[ "$P_CODE" = 403 ]' edge-b "$E_TUNNEL" /e2e/multi t1a || true
	before=$(probe_line)
	local before_ok=0
	[ "$P_CODE" = 403 ] && [[ "$P_TEXT" == *untrusted* ]] && before_ok=1
	client_sh "openssl x509 -in /work/t1a.crt -out /work/t1a-leaf.pem"
	parse_reply "$(hub_request edge-b t1a-import <<EOF
--rawfile cert /work/t1a-leaf.pem --arg name "$(digest "$E_U1")/t1a" '{action: "cert.import", domain: "$E_TUNNEL", name: \$name, cert: \$cert}'
EOF
)"
	local imp
	imp=$(reply_line)
	wait_probe 15 '[ "$P_CODE" = 502 ]' edge-b "$E_TUNNEL" /e2e/multi t1a || true
	after=$(probe_line)
	if [ "$before_ok" = 1 ] && [ "$R_OK" = true ] && [ "$R_FP" = "$E_FP1" ] && [ "$R_EDGE" = edge-b ] && [ "$P_CODE" = 502 ]; then
		pass "$name" "edge-b before: $before | $imp | after: $after (accepted; the tunnel itself lives on edge-a)"
	else
		fail "$name" "before: $before | $imp fp=${R_FP:0:16} | after: $after"
	fi
}

# T8: edge-b is cut off from the hub while t1a is revoked. edge-a refuses at
# once; edge-b keeps accepting until it is back and the queue is flushed.
edge_multi_revoke_offline() {
	local name=multi.revoke-offline a_line b_during b_after flushed
	[ -n "$E_FP1" ] || { skip "$name" "no tunnel certificate"; return; }
	echo '{}' | hub_ctl POST "block?edge=edge-b&on=1" >/dev/null
	wait_until 20 edge_offline edge-b || { fail "$name" "edge-b did not drop off the hub"; return; }
	parse_reply "$(hub_request edge-a t1a-revoke-a <<EOF
'{action: "cert.revoke", fingerprint: "$E_FP1"}'
EOF
)"
	local ra
	ra=$(reply_line)
	[ "$R_OK" = true ] || { fail "$name" "revoke on edge-a: $ra"; return; }
	parse_reply "$(hub_request edge-b t1a-revoke-b 1 <<EOF
'{action: "cert.revoke", fingerprint: "$E_FP1"}'
EOF
)"
	[ "$R_ACTION" = queued ] || { fail "$name" "revoke for offline edge-b was not queued: $(reply_line) http=$R_HTTP"; return; }
	wait_probe 15 '[ "$P_CODE" = 403 ]' edge-a "$E_TUNNEL" /e2e/revoke t1a || true
	a_line=$(probe_line)
	local a_ok=0
	[ "$P_CODE" = 403 ] && [[ "$P_TEXT" == *revoked* ]] && a_ok=1
	probe edge-b "$E_TUNNEL" /e2e/revoke t1a
	b_during=$(probe_line)
	local during_open=0
	[ "$P_CODE" = 502 ] && during_open=1
	echo '{}' | hub_ctl POST "block?edge=edge-b&on=0" >/dev/null
	flushed_ok() { hub_ctl GET "flushed?edge=edge-b" </dev/null | jqc "[.[] | select(.action == \"cert.revoked\" and .ok == true and .fingerprint == \"$E_FP1\")] | length" | grep -q '^1$'; }
	wait_until 40 flushed_ok && flushed=yes || flushed=no
	wait_probe 15 '[ "$P_CODE" = 403 ]' edge-b "$E_TUNNEL" /e2e/revoke t1a || true
	b_after=$(probe_line)
	if [ "$a_ok" = 1 ] && [ "$during_open" = 1 ] && [ "$flushed" = yes ] && [ "$P_CODE" = 403 ] && [[ "$P_TEXT" == *revoked* ]]; then
		pass "$name" "edge-a: $ra, then $a_line | edge-b cut off, queued; meanwhile $b_during | reconnected, flushed ok, then $b_after"
	else
		fail "$name" "edge-a: $ra / $a_line | edge-b during: $b_during | flushed=$flushed | after: $b_after"
	fi
}

# §6: revoking on a node that never saw the certificate answers not_found; a
# second revoke where it was revoked still succeeds (A5 through the edge).
edge_revoke_not_found() {
	local name=edge.revoke.not-found nf again
	[ "$E_READY" = 1 ] || { skip "$name" "edge setup failed"; return; }
	make_csr t1b
	parse_reply "$(hub_request edge-a t1b <<EOF
--rawfile csr /work/t1b.csr --arg name "$(digest "$E_U1")/t1b" '{action: "cert.issue", domain: "$E_TUNNEL", name: \$name, csr: \$csr, days: 30}'
EOF
)"
	E_FP2=$R_FP
	[ "$R_OK" = true ] || { fail "$name" "issue t1b: $(reply_line)"; return; }
	client_sh "jq -r .cert /work/t1b.reply > /work/t1b.crt; jq -r .chain /work/t1b.reply >> /work/t1b.crt"
	parse_reply "$(hub_request edge-b t1b-revoke <<EOF
'{action: "cert.revoke", fingerprint: "$E_FP2"}'
EOF
)"
	nf="$(reply_line)"
	local nf_ok=0
	[ "$R_OK" = false ] && [ "$R_CODE" = not_found ] && [ "$R_EDGE" = edge-b ] && nf_ok=1
	parse_reply "$(hub_request edge-a t1a-revoke-again <<EOF
'{action: "cert.revoke", fingerprint: "$E_FP1"}'
EOF
)"
	again="$(reply_line)"
	if [ "$nf_ok" = 1 ] && [ "$R_OK" = true ]; then
		pass "$name" "t1b on edge-b: $nf | t1a again on edge-a: $again"
	else
		fail "$name" "t1b on edge-b: $nf | t1a again on edge-a: $again"
	fi
}

# The tunnel changes hands: the old owner's certificate stops opening it.
edge_owner_change() {
	local name=edge.owner-change before after
	[ -n "$E_FP2" ] || { skip "$name" "no live tunnel certificate"; return; }
	wait_probe 15 '[ "$P_BODY_OK" = 1 ]' edge-a "$E_TUNNEL" /e2e/owner t1b || true
	before=$(probe_line)
	local before_ok=$P_BODY_OK
	echo "{\"domain\":\"$E_TUNNEL\",\"user\":\"$E_U2\"}" | hub_ctl POST tunnel >/dev/null
	wait_probe 15 '[ "$P_CODE" = 403 ]' edge-a "$E_TUNNEL" /e2e/owner t1b || true
	after=$(probe_line)
	if [ "$before_ok" = 1 ] && [ "$P_CODE" = 403 ] && [[ "$P_TEXT" == *owner_mismatch* ]]; then
		pass "$name" "U1's t1b before: $before | after upsert to U2: $after"
	else
		fail "$name" "before: $before | after: $after"
	fi
}

suite_edge() {
	log "--- suite: edge + multi-edge (test hub speaking §6; odac-tunnel on edge-a/edge-b) ---"
	edge_fail_closed
	if edge_setup; then
		edge_issue
		edge_issue_refused
		edge_multi_import
		edge_multi_revoke_offline
		edge_revoke_not_found
		edge_owner_change
	else
		local s
		for s in edge.issue edge.issue.refused multi.import multi.revoke-offline edge.revoke.not-found edge.owner-change; do
			skip "$s" "edge setup failed"
		done
	fi
}

leak_needles_edge() {
	client_sh 'for f in /work/t1a.csr /work/t1a.key /work/t1a.crt /work/t1b.csr /work/t1b.key /work/t1b.crt /work/t1x.csr /work/t1x.key /work/keeper-t.csr /work/keeper-t.key; do [ -f "$f" ] && grep -v -- "-----" "$f"; done' 2>/dev/null || true
}
