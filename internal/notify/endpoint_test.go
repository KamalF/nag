package notify

import "testing"

func TestTruncateEndpoint(t *testing.T) {
	tests := map[string]string{
		"https://push.example.net/send/AbCdEfGh12345678": "https://push.example.net/…12345678",
		"https://push.example.net/x":                     "https://push.example.net/x",
		"://":                                            "unparseable-endpoint",
		// url.Parse percent-decodes this into Path, so the cut must land on
		// a rune boundary, or the response carries a replacement character
		"https://push.example.net/send/caf%C3%A9-tok%C3%A9n": "https://push.example.net/…-tokén",
	}
	for in, want := range tests {
		if got := TruncateEndpoint(in); got != want {
			t.Errorf("TruncateEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
