# wolfssl package — context for Step A

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

## Step A: expose the peer certificate

After `wolfssl.Accept(ssl)` succeeds on the server side, the client cert is
available via:

```c
WOLFSSL_X509 *wolfSSL_get_peer_certificate(WOLFSSL *ssl);
void wolfSSL_FreeX509(WOLFSSL_X509 *x509);
unsigned char *wolfSSL_X509_get_der(WOLFSSL_X509 *x509, int *outSz);
```

The Go wrapper to add:

```go
// PeerCertificateDER returns the DER-encoded peer certificate presented
// during the handshake, or nil if no cert was presented.
// The caller must not retain the slice past the next wolfSSL call on ssl.
func PeerCertificateDER(ssl unsafe.Pointer) []byte {
    x509 := C.wolfSSL_get_peer_certificate((*C.WOLFSSL)(ssl))
    if x509 == nil {
        return nil
    }
    defer C.wolfSSL_FreeX509(x509)

    var sz C.int
    der := C.wolfSSL_X509_get_der(x509, &sz)
    if der == nil || sz <= 0 {
        return nil
    }
    return C.GoBytes(unsafe.Pointer(der), sz)
}
```

Then in `tlsserver/server.go`, after `wolfssl.Accept(ssl)` succeeds and
`OnHandshake` fires, call `wolfssl.PeerCertificateDER(ssl)` and pass the
DER bytes to `identity.FromCertificateDER` to get the LFDI.

The LFDI then needs to flow into the gridsim so it can update the EndDevice
record — see `internal/gridsim/CONTEXT.md` for that side.
