#!/bin/bash
# Generates self-signed TLS certificates for the mutating webhook server,
# creates a Kubernetes Secret, and patches the MutatingWebhookConfiguration
# with the CA bundle.
set -e

NAMESPACE="${NAMESPACE:-everest-system}"
SERVICE=huawei-elb-controller-webhook
SECRET=huawei-elb-controller-webhook-tls
WEBHOOK_CONFIG=huawei-elb-controller-webhook

echo "=== Generating webhook TLS certificates ==="

TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

# 1. Generate CA key and certificate.
openssl genrsa -out "$TMPDIR/ca.key" 2048 2>/dev/null
openssl req -new -x509 -days 3650 -key "$TMPDIR/ca.key" -out "$TMPDIR/ca.crt" \
  -subj "/CN=huawei-elb-controller-webhook-ca" 2>/dev/null

# 2. Generate server key.
openssl genrsa -out "$TMPDIR/server.key" 2048 2>/dev/null

# 3. Generate CSR with SANs for the webhook Service.
cat > "$TMPDIR/csr.conf" << EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[v3_req]
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${SERVICE}
DNS.2 = ${SERVICE}.${NAMESPACE}
DNS.3 = ${SERVICE}.${NAMESPACE}.svc
DNS.4 = ${SERVICE}.${NAMESPACE}.svc.cluster.local
EOF

openssl req -new -key "$TMPDIR/server.key" -out "$TMPDIR/server.csr" \
  -subj "/CN=${SERVICE}.${NAMESPACE}.svc" -config "$TMPDIR/csr.conf" 2>/dev/null

# 4. Sign server certificate with CA.
openssl x509 -req -days 3650 -in "$TMPDIR/server.csr" \
  -CA "$TMPDIR/ca.crt" -CAkey "$TMPDIR/ca.key" -CAcreateserial \
  -out "$TMPDIR/server.crt" -extensions v3_req -extfile "$TMPDIR/csr.conf" 2>/dev/null

echo "=== Creating Kubernetes Secret: ${SECRET} ==="
kubectl create secret generic "${SECRET}" \
  --namespace "${NAMESPACE}" \
  --from-file=tls.crt="$TMPDIR/server.crt" \
  --from-file=tls.key="$TMPDIR/server.key" \
  --from-file=ca.crt="$TMPDIR/ca.crt" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "=== Patching MutatingWebhookConfiguration with CA bundle ==="
# macOS base64 doesn't support -w 0, so use tr to strip newlines.
CA_BUNDLE=$(base64 < "$TMPDIR/ca.crt" | tr -d '\n')
kubectl patch mutatingwebhookconfiguration "${WEBHOOK_CONFIG}" \
  --type='json' -p="[{\"op\":\"replace\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"${CA_BUNDLE}\"}]" 2>/dev/null \
  || kubectl patch mutatingwebhookconfiguration "${WEBHOOK_CONFIG}" \
  --type='json' -p="[{\"op\":\"add\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"${CA_BUNDLE}\"}]" 2>/dev/null \
  || echo "WARN: Could not patch MutatingWebhookConfiguration (apply webhook.yaml first)"

echo "=== Done. ==="
