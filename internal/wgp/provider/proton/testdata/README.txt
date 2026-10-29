References for the Proton API fingerprint tests (wire_test.go): ClientHello and
requests of the official Proton VPN Linux client 5.8.11. All accounts, tokens
and keys are placeholders.

proton-clienthello.json
  The ClientHello (first TLS record of a connection), decimal TLS code points,
  extensions in wire order. "random_ranges" lists the random bytes (random,
  session_id, the key_share keys); "template_hex" is the record with them zeroed.

proton-requests/NN-*.http
  Raw HTTP/1.1 request bytes, CRLF kept (see .gitattributes), in send order.
  Placeholders {{name}} have the same value in every file; {{arch}} is the build
  arch in x-pm-appversion and {{len}} the body length after substitution.

x25519.json
  WireGuard keys of Ed25519 seeds (tick_test.go): the Ed25519 public key, the
  X25519 public key computed once with the Edwards to Montgomery map
  u = (1+y)/(1-y) mod p, and ClientKeyFingerprint = base64(sha512(x25519 pub)).
  Synthetic seeds.
