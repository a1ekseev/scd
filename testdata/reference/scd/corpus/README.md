# scd corpus

Input lines for `tools/golden/scd-domain.mjs`; `vless-corpus.txt` has one line per case and no blank lines or comments. `ts-parse.json` holds the result of the TS parser and normalizer for every line.

The ML-KEM client strings (`mlkem768x25519plus.native.0rtt.*`) come from `xray vlessenc` of Xray 26.10.10. The two valid ones are real client keys of throwaway server keys; the negative one is random bytes of the right length.

| Line | What it checks |
|---|---|
| 1 | Line from docker/fixtures/subscription.txt (tcp+tls, ws+tls or reality+vision with Cyrillic label) |
| 2 | Line from docker/fixtures/subscription.txt (tcp+tls, ws+tls or reality+vision with Cyrillic label) |
| 3 | Line from docker/fixtures/subscription.txt (tcp+tls, ws+tls or reality+vision with Cyrillic label) |
| 4 | Line from docker/fixtures/subscription.txt (tcp+tls, ws+tls or reality+vision with Cyrillic label) |
| 5 | Line from docker/fixtures/subscription.txt (tcp+tls, ws+tls or reality+vision with Cyrillic label) |
| 6 | Line from docker/fixtures/subscription.txt (tcp+tls, ws+tls or reality+vision with Cyrillic label) |
| 7 | Line from docker/fixtures/subscription.txt (tcp+tls, ws+tls or reality+vision with Cyrillic label) |
| 8 | Line from docker/fixtures/subscription.txt (tcp+tls, ws+tls or reality+vision with Cyrillic label) |
| 9 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 10 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 11 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 12 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 13 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 14 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 15 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 16 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 17 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 18 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 19 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 20 | Line from scd/test/fixtures/vpn-sample.txt (parser test fixture) |
| 21 | Uppercase scheme, host case kept, leading-zero port 0443, param keys lowercased |
| 22 | Port 65536 is out of range: invalid_uri |
| 23 | Empty host with port: invalid_uri |
| 24 | Space inside host: invalid_uri |
| 25 | Forbidden host character ^: invalid_uri |
| 26 | Port with trailing letters: invalid_uri |
| 27 | Port with plus sign: invalid_uri |
| 28 | No port: missing_required_field (address and port) |
| 29 | Port 0: missing_required_field (address and port) |
| 30 | Nil UUID is rejected (version/variant) |
| 31 | Uppercase UUID accepted and kept as is |
| 32 | Unknown params with array-index key, empty key, empty segment: details order ["1","zz",""] |
| 33 | Duplicate query key: last wins (sni=b) |
| 34 | Query decoding: + is space, %2F decoded, invalid %zz kept, ; is not a separator |
| 35 | IPv6 literal is compressed and lowercased, brackets kept in address; city slug from address |
| 36 | encryption=None (capitalised): unsupported_value |
| 37 | Empty headerType accepted, headerType "" in normalized |
| 38 | Empty allowInsecure: unsupported_value |
| 39 | type=tcp without security: unsupported_combo, details type/security/flow |
| 40 | REALITY without sid: missing_required_field "sid" |
| 41 | REALITY without flow: unsupported_combo |
| 42 | tcp+tls with flow vision: tcp-tls profile keeps flow |
| 43 | alpn list with space and empty items: ["h2","http/1.1"] |
| 44 | Label with bare %: decodeURIComponent throws URI malformed |
| 45 | Label with invalid UTF-8 escape: URI malformed |
| 46 | Leading U+0085 (NEL) is not JS whitespace: invalid_uri |
| 47 | Other scheme: unsupported_scheme with protocol in message |
| 48 | No :// at all: unsupported_scheme (generic) |
| 49 | vless: without // : unsupported_scheme |
| 50 | Contains :// but is not a URL: invalid_uri |
| 51 | Password in userinfo is ignored |
| 52 | Two @ in userinfo: username a@b is not a UUID |
| 53 | URL path component is ignored |
| 54 | Fragment keeps later # and ? ; flag and city extracted |
| 55 | TAB inside the line is stripped by the URL parser (label ab) but kept in raw |
| 56 | No fragment: empty label, tag falls back to host slug |
| 57 | Empty fragment: empty label |
| 58 | Opaque host: hex IPv4 form is not normalised |
| 59 | Percent escape in host kept literally |
| 60 | IPv4-mapped IPv6 literal serialised as [::ffff:102:304] |
| 61 | Leading/trailing spaces: raw keeps them, tag hash differs from the trimmed line |
| 62 | BOM prefix counts as JS whitespace for trim; raw keeps it, tag hash differs |
| 63 | 09 6.1 reference: tcp+tls with ignored path=/ |
| 64 | 09 6.2 reference: ws+tls with ?ed=2048 inside path and allowInsecure=1 |
| 65 | 09 6.3 reference: tcp+reality+vision |
| 66 | 3x-ui REALITY link with spx: unsupported_param |
| 67 | 3x-ui REALITY link with pqv (ML-DSA verify key): unsupported_param |
| 68 | v2rayN/Xray TLS link with pcs and vcn: unsupported_param listing both |
| 69 | tcp+tls with allowInsecure=1: allowInsecure true |
| 70 | REALITY with empty sid: missing_required_field "sid" |
| 71 | fp=Chrome (capitalised) is kept as is |
| 72 | Malformed pbk: TS does not validate it (accepted verbatim) |
| 73 | xhttp link: TS rejects it as unsupported_param (mode, extra); type, path and host are known params |
| 74 | grpc transport (out of scope): unsupported_param |
| 75 | Links with ech= and fm=: unsupported_param |
| 76 | Valid xray vlessenc client string, X25519 auth (32-byte key), Xray 26.10.10: TS unsupported_value (encryption) |
| 77 | Valid xray vlessenc client string, ML-KEM-768 auth (1184-byte key), Xray 26.10.10: TS unsupported_value (encryption) |
| 78 | NEGATIVE: ML-KEM key of correct length but random bytes; passes conf.Build, live Xray rejects it (mlkem: invalid polynomial encoding); TS rejects it as unsupported_value like any non-none encryption |
| 79 | ML-KEM encryption with security=none: TS unsupported_combo (security=none has no profile) |
| 80 | ws+tls without path: missing_required_field |
| 81 | headerType=http: unsupported_value |
| 82 | allowInsecure=maybe: unsupported_value |
| 83 | Label with a flag that is not a known country: country absent, tag uses xx |
| 84 | Label without flag: city only |
