package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nicholas-fedor/shoutrrr/pkg/util/jsonclient"
)

// Each §4.1 classification from a typed error, and the floor: anything
// untypeable — including a status flattened into a sentence — is "send
// failed", never a parse of the text.
func TestClassifyChannelError(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"status as a value", fmt.Errorf("ntfy: %w", jsonclient.Error{StatusCode: 401}), "http 401"},
		{"status as a pointer", fmt.Errorf("x: %w", &jsonclient.Error{StatusCode: 503}), "http 503"},
		{"dns", &net.DNSError{Err: "no such host", Name: "ntfy.example.com"}, "dns"},
		{"refused", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, "refused"},
		{"net timeout", &net.OpError{Op: "dial", Err: timeoutErr{}}, "timeout"},
		{"deadline", fmt.Errorf("send: %w", context.DeadlineExceeded), "timeout"},
		{"untypeable", errors.New("something broke"), "send failed"},
		{"status flattened into text", fmt.Errorf("got status 401: %v", jsonclient.Error{StatusCode: 401}), "send failed"},
	} {
		if got := classifyChannelError(tt.err); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// §11: a failure against a URL carrying a token stores neither the URL nor
// the token — the classification is the whole of what comes back.
func TestFailedSendCarriesNoURLAndNoToken(t *testing.T) {
	const token = "tok_s3cr3t_abc123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, "bad token %s at %s", token, r.URL)
	}))
	defer srv.Close()
	url := "generic+" + srv.URL + "/hook?token=" + token

	got := SendChannel(t.Context(), url, "hello")
	if got == "" {
		t.Fatal("a 401 was reported as success")
	}
	for _, leak := range []string{token, srv.URL, "127.0.0.1", "hook"} {
		if strings.Contains(got, leak) {
			t.Errorf("classification %q carries %q", got, leak)
		}
	}
}

func TestSendChannelSuccess(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	if got := SendChannel(t.Context(), "generic+"+srv.URL+"/hook", "Renew the domain"); got != "" {
		t.Fatalf("send = %q, want success", got)
	}
	if !strings.Contains(body, "Renew the domain") {
		t.Errorf("server got %q", body)
	}
}

// §7.2: a context that expires first abandons the send — "timeout", and
// the caller is free at once, whatever the library is still doing.
func TestSendChannelAbandonsOnDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := SendChannel(ctx, "generic+"+srv.URL+"/hook", "x")
	if got != "timeout" {
		t.Errorf("abandoned send = %q, want timeout", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("abandonment took %v; the caller's deadline must bound it", took)
	}
}

func TestValidateChannelURL(t *testing.T) {
	if err := ValidateChannelURL("ntfy://ntfy.example.com/alerts"); err != nil {
		t.Errorf("valid ntfy URL refused: %v", err)
	}
	if err := ValidateChannelURL("nosuchservice://x"); err == nil {
		t.Error("unknown service accepted")
	}
}
