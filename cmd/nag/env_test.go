package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func validServeVars() map[string]string {
	return map[string]string{
		"NAG_TOKEN":         strings.Repeat("t", 43),
		"NAG_VAPID_PUBLIC":  base64.RawURLEncoding.EncodeToString(make([]byte, 65)),
		"NAG_VAPID_PRIVATE": base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		"NAG_VAPID_SUBJECT": "mailto:kamal@example.net",
	}
}

func setVars(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, name := range []string{
		"NAG_DB", "NAG_CONFIG", "NAG_ADDR",
		"NAG_TOKEN", "NAG_VAPID_PUBLIC", "NAG_VAPID_PRIVATE", "NAG_VAPID_SUBJECT",
	} {
		t.Setenv(name, vars[name])
	}
}

func TestLoadServeEnvValid(t *testing.T) {
	setVars(t, validServeVars())
	e, err := loadServeEnv()
	if err != nil {
		t.Fatalf("loadServeEnv: %v", err)
	}
	if e.db != defaultDB || e.config != defaultConfig || e.addr != defaultAddr {
		t.Errorf("defaults not applied: %+v", e)
	}
}

func TestLoadServeEnvHTTPSSubject(t *testing.T) {
	vars := validServeVars()
	vars["NAG_VAPID_SUBJECT"] = "https://nag.example.net"
	setVars(t, vars)
	if _, err := loadServeEnv(); err != nil {
		t.Errorf("https: subject rejected: %v", err)
	}
}

func TestLoadServeEnvRefusals(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr []string
	}{
		{
			"no keypair at all prints the fix",
			func(v map[string]string) { v["NAG_VAPID_PUBLIC"] = ""; v["NAG_VAPID_PRIVATE"] = "" },
			[]string{"no VAPID keypair", "genkeys >> nag.env"},
		},
		{
			"missing token",
			func(v map[string]string) { v["NAG_TOKEN"] = "" },
			[]string{"NAG_TOKEN", "20"},
		},
		{
			"short token names the found length",
			func(v map[string]string) { v["NAG_TOKEN"] = "abc123" },
			[]string{"NAG_TOKEN", "6", "20"},
		},
		{
			"public key in standard base64",
			func(v map[string]string) {
				v["NAG_VAPID_PUBLIC"] = base64.StdEncoding.EncodeToString(make([]byte, 65))
			},
			[]string{"NAG_VAPID_PUBLIC", "base64url", "65"},
		},
		{
			"public key with a stray carriage return",
			func(v map[string]string) { v["NAG_VAPID_PUBLIC"] += "\r" },
			[]string{"NAG_VAPID_PUBLIC"},
		},
		{
			"truncated public key names both lengths",
			func(v map[string]string) {
				v["NAG_VAPID_PUBLIC"] = base64.RawURLEncoding.EncodeToString(make([]byte, 63))
			},
			[]string{"NAG_VAPID_PUBLIC", "63", "65"},
		},
		{
			"private key of the wrong length",
			func(v map[string]string) {
				v["NAG_VAPID_PRIVATE"] = base64.RawURLEncoding.EncodeToString(make([]byte, 31))
			},
			[]string{"NAG_VAPID_PRIVATE", "31", "32"},
		},
		{
			"missing private key",
			func(v map[string]string) { v["NAG_VAPID_PRIVATE"] = "" },
			[]string{"NAG_VAPID_PRIVATE", "32"},
		},
		{
			"missing subject",
			func(v map[string]string) { v["NAG_VAPID_SUBJECT"] = "" },
			[]string{"NAG_VAPID_SUBJECT"},
		},
		{
			"placeholder subject rejected by name",
			func(v map[string]string) { v["NAG_VAPID_SUBJECT"] = "mailto:you@example.com" },
			[]string{"NAG_VAPID_SUBJECT", "placeholder", "mailto:you@example.com", "your own address"},
		},
		{
			"subject with a non-mailto non-https scheme",
			func(v map[string]string) { v["NAG_VAPID_SUBJECT"] = "http://nag.example.net" },
			[]string{"NAG_VAPID_SUBJECT", "http://nag.example.net"},
		},
		{
			"relative subject",
			func(v map[string]string) { v["NAG_VAPID_SUBJECT"] = "kamal@example.net" },
			[]string{"NAG_VAPID_SUBJECT", "kamal@example.net"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := validServeVars()
			tt.mutate(vars)
			setVars(t, vars)
			_, err := loadServeEnv()
			if err == nil {
				t.Fatal("loadServeEnv accepted the environment, want refusal")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// §5.1: genkeys exists to produce the required values, so it must run with
// nothing set at all.
func TestGenkeysNeedsNoEnvironment(t *testing.T) {
	setVars(t, nil)
	var stdout, stderr strings.Builder
	if got := run([]string{"genkeys"}, &stdout, &stderr); got != 0 {
		t.Errorf("genkeys exit = %d with an empty environment, want 0", got)
	}
}
