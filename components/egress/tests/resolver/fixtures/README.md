Test-only TLS material for the OSEP-0025 resolver stub (`../resolver.py`).

`test-localhost-cert.pem` / `test-localhost-key.pem` are a self-signed pair
for CN=localhost (SAN: DNS:localhost, IP:127.0.0.1). They certify nothing and
must never be trusted outside local egress tests; regenerate with:

    openssl req -x509 -newkey rsa:2048 -days 3650 -nodes \
      -subj "/CN=localhost" \
      -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
      -keyout test-localhost-key.pem -out test-localhost-cert.pem
