#!/bin/sh
# The run's PKI, made fresh by the one-shot `pki` service into the `pki` volume,
# which compose down -v removes with the stack. Nothing is reused across runs.
#
#   root.pem/.key       test root: signs the TLS certificates below
#   server.pem/.key     every TLS name in the run: the nodes' fallback
#                       certificate (tunnel.odac.run included) and the hub
#   mtls-ca.pem/.key    the client CA the edges share (A7/K18)
set -eu
cd /e2e/pki
[ -f done ] && exit 0
umask 077

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
	-keyout root.key -out root.pem -days 30 -subj "/CN=mtls-e2e test root" \
	-addext "basicConstraints=critical,CA:TRUE" \
	-addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
	-keyout server.key -out server.csr -subj "/CN=mtls-e2e server" 2>/dev/null
cat >server.ext <<'EXT'
basicConstraints=CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=DNS:hub,DNS:tunnel.odac.run,DNS:mtls.test,DNS:*.mtls.test,DNS:tun.mtls.test,DNS:*.tun.mtls.test,DNS:shared.test,DNS:other.test
EXT
openssl x509 -req -in server.csr -CA root.pem -CAkey root.key -CAcreateserial \
	-out server.pem -days 30 -extfile server.ext 2>/dev/null
rm -f server.csr server.ext root.srl

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
	-keyout mtls-ca.key -out mtls-ca.pem -days 365 -subj "/CN=mtls-e2e edge client CA" \
	-addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
	-addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

chmod 0644 root.pem server.pem mtls-ca.pem
touch done
echo "pki ready"
