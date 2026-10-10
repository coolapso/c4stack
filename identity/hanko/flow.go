package hanko

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// flowState is one step of a Hanko flow. A state accepts one action; the action's
// response is the next state.
type flowState struct {
	HTTPStatus int             `json:"-"`
	Name       string          `json:"name"`
	Status     int             `json:"status"`
	CSRFToken  string          `json:"csrf_token"`
	Actions    flowActions     `json:"actions"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Error      *struct {
		Code string `json:"code"`
	} `json:"error,omitempty"`
}

type flowActions map[string]struct {
	Href string `json:"href"`
}

// ok reports whether a profile action succeeded and returned to the profile.
func (s flowState) ok() bool {
	return s.HTTPStatus == http.StatusOK && s.Status == http.StatusOK && s.Name == "profile_init" && s.Error == nil
}

// expired reports whether Hanko discarded the flow.
func (s flowState) expired() bool {
	return s.HTTPStatus == http.StatusGone || (s.Error != nil && s.Error.Code == "flow_expired_error")
}

// startProfile starts a profile flow for the session and registers the client's
// capabilities when Hanko asks for them. The server has no WebAuthn.
func (h *Hanko) startProfile(ctx context.Context, token string) (flowState, error) {
	state, err := h.flow(ctx, token, "/profile", map[string]any{"input_data": map[string]any{}})
	if err != nil {
		return flowState{}, err
	}
	if _, ok := state.Actions["register_client_capabilities"]; ok {
		state, err = h.act(ctx, token, state, "register_client_capabilities", map[string]bool{
			"webauthn_available":                        false,
			"webauthn_conditional_mediation_available":  false,
			"webauthn_platform_authenticator_available": false,
		})
		if err != nil {
			return flowState{}, err
		}
	}
	if state.HTTPStatus == http.StatusUnauthorized {
		return flowState{}, errSessionRejected
	}
	return state, nil
}

var errSessionRejected = errors.New("hanko: the session was rejected")

// act runs one action of a flow state.
func (h *Hanko) act(ctx context.Context, token string, state flowState, action string, input any) (flowState, error) {
	a, ok := state.Actions[action]
	if !ok || a.Href == "" {
		return flowState{}, fmt.Errorf("hanko: the flow does not offer %q", action)
	}
	if state.CSRFToken == "" {
		return flowState{}, errors.New("hanko: the flow returned no CSRF token")
	}
	return h.flow(ctx, token, a.Href, map[string]any{"input_data": input, "csrf_token": state.CSRFToken})
}

func (h *Hanko) flow(ctx context.Context, token, href string, payload any) (flowState, error) {
	endpoint, err := h.flowURL(href)
	if err != nil {
		return flowState{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return flowState{}, err
	}
	status, data, err := h.post(ctx, endpoint, token, body)
	if err != nil {
		return flowState{}, err
	}
	state := flowState{HTTPStatus: status}
	if err := json.Unmarshal(data, &state); err != nil {
		return flowState{}, fmt.Errorf("hanko: unreadable flow response (status %d)", status)
	}
	return state, nil
}

// flowURL resolves an action href against the API URL. Hanko returns hrefs without a
// self-hosted base path, so the base path is kept; an href pointing elsewhere is
// refused rather than contacted.
func (h *Hanko) flowURL(href string) (string, error) {
	ref, err := url.Parse(href)
	if err != nil || href == "" {
		return "", errors.New("hanko: invalid flow URL")
	}
	if ref.IsAbs() || ref.Host != "" {
		if ref.Scheme != h.api.Scheme || ref.Host != h.api.Host || ref.User != nil {
			return "", errors.New("hanko: the flow URL points to another origin")
		}
		return ref.String(), nil
	}
	base := strings.TrimSuffix(h.api.Path, "/")
	if base != "" && !strings.HasPrefix(ref.Path, base+"/") {
		ref.Path = base + ref.Path
	}
	target := *h.api
	target.Path = ref.Path
	target.RawQuery = ref.RawQuery
	return target.String(), nil
}

// profile is the part of the profile payload the email change reads.
type profile struct {
	User struct {
		ID     string         `json:"user_id"`
		Emails []profileEmail `json:"emails"`
	} `json:"user"`
	Sessions []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
	} `json:"sessions"`
}

type profileEmail struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	IsPrimary  bool   `json:"is_primary"`
	IsVerified bool   `json:"is_verified"`
}

// profileFor decodes a profile payload and checks it belongs to subject.
func profileFor(state flowState, subject string) (profile, error) {
	var p profile
	if err := json.Unmarshal(state.Payload, &p); err != nil {
		return profile{}, errors.New("hanko: unreadable profile payload")
	}
	if p.User.ID == "" || p.User.ID != subject {
		return profile{}, errors.New("hanko: the profile belongs to another user")
	}
	return p, nil
}

func (p profile) email(match func(profileEmail) bool) (profileEmail, bool) {
	for _, e := range p.User.Emails {
		if match(e) {
			return e, true
		}
	}
	return profileEmail{}, false
}
