package hanko

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHanko imitates the parts of Hanko's API the adapter uses: session validation,
// logout and the profile flow. Each flow state's CSRF token must come back with the
// next action, as with the real API.
type fakeHanko struct {
	t        *testing.T
	srv      *httptest.Server
	basePath string

	mu        sync.Mutex
	sessions  map[string]string // token -> subject
	issuedAt  time.Time
	emails    []profileEmail
	others    []string // other session IDs
	csrf      int
	deleted   bool
	calls     []string
	failNext  map[string]int // action -> HTTP status to answer once
	logoutErr bool
}

func newFakeHanko(t *testing.T, basePath string) *fakeHanko {
	f := &fakeHanko{
		t:        t,
		basePath: basePath,
		sessions: map[string]string{"valid-token": "user-1"},
		issuedAt: time.Now(),
		emails:   []profileEmail{{ID: "email-1", Address: "ada@example.com", IsPrimary: true, IsVerified: true}},
		others:   []string{"session-2"},
		failNext: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+basePath+"/sessions/validate", f.validate)
	mux.HandleFunc("POST "+basePath+"/logout", f.logout)
	mux.HandleFunc("POST "+basePath+"/profile", f.profile)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHanko) url() string { return f.srv.URL + f.basePath }

func (f *fakeHanko) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeHanko) validate(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body struct {
		SessionToken string `json:"session_token"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	subject, ok := f.sessions[body.SessionToken]
	if !ok {
		json.NewEncoder(w).Encode(map[string]any{"is_valid": false})
		return
	}
	primary := f.emails[0]
	for _, e := range f.emails {
		if e.IsPrimary {
			primary = e
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"is_valid": true,
		"claims": map[string]any{
			"subject":   subject,
			"issued_at": f.issuedAt.Format(time.RFC3339),
			"email":     map[string]any{"address": primary.Address, "is_verified": primary.IsVerified},
		},
	})
}

func (f *fakeHanko) logout(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("logout")
	if f.logoutErr {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	c, err := r.Cookie("hanko")
	if err == nil {
		delete(f.sessions, c.Value)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeHanko) state(w http.ResponseWriter, httpStatus int, name string, actions ...string) {
	f.csrf++
	acts := map[string]map[string]string{}
	for _, a := range actions {
		acts[a] = map[string]string{"href": "/profile?action=" + a + "@flow"}
	}
	var sessions []map[string]any
	sessions = append(sessions, map[string]any{"id": "session-1", "current": true})
	for _, id := range f.others {
		sessions = append(sessions, map[string]any{"id": id, "current": false})
	}
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(map[string]any{
		"name":       name,
		"status":     httpStatus,
		"csrf_token": fmt.Sprintf("csrf-%d", f.csrf),
		"actions":    acts,
		"payload":    map[string]any{"user": map[string]any{"user_id": "user-1", "emails": f.emails}, "sessions": sessions},
	})
}

var profileActions = []string{"email_create", "email_set_primary", "email_delete", "session_delete", "account_delete"}

func (f *fakeHanko) profile(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := r.Cookie("hanko")
	if err != nil || f.sessions[c.Value] == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"name": "error"})
		return
	}
	var body struct {
		InputData map[string]any `json:"input_data"`
		CSRFToken string         `json:"csrf_token"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	action := strings.TrimSuffix(r.URL.Query().Get("action"), "@flow")
	if action != "" && body.CSRFToken != fmt.Sprintf("csrf-%d", f.csrf) {
		f.t.Errorf("action %s sent CSRF token %q, want csrf-%d", action, body.CSRFToken, f.csrf)
	}
	f.record(action)
	if status, ok := f.failNext[action]; ok {
		delete(f.failNext, action)
		f.state(w, status, "error")
		return
	}
	input := func(k string) string { s, _ := body.InputData[k].(string); return s }

	switch action {
	case "":
		f.state(w, http.StatusOK, "profile_init", "register_client_capabilities")
	case "register_client_capabilities":
		f.state(w, http.StatusOK, "profile_init", profileActions...)
	case "email_create":
		if input("email") == "taken@example.com" {
			f.state(w, http.StatusBadRequest, "profile_init", profileActions...)
			return
		}
		f.emails = append(f.emails, profileEmail{ID: "email-2", Address: input("email")})
		f.state(w, http.StatusOK, "passcode_confirmation", "verify_passcode", "resend_passcode")
	case "resend_passcode":
		f.state(w, http.StatusOK, "passcode_confirmation", "verify_passcode", "resend_passcode")
	case "verify_passcode":
		if input("code") != "123456" {
			f.state(w, http.StatusBadRequest, "passcode_confirmation", "verify_passcode", "resend_passcode")
			return
		}
		f.emails[len(f.emails)-1].IsVerified = true
		f.state(w, http.StatusOK, "profile_init", profileActions...)
	case "email_set_primary":
		for i := range f.emails {
			f.emails[i].IsPrimary = f.emails[i].ID == input("email_id")
		}
		f.state(w, http.StatusOK, "profile_init", profileActions...)
	case "email_delete":
		f.emails = slices.DeleteFunc(f.emails, func(e profileEmail) bool { return e.ID == input("email_id") })
		f.state(w, http.StatusOK, "profile_init", profileActions...)
	case "session_delete":
		f.others = slices.DeleteFunc(f.others, func(id string) bool { return id == input("session_id") })
		f.state(w, http.StatusOK, "profile_init", profileActions...)
	case "account_delete":
		f.deleted = true
		delete(f.sessions, c.Value)
		f.state(w, http.StatusOK, "account_deleted")
	default:
		f.t.Errorf("unexpected action %q", action)
		w.WriteHeader(http.StatusBadRequest)
	}
}
