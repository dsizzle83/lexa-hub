package main

// cmd_fingerprint.go implements `lexactl fingerprint`: prints the local
// lexa-api HTTPS cert's sha256 fingerprint. A TOFU aid — it reads the cert
// FILE directly rather than asking a running lexa-api about itself, so it
// works even with no lexa-api process up (e.g. right after first boot,
// before the service has started, or while debugging a crash-looping API).
import (
	"fmt"
	"io"
)

func cmdFingerprint(c *client, args []string, stdout io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stdout, "usage: lexactl fingerprint")
		return 2
	}
	fp, err := fingerprintFromCertFile(c.certFile)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, fp)
	return 0
}
