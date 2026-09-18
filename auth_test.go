package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type issuerCode struct{ Challenge, Nonce, Redirect, Role string }
type testIssuer struct {
	server    *httptest.Server
	key       *rsa.PrivateKey
	mu        sync.Mutex
	codes     map[string]issuerCode
	claims    func(map[string]any)
	tokenMode string
	expire    func()
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &testIssuer{key: key, codes: map[string]issuerCode{}}
	i.server = httptest.NewTLSServer(http.HandlerFunc(i.serveHTTP))
	t.Cleanup(i.server.Close)
	return i
}

func (i *testIssuer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		writeJSON(w, 200, map[string]any{"issuer": i.server.URL, "authorization_endpoint": i.server.URL + "/authorize", "token_endpoint": i.server.URL + "/token", "jwks_uri": i.server.URL + "/jwks", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	case "/jwks":
		writeJSON(w, 200, map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "fixture", "n": base64.RawURLEncoding.EncodeToString(i.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(i.key.E)).Bytes())}}})
	case "/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != "console" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" {
			http.Error(w, "invalid authorization request", 400)
			return
		}
		role := q.Get("role")
		if role == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>Test identity provider</title></head><body><h1>Test identity provider</h1><p>Choose a test identity. No Kubernetes or external provider is used.</p>`)
			for _, choice := range []string{"admin", "viewer", "denied"} {
				q.Set("role", choice)
				fmt.Fprintf(w, `<p><a href="%s">%s</a></p>`, html.EscapeString("/authorize?"+q.Encode()), strings.Title(choice))
			}
			fmt.Fprint(w, `</body></html>`)
			return
		}
		code, _ := randomToken()
		i.mu.Lock()
		i.codes[code] = issuerCode{Challenge: q.Get("code_challenge"), Nonce: q.Get("nonce"), Redirect: q.Get("redirect_uri"), Role: role}
		i.mu.Unlock()
		target, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "invalid redirect", 400)
			return
		}
		query := target.Query()
		query.Set("code", code)
		query.Set("state", q.Get("state"))
		target.RawQuery = query.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
	case "/token":
		if r.Method != "POST" || r.ParseForm() != nil {
			http.Error(w, "bad request", 400)
			return
		}
		client, secret, _ := r.BasicAuth()
		if client == "" {
			client, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
		}
		i.mu.Lock()
		code, ok := i.codes[r.Form.Get("code")]
		delete(i.codes, r.Form.Get("code"))
		mutate := i.claims
		i.mu.Unlock()
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || client != "console" || secret != "fixture-secret" || r.Form.Get("grant_type") != "authorization_code" || code.Redirect != r.Form.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(challenge[:]) != code.Challenge {
			writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		claims := map[string]any{"iss": i.server.URL, "aud": "console", "sub": code.Role + "-subject", "nonce": code.Nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(2 * time.Hour).Unix(), "groups": []string{code.Role}}
		if mutate != nil {
			mutate(claims)
		}
		token := i.sign(claims)
		i.mu.Lock()
		mode := i.tokenMode
		i.mu.Unlock()
		if mode == "signature" {
			token = token[:strings.LastIndex(token, ".")+1] + base64.RawURLEncoding.EncodeToString(make([]byte, 256))
		}
		if mode == "key" {
			token = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"unknown"}`)) + token[strings.Index(token, "."):]
		}
		response := map[string]any{"access_token": "never-expose-provider-token", "token_type": "Bearer", "id_token": token}
		if mode == "missing" {
			delete(response, "id_token")
		}
		writeJSON(w, 200, response)
	case "/expire":
		if r.Method == "POST" && i.expire != nil {
			i.expire()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<h1>Fixture session control</h1><form method="post"><button>Expire all console sessions</button></form>`)
	default:
		http.NotFound(w, r)
	}
}

func (i *testIssuer) sign(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"fixture"}`))
	body, _ := json.Marshal(claims)
	data := header + "." + base64.RawURLEncoding.EncodeToString(body)
	hash := sha256.Sum256([]byte(data))
	signature, _ := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, hash[:])
	return data + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func oidcTestApp(t *testing.T) (*App, *testIssuer) {
	t.Helper()
	i := newTestIssuer(t)
	a, err := newOIDCAuth(context.Background(), oidcSettings{Issuer: i.server.URL, ClientID: "console", ClientSecret: "fixture-secret", Origin: "https://console.example", GroupsClaim: "groups", Scopes: []string{"openid", "profile"}, Policy: rolePolicy{AdminGroups: []string{"admin"}, ViewerGroups: []string{"viewer"}}}, i.server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(t, true)
	app.auth = a
	return app, i
}

func authRequest(app *App, method, path string, cookies []*http.Cookie, origin, csrf, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	r.Header.Set("Origin", origin)
	r.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func beginLogin(t *testing.T, app *App, issuer *testIssuer, role string, cookies ...[]*http.Cookie) (string, []*http.Cookie) {
	t.Helper()
	var browser []*http.Cookie
	if len(cookies) > 0 {
		browser = cookies[0]
	}
	start := authRequest(app, "GET", "/api/auth/login", browser, "", "", "")
	if start.Code != 302 {
		t.Fatalf("login = %d %s", start.Code, start.Body.String())
	}
	location, _ := url.Parse(start.Header().Get("Location"))
	q := location.Query()
	q.Set("role", role)
	location.RawQuery = q.Encode()
	client := *issuer.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Get(location.String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 302 {
		t.Fatalf("issuer = %d", response.StatusCode)
	}
	return response.Header.Get("Location"), start.Result().Cookies()
}

func loginAs(t *testing.T, app *App, issuer *testIssuer, role string, cookies ...[]*http.Cookie) ([]*http.Cookie, string) {
	t.Helper()
	callback, browser := beginLogin(t, app, issuer, role, cookies...)
	response := authRequest(app, "GET", callback, browser, "", "", "")
	if response.Code != 303 || strings.Contains(response.Header().Get("Location"), "failed") {
		t.Fatalf("callback = %d %s", response.Code, response.Header().Get("Location"))
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie {
			if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Domain != "" || cookie.Path != "/" || cookie.MaxAge > 3600 {
				t.Fatalf("unsafe cookie: %+v", cookie)
			}
			cookies := append([]*http.Cookie{cookie}, browser...)
			discovery := authRequest(app, "GET", "/api/auth", cookies, "", "", "")
			var data struct {
				CSRF string `json:"csrfToken"`
				Role string `json:"role"`
			}
			if err := json.Unmarshal(discovery.Body.Bytes(), &data); err != nil || data.Role != role || data.CSRF == "" {
				t.Fatalf("discovery: %s", discovery.Body.String())
			}
			return cookies, data.CSRF
		}
	}
	t.Fatal("no session cookie")
	return nil, ""
}

func TestOIDCLoginAndLogout(t *testing.T) {
	app, issuer := oidcTestApp(t)
	cookie, csrf := loginAs(t, app, issuer, "viewer")
	if res := authRequest(app, "GET", "/api/bootstrap", cookie, "", "", ""); res.Code != 200 {
		t.Fatal(res.Code)
	}
	for _, pair := range [][2]string{{"", csrf}, {app.auth.settings.Origin, ""}, {"https://evil.example", csrf}, {app.auth.settings.Origin + "/", csrf}, {app.auth.settings.Origin, "wrong"}} {
		if res := authRequest(app, "POST", "/api/auth/logout", cookie, pair[0], pair[1], ""); res.Code != 403 {
			t.Fatalf("logout CSRF = %d", res.Code)
		}
	}
	if res := authRequest(app, "POST", "/api/auth/logout", cookie, app.auth.settings.Origin, csrf, ""); res.Code != 204 {
		t.Fatal(res.Code)
	}
	if res := authRequest(app, "GET", "/api/bootstrap", cookie, "", "", ""); res.Code != 401 {
		t.Fatal("copied cookie remained valid")
	}
}

func TestOIDCRejectsInvalidClaims(t *testing.T) {
	app, issuer := oidcTestApp(t)
	cases := map[string]func(map[string]any){
		"nonce":          func(c map[string]any) { c["nonce"] = "wrong" },
		"issuer":         func(c map[string]any) { c["iss"] = "https://wrong.example" },
		"audience":       func(c map[string]any) { c["aud"] = "other-client" },
		"expiry":         func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"subject":        func(c map[string]any) { c["sub"] = "" },
		"azp":            func(c map[string]any) { c["azp"] = "other-client" },
		"multi-audience": func(c map[string]any) { c["aud"] = []string{"console", "other"} },
		"groups-type":    func(c map[string]any) { c["groups"] = "admin" },
		"unmatched":      func(c map[string]any) { c["groups"] = []string{"other"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			issuer.mu.Lock()
			issuer.claims = mutate
			issuer.mu.Unlock()
			callback, browser := beginLogin(t, app, issuer, "admin")
			res := authRequest(app, "GET", callback, browser, "", "", "")
			if res.Header().Get("Location") != app.auth.settings.Origin+"/?signin=failed" {
				t.Fatal("accepted invalid claims")
			}
			if strings.Contains(res.Body.String(), "fixture-secret") || strings.Contains(res.Body.String(), "never-expose") {
				t.Fatal("provider credentials leaked")
			}
		})
	}
}

func TestOIDCStateReplayAndExpiry(t *testing.T) {
	app, issuer := oidcTestApp(t)
	callback, browser := beginLogin(t, app, issuer, "admin")
	for _, cookie := range [][]*http.Cookie{nil, {{Name: loginCookie, Value: "wrong"}}} {
		if res := authRequest(app, "GET", callback, cookie, "", "", ""); !strings.Contains(res.Header().Get("Location"), "failed") {
			t.Fatal("browser binding bypass")
		}
	}
	res := authRequest(app, "GET", callback, browser, "", "", "")
	if strings.Contains(res.Header().Get("Location"), "failed") {
		t.Fatal("valid callback failed")
	}
	if res := authRequest(app, "GET", callback, browser, "", "", ""); !strings.Contains(res.Header().Get("Location"), "failed") {
		t.Fatal("replay accepted")
	}
	callback, browser = beginLogin(t, app, issuer, "admin")
	app.auth.mu.Lock()
	for key, tx := range app.auth.pending {
		tx.Expires = time.Now().Add(-time.Second)
		app.auth.pending[key] = tx
	}
	app.auth.mu.Unlock()
	if res := authRequest(app, "GET", callback, browser, "", "", ""); !strings.Contains(res.Header().Get("Location"), "failed") {
		t.Fatal("expired transaction accepted")
	}
	cookie, _ := loginAs(t, app, issuer, "admin")
	app.auth.mu.Lock()
	s := app.auth.sessions[cookie[0].Value]
	s.Expires = time.Now().Add(-time.Second)
	app.auth.sessions[cookie[0].Value] = s
	app.auth.mu.Unlock()
	if res := authRequest(app, "GET", "/api/bootstrap", cookie, "", "", ""); res.Code != 401 {
		t.Fatal("expired session accepted")
	}
}

type countingOrchestrator struct{ calls int }

func (c *countingOrchestrator) Deploy(context.Context, Server) error  { c.calls++; return nil }
func (c *countingOrchestrator) Restart(context.Context, Server) error { c.calls++; return nil }
func (c *countingOrchestrator) Scale(context.Context, Server, int) error {
	c.calls++
	return nil
}
func (c *countingOrchestrator) Logs(context.Context, Server, int) ([]LogLine, error) {
	c.calls++
	return nil, nil
}
func (c *countingOrchestrator) Refresh(_ context.Context, s Server) (Server, error) {
	c.calls++
	return s, nil
}
func (c *countingOrchestrator) CheckUpdate(_ context.Context, s Server) (Server, error) {
	c.calls++
	return s, nil
}

func TestOIDCRoutePolicyAndZeroEffects(t *testing.T) {
	app, issuer := oidcTestApp(t)
	orchestrator := &countingOrchestrator{}
	app.orchestrator = orchestrator
	viewer, csrf := loginAs(t, app, issuer, "viewer")
	routes := [][2]string{{"GET", "/api/bootstrap"}, {"GET", "/api/servers/scuffedtards/telemetry"}, {"GET", "/api/events"}, {"GET", "/api/servers/scuffedtards/logs"}, {"POST", "/api/servers"}, {"POST", "/api/servers/scuffedtards/actions/restart"}, {"POST", "/api/servers/scuffedtards/actions/update"}, {"POST", "/api/servers/scuffedtards/actions/check-update"}, {"POST", "/api/servers/scuffedtards/actions/stop"}, {"POST", "/api/servers/scuffedtards/actions/start"}, {"GET", "/api/future"}, {"POST", "/api/bootstrap"}, {"GET", "/api/servers/scuffedtards/telemetry/extra"}, {"POST", "/api/session"}, {"GET", "/api/image-tags"}}
	before := app.store.Snapshot()
	for _, route := range routes {
		if res := authRequest(app, route[0], route[1], nil, "", "", "{"); res.Code != 401 {
			t.Fatalf("anonymous %v = %d", route, res.Code)
		}
		want := 403
		if route[0] == "GET" && (route[1] == "/api/bootstrap" || strings.HasSuffix(route[1], "/telemetry")) {
			want = 200
		}
		if res := authRequest(app, route[0], route[1], viewer, app.auth.settings.Origin, csrf, "{"); res.Code != want {
			t.Fatalf("viewer %v = %d", route, res.Code)
		}
	}
	if orchestrator.calls != 0 || !reflect.DeepEqual(before, app.store.Snapshot()) {
		t.Fatal("denied or viewer request had effects")
	}
	admin, adminCSRF := loginAs(t, app, issuer, "admin")
	for _, route := range routes[2:8] {
		body := `{"imageTag":"0.1.2"}`
		if route[1] == "/api/servers" {
			body = `{"name":"new-world","ownerId":"0123456789abcdef0123456789abcdef","maxPlayers":4}`
		}
		if route[0] == "POST" {
			for _, pair := range [][2]string{{"", adminCSRF}, {"https://evil.example", adminCSRF}, {app.auth.settings.Origin, ""}, {app.auth.settings.Origin, "wrong"}} {
				if res := authRequest(app, route[0], route[1], admin, pair[0], pair[1], body); res.Code != 403 {
					t.Fatal("admin CSRF bypass")
				}
			}
		}
		if res := authRequest(app, route[0], route[1], admin, app.auth.settings.Origin, adminCSRF, body); res.Code >= 300 {
			t.Fatalf("admin %v = %d %s", route, res.Code, res.Body.String())
		}
	}
	if orchestrator.calls != 4 {
		t.Fatalf("admin operations = %d", orchestrator.calls)
	}
	for _, bearer := range []string{"Bearer fixture-secret", "Bearer old-admin"} {
		r := httptest.NewRequest("GET", "/api/bootstrap", nil)
		for _, cookie := range admin {
			r.AddCookie(cookie)
		}
		r.Header.Set("Authorization", bearer)
		r.Header.Set("X-Forwarded-User", "admin")
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("mixed credentials accepted")
		}
	}
}

func TestOIDCViewerProjection(t *testing.T) {
	app, issuer := oidcTestApp(t)
	const secret = "SECRET-MARKER"
	_ = app.store.Update(func(s *State) error {
		server := s.Servers["scuffedtards"]
		server.OwnerID = secret
		server.PasswordSecret = secret
		server.AdminIDs = secret
		server.AdditionalArgs = secret
		server.Endpoint = secret
		server.CurrentImage = secret
		server.DesiredImage = secret
		server.Namespace = secret
		server.Release = secret
		server.WorldName = "Public world"
		server.OwnershipToken = secret
		s.Servers[server.ID] = server
		s.Events[0].Details = secret
		return nil
	})
	now := time.Now().UTC()
	metrics := emptyMetrics()
	setReading(metrics, "players", 7, now)
	metrics["cpuCores"] = MetricReading{Status: "error", Source: secret, Reason: secret, Unit: secret}
	metrics[secret] = MetricReading{Reason: secret}
	app.observations().history["scuffedtards"] = []observation{{at: now, metrics: metrics, status: StatusOnline, image: secret}}
	cookie, _ := loginAs(t, app, issuer, "viewer")
	for _, path := range []string{"/api/bootstrap", "/api/servers/scuffedtards/telemetry"} {
		res := authRequest(app, "GET", path, cookie, "", "", "")
		if res.Code != 200 || strings.Contains(res.Body.String(), secret) {
			t.Fatalf("projection leaked: %s", res.Body.String())
		}
		for _, field := range []string{`"ownerId"`, `"namespace"`, `"passwordSecret"`, `"events":[`, `"currentImage"`, `"additionalArgs"`} {
			if strings.Contains(res.Body.String(), field) {
				t.Fatalf("unexpected field %s", field)
			}
		}
		if !strings.Contains(res.Body.String(), "ScuffedTards") || !strings.Contains(res.Body.String(), `"worldName":"Public world"`) || !strings.Contains(res.Body.String(), `"value":7`) {
			t.Fatal("viewer lost useful data")
		}
	}
}

func TestOIDCRolePolicy(t *testing.T) {
	p := rolePolicy{AdminSubjects: []string{"admin"}, ViewerSubjects: []string{"viewer"}, AdminGroups: []string{"ops"}, ViewerGroups: []string{"readers", "ops"}}
	for _, tc := range []struct {
		subject string
		groups  []string
		want    Role
	}{{"", []string{"ops"}, RoleDenied}, {"other", nil, RoleDenied}, {"admin", nil, RoleAdmin}, {"viewer", nil, RoleViewer}, {"other", []string{"ops", "readers"}, RoleAdmin}, {"other", []string{"readers"}, RoleViewer}, {"other", []string{"READERS"}, RoleDenied}} {
		if got := p.role(tc.subject, tc.groups); got != tc.want {
			t.Fatalf("role = %v want %v", got, tc.want)
		}
	}
}

func TestOIDCRejectsSignatureAndMissingToken(t *testing.T) {
	app, issuer := oidcTestApp(t)
	for _, mode := range []string{"signature", "key", "missing"} {
		t.Run(mode, func(t *testing.T) {
			issuer.mu.Lock()
			issuer.tokenMode = mode
			issuer.mu.Unlock()
			callback, browser := beginLogin(t, app, issuer, "admin")
			if res := authRequest(app, "GET", callback, browser, "", "", ""); !strings.Contains(res.Header().Get("Location"), "failed") {
				t.Fatal("accepted invalid token")
			}
		})
	}
}

func TestOIDCAmbiguityAndAuthenticatedLogin(t *testing.T) {
	app, issuer := oidcTestApp(t)
	callback, browser := beginLogin(t, app, issuer, "admin")
	for _, suffix := range []string{"&state=extra", "&code=extra", "&error=denied", "&extra=" + strings.Repeat("a", 8192), "&broken=%zz"} {
		if res := authRequest(app, "GET", callback+suffix, browser, "", "", ""); !strings.Contains(res.Header().Get("Location"), "failed") {
			t.Fatal("ambiguous callback accepted")
		}
	}
	for _, name := range []string{loginCookie, sessionCookie} {
		r := httptest.NewRequest("GET", callback, nil)
		r.AddCookie(browser[0])
		r.AddCookie(&http.Cookie{Name: name, Value: "first"})
		r.AddCookie(&http.Cookie{Name: name, Value: "second"})
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal("duplicate cookies accepted")
		}
	}
	admin, csrf := loginAs(t, app, issuer, "admin")
	before := len(app.auth.pending)
	res := authRequest(app, "GET", "/api/auth/login", admin, "", "", "")
	if res.Code != 303 || res.Header().Get("Location") != app.auth.settings.Origin+"/" || len(app.auth.pending) != before {
		t.Fatal("authenticated login created transaction")
	}
	r := httptest.NewRequest("POST", "/api/auth/logout", nil)
	for _, cookie := range admin {
		r.AddCookie(cookie)
	}
	r.Header.Set("Origin", app.auth.settings.Origin)
	r.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	if res := authRequest(app, "GET", callback, nil, "", "", ""); !strings.Contains(res.Header().Get("Location"), "failed") {
		t.Fatal("callback accepted without logged-out binding")
	}
}

func TestOIDCSessionLimits(t *testing.T) {
	app, issuer := oidcTestApp(t)
	expiry := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	issuer.mu.Lock()
	issuer.claims = func(c map[string]any) {
		c["exp"] = expiry.Unix()
		c["aud"] = []string{"console", "other"}
		c["azp"] = "console"
	}
	issuer.mu.Unlock()
	cookie, _ := loginAs(t, app, issuer, "admin")
	if got := app.auth.sessions[cookie[0].Value].Expires; !got.Equal(expiry) {
		t.Fatalf("expiry = %s", got)
	}
	for range 3 {
		if res := authRequest(app, "GET", "/api/bootstrap", cookie, "", "", ""); res.Code != 200 {
			t.Fatal(res.Code)
		}
	}
	if !app.auth.sessions[cookie[0].Value].Expires.Equal(expiry) {
		t.Fatal("poll renewed session")
	}
	for index := len(app.auth.sessions); index < maxSessions; index++ {
		app.auth.sessions[fmt.Sprint(index)] = authSession{Expires: time.Now().Add(time.Hour)}
	}
	callback, browser := beginLogin(t, app, issuer, "admin")
	if res := authRequest(app, "GET", callback, browser, "", "", ""); !strings.Contains(res.Header().Get("Location"), "failed") {
		t.Fatal("session capacity exceeded")
	}
	if _, ok := app.auth.sessions[cookie[0].Value]; !ok {
		t.Fatal("active session evicted")
	}
	for index := range maxLogins {
		app.auth.pending[fmt.Sprint(index)] = loginTransaction{Expires: time.Now().Add(time.Minute)}
	}
	if res := authRequest(app, "GET", "/api/auth/login", nil, "", "", ""); res.Code != 503 {
		t.Fatal("transaction capacity exceeded")
	}
	app.auth.pending["expired"] = loginTransaction{Expires: time.Now().Add(-time.Second)}
	app.auth.sessions["expired"] = authSession{Expires: time.Now().Add(-time.Second)}
	_ = authRequest(app, "GET", "/api/bootstrap", cookie, "", "", "")
	if _, ok := app.auth.pending["expired"]; ok {
		t.Fatal("transaction was not pruned")
	}
	if _, ok := app.auth.sessions["expired"]; ok {
		t.Fatal("session was not pruned")
	}
	restarted, err := newOIDCAuth(context.Background(), app.auth.settings, issuer.server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	app.auth = restarted
	if res := authRequest(app, "GET", "/api/bootstrap", cookie, "", "", ""); res.Code != 401 {
		t.Fatal("session survived restart")
	}
}

func TestOIDCConfigurationFailsClosed(t *testing.T) {
	app, issuer := oidcTestApp(t)
	for name, mutate := range map[string]func(*oidcSettings){
		"http issuer":   func(s *oidcSettings) { s.Issuer = "http://example.com" },
		"issuer query":  func(s *oidcSettings) { s.Issuer += "?x=1" },
		"http origin":   func(s *oidcSettings) { s.Origin = "http://console.example" },
		"origin path":   func(s *oidcSettings) { s.Origin += "/path" },
		"origin query":  func(s *oidcSettings) { s.Origin += "?x=1" },
		"origin port":   func(s *oidcSettings) { s.Origin = "https://console.example:65536" },
		"origin zone":   func(s *oidcSettings) { s.Origin = "https://[fe80::1%25eth0]" },
		"mapped ipv6":   func(s *oidcSettings) { s.Origin = "https://[::ffff:127.0.0.1]" },
		"client":        func(s *oidcSettings) { s.ClientID = "" },
		"secret":        func(s *oidcSettings) { s.ClientSecret = "" },
		"policy":        func(s *oidcSettings) { s.Policy = rolePolicy{} },
		"empty mapping": func(s *oidcSettings) { s.Policy.AdminGroups = []string{" "} },
		"scope":         func(s *oidcSettings) { s.Scopes = []string{"profile"} },
		"refresh":       func(s *oidcSettings) { s.Scopes = []string{"openid", "offline_access"} },
		"discovery":     func(s *oidcSettings) { s.Issuer += "/missing" },
	} {
		t.Run(name, func(t *testing.T) {
			settings := app.auth.settings
			mutate(&settings)
			if _, err := newOIDCAuth(context.Background(), settings, issuer.server.Client().Transport); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
	if !httpsURL("https://issuer.example/authorize?tenant=one") || issuerURL("https://issuer.example?tenant=one", false) {
		t.Fatal("endpoint and issuer validation conflated")
	}
	for _, env := range os.Environ() {
		key := strings.SplitN(env, "=", 2)[0]
		if strings.HasPrefix(key, "RSDW_OIDC_") || key == "RSDW_ADMIN_TOKEN" || key == "RSDW_AUTH_MODE" {
			t.Setenv(key, "")
		}
	}
	if _, err := authFromEnv(context.Background(), false); err == nil {
		t.Fatal("missing token accepted")
	}
	t.Setenv("RSDW_AUTH_MODE", "oidc")
	t.Setenv("RSDW_ADMIN_TOKEN", "mixed")
	if _, err := authFromEnv(context.Background(), true); err == nil {
		t.Fatal("mixed credentials accepted")
	}
	t.Setenv("RSDW_AUTH_MODE", "token")
	t.Setenv("RSDW_OIDC_ISSUER", issuer.server.URL)
	if _, err := authFromEnv(context.Background(), true); err == nil {
		t.Fatal("OIDC settings ignored")
	}
	if res := authRequest(&App{}, "GET", "/api/bootstrap", nil, "", "", ""); res.Code != 401 {
		t.Fatal("unconfigured app allowed access")
	}
}

func TestViewerFiniteMetrics(t *testing.T) {
	for _, value := range []float64{math.Inf(1), math.NaN(), -1} {
		metrics := map[string]MetricReading{"players": {Value: &value, Status: "available", Reason: "secret"}}
		view := viewerMetrics(metrics)
		if view["players"].Value != nil || view["players"].Status != "unavailable" {
			t.Fatal("invalid numeric metric exposed")
		}
		if _, err := json.Marshal(view); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOIDCCanonicalPublicOrigin(t *testing.T) {
	app, issuer := oidcTestApp(t)
	for _, tc := range []struct{ configured, canonical string }{
		{"https://CONSOLE.Example:443", "https://console.example"},
		{"https://CONSOLE.Example:8443", "https://console.example:8443"},
		{"https://[::1]:443", "https://[::1]"},
		{"https://console.example:0443", "https://console.example"},
		{"https://console.example:08443", "https://console.example:8443"},
		{"https://[0:0:0:0:0:0:0:1]", "https://[::1]"},
		{"https://[0:0:0:0:0:0:0:1]:08443", "https://[::1]:8443"},
	} {
		settings := app.auth.settings
		settings.Origin = tc.configured
		auth, err := newOIDCAuth(context.Background(), settings, issuer.server.Client().Transport)
		if err != nil {
			t.Fatal(err)
		}
		if auth.settings.Origin != tc.canonical || auth.oauth.RedirectURL != tc.canonical+"/api/auth/callback" {
			t.Fatal("origin was not canonicalized")
		}
		app.auth = auth
		cookie, csrf := loginAs(t, app, issuer, "viewer")
		if res := authRequest(app, "POST", "/api/auth/logout", cookie, tc.canonical, csrf, ""); res.Code != 204 {
			t.Fatal("browser's normalized Origin was denied")
		}
	}
}

func TestReviewLogoutDuringCallback(t *testing.T) {
	app, issuer := oidcTestApp(t)
	callback, browser := beginLogin(t, app, issuer, "admin")
	entered, release := make(chan struct{}), make(chan struct{})
	issuer.mu.Lock()
	issuer.claims = func(c map[string]any) {
		if c["sub"] == "admin-subject" {
			close(entered)
			<-release
		}
	}
	issuer.mu.Unlock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("GET", callback, nil)
		r.AddCookie(browser[0])
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		done <- w
	}()
	<-entered
	admin, csrf := loginAs(t, app, issuer, "viewer")
	r := httptest.NewRequest("POST", "/api/auth/logout", nil)
	for _, cookie := range admin {
		r.AddCookie(cookie)
	}
	r.Header.Set("Origin", app.auth.settings.Origin)
	r.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	close(release)
	res := <-done
	if w.Code != 204 {
		t.Fatalf("logout = %d", w.Code)
	}
	if authRequest(app, "GET", "/api/bootstrap", admin, "", "", "").Code != 401 {
		t.Fatal("old session was not revoked")
	}
	if strings.Contains(res.Header().Get("Location"), "failed") {
		return
	}
	for _, cookie := range res.Result().Cookies() {
		if cookie.Name == sessionCookie && cookie.Value != "" {
			if got := authRequest(app, "GET", "/api/bootstrap", []*http.Cookie{cookie}, "", "", "").Code; got == http.StatusOK {
				t.Fatal("callback minted a working admin session after successful logout")
			}
		}
	}
}

func TestOIDCNewLoginSupersedesInFlightCallback(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(fmt.Sprint("logout=", logout), func(t *testing.T) {
			app, issuer := oidcTestApp(t)
			other, _ := loginAs(t, app, issuer, "viewer")
			callback, browser := beginLogin(t, app, issuer, "admin")
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			issuer.mu.Lock()
			issuer.claims = func(c map[string]any) {
				if c["sub"] == "admin-subject" {
					close(entered)
					<-release
				}
			}
			issuer.mu.Unlock()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- authRequest(app, "GET", callback, browser, "", "", "") }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("token exchange did not start")
			}
			if res := authRequest(app, "GET", callback, browser, "", "", ""); !strings.Contains(res.Header().Get("Location"), "failed") {
				t.Fatal("in-flight callback replay accepted")
			}
			viewer, csrf := loginAs(t, app, issuer, "viewer", browser)
			app.auth.mu.Lock()
			lineage := app.auth.sessions[viewer[0].Value].Browser
			app.auth.mu.Unlock()
			if lineage == browser[0].Value {
				t.Fatal("new login reused browser binding")
			}
			if logout {
				if res := authRequest(app, "POST", "/api/auth/logout", viewer, app.auth.settings.Origin, csrf, ""); res.Code != 204 {
					t.Fatalf("logout = %d", res.Code)
				}
			}
			once.Do(func() { close(release) })
			res := <-done
			if !strings.Contains(res.Header().Get("Location"), "failed") {
				t.Fatal("superseded callback admitted a session")
			}
			for _, cookie := range res.Result().Cookies() {
				if cookie.Name == sessionCookie && cookie.Value != "" {
					t.Fatal("superseded callback wrote session cookie")
				}
			}
			want := 200
			if logout {
				want = 401
			}
			if res := authRequest(app, "GET", "/api/bootstrap", viewer, "", "", ""); res.Code != want {
				t.Fatalf("viewer status = %d", res.Code)
			}
			if res := authRequest(app, "GET", "/api/bootstrap", other, "", "", ""); res.Code != 200 {
				t.Fatal("other browser was revoked")
			}
		})
	}
}

func fixtureHTTPApp(app *App) http.Handler {
	names := map[string]string{sessionCookie: "rsdw-fixture-session", loginCookie: "rsdw-fixture-login"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := r.Clone(r.Context())
		request.Header.Del("Cookie")
		for _, cookie := range r.Cookies() {
			for production, fixture := range names {
				if cookie.Name == fixture {
					cookie.Name = production
				}
			}
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		app.ServeHTTP(response, request)
		for key, values := range response.Header() {
			for _, value := range values {
				if key == "Set-Cookie" {
					if cookie, err := http.ParseSetCookie(value); err == nil {
						if name, ok := names[cookie.Name]; ok {
							cookie.Name, cookie.Secure = name, false
							value = cookie.String()
						}
					}
				}
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	})
}

func newHTTPBrowserFixture(t *testing.T, app *App, issuer *testIssuer) (*httptest.Server, *httptest.Server) {
	t.Helper()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/authorize" && r.URL.Path != "/expire" {
			http.NotFound(w, r)
			return
		}
		issuer.serveHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	server := httptest.NewServer(fixtureHTTPApp(app))
	t.Cleanup(server.Close)
	app.auth.settings.Origin = server.URL
	app.auth.oauth.RedirectURL = server.URL + "/api/auth/callback"
	app.auth.oauth.Endpoint.AuthURL = front.URL + "/authorize"
	return server, front
}

func TestOIDCHTTPBrowserAdapter(t *testing.T) {
	app, issuer := oidcTestApp(t)
	server, front := newHTTPBrowserFixture(t, app, issuer)
	if !strings.HasPrefix(app.auth.settings.Issuer, "https://") || !strings.HasPrefix(app.auth.oauth.Endpoint.TokenURL, "https://") {
		t.Fatal("fixture changed backchannel TLS")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	response, err := client.Get(server.URL + "/api/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !strings.HasPrefix(response.Request.URL.String(), front.URL+"/authorize") {
		t.Fatal("login did not use HTTP issuer front")
	}
	authorize := *response.Request.URL
	query := authorize.Query()
	query.Set("role", "viewer")
	authorize.RawQuery = query.Encode()
	response, err = client.Get(authorize.String())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(body), "DRAGONWILDS") {
		t.Fatalf("embedded UI was not served: %s", body)
	}
	response, err = client.Get(server.URL + "/api/auth")
	if err != nil {
		t.Fatal(err)
	}
	var auth struct {
		Role string `json:"role"`
		CSRF string `json:"csrfToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&auth); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if auth.Role != "viewer" || auth.CSRF == "" {
		t.Fatal("adapter failed to authenticate viewer")
	}
	response, err = client.Get(server.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("HTTP fixture bypassed role policy")
	}
	request, _ := http.NewRequest("POST", server.URL+"/api/auth/logout", nil)
	request.Header.Set("Origin", server.URL)
	request.Header.Set("X-CSRF-Token", auth.CSRF)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal("HTTP fixture logout failed")
	}
	for _, cookie := range response.Cookies() {
		if strings.HasPrefix(cookie.Name, "__Host-") || cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
			t.Fatalf("unexpected fixture cookie: %+v", cookie)
		}
	}
	response, err = client.Get(server.URL + "/api/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("HTTP fixture remained authenticated")
	}
}

func TestOIDCBrowserFixture(t *testing.T) {
	if os.Getenv("RSDW_OIDC_BROWSER_TEST") != "1" {
		t.Skip("set RSDW_OIDC_BROWSER_TEST=1 for the local browser fixture")
	}
	app, issuer := oidcTestApp(t)
	issuer.expire = func() { app.auth.mu.Lock(); defer app.auth.mu.Unlock(); clear(app.auth.sessions) }
	var server *httptest.Server
	issuerURL := issuer.server.URL
	if os.Getenv("RSDW_OIDC_BROWSER_HTTP") == "1" {
		var front *httptest.Server
		server, front = newHTTPBrowserFixture(t, app, issuer)
		issuerURL = front.URL
		fmt.Println("HTTP loopback fixture: browser cookies are test-only translations. Production Secure cookies are verified by TLS unit tests, not this browser mode.")
	} else {
		server = httptest.NewTLSServer(app)
		t.Cleanup(server.Close)
		app.auth.settings.Origin = server.URL
		app.auth.oauth.RedirectURL = server.URL + "/api/auth/callback"
		fmt.Println("TLS fixture: accept the test issuer certificate before opening the console.")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cache := app.observations()
	populate := func() {
		now := time.Now().UTC()
		metrics := emptyMetrics()
		for key, value := range map[string]float64{"players": 7, "tickRate": 60, "cpuCores": .4, "cpuPercent": 40, "memoryUsedBytes": 1500000000, "memoryLimitBytes": 4000000000, "uptimeSeconds": 7200, "engineReady": 1, "inboundBytesPerSecond": 1200, "outboundBytesPerSecond": 2400} {
			setReading(metrics, key, value, now)
		}
		cache.mu.Lock()
		history := append(cache.history["scuffedtards"], observation{at: now, metrics: metrics, status: StatusOnline})
		if len(history) > 240 {
			history = history[len(history)-240:]
		}
		cache.history["scuffedtards"] = history
		cache.mu.Unlock()
	}
	populate()
	fmt.Printf("\nConsole: %s\nTest issuer: %s/expire\nExpiration control: %s/expire\nChoose Admin, Viewer, or Denied during sign in. Ctrl-C stops the fixture.\n", server.URL, issuerURL, issuerURL)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			populate()
		}
	}
}
