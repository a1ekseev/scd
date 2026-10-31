# D26 corpus keys

Every key in `d26-links.txt` is a real public key/client string printed by the release binary
of Xray 26.10.10 (throwaway keys generated for this corpus only; private parts were not kept and
are not needed). Random bytes appear only in negative lines.

```
$ xray version
Xray 26.10.10 (Xray, Penetrates Everything.) 701af60 (go1.27.0 linux/amd64)
```

Binary: release asset `Xray-linux-64.zip` of `v26.10.10` (sha256 checked against `.dgst`,
see the plan README "Локальный образ Xray"; usable as `SCD_XRAY_BIN`).

| Key | Command | Value used | Lines |
|---|---|---|---|
| X25519 auth (32 bytes, 43 chars) | `xray vlessenc` | client `"encryption"` of block "Authentication: X25519" (`mlkem768x25519plus.native.0rtt.<key>`); the key is reused with other modes/padding | 14, 16, 18, 20, 23, 25, 28, 30, 31, 34, 35, 59, 89 |
| ML-KEM-768 (1184 bytes, 1579 chars) | `xray vlessenc` | client `"encryption"` of block "Authentication: ML-KEM-768, Post-Quantum" | 15, 17, 19, 21, 22, 24, 29, 31, 80 |
| REALITY `pbk` | `xray x25519` | `Password (PublicKey)` | 5, 6, 9, 11, 12, 23, 24, 50–54, 58, 69, 72, 76–79, 81, 94, 95, 99–101 |
| REALITY `pbk`, std base64 | same key | re-encoded as std base64 with `=`, then `url.QueryEscape` | 49 |
| REALITY `pqv` (1952 bytes) | `xray mldsa65` | `Verify` | 53, 72, 77 |
| `ech` (ECHConfigList) | `xray tls ech --serverName ech.example.test` | `ECH config list`, `url.QueryEscape` | 46 |
| RS1 ML-KEM-768 (`native.0rtt`) | — | copied byte-for-byte from `docker/fixtures/real-shapes/remnawave.txt` line 1 (that key was itself made with `xray vlessenc`) | 83 |

Not keys (on purpose):

| Value | Origin | Lines |
|---|---|---|
| 1184 random bytes, base64url | `os.urandom(1184)`; negative: `conf.Build` accepts it, live `AddOutbound` fails `mlkem: invalid polynomial encoding`, normalize skips it with "Invalid VLESS encryption key." (D25) | 33 |
| 16 random bytes as `pbk` | `os.urandom(16)`; negative: `invalid "password"` | 102 |
| `pcs` values | `sha256("d26-pcs-1")` in OpenSSL form (`AB:CD:…`) and `sha256("d26-pcs-2")` as plain hex | 43, 44 |

The X25519 and ML-KEM strings are the "encryption" halves of `xray vlessenc` output; the
matching "decryption" (server) halves were discarded.
