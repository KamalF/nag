package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/KamalF/nag/internal/notify"
)

// §7.5's two numbers. The budget is derived from WriteTimeout — written as
// the subtraction rather than as the 12 it works out to, so that tuning the
// write deadline cannot break the derivation quietly: 12 seconds of
// sequential sends deliberately under the 15, so the response always has
// three seconds of the write deadline to itself — this is the one handler
// that legitimately spends seconds on third-party sockets, and also the one
// whose whole value is the answer it prints. The per-send timeout is 3 s
// rather than the sweep's 10 because a diagnostic probe optimises for
// answering the caller: a subscription that cannot answer in three seconds
// comes back as `timeout`, and that *is* the diagnosis.
const (
	pushTestBudget  = WriteTimeout - 3*time.Second
	pushTestPerSend = 3 * time.Second
)

// pushTestResult is one entry of §7.5's response, and every field is
// nullable exactly where §8.2 says: status is null when the attempt
// produced no HTTP status at all — a 0 would be indistinguishable from a
// code and would sort as one — and error is null on success.
type pushTestResult struct {
	Endpoint string  `json:"endpoint"`
	Status   *int    `json:"status"`
	Deleted  bool    `json:"deleted"`
	Error    *string `json:"error"`
}

// handlePushTest is §7.5: one fixed push to every stored subscription,
// through the same §7.1 ladder the sweep uses, with a per-endpoint verdict
// for each. It ignores the 30-minute cooldown and touches no sweep state —
// you are asking whether the transport works, and an endpoint that answers
// "come back in twenty minutes" would be answering a different question.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	// Read before anything that can block. The budget is derived from
	// WriteTimeout (§8.2), and a clock read taken after a query that can wait
	// on the single connection (§4.3) starts the budget seconds into the
	// request — which is the derivation broken quietly.
	now := time.Now()

	subs, err := s.store.ListSubscriptions(r.Context())
	if err != nil {
		s.log.Error("push test: list subscriptions", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	payload, err := notify.TestPayload(now.Unix())
	if err != nil {
		s.log.Error("push test: build payload", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}

	// One clock read before the first send, so the sends together can
	// never run past the budget however many rows there are (§7.5).
	ctx, cancel := context.WithDeadline(r.Context(), now.Add(s.pushBudget))
	defer cancel()

	notAttempted := "not attempted"
	results := make([]pushTestResult, 0, len(subs))
	attempted, deleted := 0, 0
	for _, sub := range subs {
		truncated := notify.TruncateEndpoint(sub.Endpoint)
		if ctx.Err() != nil {
			// reported, never silently omitted: the array always holds
			// exactly one entry per stored subscription, so "no result" is
			// never how a caller learns something (§7.5)
			results = append(results, pushTestResult{Endpoint: truncated, Error: &notAttempted})
			continue
		}
		// The send derives its own deadline from this one, so it really is
		// §7.5's "smaller of 3 seconds and whatever is left of the budget" —
		// and so is the row deletion a 404/410 triggers, which is why the
		// budget bounds the handler rather than only its sends.
		res := s.push.SendTo(ctx, pushTestPerSend, payload, sub)

		attempted++
		if res.Deleted {
			deleted++
		}
		entry := pushTestResult{Endpoint: truncated, Deleted: res.Deleted}
		if res.Status != 0 {
			entry.Status = &res.Status
		}
		if res.Err != "" {
			entry.Error = &res.Err
		}
		results = append(results, entry)
	}

	// §10.4: this endpoint has no reminder id and no single endpoint to
	// truncate, so its line names the only two things it knows — and
	// between them the question anybody reads that line for.
	setLogAttrs(r, "attempted", attempted, "deleted", deleted)
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
