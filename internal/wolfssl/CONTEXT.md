# wolfssl package — context for Step A (completed)

## What this package is

The only cgo package in the repo. Every other package calls these typed Go
wrappers; nothing else touches C directly.

## How to extend it (the pattern)

Every new wolfSSL C function follows the same ~20-line template used for
`RequireClientCert` (around line 143):

1. Look up the C function signature in the wolfSSL manual.
2. Add a Go function that casts the `unsafe.Pointer` args to the right C types,
   calls the C function, and maps the return value to a Go error.
3. No new C files needed — everything goes in `wolfssl.go`.

## Step A: expose the peer certificate — DONE

This wrapper is already implemented — see `PeerCertificateDER` in
`wolfssl.go`. It is built on the same three C calls (the free function is
`wolfSSL_X509_free`):

```c
WOLFSSL_X509 *wolfSSL_get_peer_certificate(WOLFSSL *ssl);
void wolfSSL_X509_free(WOLFSSL_X509 *x509);
unsigned char *wolfSSL_X509_get_der(WOLFSSL_X509 *x509, int *outSz);
```

The server-side wiring that consumes the DER bytes — calling
`PeerCertificateDER` after the handshake, feeding them to
`identity.FromCertificateDER` for the LFDI, and flowing that LFDI into the
gridsim's EndDevice record — does NOT live in this repo. There is no
`tlsserver` package and no `internal/gridsim` here; that side lives in the
bench repo `csip-tls-test`. This repo's wolfSSL usage is client-side
(`Connect`) for lexa-northbound and lexa-telemetry.
