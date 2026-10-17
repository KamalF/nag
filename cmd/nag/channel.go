package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/KamalF/nag/internal/notify"
	"github.com/KamalF/nag/internal/store"
)

// slugPattern is the channel name rule of §7.4. The name is a JSON array
// element in extra_channels, a chip label and a CLI argument. SQLite's UNIQUE
// on TEXT is case-sensitive. Without this rule, "ntfy" and "NTFY" are two rows
// and two chips for what looks like one channel. A rule that accepts only lowercase at input
// is one rule in one place. The alternative, COLLATE NOCASE, also needs a
// matching case fold at every send.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

const channelUsage = `usage: nag channel add <name> <shoutrrr-url>
       nag channel list
       nag channel rm <name>
       nag channel enable <name>
       nag channel disable <name>
       nag channel test <name>
`

// runChannel is `nag channel …` (§7.4). The final image has no shell and no
// sqlite3, so this command is the only way to manage the table. There is no
// API for it either: a URL carries secrets, and to use this write path you
// must already have a shell on the box. It needs NAG_DB and nothing else. It
// opens the database through the same store.Open as serve, so it creates and
// migrates the file if the file does not exist yet (§5.1).
func runChannel(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, channelUsage)
		return 2
	}
	verb, rest := args[0], args[1:]
	want := map[string]int{"add": 2, "list": 0, "rm": 1, "enable": 1, "disable": 1, "test": 1}
	n, known := want[verb]
	if !known || len(rest) != n {
		fmt.Fprint(stderr, channelUsage)
		return 2
	}

	st, err := store.Open(envOr("NAG_DB", defaultDB))
	if err != nil {
		fmt.Fprintf(stderr, "FATAL: %v\n", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()

	switch verb {
	case "add":
		err = channelAdd(ctx, st, rest[0], rest[1], stdout)
	case "list":
		err = channelList(ctx, st, stdout)
	case "rm":
		err = channelRemove(ctx, st, rest[0], stdout)
	case "enable", "disable":
		err = channelSetEnabled(ctx, st, rest[0], verb == "enable", stdout)
	case "test":
		err = channelTest(ctx, st, rest[0], stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", describeChannelError(err))
		return 1
	}
	return 0
}

// describeChannelError says in plain words what a busy database means: the
// serving container holds the write lock, and busy_timeout ran out (§7.4).
func describeChannelError(err error) error {
	if store.IsBusy(err) {
		return errors.New("the database is locked: the running server is probably writing to it. Retry in a moment.")
	}
	return err
}

// channelAdd gives the URL to shoutrrr's router before it writes the row. If
// the router cannot parse the URL, channelAdd refuses it and names the parse
// error. Without this check, the row stores a typo with no complaint, and the
// typo shows weeks later as a delivery_error on a reminder that you cared
// about.
func channelAdd(ctx context.Context, st *store.Store, name, rawURL string, stdout io.Writer) error {
	if !slugPattern.MatchString(name) {
		return fmt.Errorf("channel name %s must match %s", shownName(name), slugPattern)
	}
	if err := notify.ValidateChannelURL(rawURL); err != nil {
		return fmt.Errorf("shoutrrr cannot use this URL: %s", redactURL(err.Error(), rawURL))
	}
	if err := st.AddChannel(ctx, name, rawURL); err != nil {
		if errors.Is(err, store.ErrChannelExists) {
			return fmt.Errorf("a channel named %q already exists — to change its URL, run `nag channel rm %s`, then add it again", name, name)
		}
		return err
	}
	fmt.Fprintf(stdout, "added %s (%s)\n", name, maskChannelURL(rawURL))
	return nil
}

// channelList prints every channel with its URL masked, never in full.
func channelList(ctx context.Context, st *store.Store, stdout io.Writer) error {
	rows, err := st.ChannelRows(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no channels")
		return nil
	}
	for _, c := range rows {
		fmt.Fprintf(stdout, "%-32s %-8s %s\n", c.Name, stateWord(c.Enabled), maskChannelURL(c.URL))
	}
	return nil
}

// channelRemove warns and then removes the channel. There is no --force,
// because there are no flags (§3). A confirmation prompt is of no use in a
// compose run, which is not always a TTY. Reminders that still name the
// channel become orphans, and orphans are defined behavior (§4.1).
func channelRemove(ctx context.Context, st *store.Store, name string, stdout io.Writer) error {
	refs, err := st.ChannelReferences(ctx, name)
	if err != nil {
		return err
	}
	if err := st.RemoveChannel(ctx, name); err != nil {
		if errors.Is(err, store.ErrNoSuchChannel) {
			return noSuchChannel(name)
		}
		return err
	}
	fmt.Fprintf(stdout, "removed %s\n", name)
	if refs > 0 {
		fmt.Fprintf(stdout, "warning: %d reminder(s) still name %s. They skip it when they fire, and keep the name until you edit it out.\n",
			refs, name)
	}
	return nil
}

func channelSetEnabled(ctx context.Context, st *store.Store, name string, enabled bool, stdout io.Writer) error {
	if err := st.SetChannelEnabled(ctx, name, enabled); err != nil {
		if errors.Is(err, store.ErrNoSuchChannel) {
			return noSuchChannel(name)
		}
		return err
	}
	fmt.Fprintf(stdout, "%s %s\n", name, stateWord(enabled))
	return nil
}

func stateWord(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func noSuchChannel(name string) error {
	return fmt.Errorf("no channel named %s", shownName(name))
}

// shownName quotes a name for an error message. The exception is a URL typed
// where the name goes (`add` with its arguments swapped, or `test` given the
// URL). A quoted URL prints the credential back, so shownName masks it like
// any URL that this command shows (§7.4).
func shownName(name string) string {
	if strings.Contains(name, "://") {
		return maskChannelURL(name)
	}
	return strconv.Quote(name)
}

// testMessage is what `nag channel test` sends (§7.4).
const testMessage = "Nag test notification"

// errTestFailed makes a failed test exit 1 after its line is printed.
var errTestFailed = errors.New("the test send failed")

// channelTest is the fan-out counterpart of POST /api/push/test. It answers
// "did that work" at the moment you paste a URL, not when a reminder fires.
// It also sends to a disabled channel, and says so: the question is about
// the URL, not about the mute switch. It uses the sweep's 10-second wrapper
// and classification. As a result, the word that it prints for a dead host
// is the word that a delivery_error holds for the same failure. No HTTP
// WriteTimeout applies to this command, so it needs no shorter time budget
// (§7.4).
func channelTest(ctx context.Context, st *store.Store, name string, stdout io.Writer) error {
	c, err := st.ChannelByName(ctx, name)
	if errors.Is(err, store.ErrNoSuchChannel) {
		return noSuchChannel(name)
	}
	if err != nil {
		return err
	}
	note := ""
	if !c.Enabled {
		note = " (disabled: sent anyway, but reminders skip it)"
	}
	if verdict := notify.SendChannel(ctx, c.URL, testMessage); verdict != "" {
		fmt.Fprintf(stdout, "%s: %s%s\n", name, verdict, note)
		return errTestFailed
	}
	fmt.Fprintf(stdout, "%s: sent%s\n", name, note)
	return nil
}

// redactURL removes the URL and its secret-bearing parts from a library
// message before the message is printed. shoutrrr's parse errors often embed
// the whole URL, or only its token ("invalid telegram token: …"). No URL is
// ever printed unmasked (§7.4), not even to the operator who typed it,
// because `compose run` output is captured wherever it goes. What is left is
// the parse error itself, and add must name that error.
func redactURL(message, rawURL string) string {
	secrets := []string{rawURL}
	if u, err := url.Parse(rawURL); err == nil {
		// some services keep the key in the host (wecom://<key>). Others
		// use their own name as a fixed host (telegram://…@telegram), and
		// that host is kept so that the message still names the service
		service, _, _ := strings.Cut(u.Scheme, "+")
		for _, host := range []string{u.Host, u.Hostname()} {
			if !strings.EqualFold(host, service) {
				secrets = append(secrets, host)
			}
		}
		if u.User != nil {
			secrets = append(secrets, u.User.String(), u.User.Username())
			if p, ok := u.User.Password(); ok {
				secrets = append(secrets, p)
			}
		}
		secrets = append(secrets, u.RawQuery)
		for _, values := range u.Query() {
			secrets = append(secrets, values...)
		}
		secrets = append(secrets, strings.Split(strings.Trim(u.Path, "/"), "/")...)
	} else if _, rest, ok := strings.Cut(rawURL, "://"); ok {
		// unparseable: all text after the scheme can be a credential
		secrets = append(secrets, rest)
	}
	// shoutrrr prints values through %q, which escapes a backslash or a
	// quote. So the search also looks for each secret in its quoted form
	for _, secret := range secrets {
		secrets = append(secrets, strings.Trim(strconv.Quote(secret), `"`))
	}
	// longest first, so a whole URL is replaced before its parts
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, secret := range secrets {
		if len(secret) >= 3 {
			message = strings.ReplaceAll(message, secret, "…")
		}
	}
	return message
}

// maskChannelURL is the pinned mask of §7.4: the scheme, then a literal "…"
// that stands for everything else. Services put the secret in different
// places. It can be in the userinfo and the query (ntfy, telegram), or in the
// last path segment (gotify's token, a webhook's, a topic on public ntfy.sh).
// It can also be the host itself (ifttt's and wecom's keys, pushbullet's
// dotted o.… token). So no part after the scheme is safe to print, and only a
// per-service table can find the secret. The channel's name says which
// channel it is.
//
//	ntfy://user:tok_abc123@ntfy.example.com/alerts → ntfy://…
func maskChannelURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "(unparseable URL)"
	}
	return u.Scheme + "://…"
}
