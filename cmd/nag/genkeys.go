package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// placeholderSubject is the literal subject that genkeys writes. Boot
// rejects it (§5.1), so its replacement is the one required hand edit of
// the setup.
const placeholderSubject = "mailto:you@example.com"

// runGenkeys writes a complete, pipeable env file to stdout and nothing
// else, so `docker compose run --rm -T nag genkeys >> nag.env` is safe. All
// advice goes to stderr, so the operator still sees it after the redirection
// (§5.2).
func runGenkeys(stdout, stderr io.Writer) int {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		fmt.Fprintf(stderr, "FATAL: generate token: %v\n", err)
		return 1
	}
	private, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		fmt.Fprintf(stderr, "FATAL: generate VAPID keypair: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "NAG_TOKEN=%s\n", base64.RawURLEncoding.EncodeToString(token))
	fmt.Fprintf(stdout, "NAG_VAPID_PUBLIC=%s\n", public)
	fmt.Fprintf(stdout, "NAG_VAPID_PRIVATE=%s\n", private)
	fmt.Fprintf(stdout, "NAG_VAPID_SUBJECT=%s\n", placeholderSubject)
	fmt.Fprintf(stderr, "Keys written. One edit is still required: replace %s\nwith your own address in nag.env. nag refuses to start while the placeholder is there.\n",
		placeholderSubject)
	return 0
}
