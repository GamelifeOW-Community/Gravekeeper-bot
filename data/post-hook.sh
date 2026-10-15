CERTS_DIR=${CERTS_DIR:-"/data/letsencrypt/certs"}

for dir in "$CERTS_DIR"/*/; do
    if [ -f "$dir/certificate.crt" ] && [ -f "$dir/privatekey.key" ]; then
        openssl pkcs12 -export \
        -in $dir/certificate.crt \
        -inkey $dir/privatekey.key \
        -out $dir/keystore.p12 \
        -name "$(basename "$dir")" \
        -passout pass:
    fi
    if [[ "$(basename "$dir")" == *"postgres"* ]]; then
        chown -R 999:999 "$dir"
    fi
    if [[ "$(basename "$dir")" == *"pgadmin"* ]]; then
        chown -R 5050:5050 "$dir"
    fi
done
