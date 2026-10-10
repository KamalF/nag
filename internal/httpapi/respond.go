package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
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

// decodeBody reads the request body as exactly one JSON value into v,
// rejecting unknown fields, and returns the §8.3 message for the client, or
// "" on success. Every handler with a body comes through here, so the rules
// hold everywhere at once:
//
//   - invalid UTF-8 and lone surrogate escapes (\ud800) are refused up
//     front, because encoding/json never refuses them: it decodes both to
//     U+FFFD, silently, and the reminder would be stored altered;
//   - a json.Decoder stops after the first value, so `{…} junk` would
//     otherwise decode cleanly;
//   - an unknown field is named, the same way PATCH names its leftovers.
//
// DisallowUnknownFields has no effect on PATCH's map decode, which checks
// its leftovers itself.
func decodeBody(r *http.Request, v any) string {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "malformed json body" // includes the 16 KB cap (§8.2)
	}
	if !utf8.Valid(raw) || hasLoneSurrogate(raw) {
		return "request body is not valid utf-8"
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		// encoding/json reports an unknown field only in its error text
		if quoted, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			if name, err := strconv.Unquote(quoted); err == nil {
				return unknownFieldMessage(name)
			}
		}
		return "malformed json body"
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "malformed json body"
	}
	return ""
}

func unknownFieldMessage(name string) string {
	return fmt.Sprintf("unknown field %q", name)
}

// hasLoneSurrogate walks the escapes of a JSON text and reports a
// \uD800–\uDFFF escape that is not a high surrogate followed by a low one.
func hasLoneSurrogate(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++ // the escaped character; a \\ pair is skipped whole
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		r, ok := hex4(raw, i+1)
		if !ok {
			return false // malformed: the decoder rejects it on its own
		}
		i += 4
		switch {
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		case r >= 0xD800 && r <= 0xDBFF:
			if !bytes.HasPrefix(raw[i+1:], []byte(`\u`)) {
				return true
			}
			if low, ok := hex4(raw, i+3); !ok || low < 0xDC00 || low > 0xDFFF {
				return true
			}
			i += 6
		}
	}
	return false
}

func hex4(b []byte, at int) (rune, bool) {
	if at+4 > len(b) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(b[at:at+4]), 16, 32)
	return rune(n), err == nil
}
