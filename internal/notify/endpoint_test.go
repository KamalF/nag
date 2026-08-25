package notify

import "testing"

func TestTruncateEndpoint(t *testing.T) {
	tests := map[string]string{
		"https://push.example.net/send/AbCdEfGh12345678": "https://push.example.net/…12345678",
		"https://push.example.net/x":                     "https://push.example.net/x",
		"://":                                            "unparseable-endpoint",
	}
	for in, want := range tests {
		if got := TruncateEndpoint(in); got != want {
			t.Errorf("TruncateEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
