// Package wolfssl is a thin cgo wrapper around the subset of the
// wolfSSL C API used by both the tlsclient and tlsserver packages.
//
// This is the only package in the project that touches cgo directly.
// Everything else interacts with wolfSSL through the typed Go functions
// here, which keeps the cgo blast radius small and makes the rest of
// the codebase refactorable without worrying about C type universes.
//
// Lifecycle: Init() must be called exactly once per process before any
// other function. Cleanup() should be called once during process
// shutdown after all CTX/SSL handles are freed. Test binaries handle
// this in their TestMain; production binaries call Init from main.
package wolfssl

/*
#cgo LDFLAGS: -lwolfssl
#include <wolfssl/options.h>
#include <wolfssl/ssl.h>
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// Success is the wolfSSL success sentinel (WOLFSSL_SUCCESS == 1).
// Hardcoded because cgo #define constants aren't always usable in Go
// const expressions, and the value has been stable across all wolfSSL
// versions.
const Success = 1

// Init initializes the wolfSSL library. Must be called exactly once
// per process before any other function in this package.
func Init() {
	if int(C.wolfSSL_Init()) != Success {
		panic("wolfSSL_Init failed")
	}
}

// Cleanup releases wolfSSL library-global resources.
func Cleanup() {
	C.wolfSSL_Cleanup()
}

// --- CTX construction -------------------------------------------------------

// NewServerCtx allocates a TLS 1.2 server context. The returned pointer
// is opaque to callers and must be freed with FreeCtx.
func NewServerCtx() (unsafe.Pointer, error) {
	method := C.wolfTLSv1_2_server_method()
	if method == nil {
		return nil, errors.New("wolfTLSv1_2_server_method returned nil")
	}
	ctx := C.wolfSSL_CTX_new(method)
	if ctx == nil {
		return nil, errors.New("wolfSSL_CTX_new returned nil")
	}
	return unsafe.Pointer(ctx), nil
}

// NewClientCtx allocates a TLS 1.2 client context.
func NewClientCtx() (unsafe.Pointer, error) {
	method := C.wolfTLSv1_2_client_method()
	if method == nil {
		return nil, errors.New("wolfTLSv1_2_client_method returned nil")
	}
	ctx := C.wolfSSL_CTX_new(method)
	if ctx == nil {
		return nil, errors.New("wolfSSL_CTX_new returned nil")
	}
	return unsafe.Pointer(ctx), nil
}

// FreeCtx releases a context allocated by NewServerCtx or NewClientCtx.
// Safe to call with nil.
func FreeCtx(ctx unsafe.Pointer) {
	if ctx == nil {
		return
	}
	C.wolfSSL_CTX_free((*C.WOLFSSL_CTX)(ctx))
}

// --- CTX configuration ------------------------------------------------------

// SetCipherList restricts the context to a specific OpenSSL-format
// cipher list. For CSIP compliance this should always be
// "ECDHE-ECDSA-AES128-CCM-8".
func SetCipherList(ctx unsafe.Pointer, list string) error {
	c := C.CString(list)
	defer C.free(unsafe.Pointer(c))
	if int(C.wolfSSL_CTX_set_cipher_list((*C.WOLFSSL_CTX)(ctx), c)) != Success {
		return fmt.Errorf("wolfSSL_CTX_set_cipher_list(%q) failed", list)
	}
	return nil
}

// UseCertFile loads a PEM-encoded certificate (server cert OR client
// cert depending on context type) into the context.
func UseCertFile(ctx unsafe.Pointer, path string) error {
	c := C.CString(path)
	defer C.free(unsafe.Pointer(c))
	if int(C.wolfSSL_CTX_use_certificate_file(
		(*C.WOLFSSL_CTX)(ctx), c, C.WOLFSSL_FILETYPE_PEM)) != Success {
		return fmt.Errorf("wolfSSL_CTX_use_certificate_file(%q) failed", path)
	}
	return nil
}

// UseKeyFile loads the PEM-encoded private key matching the cert
// loaded by UseCertFile.
func UseKeyFile(ctx unsafe.Pointer, path string) error {
	c := C.CString(path)
	defer C.free(unsafe.Pointer(c))
	if int(C.wolfSSL_CTX_use_PrivateKey_file(
		(*C.WOLFSSL_CTX)(ctx), c, C.WOLFSSL_FILETYPE_PEM)) != Success {
		return fmt.Errorf("wolfSSL_CTX_use_PrivateKey_file(%q) failed", path)
	}
	return nil
}

// LoadVerifyLocations loads the PEM-encoded CA cert that will be used
// to verify the peer's certificate during handshake. For a client this
// is the CA that signs the server cert; for a server this is the CA
// that signs client certs.
func LoadVerifyLocations(ctx unsafe.Pointer, caFile string) error {
	c := C.CString(caFile)
	defer C.free(unsafe.Pointer(c))
	if int(C.wolfSSL_CTX_load_verify_locations(
		(*C.WOLFSSL_CTX)(ctx), c, nil)) != Success {
		return fmt.Errorf("wolfSSL_CTX_load_verify_locations(%q) failed", caFile)
	}
	return nil
}

// RequireClientCert enables full mTLS on a server context. Without
// this call, the server happily accepts unauthenticated clients
// regardless of what CAs are loaded — this is wolfSSL's default
// behavior and the entire reason this bridge exists (the function
// is not exposed by go-wolfssl).
func RequireClientCert(ctx unsafe.Pointer) {
	C.wolfSSL_CTX_set_verify(
		(*C.WOLFSSL_CTX)(ctx),
		C.WOLFSSL_VERIFY_PEER|C.WOLFSSL_VERIFY_FAIL_IF_NO_PEER_CERT,
		nil,
	)
}

// --- SSL (per-connection) ---------------------------------------------------

// NewSSL creates a per-connection SSL session from a context.
func NewSSL(ctx unsafe.Pointer) (unsafe.Pointer, error) {
	ssl := C.wolfSSL_new((*C.WOLFSSL_CTX)(ctx))
	if ssl == nil {
		return nil, errors.New("wolfSSL_new returned nil")
	}
	return unsafe.Pointer(ssl), nil
}

// FreeSSL releases an SSL session. Safe to call with nil.
func FreeSSL(ssl unsafe.Pointer) {
	if ssl == nil {
		return
	}
	C.wolfSSL_free((*C.WOLFSSL)(ssl))
}

// SetFD attaches an SSL session to an existing socket file descriptor.
func SetFD(ssl unsafe.Pointer, fd int) error {
	if int(C.wolfSSL_set_fd((*C.WOLFSSL)(ssl), C.int(fd))) != Success {
		return fmt.Errorf("wolfSSL_set_fd(%d) failed", fd)
	}
	return nil
}

// PeerCertificateDER returns the DER-encoded peer certificate presented
// during the handshake, or nil if no cert was presented.
// Only valid after a successful Accept or Connect call.
func PeerCertificateDER(ssl unsafe.Pointer) []byte {
	x509 := C.wolfSSL_get_peer_certificate((*C.WOLFSSL)(ssl))
	if x509 == nil {
		return nil
	}
	defer C.wolfSSL_X509_free(x509)

	var sz C.int
	der := C.wolfSSL_X509_get_der(x509, &sz)
	if der == nil || sz <= 0 {
		return nil
	}
	return C.GoBytes(unsafe.Pointer(der), sz)
}

// Accept performs the server-side TLS handshake.
func Accept(ssl unsafe.Pointer) error {
	ret := int(C.wolfSSL_accept((*C.WOLFSSL)(ssl)))
	if ret != Success {
		errCode := int(C.wolfSSL_get_error((*C.WOLFSSL)(ssl), C.int(ret)))
		return fmt.Errorf("wolfSSL_accept failed: ret=%d err=%d", ret, errCode)
	}
	return nil
}

// SetVerifyDomain pins the SSL session to the given hostname so that
// wolfSSL verifies the peer cert's CN / Subject Alternative Names
// match host during Connect. Must be called before Connect.
func SetVerifyDomain(ssl unsafe.Pointer, host string) error {
	c := C.CString(host)
	defer C.free(unsafe.Pointer(c))
	if int(C.wolfSSL_check_domain_name((*C.WOLFSSL)(ssl), c)) != Success {
		return fmt.Errorf("wolfSSL_check_domain_name(%q) failed", host)
	}
	return nil
}

// Connect performs the client-side TLS handshake.
func Connect(ssl unsafe.Pointer) error {
	ret := int(C.wolfSSL_connect((*C.WOLFSSL)(ssl)))
	if ret != Success {
		errCode := int(C.wolfSSL_get_error((*C.WOLFSSL)(ssl), C.int(ret)))
		return fmt.Errorf("wolfSSL_connect failed: ret=%d err=%d", ret, errCode)
	}
	return nil
}

// Read reads from an SSL session. Returns the number of bytes read,
// or an error if the read fails.
func Read(ssl unsafe.Pointer, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	n := int(C.wolfSSL_read(
		(*C.WOLFSSL)(ssl),
		unsafe.Pointer(&buf[0]),
		C.int(len(buf)),
	))
	if n < 0 {
		return 0, fmt.Errorf("wolfSSL_read returned %d", n)
	}
	return n, nil
}

// Write writes to an SSL session.
func Write(ssl unsafe.Pointer, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	n := int(C.wolfSSL_write(
		(*C.WOLFSSL)(ssl),
		unsafe.Pointer(&buf[0]),
		C.int(len(buf)),
	))
	if n < 0 {
		return 0, fmt.Errorf("wolfSSL_write returned %d", n)
	}
	return n, nil
}

// Shutdown initiates the TLS close-notify exchange.
func Shutdown(ssl unsafe.Pointer) {
	if ssl == nil {
		return
	}
	C.wolfSSL_shutdown((*C.WOLFSSL)(ssl))
}

// CipherName returns the negotiated cipher suite name.
func CipherName(ssl unsafe.Pointer) string {
	c := C.wolfSSL_get_cipher_name((*C.WOLFSSL)(ssl))
	if c == nil {
		return ""
	}
	return C.GoString(c)
}

// Version returns the negotiated TLS protocol version string.
func Version(ssl unsafe.Pointer) string {
	c := C.wolfSSL_get_version((*C.WOLFSSL)(ssl))
	if c == nil {
		return ""
	}
	return C.GoString(c)
}
