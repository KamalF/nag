package httpapi

import (
	"encoding/json"
	"net/http"
)

// internalErrorMessage is the fixed 5xx sentence (§8.3): the §9.2 toast
// and the §10.4 log both read the response body, so an internal error
// string here would have no single place to be redacted. The real error
// belongs in the request's log line.
const internalErrorMessage = "internal error, see the server log"

// writeError writes the §8.3 one-field shape. The message reaches the UI
// verbatim, so write it for a human — lowercase sentence. For statuses of
// 500 and above the message is ignored and the fixed sentence is sent.
func writeError(w http.ResponseWriter, status int, message string) {
	if status >= 500 {
		message = internalErrorMessage
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
