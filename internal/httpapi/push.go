package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KamalF/nag/internal/notify"
)

// handleSubscribe is POST /api/push/subscribe (§8.2): shape-check all
// three cryptographic fields, upsert by endpoint, 204 with no body — there
// is no subscription object in the API, so the only thing a body could
// carry is an echo of the request.
func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		ApplicationServerKey string `json:"application_server_key"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "malformed json body")
		return
	}
	if msg := endpointError(body.Endpoint); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// The same shape check §5.1 applies to the server's own keys: a
	// mangled value here is accepted by the table and by nothing after it
	// — it would surface as a send failure on every sweep (§8.2).
	for _, check := range []struct {
		field, value string
		wantBytes    int
	}{
		{"application_server_key", body.ApplicationServerKey, 65},
		{"keys.p256dh", body.Keys.P256dh, 65},
		{"keys.auth", body.Keys.Auth, 16},
	} {
		if msg := base64FieldError(check.field, check.value, check.wantBytes); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}

	// stored key = the key the client says it subscribed under, never the
	// configured one (§7.1)
	err := s.store.UpsertSubscription(r.Context(), body.Endpoint,
		body.Keys.P256dh, body.Keys.Auth, body.ApplicationServerKey, time.Now().Unix())
	if err != nil {
		s.log.Error("subscribe", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	setLogID(r, notify.TruncateEndpoint(body.Endpoint)) // never the URL (§10.4)
	w.WriteHeader(http.StatusNoContent)
}

// handleUnsubscribe is POST /api/push/unsubscribe: {endpoint} → 204,
// stranger or not. The row that strands when a client can't call this is
// deleted on the next 404/410 anyway (§7.1).
func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "malformed json body")
		return
	}
	if err := s.store.DeleteSubscription(r.Context(), body.Endpoint); err != nil {
		s.log.Error("unsubscribe", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	setLogID(r, notify.TruncateEndpoint(body.Endpoint))
	w.WriteHeader(http.StatusNoContent)
}

func endpointError(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "endpoint must be an absolute https: url"
	}
	return ""
}

// base64FieldError wants unpadded base64url decoding to exactly wantBytes,
// rejected with a 400 naming the field (§8.2). CR/LF is rejected before
// decoding — Go's decoder ignores those bytes silently.
func base64FieldError(field, value string, wantBytes int) string {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Sprintf("%s contains a carriage return or newline", field)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return fmt.Sprintf("%s is not unpadded base64url (expected %d decoded bytes)", field, wantBytes)
	}
	if len(decoded) != wantBytes {
		return fmt.Sprintf("%s decodes to %d bytes, expected exactly %d", field, len(decoded), wantBytes)
	}
	return ""
}
