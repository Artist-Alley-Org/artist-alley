#!/usr/bin/env bash
# scripts/ci/ephemeral-studio-b-tls.sh <cert-dir>: a throwaway CA + studio-b.local server cert for one CI run.
#
# Replaces the self-hosted runner's pre-provisioned /runner-tls bundle (#1514). Writes exactly the three
# files scripts/dogfood/up.sh's "pre-existing certs" path expects, under the names it expects:
#
#   mkcert-rootCA.pem        the CA certificate (0644). Mounted read-only into app + app-b by the dogfood
#                            override, which adds it to Go's trust (SSL_CERT_DIR); app (studio-a) dials
#                            https://studio-b.local:9443 and must verify this chain. The file keeps the
#                            mkcert name only because up.sh and the override look for that name; it is
#                            not produced by mkcert.
#   studio-b.local.pem       the leaf certificate (0644), SAN DNS:studio-b.local, serverAuth, served by nginx-b.
#   studio-b.local-key.pem   the leaf private key (0600). Read by nginx-b's root master process.
#
# The CA private key is generated in a private temp dir and deleted on exit, so the CA can sign nothing
# beyond this one leaf. Both certificates expire after 2 days. Nothing is added to the runner's own
# trust store: pair.sh and the scenarios use `curl -k`, and Playwright sets ignoreHTTPSErrors, exactly as
# they did on the self-hosted runner, so trust on the runner side is unchanged.

set -euo pipefail

[ $# -eq 1 ] || { echo "usage: $0 <cert-dir>" >&2; exit 2; }
dir="$1"
mkdir -p "$dir"
for f in mkcert-rootCA.pem studio-b.local.pem studio-b.local-key.pem; do
    [ ! -e "${dir}/${f}" ] || { echo "::error::${dir}/${f} already exists; refusing to overwrite" >&2; exit 1; }
done

umask 077
work="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/aa-ci-tls.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT

# Self-contained configs, so the result does not depend on the distro's openssl.cnf defaults.
cat > "${work}/ca.cnf" <<'EOF'
[req]
distinguished_name = dn
prompt = no
x509_extensions = v3_ca
[dn]
CN = artist-alley CI ephemeral root (one run, key destroyed)
[v3_ca]
basicConstraints = critical, CA:TRUE, pathlen:0
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
EOF
cat > "${work}/leaf.cnf" <<'EOF'
[req]
distinguished_name = dn
prompt = no
[dn]
CN = studio-b.local
[v3_leaf]
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = DNS:studio-b.local
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
EOF

openssl req -x509 -new -config "${work}/ca.cnf" -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -sha256 -days 2 -keyout "${work}/ca.key" -out "${work}/ca.pem" 2>/dev/null
openssl req -new -config "${work}/leaf.cnf" -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "${work}/leaf.key" -out "${work}/leaf.csr" 2>/dev/null
openssl x509 -req -in "${work}/leaf.csr" -CA "${work}/ca.pem" -CAkey "${work}/ca.key" \
    -set_serial "0x$(openssl rand -hex 16)" -sha256 -days 2 \
    -extfile "${work}/leaf.cnf" -extensions v3_leaf -out "${work}/leaf.pem" 2>/dev/null

# Fail here, not at the first federation delivery, if the chain or the name is wrong.
openssl verify -x509_strict -purpose sslserver -verify_hostname studio-b.local \
    -CAfile "${work}/ca.pem" "${work}/leaf.pem" >/dev/null

install -m 0644 "${work}/ca.pem" "${dir}/mkcert-rootCA.pem"
install -m 0644 "${work}/leaf.pem" "${dir}/studio-b.local.pem"
install -m 0600 "${work}/leaf.key" "${dir}/studio-b.local-key.pem"
echo "ephemeral studio-b.local TLS written to ${dir} (CA key destroyed; leaf $(openssl x509 -in "${dir}/studio-b.local.pem" -noout -enddate))"
