package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenkeysStdoutIsACompleteEnvFile(t *testing.T) {
	var stdout, stderr strings.Builder
	if got := run([]string{"genkeys"}, &stdout, &stderr); got != 0 {
		t.Fatalf("exit = %d (stderr %q), want 0", got, stderr.String())
	}

	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("stdout has %d lines, want exactly 4:\n%s", len(lines), stdout.String())
	}
	values := map[string]string{}
	for i, prefix := range []string{
		"NAG_TOKEN=", "NAG_VAPID_PUBLIC=", "NAG_VAPID_PRIVATE=", "NAG_VAPID_SUBJECT=",
	} {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("line %d = %q, want prefix %q", i+1, lines[i], prefix)
		}
		values[prefix] = strings.TrimPrefix(lines[i], prefix)
	}

	if token := values["NAG_TOKEN="]; len(token) != 43 {
		t.Errorf("token is %d chars, want 43 (32 bytes base64url unpadded)", len(token))
	}
	assertBase64URLLen(t, "NAG_VAPID_PUBLIC", values["NAG_VAPID_PUBLIC="], 65)
	assertBase64URLLen(t, "NAG_VAPID_PRIVATE", values["NAG_VAPID_PRIVATE="], 32)
	if got := values["NAG_VAPID_SUBJECT="]; got != "mailto:you@example.com" {
		t.Errorf("subject = %q, want the literal placeholder", got)
	}

	if stderr.String() == "" {
		t.Error("stderr is empty, want the advisory naming the required subject edit")
	}
}

func assertBase64URLLen(t *testing.T, name, value string, wantBytes int) {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Errorf("%s %q is not unpadded base64url: %v", name, value, err)
		return
	}
	if len(decoded) != wantBytes {
		t.Errorf("%s decodes to %d bytes, want %d", name, len(decoded), wantBytes)
	}
}
