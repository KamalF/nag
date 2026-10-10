package main

import (
	"context"
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

// §7.4: the URL is parsed before the row is written, and a duplicate name
// is refused.
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

// §7.4's mask, pinned: scheme, host, last path segment; userinfo and query
// always elided behind a literal ellipsis.
func TestMaskChannelURL(t *testing.T) {
	for in, want := range map[string]string{
		ntfyURL: "ntfy://ntfy.example.com/…/alerts",
		"telegram://123:token@telegram?chats=@me":        "telegram://telegram/…",
		"ntfy://ntfy.example.com/a/b/c/topic?priority=5": "ntfy://ntfy.example.com/…/topic",
		"generic://hook.example.com/":                    "generic://hook.example.com/…",
		"smtp://u:p@mail.example.com:587/?to=me@x.y":     "smtp://mail.example.com:587/…",
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
	if !strings.Contains(out, "ntfy://ntfy.example.com/…/alerts") || !strings.Contains(out, "disabled") {
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
