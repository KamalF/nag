package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/KamalF/nag/internal/notify"
)

// These are the two numbers of §7.5. The budget is derived from
// WriteTimeout. It is written as the subtraction, not as its result of 12
// seconds, so that a change to WriteTimeout cannot silently break the
// relation between the two. The 12 seconds of sequential sends stay
// deliberately under the 15 seconds of WriteTimeout, so the response always
// has three seconds of the write deadline to itself. This is the one
// handler that legitimately spends seconds on third-party sockets. It is
// also the one whose whole value is the answer that it returns. The
// per-send timeout is 3 s, not the sweep's 10, because this diagnostic
// probe gives priority to an answer for the caller, not to patient
// delivery. A subscription that cannot answer in three seconds comes back
// as `timeout`, and that *is* the diagnosis.
const (
	pushTestBudget  = WriteTimeout - 3*time.Second
	pushTestPerSend = 3 * time.Second
)

// pushTestResult is one entry of §7.5's response. Every field is nullable
// exactly where §8.2 says. status is null when the attempt produced no
// HTTP status at all, because a client cannot tell a 0 apart from a real
// code, and a 0 sorts as one. error is null on success.
type pushTestResult struct {
	Endpoint string  `json:"endpoint"`
	Status   *int    `json:"status"`
	Deleted  bool    `json:"deleted"`
	Error    *string `json:"error"`
}

// handlePushTest is §7.5: one fixed push to every stored subscription,
// through the same §7.1 response handling that the sweep uses, with one
// verdict per endpoint. It ignores the 30-minute cooldown and touches no
// sweep state. The caller asks whether the transport works. An endpoint
// that answers "come back in twenty minutes" answers a different question.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	// The handler reads the clock before anything that can block. The
	// budget is derived from WriteTimeout (§8.2). A query can wait for the
	// single database connection (§4.3). If the clock read came after the
	// query, the budget started seconds into the request, and the derivation
	// broke silently.
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

	// The deadline comes from the one clock read before the first send. So
	// the sends together can never run past the budget, however many rows
	// there are (§7.5).
	ctx, cancel := context.WithDeadline(r.Context(), now.Add(s.pushBudget))
	defer cancel()

	notAttempted := "not attempted"
	results := make([]pushTestResult, 0, len(subs))
	attempted, deleted := 0, 0
	for _, sub := range subs {
		truncated := notify.TruncateEndpoint(sub.Endpoint)
		if ctx.Err() != nil {
			// A subscription that the budget did not reach is reported,
			// never silently omitted: the array always holds exactly one
			// entry per stored subscription. A caller never learns
			// something from a missing result (§7.5).
			results = append(results, pushTestResult{Endpoint: truncated, Error: &notAttempted})
			continue
		}
		// The send derives its own deadline from this one. So the send
		// really gets §7.5's
		// "smaller of 3 seconds and whatever is left of the budget".
		// The row deletion that a 404/410 triggers gets the same deadline.
		// That is why the budget bounds the handler and not only its sends.
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

	// §10.4: this endpoint has no reminder id and no single push endpoint
	// to truncate, so its line names the only two things that it knows.
	// Together they answer the question that anybody reads that line for.
	setLogAttrs(r, "attempted", attempted, "deleted", deleted)
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
