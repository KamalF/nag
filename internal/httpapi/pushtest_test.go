package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KamalF/nag/internal/notify"
	"github.com/KamalF/nag/internal/store"
)

// fakePush answers as the §7.1 ladder does, with one canned verdict per
// call. delay lets a test exhaust the send budget without spending twelve
// real seconds on it.
type fakePush struct {
	results  []notify.SendResult
	delay    time.Duration
	payloads []string
	calls    int
}

func (f *fakePush) SendTo(ctx context.Context, perSend time.Duration, payload []byte, _ store.Subscription) notify.SendResult {
	ctx, cancel := context.WithTimeout(ctx, perSend)
	defer cancel()
	f.payloads = append(f.payloads, string(payload))
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
		}
	}
	res := f.results[min(f.calls, len(f.results)-1)]
	f.calls++
	return res
}

// pushTestServer stores subs endpoints and wires the fake sender in.
func pushTestServer(t *testing.T, logs io.Writer, push *fakePush, endpoints ...string) *Server {
	t.Helper()
	s := testServer(t, logs)
	s.push = push
	for _, e := range endpoints {
		if err := s.store.UpsertSubscription(t.Context(), e, "p256dh", "auth", testVAPIDPublic, 1); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func pushTestResults(t *testing.T, s *Server) (int, []pushTestResult) {
	t.Helper()
	rec := authedJSON(t, s.Handler(), http.MethodPost, "/api/push/test", "")
	var body struct {
		Results []pushTestResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not json: %v (%q)", err, rec.Body.String())
	}
	return rec.Code, body.Results
}

// The happy path, and the two things §7.5 pins about it: the endpoint comes
// back truncated, and the payload is the fixed n == 1 form with no id.
func TestPushTestSendsThePinnedPayload(t *testing.T) {
	push := &fakePush{results: []notify.SendResult{{Status: 201}}}
	s := pushTestServer(t, nil, push, "https://push.example.net/send/AbCdEfGh12345678")

	code, results := pushTestResults(t, s)

	if code != http.StatusOK || len(results) != 1 {
		t.Fatalf("status %d with %d results, want 200 with 1", code, len(results))
	}
	got := results[0]
	if got.Status == nil || *got.Status != 201 || got.Deleted || got.Error != nil {
		t.Errorf("result = %+v, want status 201, deleted false, error null", got)
	}
	if got.Endpoint != "https://push.example.net/…12345678" {
		t.Errorf("endpoint = %q, want the truncated form", got.Endpoint)
	}
	var payload struct {
		N     int      `json:"n"`
		ID    *int64   `json:"id"`
		DueAt *int64   `json:"due_at"`
		Texts []string `json:"texts"`
	}
	if err := json.Unmarshal([]byte(push.payloads[0]), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.N != 1 || payload.ID != nil || payload.DueAt == nil {
		t.Errorf("payload = %+v, want n=1 with due_at and no id", payload)
	}
	if len(payload.Texts) != 1 || payload.Texts[0] != "Nag test notification" {
		t.Errorf("texts = %v, want the fixed text", payload.Texts)
	}
}

// "deleted: true on a 403 is the fastest available answer to 'is this
// browser still bound to the keypair I rotated away from'."
func TestPushTestReportsStaleKeyDeletion(t *testing.T) {
	push := &fakePush{results: []notify.SendResult{
		{Status: 403, Deleted: true, Err: "stale vapid key"},
	}}
	s := pushTestServer(t, nil, push, "https://push.example.net/send/AbCdEfGh12345678")

	_, results := pushTestResults(t, s)

	got := results[0]
	if got.Status == nil || *got.Status != 403 || !got.Deleted ||
		got.Error == nil || *got.Error != "stale vapid key" {
		t.Fatalf("result = %+v, want 403 deleted with the stale-key verdict", got)
	}
}

// §8.2: status is null when the attempt produced no HTTP status at all. A 0
// cannot be told apart from a code, and sorts as one.
func TestPushTestReportsNullStatusOnATransportFailure(t *testing.T) {
	push := &fakePush{results: []notify.SendResult{{Err: "timeout"}}}
	s := pushTestServer(t, nil, push, "https://push.example.net/send/AbCdEfGh12345678")

	rec := authedJSON(t, s.Handler(), http.MethodPost, "/api/push/test", "")

	if !strings.Contains(rec.Body.String(), `"status":null`) {
		t.Errorf("body = %q, want a null status", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"timeout"`) {
		t.Errorf("body = %q, want the timeout verdict", rec.Body.String())
	}
}

// "A subscription list with zero rows returns {"results":[]} and a 200,
// not an error; that is itself the answer to 'is anything subscribed'."
func TestPushTestWithNoSubscriptions(t *testing.T) {
	s := pushTestServer(t, nil, &fakePush{results: []notify.SendResult{{Status: 201}}})

	rec := authedJSON(t, s.Handler(), http.MethodPost, "/api/push/test", "")

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"results":[]}` {
		t.Fatalf("status %d body %q, want 200 {\"results\":[]}", rec.Code, rec.Body.String())
	}
}

// Budget exhaustion produces complete results: every stored subscription
// gets an entry, and the ones the budget never reached say so rather than
// going missing (§7.5).
func TestPushTestBudgetExhaustionStillReportsEveryRow(t *testing.T) {
	push := &fakePush{
		results: []notify.SendResult{{Err: "timeout"}},
		delay:   50 * time.Millisecond,
	}
	s := pushTestServer(t, nil, push,
		"https://a.example.net/send/AAAAAAAA",
		"https://b.example.net/send/BBBBBBBB",
		"https://c.example.net/send/CCCCCCCC")
	s.pushBudget = 20 * time.Millisecond // the first send eats it whole

	code, results := pushTestResults(t, s)

	if code != http.StatusOK || len(results) != 3 {
		t.Fatalf("status %d with %d results, want 200 with one entry per subscription", code, len(results))
	}
	if push.calls != 1 {
		t.Fatalf("sent %d times past a spent budget, want 1", push.calls)
	}
	for _, got := range results[1:] {
		if got.Status != nil || got.Deleted || got.Error == nil || *got.Error != "not attempted" {
			t.Errorf("unreached result = %+v, want status null, deleted false, error \"not attempted\"", got)
		}
	}
}

// §10.4: the line for this endpoint carries how many subscriptions it
// attempted and how many rows it deleted, the only two things it knows.
// It carries no endpoint in full.
func TestPushTestLogsAttemptedAndDeletedCounts(t *testing.T) {
	var logs strings.Builder
	push := &fakePush{results: []notify.SendResult{
		{Status: 410, Deleted: true, Err: "http 410"},
		{Status: 201},
	}}
	s := pushTestServer(t, &logs, push,
		"https://a.example.net/send/AAAAAAAA",
		"https://b.example.net/send/BBBBBBBB")

	authedJSON(t, s.Handler(), http.MethodPost, "/api/push/test", "")

	if !strings.Contains(logs.String(), "attempted=2") || !strings.Contains(logs.String(), "deleted=1") {
		t.Errorf("log line missing the counts: %q", logs.String())
	}
	if strings.Contains(logs.String(), "send/AAAAAAAA") {
		t.Errorf("log line carries an endpoint in full: %q", logs.String())
	}
}

// §7.5's budget is derived from WriteTimeout, not stated as a number, so
// that tuning the write deadline cannot quietly leave this handler spending
// longer on third-party sockets than the deadline it must answer within.
// The two checks catch different things. The first cannot fail while the
// derivation stands, because that is what the derivation is for. It is here
// to catch a restatement of the budget as a literal that no longer fits.
func TestPushTestBudgetLeavesHeadroomUnderWriteTimeout(t *testing.T) {
	if pushTestBudget >= WriteTimeout {
		t.Fatalf("budget %s does not fit under WriteTimeout %s", pushTestBudget, WriteTimeout)
	}
	// This is the half that the derivation cannot guarantee. If WriteTimeout
	// is small enough, the budget after the headroom is zero or negative.
	// Then the handler's deadline has already expired at its first send, and
	// every subscription comes back "not attempted" under a 200. That is a
	// diagnostic endpoint that diagnoses nothing.
	if pushTestBudget < pushTestPerSend {
		t.Fatalf("budget %s is under a single send (%s) — the handler could not attempt one",
			pushTestBudget, pushTestPerSend)
	}
}
