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
	"sync/atomic"
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
		{"dns", &net.DNSError{Err: "no such host", Name: "ntfy.example.com"}, "dns"},
		{"refused", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, "refused"},
		{"net timeout", &net.OpError{Op: "dial", Err: timeoutErr{}}, "timeout"},
		{"deadline", fmt.Errorf("send: %w", context.DeadlineExceeded), "timeout"},
		{"untypeable", errors.New("something broke"), "send failed"},
		{"a 2xx with a non-JSON body is no failure status", fmt.Errorf("x: %w", jsonclient.Error{StatusCode: 200}), "send failed"},
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

// An abandoned send must still end on its own (§7.2): a generic webhook
// that accepts and never answers would otherwise hold its goroutine and
// socket for the life of the process, one per fan-out.
func TestAbandonedGenericSendEnds(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	old := abandonedSendTimeout
	abandonedSendTimeout = 200 * time.Millisecond
	defer func() { abandonedSendTimeout = old }()

	done := make(chan error, 1)
	go func() { done <- sendOnce(context.Background(), "generic+"+srv.URL+"/hook", "x") }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a send nobody answered succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the generic send never ended on its own")
	}
}

// ntfy keeps the client it builds for its TLS options: a self-signed host
// with disabletlsverification=yes must be reachable.
func TestNtfyKeepsItsTLSOptions(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","time":1,"event":"message","topic":"t"}`)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	if got := SendChannel(t.Context(), "ntfy://"+host+"/t?disabletlsverification=yes", "x"); got != "" {
		t.Errorf("send to a self-signed ntfy = %q, want success", got)
	}
}

// However a service hangs — smtp has no deadline after its dial — a send
// still running keeps new ones to its URL from starting: they are
// NotAttempted, so a hung host costs one goroutine, not one per reminder.
// Past busyFor the slot is taken over, so the channel is never muted for
// good.
func TestStuckSendBlocksItsChannelForAWhile(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			<-release // only the first request hangs
		}
	}))
	defer srv.Close()
	defer close(release)
	url := "generic+" + srv.URL + "/hook"
	old := busyFor
	busyFor = 300 * time.Millisecond
	defer func() { busyFor = old }()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	if got := SendChannel(ctx, url, "x"); got != "timeout" {
		t.Errorf("first send to a hung host = %q, want timeout", got)
	}
	cancel()
	if got := SendChannel(t.Context(), url, "x"); got != NotAttempted {
		t.Errorf("send while the first is stuck = %q, want %q", got, NotAttempted)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests reached the host, want 1", n)
	}

	time.Sleep(busyFor)
	if got := SendChannel(t.Context(), url, "x"); got != "" {
		t.Errorf("send after busyFor = %q, want a real (successful) attempt", got)
	}
}

// A finished send frees its slot before its verdict is returned: sending
// again at once is a real attempt, never a stale "busy".
func TestBackToBackSendsAreNeverBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	url := "generic+" + srv.URL + "/hook"
	for i := range 500 {
		if got := SendChannel(t.Context(), url, "x"); got != "" {
			t.Fatalf("send %d = %q, want success", i, got)
		}
	}
}

// Services that configure their own client must not get an injected one:
// pushbullet would lose its Access-Token, ntfy and gotify their TLS options.
func TestNoClientInjectedIntoSelfConfiguredServices(t *testing.T) {
	for _, scheme := range []string{"pushbullet", "ntfy", "gotify"} {
		if timedClientServices[scheme] {
			t.Errorf("a client is injected into %s", scheme)
		}
	}
}
