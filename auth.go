package main

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type Role uint8

const (
	RoleDenied Role = iota
	RoleViewer
	RoleAdmin
)

func (r Role) String() string {
	switch r {
	case RoleAdmin:
		return "admin"
	case RoleViewer:
		return "viewer"
	default:
		return "denied"
	}
}

type Principal struct {
	Subject string `json:"subject"`
	Role    Role   `json:"-"`
}

type principalKey struct{}

func requestPrincipal(r *http.Request) Principal {
	p, _ := r.Context().Value(principalKey{}).(Principal)
	return p
}

type rolePolicy struct {
	AdminSubjects  []string `json:"adminSubjects"`
	ViewerSubjects []string `json:"viewerSubjects"`
	AdminGroups    []string `json:"adminGroups"`
	ViewerGroups   []string `json:"viewerGroups"`
}

func (p rolePolicy) role(subject string, groups []string) Role {
	if subject == "" {
		return RoleDenied
	}
	if slices.Contains(p.AdminSubjects, subject) || overlaps(p.AdminGroups, groups) {
		return RoleAdmin
	}
	if slices.Contains(p.ViewerSubjects, subject) || overlaps(p.ViewerGroups, groups) {
		return RoleViewer
	}
	return RoleDenied
}

func overlaps(a, b []string) bool {
	for _, value := range a {
		if slices.Contains(b, value) {
			return true
		}
	}
	return false
}

type oidcSettings struct {
	Issuer, ClientID, ClientSecret, Origin, GroupsClaim string
	Scopes                                              []string
	Policy                                              rolePolicy
}

type loginTransaction struct {
	Browser, Nonce, Verifier string
	Expires                  time.Time
	Consumed                 bool
}

type authSession struct {
	Principal Principal
	CSRF      string
	Expires   time.Time
	Browser   string
}

const (
	sessionCookie = "__Host-rsdw-session"
	loginCookie   = "__Host-rsdw-login"
	maxSessions   = 4096
	maxLogins     = 1024
)

type Auth struct {
	token     string
	demo      bool
	settings  oidcSettings
	verifier  *oidc.IDTokenVerifier
	oauth     oauth2.Config
	client    *http.Client
	mu        sync.Mutex
	sessions  map[string]authSession
	pending   map[string]loginTransaction
	exchanges chan struct{}
}

func authFromEnv(ctx context.Context, demo bool) (*Auth, error) {
	mode := envOr("RSDW_AUTH_MODE", "token")
	if mode == "token" {
		for _, env := range os.Environ() {
			if strings.HasPrefix(env, "RSDW_OIDC_") && strings.SplitN(env, "=", 2)[1] != "" {
				return nil, errors.New("OIDC settings require RSDW_AUTH_MODE=oidc")
			}
		}
		token := os.Getenv("RSDW_ADMIN_TOKEN")
		if token == "" && !demo {
			return nil, errors.New("RSDW_ADMIN_TOKEN is required in token mode")
		}
		return &Auth{token: token, demo: demo}, nil
	}
	if mode != "oidc" || os.Getenv("RSDW_ADMIN_TOKEN") != "" {
		return nil, errors.New("select exactly one authentication mode")
	}
	settings := oidcSettings{
		Issuer: os.Getenv("RSDW_OIDC_ISSUER"), ClientID: os.Getenv("RSDW_OIDC_CLIENT_ID"),
		ClientSecret: os.Getenv("RSDW_OIDC_CLIENT_SECRET"), Origin: os.Getenv("RSDW_OIDC_PUBLIC_ORIGIN"),
		GroupsClaim: envOr("RSDW_OIDC_GROUPS_CLAIM", "groups"), Scopes: strings.Fields(envOr("RSDW_OIDC_SCOPES", "openid profile")),
	}
	decoder := json.NewDecoder(strings.NewReader(os.Getenv("RSDW_OIDC_ROLE_POLICY")))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings.Policy); err != nil {
		return nil, errors.New("RSDW_OIDC_ROLE_POLICY must be a role policy JSON object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid role policy JSON")
	}
	return newOIDCAuth(ctx, settings, http.DefaultTransport)
}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

func issuerURL(raw string, origin bool) bool {
	if !httpsURL(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	return u.RawQuery == "" && !u.ForceQuery && (!origin || (u.Path == "" && u.RawPath == ""))
}

func newOIDCAuth(ctx context.Context, settings oidcSettings, transport http.RoundTripper) (*Auth, error) {
	if !issuerURL(settings.Issuer, false) || !issuerURL(settings.Origin, true) || strings.TrimSpace(settings.ClientID) == "" || settings.ClientSecret == "" || settings.GroupsClaim == "" {
		return nil, errors.New("OIDC requires an HTTPS issuer, HTTPS public origin without a path, client ID, client secret and groups claim")
	}
	origin, _ := url.Parse(settings.Origin)
	origin.Host = strings.ToLower(origin.Host)
	if origin.Port() == "443" {
		origin.Host = strings.TrimSuffix(origin.Host, ":443")
	}
	settings.Origin = origin.String()
	count := 0
	for _, entries := range [][]string{settings.Policy.AdminSubjects, settings.Policy.ViewerSubjects, settings.Policy.AdminGroups, settings.Policy.ViewerGroups} {
		for _, entry := range entries {
			if strings.TrimSpace(entry) == "" {
				return nil, errors.New("role policy entries cannot be empty")
			}
			count++
		}
	}
	if count == 0 {
		return nil, errors.New("OIDC requires explicit role assignments")
	}
	if !slices.Contains(settings.Scopes, oidc.ScopeOpenID) {
		return nil, errors.New("OIDC scopes must include openid")
	}
	if slices.Contains(settings.Scopes, "offline_access") {
		return nil, errors.New("offline_access is not supported")
	}
	client := &http.Client{Transport: oidcTransport{transport}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("OIDC redirects are not allowed") }}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), settings.Issuer)
	if err != nil {
		return nil, errors.New("OIDC discovery failed")
	}
	var discovery struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := provider.Claims(&discovery); err != nil || !httpsURL(discovery.JWKS) || !httpsURL(provider.Endpoint().AuthURL) || !httpsURL(provider.Endpoint().TokenURL) {
		return nil, errors.New("OIDC discovery requires HTTPS endpoints")
	}
	return &Auth{settings: settings, client: client,
		verifier: provider.Verifier(&oidc.Config{ClientID: settings.ClientID}),
		oauth:    oauth2.Config{ClientID: settings.ClientID, ClientSecret: settings.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: settings.Origin + "/api/auth/callback", Scopes: settings.Scopes},
		sessions: make(map[string]authSession), pending: make(map[string]loginTransaction), exchanges: make(chan struct{}, 8)}, nil
}

type oidcTransport struct{ base http.RoundTripper }

func (t oidcTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, errors.New("OIDC requires HTTPS")
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("OIDC response exceeds limit or cannot be read")
	}
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	return response, nil
}

func (a *Auth) isOIDC() bool { return a != nil && a.verifier != nil }

func (a *Auth) prune(now time.Time) {
	for id, session := range a.sessions {
		if !now.Before(session.Expires) {
			delete(a.sessions, id)
		}
	}
	for id, transaction := range a.pending {
		if !now.Before(transaction.Expires) {
			delete(a.pending, id)
		}
	}
}

func (a *Auth) authenticate(r *http.Request) (Principal, authSession) {
	if a == nil {
		return Principal{}, authSession{}
	}
	if !a.isOIDC() {
		if (a.demo && a.token == "") || (a.token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.token)) == 1) {
			return Principal{Subject: "token-admin", Role: RoleAdmin}, authSession{}
		}
		return Principal{}, authSession{}
	}
	if r.Header.Get("Authorization") != "" {
		return Principal{}, authSession{}
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return Principal{}, authSession{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prune(time.Now())
	session := a.sessions[cookie.Value]
	return session.Principal, session
}

func capabilities(role Role) map[string]bool {
	read, admin := role == RoleViewer || role == RoleAdmin, role == RoleAdmin
	return map[string]bool{"dashboard": read, "telemetry": read, "events": admin, "maintenance": admin, "create": admin, "restart": admin, "update": admin, "logs": admin, "updateCheck": admin}
}

func viewerRoute(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.URL.Path == "/api/bootstrap" {
		return true
	}
	parts := strings.Split(r.URL.Path, "/")
	return len(parts) == 5 && parts[1] == "api" && parts[2] == "servers" && parts[3] != "" && parts[4] == "telemetry"
}

func (a *Auth) authorize(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	p, session := a.authenticate(r)
	if p.Role == RoleDenied {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return r, false
	}
	if a.isOIDC() {
		w.Header().Set("X-RSDW-Session", session.CSRF)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !a.validCSRF(r, session) {
			writeError(w, http.StatusForbidden, "request verification failed")
			return r, false
		}
	}
	if p.Role != RoleAdmin && !viewerRoute(r) {
		writeError(w, http.StatusForbidden, "permission denied")
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p)), true
}

func (a *Auth) validCSRF(r *http.Request, session authSession) bool {
	return r.Header.Get("Origin") == a.settings.Origin && session.CSRF != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRF)) == 1
}

func authCookie(w http.ResponseWriter, name, value string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if value == "" {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: maxAge})
}

func (a *Auth) routes(w http.ResponseWriter, r *http.Request) bool {
	if a.isOIDC() && (len(r.CookiesNamed(sessionCookie)) > 1 || len(r.CookiesNamed(loginCookie)) > 1) {
		writeError(w, http.StatusBadRequest, "ambiguous authentication cookies")
		return true
	}
	if r.URL.Path == "/api/auth" && r.Method == http.MethodGet {
		p, session := a.authenticate(r)
		mode := "token"
		if a.isOIDC() {
			mode = "oidc"
		}
		writeJSON(w, http.StatusOK, map[string]any{"mode": mode, "required": a == nil || a.isOIDC() || a.token != "" || !a.demo, "authenticated": p.Role != RoleDenied, "subject": p.Subject, "role": p.Role.String(), "csrfToken": session.CSRF, "capabilities": capabilities(p.Role)})
		return true
	}
	if a.isOIDC() {
		switch r.URL.Path {
		case "/api/auth/login":
			if r.Method == http.MethodGet {
				a.login(w, r)
				return true
			}
		case "/api/auth/callback":
			if r.Method == http.MethodGet {
				a.callback(w, r)
				return true
			}
		case "/api/auth/logout":
			if r.Method == http.MethodPost {
				p, session := a.authenticate(r)
				if p.Role == RoleDenied {
					writeError(w, http.StatusUnauthorized, "authentication required")
					return true
				}
				if !a.validCSRF(r, session) {
					writeError(w, http.StatusForbidden, "request verification failed")
					return true
				}
				cookie, _ := r.Cookie(sessionCookie)
				a.mu.Lock()
				delete(a.sessions, cookie.Value)
				browser, _ := r.Cookie(loginCookie)
				for key, tx := range a.pending {
					if tx.Browser == session.Browser || (browser != nil && tx.Browser == browser.Value) {
						delete(a.pending, key)
					}
				}
				a.mu.Unlock()
				authCookie(w, sessionCookie, "", time.Unix(1, 0))
				authCookie(w, loginCookie, "", time.Unix(1, 0))
				writeJSON(w, http.StatusNoContent, nil)
				return true
			}
		}
		return false
	}
	if r.URL.Path == "/api/session" && r.Method == http.MethodPost {
		var body struct {
			Token string `json:"token"`
		}
		err := decodeJSON(r, &body)
		if a == nil || err != nil || (a.token == "" && !a.demo) || subtle.ConstantTimeCompare([]byte(body.Token), []byte(a.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid admin token")
		} else {
			writeJSON(w, http.StatusNoContent, nil)
		}
		return true
	}
	return false
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	if p, _ := a.authenticate(r); p.Role != RoleDenied {
		http.Redirect(w, r, a.settings.Origin+"/", http.StatusSeeOther)
		return
	}
	values := make([]string, 3)
	for i := range values {
		value, err := randomToken()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "sign in unavailable")
			return
		}
		values[i] = value
	}
	state, browser, nonce := values[0], values[1], values[2]
	a.mu.Lock()
	a.prune(time.Now())
	if previous, err := r.Cookie(loginCookie); err == nil {
		if decoded, err := hex.DecodeString(previous.Value); err == nil && len(decoded) == 24 {
			browser = previous.Value
		}
		for key, tx := range a.pending {
			if tx.Browser == previous.Value {
				delete(a.pending, key)
			}
		}
	}
	transaction := loginTransaction{Browser: browser, Nonce: nonce, Verifier: oauth2.GenerateVerifier(), Expires: time.Now().Add(5 * time.Minute)}
	if len(a.pending) >= maxLogins {
		a.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "sign in busy")
		return
	}
	a.pending[state] = transaction
	a.mu.Unlock()
	authCookie(w, loginCookie, browser, transaction.Expires)
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(transaction.Verifier), oidc.Nonce(nonce)), http.StatusFound)
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	fail := func() { http.Redirect(w, r, a.settings.Origin+"/?signin=failed", http.StatusSeeOther) }
	if len(r.URL.RawQuery) > 8192 {
		fail()
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["state"]) != 1 || query.Get("state") == "" || len(query["code"]) > 1 || len(query["error"]) > 1 || (len(query["code"]) == 1) == (len(query["error"]) == 1) {
		fail()
		return
	}
	cookie, err := r.Cookie(loginCookie)
	if err != nil {
		fail()
		return
	}
	a.mu.Lock()
	a.prune(time.Now())
	transaction, ok := a.pending[query.Get("state")]
	if ok && !transaction.Consumed && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(transaction.Browser)) == 1 {
		transaction.Consumed = true
		a.pending[query.Get("state")] = transaction
	} else {
		ok = false
	}
	a.mu.Unlock()
	if !ok {
		fail()
		return
	}
	defer func() {
		a.mu.Lock()
		delete(a.pending, query.Get("state"))
		a.mu.Unlock()
	}()
	if len(query["error"]) != 0 || query.Get("code") == "" {
		fail()
		return
	}
	select {
	case a.exchanges <- struct{}{}:
		defer func() { <-a.exchanges }()
	default:
		fail()
		return
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), a.client), 10*time.Second)
	defer cancel()
	token, err := a.oauth.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(transaction.Verifier))
	if err != nil {
		fail()
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		fail()
		return
	}
	id, err := a.verifier.Verify(ctx, raw)
	if err != nil || id.Subject == "" || id.Nonce != transaction.Nonce {
		fail()
		return
	}
	var claims map[string]json.RawMessage
	if err := id.Claims(&claims); err != nil {
		fail()
		return
	}
	var azp string
	if value, exists := claims["azp"]; exists {
		if json.Unmarshal(value, &azp) != nil || azp != a.settings.ClientID {
			fail()
			return
		}
	} else if len(id.Audience) > 1 {
		fail()
		return
	}
	var groups []string
	if value, exists := claims[a.settings.GroupsClaim]; exists && json.Unmarshal(value, &groups) != nil {
		fail()
		return
	}
	role := a.settings.Policy.role(id.Subject, groups)
	if role == RoleDenied {
		fail()
		return
	}
	sessionID, err := randomToken()
	if err != nil {
		fail()
		return
	}
	csrf, err := randomToken()
	if err != nil {
		fail()
		return
	}
	expires := time.Now().Add(time.Hour)
	if id.Expiry.Before(expires) {
		expires = id.Expiry
	}
	if !time.Now().Before(expires) {
		fail()
		return
	}
	a.mu.Lock()
	a.prune(time.Now())
	current, active := a.pending[query.Get("state")]
	if !active || !current.Consumed || current.Browser != transaction.Browser || len(a.sessions) >= maxSessions {
		a.mu.Unlock()
		fail()
		return
	}
	if old, err := r.Cookie(sessionCookie); err == nil {
		delete(a.sessions, old.Value)
	}
	for id, session := range a.sessions {
		if session.Browser == transaction.Browser {
			delete(a.sessions, id)
		}
	}
	a.sessions[sessionID] = authSession{Principal: Principal{Subject: id.Subject, Role: role}, CSRF: csrf, Expires: expires, Browser: transaction.Browser}
	a.mu.Unlock()
	authCookie(w, loginCookie, "", time.Unix(1, 0))
	authCookie(w, sessionCookie, sessionID, expires)
	http.Redirect(w, r, a.settings.Origin+"/", http.StatusSeeOther)
}
