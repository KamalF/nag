package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KamalF/nag/internal/store"
)

// channelCLI runs `nag channel …` against a fresh database in a temp dir and
// returns the exit code and both streams.
type channelCLI struct {
	t  *testing.T
	db string
}

func newChannelCLI(t *testing.T) *channelCLI {
	t.Helper()
	db := filepath.Join(t.TempDir(), "nag.db")
	setVars(t, map[string]string{"NAG_DB": db})
	return &channelCLI{t: t, db: db}
}

func (c *channelCLI) run(args ...string) (int, string, string) {
	c.t.Helper()
	var stdout, stderr strings.Builder
	code := run(append([]string{"channel"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

const ntfyURL = "ntfy://user:tok_abc123@ntfy.example.com/alerts"

// §7.4: the slug rule, with the pattern printed on a refusal.
func TestChannelAddSlugRule(t *testing.T) {
	for _, tt := range []struct {
		name string
		ok   bool
	}{
		{"ntfy", true},
		{"a", true},
		{"0phone", true},
		{"tg_home-2", true},
		{strings.Repeat("a", 32), true},
		{strings.Repeat("a", 33), false},
		{"NTFY", false},
		{"-ntfy", false},
		{"_ntfy", false},
		{"nt fy", false},
		{"ntfy.home", false},
		{"", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cli := newChannelCLI(t)
			code, _, stderr := cli.run("add", tt.name, ntfyURL)
			if (code == 0) != tt.ok {
				t.Fatalf("add %q = %d (%s), want ok=%v", tt.name, code, stderr, tt.ok)
			}
			if !tt.ok && !strings.Contains(stderr, slugPattern.String()) {
				t.Errorf("refusal does not print the pattern: %q", stderr)
			}
		})
	}
}

// §7.4: add parses the URL before it writes the row, and refuses a duplicate
// name.
func TestChannelAddRefusals(t *testing.T) {
	cli := newChannelCLI(t)
	if code, _, stderr := cli.run("add", "bad", "nosuchservice://x/y"); code != 1 || !strings.Contains(stderr, "shoutrrr") {
		t.Errorf("unparseable URL: code %d, stderr %q", code, stderr)
	}
	if code, _, _ := cli.run("add", "ntfy", ntfyURL); code != 0 {
		t.Fatal("first add failed")
	}
	if code, _, stderr := cli.run("add", "ntfy", ntfyURL); code != 1 || !strings.Contains(stderr, "already exists") {
		t.Errorf("duplicate: code %d, stderr %q", code, stderr)
	}
	_, out, _ := cli.run("list")
	if strings.Count(out, "ntfy ") != 1 || strings.Contains(out, "bad") {
		t.Errorf("list after refusals = %q", out)
	}
}

// No URL is printed unmasked, not even on add's refusal: shoutrrr's parse
// errors embed the whole URL, or only its token.
func TestChannelAddRefusalHidesTheURL(t *testing.T) {
	for _, raw := range []string{
		"ntfy://user:tok_SECRET@ntfy.example.com:badport/alerts",
		"ntfy://user:tok_SECRET%zz@ntfy.example.com/alerts",
		"telegram://tok_SECRET@telegram",
		`ntfy://host/topic?tokn=tk%5Ctok_SECRET`,
		`ntfy://host/topic?tokn=tk%22tok_SECRET`,
		"wecom://0123abcd-tok_SECRET;9", // wecom's key is the host
	} {
		cli := newChannelCLI(t)
		code, _, stderr := cli.run("add", "x", raw)
		if code != 1 {
			t.Errorf("add %q = %d, want a refusal", raw, code)
		}
		if strings.Contains(stderr, "tok_SECRET") {
			t.Errorf("refusal printed the secret: %q", stderr)
		}
		if !strings.Contains(stderr, "shoutrrr cannot use this URL") {
			t.Errorf("refusal does not name the parse error: %q", stderr)
		}
	}
	// a fixed host that is the service's own name is not a secret. If the
	// mask removes it, the message no longer names the service
	cli := newChannelCLI(t)
	_, _, stderr := cli.run("add", "x", "telegram://tok_SECRET@telegram")
	if !strings.Contains(stderr, "telegram: invalid telegram token") {
		t.Errorf("refusal lost the service name: %q", stderr)
	}
}

// A URL typed where the name goes is masked, not echoed: swapped arguments
// on add, or the URL given to test, rm, enable or disable.
func TestURLInTheNameSlotIsMasked(t *testing.T) {
	cli := newChannelCLI(t)
	for _, args := range [][]string{
		{"add", ntfyURL, "phone"},
		{"test", ntfyURL},
		{"rm", ntfyURL},
		{"enable", ntfyURL},
	} {
		code, _, stderr := cli.run(args...)
		if code != 1 || strings.Contains(stderr, "tok_abc123") {
			t.Errorf("channel %v = %d, stderr %q", args, code, stderr)
		}
	}
}

// §7.4's mask, pinned: the scheme, then a literal ellipsis for everything
// else — including a host, which is pushbullet's and ifttt's key.
func TestMaskChannelURL(t *testing.T) {
	for in, want := range map[string]string{
		ntfyURL:                                 "ntfy://…",   // secret in the userinfo
		"gotify://gotify.example.com/AbCdToken": "gotify://…", // in the path
		"pushbullet://o.AbCdEfGhIjKlMnOpQrStUvWxYz012345/device": "pushbullet://…", // in the host, dot and all
	} {
		if got := maskChannelURL(in); got != want {
			t.Errorf("mask(%q) = %q, want %q", in, got, want)
		}
	}
}

// No URL is ever printed unmasked, by add or by list.
func TestChannelListMasksAndShowsState(t *testing.T) {
	cli := newChannelCLI(t)
	_, addOut, _ := cli.run("add", "ntfy", ntfyURL)
	cli.run("disable", "ntfy")
	_, out, _ := cli.run("list")
	for _, printed := range []string{addOut, out} {
		if strings.Contains(printed, "tok_abc123") || strings.Contains(printed, "user") {
			t.Errorf("a credential was printed: %q", printed)
		}
	}
	if !strings.Contains(out, "ntfy://…") || !strings.Contains(out, "disabled") {
		t.Errorf("list = %q", out)
	}
	cli.run("enable", "ntfy")
	if _, out, _ := cli.run("list"); !strings.Contains(out, "enabled") {
		t.Errorf("list after enable = %q", out)
	}
}

// §7.4: rm prints how many reminders still name the channel, then proceeds.
func TestChannelRemoveWarnsAndProceeds(t *testing.T) {
	cli := newChannelCLI(t)
	cli.run("add", "ntfy", ntfyURL)
	st, err := store.Open(cli.db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, channels := range [][]string{{"ntfy"}, {"matrix", "ntfy"}, nil} {
		if _, err := st.CreateReminder(ctx, "x", 4000000000, channels, 1); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	code, out, _ := cli.run("rm", "ntfy")
	if code != 0 || !strings.Contains(out, "2 reminder(s)") {
		t.Errorf("rm = %d, %q", code, out)
	}
	if _, out, _ := cli.run("list"); !strings.Contains(out, "no channels") {
		t.Errorf("list after rm = %q", out)
	}
	if code, _, stderr := cli.run("rm", "ntfy"); code != 1 || !strings.Contains(stderr, "no channel") {
		t.Errorf("second rm = %d, %q", code, stderr)
	}
}

// Every subcommand that opens the database creates and migrates it (§5.1),
// so adding a channel before the first serve is a supported order.
func TestChannelAddCreatesTheDatabase(t *testing.T) {
	cli := newChannelCLI(t)
	if code, _, stderr := cli.run("add", "ntfy", ntfyURL); code != 0 {
		t.Fatalf("add on a missing db = %d: %s", code, stderr)
	}
}

func TestChannelUsage(t *testing.T) {
	cli := newChannelCLI(t)
	for _, args := range [][]string{{}, {"frobnicate"}, {"add", "only-name"}, {"list", "extra"}} {
		if code, _, _ := cli.run(args...); code != 2 {
			t.Errorf("channel %v = %d, want 2", args, code)
		}
	}
}

// §7.4: test sends now, also to a disabled channel, and prints the same
// classification that a delivery_error holds.
func TestChannelTest(t *testing.T) {
	var got string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	defer ok.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()

	cli := newChannelCLI(t)
	cli.run("add", "hook", "generic+"+ok.URL+"/x")
	cli.run("disable", "hook")
	code, out, _ := cli.run("test", "hook")
	if code != 0 || !strings.Contains(out, "hook: sent") || !strings.Contains(out, "disabled") {
		t.Errorf("test = %d, %q", code, out)
	}
	if !strings.Contains(got, "Nag test notification") {
		t.Errorf("server got %q", got)
	}

	cli.run("add", "down", "generic+"+down.URL+"/x?token=secret")
	code, out, _ = cli.run("test", "down")
	if code != 1 || !strings.HasPrefix(out, "down: ") || strings.Contains(out, "secret") {
		t.Errorf("failed test = %d, %q", code, out)
	}
	if code, _, stderr := cli.run("test", "nope"); code != 1 || !strings.Contains(stderr, "no channel") {
		t.Errorf("unknown channel = %d, %q", code, stderr)
	}
}
