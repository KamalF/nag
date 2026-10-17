package httpapi

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KamalF/nag/web"
)

// unknownPresetPrefix is the start of the 400 that app.js reads to repair
// its chips (§9.2). The server writes the message, and the client matches
// its prefix, so a reword on one side breaks the repair silently.
const unknownPresetPrefix = "unknown preset "

func TestUnknownPresetErrorKeepsTheClientPrefix(t *testing.T) {
	h := testServer(t, nil).Handler()
	created := createReminder(t, h, fmt.Sprintf(`{"text":"x","due_at":%d}`, time.Now().Unix()+3600))

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"POST":  authedJSON(t, h, http.MethodPost, "/api/reminders", `{"text":"x","preset":"gone"}`),
		"PATCH": patch(t, h, created["id"], `{"preset":"gone"}`),
	} {
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body %s: %v", name, rec.Body.String(), err)
		}
		if rec.Code != http.StatusBadRequest || !strings.HasPrefix(body.Error, unknownPresetPrefix) {
			t.Errorf("%s: %d %q, want a 400 that starts with %q", name, rec.Code, body.Error, unknownPresetPrefix)
		}
	}

	appJS, err := fs.ReadFile(web.Files, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(appJS), "startsWith('"+unknownPresetPrefix+"')") {
		t.Errorf("app.js no longer matches the prefix %q", unknownPresetPrefix)
	}
}

// The 16 KB cap of §8.2 also covers /login, which needs no token: without
// it, anyone can stream a large body into the decoder.
func TestBodyOverTheCapIsRejectedBeforeAuth(t *testing.T) {
	h := testServer(t, nil).Handler()
	big := `{"token":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertBadRequest(t, rec, "larger than 16 kb")
}
