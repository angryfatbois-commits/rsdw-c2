package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func authBrowser(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	client := *server.Client()
	client.Jar, _ = cookiejar.New(nil)
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func browserRequest(t *testing.T, client *http.Client, method, target, csrf string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		origin := *request.URL
		origin.Path, origin.RawQuery = "", ""
		request.Header.Set("Origin", origin.String())
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Error(err)
		return nil, nil
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Error(err)
	}
	return response, body
}

func browserAuthorize(t *testing.T, client *http.Client, login *http.Response, role string) string {
	t.Helper()
	if login == nil || login.StatusCode != http.StatusFound {
		t.Fatal("login did not redirect to issuer")
	}
	target, err := url.Parse(login.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := target.Query()
	query.Set("role", role)
	target.RawQuery = query.Encode()
	response, _ := browserRequest(t, client, "GET", target.String(), "")
	if response == nil || response.StatusCode != http.StatusFound {
		t.Fatal("issuer did not redirect to callback")
	}
	return response.Header.Get("Location")
}

func browserAuth(t *testing.T, client *http.Client, origin, role string) string {
	t.Helper()
	response, body := browserRequest(t, client, "GET", origin+"/api/auth", "")
	var discovery struct {
		Authenticated bool   `json:"authenticated"`
		Role          string `json:"role"`
		CSRF          string `json:"csrfToken"`
	}
	if response == nil || response.StatusCode != http.StatusOK || json.Unmarshal(body, &discovery) != nil || discovery.Role != role || discovery.Authenticated != (role != "denied") {
		t.Fatalf("want role %s, discovery = %s", role, body)
	}
	return discovery.CSRF
}

func TestOIDCBrowserDelayedCallbackAfterLogout(t *testing.T) {
	for _, hold := range []string{"exchange", "admitted-response"} {
		t.Run(hold, func(t *testing.T) {
			app, issuer := oidcTestApp(t)
			initialReady, releaseInitial := make(chan struct{}), make(chan struct{})
			callbackReady, releaseCallback := make(chan struct{}), make(chan struct{})
			var initialOnce, callbackOnce sync.Once
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("hold") == "yes" {
					buffer := httptest.NewRecorder()
					app.ServeHTTP(buffer, r)
					if r.URL.Path == "/api/auth/login" {
						if len(r.Cookies()) != 0 {
							t.Error("initial request already had cookies")
						}
						close(initialReady)
						<-releaseInitial
					} else {
						if buffer.Header().Get("Location") != app.auth.settings.Origin+"/" {
							t.Error("held callback did not admit session")
						}
						close(callbackReady)
						<-releaseCallback
					}
					for name, values := range buffer.Header() {
						for _, value := range values {
							w.Header().Add(name, value)
						}
					}
					w.WriteHeader(buffer.Code)
					_, _ = w.Write(buffer.Body.Bytes())
					return
				}
				app.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() {
				initialOnce.Do(func() { close(releaseInitial) })
				callbackOnce.Do(func() { close(releaseCallback) })
			})
			app.auth.settings.Origin = server.URL
			app.auth.oauth.RedirectURL = server.URL + "/api/auth/callback"
			client := authBrowser(t, server)
			other := authBrowser(t, server)
			otherStart, _ := browserRequest(t, other, "GET", server.URL+"/api/auth/login", "")
			otherCallback := browserAuthorize(t, other, otherStart, "viewer")
			browserRequest(t, other, "GET", otherCallback, "")
			browserAuth(t, other, server.URL, "viewer")
			pendingBrowser := authBrowser(t, server)
			pendingStart, _ := browserRequest(t, pendingBrowser, "GET", server.URL+"/api/auth/login", "")
			pendingCallback := browserAuthorize(t, pendingBrowser, pendingStart, "viewer")

			initialDone := make(chan *http.Response, 1)
			go func() {
				response, _ := browserRequest(t, client, "GET", server.URL+"/api/auth/login?hold=yes", "")
				initialDone <- response
			}()
			select {
			case <-initialReady:
			case <-time.After(5 * time.Second):
				t.Fatal("initial login did not reach response barrier")
			}
			first, _ := browserRequest(t, client, "GET", server.URL+"/api/auth/login", "")
			callback := browserAuthorize(t, client, first, "admin")
			if hold == "exchange" {
				issuer.mu.Lock()
				issuer.claims = func(claims map[string]any) {
					if claims["sub"] == "admin-subject" {
						close(callbackReady)
						<-releaseCallback
					}
				}
				issuer.mu.Unlock()
			} else {
				callback += "&hold=yes"
			}
			callbackDone := make(chan *http.Response, 1)
			go func() {
				response, _ := browserRequest(t, client, "GET", callback, "")
				callbackDone <- response
			}()
			select {
			case <-callbackReady:
			case <-time.After(5 * time.Second):
				t.Fatal("callback did not reach barrier")
			}
			initialOnce.Do(func() { close(releaseInitial) })
			secondCallback := browserAuthorize(t, client, <-initialDone, "viewer")
			browserRequest(t, client, "GET", secondCallback, "")
			csrf := browserAuth(t, client, server.URL, "viewer")
			logout, _ := browserRequest(t, client, "POST", server.URL+"/api/auth/logout", csrf)
			if logout == nil || logout.StatusCode != http.StatusNoContent {
				t.Fatal("logout did not succeed")
			}
			callbackOnce.Do(func() { close(releaseCallback) })
			late := <-callbackDone
			if late == nil {
				t.Fatal("late callback did not return")
			}
			browserAuth(t, client, server.URL, "denied")
			response, _ := browserRequest(t, client, "GET", server.URL+"/api/bootstrap", "")
			if response == nil || response.StatusCode != http.StatusUnauthorized {
				t.Fatal("late callback restored protected access")
			}
			browserAuth(t, other, server.URL, "viewer")
			browserRequest(t, pendingBrowser, "GET", pendingCallback, "")
			browserAuth(t, pendingBrowser, server.URL, "viewer")
			for _, cookie := range late.Cookies() {
				if cookie.Name == loginCookie {
					t.Fatal("callback changed browser binding")
				}
			}
		})
	}
}

func TestOIDCBrowserRepeatedLoginAndFailure(t *testing.T) {
	app, _ := oidcTestApp(t)
	server := httptest.NewTLSServer(app)
	t.Cleanup(server.Close)
	app.auth.settings.Origin = server.URL
	app.auth.oauth.RedirectURL = server.URL + "/api/auth/callback"
	client := authBrowser(t, server)
	first, _ := browserRequest(t, client, "GET", server.URL+"/api/auth/login", "")
	oldCallback := browserAuthorize(t, client, first, "admin")
	second, _ := browserRequest(t, client, "GET", server.URL+"/api/auth/login", "")
	deniedCallback := browserAuthorize(t, client, second, "denied")
	if first.Cookies()[0].Value == second.Cookies()[0].Value {
		t.Fatal("repeated login reused binding")
	}
	for _, callback := range []string{oldCallback, deniedCallback, deniedCallback} {
		response, _ := browserRequest(t, client, "GET", callback, "")
		if response == nil || !strings.Contains(response.Header.Get("Location"), "signin=failed") || len(response.Cookies()) != 0 {
			t.Fatal("failed callback changed cookies or did not fail")
		}
		browserAuth(t, client, server.URL, "denied")
	}
	third, _ := browserRequest(t, client, "GET", server.URL+"/api/auth/login", "")
	callback := browserAuthorize(t, client, third, "viewer")
	response, _ := browserRequest(t, client, "GET", callback, "")
	if response == nil || response.Header.Get("Location") != server.URL+"/" {
		t.Fatal("fresh login after failure did not succeed")
	}
	csrf := browserAuth(t, client, server.URL, "viewer")
	for _, stale := range []string{oldCallback, deniedCallback, callback} {
		response, _ := browserRequest(t, client, "GET", stale, "")
		if response == nil || !strings.Contains(response.Header.Get("Location"), "signin=failed") || len(response.Cookies()) != 0 {
			t.Fatal("stale callback changed cookies or did not fail")
		}
		browserAuth(t, client, server.URL, "viewer")
	}
	response, _ = browserRequest(t, client, "GET", server.URL+"/api/auth/login", "")
	if response == nil || response.StatusCode != http.StatusSeeOther || len(response.Cookies()) != 0 {
		t.Fatal("authenticated login changed cookies")
	}
	logout, _ := browserRequest(t, client, "POST", server.URL+"/api/auth/logout", csrf)
	if logout == nil || logout.StatusCode != http.StatusNoContent {
		t.Fatal("logout failed")
	}
	browserAuth(t, client, server.URL, "denied")
}

func TestOIDCBrowserBindingValidationAndLifetime(t *testing.T) {
	app, _ := oidcTestApp(t)
	server := httptest.NewTLSServer(app)
	t.Cleanup(server.Close)
	app.auth.settings.Origin = server.URL
	app.auth.oauth.RedirectURL = server.URL + "/api/auth/callback"
	client := authBrowser(t, server)
	start, _ := browserRequest(t, client, "GET", server.URL+"/api/auth/login", "")
	callback := browserAuthorize(t, client, start, "viewer")
	binding := start.Cookies()[0]
	if binding.Name != loginCookie || !binding.Secure || !binding.HttpOnly || binding.Domain != "" || binding.Path != "/" || binding.SameSite != http.SameSiteLaxMode || binding.MaxAge < 7190 || binding.MaxAge > 7200 {
		t.Fatalf("unsafe or short-lived binding: %+v", binding)
	}
	parsed, _ := url.Parse(callback)
	app.auth.mu.Lock()
	transaction := app.auth.pending[parsed.Query().Get("state")]
	app.auth.mu.Unlock()
	if !binding.Expires.After(transaction.Expires.Add(time.Hour)) {
		t.Fatal("binding can expire before maximum session horizon")
	}
	completed, _ := browserRequest(t, client, "GET", callback, "")
	if completed == nil || len(completed.Cookies()) != 1 || completed.Cookies()[0].Name != sessionCookie {
		t.Fatal("successful callback must only set the session cookie")
	}
	session := completed.Cookies()[0]
	if !binding.Expires.After(session.Expires) {
		t.Fatal("binding ends session early")
	}
	csrf := browserAuth(t, client, server.URL, "viewer")
	withoutJar := *client
	withoutJar.Jar = nil
	for _, tc := range []struct {
		name     string
		bindings []*http.Cookie
		status   int
	}{
		{"missing", nil, http.StatusUnauthorized},
		{"empty", []*http.Cookie{{Name: loginCookie}}, http.StatusUnauthorized},
		{"malformed", []*http.Cookie{{Name: loginCookie, Value: "invalid"}}, http.StatusUnauthorized},
		{"wrong", []*http.Cookie{{Name: loginCookie, Value: strings.Repeat("a", 48)}}, http.StatusUnauthorized},
		{"duplicate", []*http.Cookie{binding, binding}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, _ := http.NewRequest("GET", server.URL+"/api/bootstrap", nil)
			request.AddCookie(session)
			for _, cookie := range tc.bindings {
				request.AddCookie(cookie)
			}
			response, err := withoutJar.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("binding rejection = %d, want %d", response.StatusCode, tc.status)
			}
			browserAuth(t, client, server.URL, "viewer")
		})
	}
	logout, _ := browserRequest(t, client, "POST", server.URL+"/api/auth/logout", csrf)
	if logout == nil || logout.StatusCode != http.StatusNoContent {
		t.Fatal("logout failed")
	}
	origin, _ := url.Parse(server.URL)
	if len(client.Jar.Cookies(origin)) != 0 {
		t.Fatal("logout did not clear both cookies")
	}
	request, _ := http.NewRequest("GET", server.URL+"/api/bootstrap", nil)
	request.AddCookie(session)
	request.AddCookie(binding)
	response, err := withoutJar.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("copied session and binding survived logout")
	}
}

func TestOIDCBrowserDelayedInitialLoginAfterLogout(t *testing.T) {
	app, _ := oidcTestApp(t)
	ready, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" && r.URL.Query().Get("hold") == "yes" {
			buffer := httptest.NewRecorder()
			app.ServeHTTP(buffer, r)
			close(ready)
			<-release
			for name, values := range buffer.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(buffer.Code)
			_, _ = w.Write(buffer.Body.Bytes())
			return
		}
		app.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	app.auth.settings.Origin = server.URL
	app.auth.oauth.RedirectURL = server.URL + "/api/auth/callback"
	client := authBrowser(t, server)
	done := make(chan *http.Response, 1)
	go func() {
		response, _ := browserRequest(t, client, "GET", server.URL+"/api/auth/login?hold=yes", "")
		done <- response
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("initial response did not reach barrier")
	}
	start, _ := browserRequest(t, client, "GET", server.URL+"/api/auth/login", "")
	callback := browserAuthorize(t, client, start, "viewer")
	browserRequest(t, client, "GET", callback, "")
	csrf := browserAuth(t, client, server.URL, "viewer")
	logout, _ := browserRequest(t, client, "POST", server.URL+"/api/auth/logout", csrf)
	if logout == nil || logout.StatusCode != http.StatusNoContent {
		t.Fatal("logout failed")
	}
	once.Do(func() { close(release) })
	delayed := <-done
	browserAuth(t, client, server.URL, "denied")
	newCallback := browserAuthorize(t, client, delayed, "admin")
	browserRequest(t, client, "GET", newCallback, "")
	browserAuth(t, client, server.URL, "admin")
}
