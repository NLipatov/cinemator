package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cinemator/presentation/settings"

	"golang.org/x/crypto/bcrypt"
)

const testSessionSecret = "0123456789abcdef0123456789abcdef"

func TestNewAuthenticatorIsOptionalAndValidatesConfiguration(t *testing.T) {
	auth, err := newAuthenticator("", "")
	if err != nil {
		t.Fatalf("newAuthenticator(empty) error = %v", err)
	}
	if auth != nil {
		t.Fatal("newAuthenticator(empty) did not disable authentication")
	}

	if _, err := newAuthenticator("not-a-bcrypt-hash", testSessionSecret); err == nil {
		t.Fatal("newAuthenticator(invalid) error = nil, want error")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword() error = %v", err)
	}
	if _, err := newAuthenticator(string(hash), "too-short"); err == nil {
		t.Fatal("newAuthenticator(short secret) error = nil, want error")
	}
}

func TestAuthenticationFlowProtectsApplication(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword() error = %v", err)
	}
	auth, err := newAuthenticator(string(hash), testSessionSecret)
	if err != nil {
		t.Fatalf("newAuthenticator() error = %v", err)
	}
	server := HttpServer{
		mgr:      fakeTorrentManager{},
		settings: settings.NewSettings(),
		auth:     auth,
	}
	handler := server.handler()

	t.Run("browser redirected to login", func(t *testing.T) {
		rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/", nil), nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
		}
		if got := rec.Header().Get("Location"); got != "/login" {
			t.Fatalf("Location = %q, want /login", got)
		}
	})

	t.Run("api rejects missing session", func(t *testing.T) {
		for _, path := range []string{"/api/downloads", "/api/hls/example/master.m3u8"} {
			rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, path, nil), nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("GET %s status = %d, want %d", path, rec.Code, http.StatusUnauthorized)
			}
		}
	})

	t.Run("wrong password rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"wrong"}`))
		rec := serveRequest(handler, req, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	loginRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/auth/login",
		strings.NewReader(`{"password":"correct horse"}`),
	)
	loginResponse := serveRequest(handler, loginRequest, nil)
	if loginResponse.Code != http.StatusNoContent {
		t.Fatalf("login status = %d, want %d; body = %q", loginResponse.Code, http.StatusNoContent, loginResponse.Body.String())
	}
	cookies := loginResponse.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %d, want 1", len(cookies))
	}
	session := cookies[0]
	if session.Name != sessionCookieName || session.Value == "" {
		t.Fatalf("session cookie = %#v", session)
	}
	if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie flags = HttpOnly:%v Secure:%v SameSite:%v", session.HttpOnly, session.Secure, session.SameSite)
	}

	t.Run("valid session reaches api", func(t *testing.T) {
		rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), session)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
	})

	t.Run("authenticated page disables caching", func(t *testing.T) {
		protectedPage := server.requireAuthentication(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		rec := serveRequest(protectedPage, httptest.NewRequest(http.MethodGet, "/", nil), session)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
	})

	t.Run("another instance accepts session signed with the same secret", func(t *testing.T) {
		peerAuth, err := newAuthenticator(string(hash), testSessionSecret)
		if err != nil {
			t.Fatalf("newAuthenticator() error = %v", err)
		}
		peer := HttpServer{
			mgr:      fakeTorrentManager{},
			settings: settings.NewSettings(),
			auth:     peerAuth,
		}
		rec := serveRequest(peer.handler(), httptest.NewRequest(http.MethodGet, "/api/downloads", nil), session)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
		}
	})

	t.Run("different secret rejects session", func(t *testing.T) {
		peerAuth, err := newAuthenticator(string(hash), "abcdef0123456789abcdef0123456789")
		if err != nil {
			t.Fatalf("newAuthenticator() error = %v", err)
		}
		peer := HttpServer{mgr: fakeTorrentManager{}, settings: settings.NewSettings(), auth: peerAuth}
		rec := serveRequest(peer.handler(), httptest.NewRequest(http.MethodGet, "/api/downloads", nil), session)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("tampered session is rejected", func(t *testing.T) {
		tampered := *session
		signatureStart := strings.LastIndex(tampered.Value, ".") + 1
		replacement := byte('A')
		if tampered.Value[signatureStart] == replacement {
			replacement = 'B'
		}
		tampered.Value = tampered.Value[:signatureStart] + string(replacement) + tampered.Value[signatureStart+1:]
		rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), &tampered)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("expired session is rejected", func(t *testing.T) {
		token, err := auth.newSessionToken(time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("newSessionToken() error = %v", err)
		}
		expired := &http.Cookie{Name: sessionCookieName, Value: token}
		rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), expired)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("authenticated login page redirects home", func(t *testing.T) {
		rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/login", nil), session)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
			t.Fatalf("status = %d, Location = %q", rec.Code, rec.Header().Get("Location"))
		}
	})

	logoutResponse := serveRequest(
		handler,
		httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil),
		session,
	)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d", logoutResponse.Code, http.StatusNoContent)
	}
	logoutCookies := logoutResponse.Result().Cookies()
	if len(logoutCookies) != 1 || logoutCookies[0].MaxAge >= 0 {
		t.Fatalf("logout cookie = %#v, want expired cookie", logoutCookies)
	}

}

func TestSignInRequestRequiresSameHostOrigin(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword() error = %v", err)
	}
	auth, err := newAuthenticator(string(hash), testSessionSecret)
	if err != nil {
		t.Fatalf("newAuthenticator() error = %v", err)
	}
	server := HttpServer{mgr: fakeTorrentManager{}, settings: settings.NewSettings(), auth: auth}
	handler := server.handler()

	for _, origin := range []string{"", "https://other.test"} {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/sign-in-requests", nil)
		req.Host = "cinemator.test"
		req.Header.Set("Origin", origin)
		rec := serveRequest(handler, req, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("origin %q status = %d, want %d", origin, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestSignInRequestFlow(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword() error = %v", err)
	}
	auth, err := newAuthenticator(string(hash), testSessionSecret)
	if err != nil {
		t.Fatalf("newAuthenticator() error = %v", err)
	}
	server := HttpServer{mgr: fakeTorrentManager{}, settings: settings.NewSettings(), auth: auth}
	handler := server.handler()

	type startedSignInRequest struct {
		DeviceToken string    `json:"deviceToken"`
		Code        string    `json:"code"`
		QRCode      string    `json:"qrCode"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}
	start := func(t *testing.T) (startedSignInRequest, *signInRequest) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/sign-in-requests", nil)
		req.Host = "cinemator.test"
		req.Header.Set("Origin", "https://cinemator.test")
		rec := serveRequest(handler, req, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("start status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
		}
		var started startedSignInRequest
		if err := json.NewDecoder(rec.Body).Decode(&started); err != nil {
			t.Fatalf("decode start response: %v", err)
		}
		if started.DeviceToken == "" || started.Code == "" || started.QRCode == "" || !started.ExpiresAt.After(time.Now()) {
			t.Fatalf("start response = %#v", started)
		}

		auth.signInMu.Lock()
		request, ok := auth.signInRequests[started.DeviceToken]
		auth.signInMu.Unlock()
		if !ok {
			t.Fatal("sign-in request was not stored")
		}
		wantURL := "https://cinemator.test/sign-in-approvals/" + request.approvalToken
		if request.approvalURL != wantURL {
			t.Fatalf("approval URL = %q, want %q", request.approvalURL, wantURL)
		}
		return started, request
	}
	sessionPath := func(deviceToken string) string {
		return "/api/auth/sign-in-requests/" + deviceToken + "/session"
	}

	started, request := start(t)
	qr := serveRequest(handler, httptest.NewRequest(http.MethodGet, started.QRCode, nil), nil)
	if qr.Code != http.StatusOK || qr.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("QR response = %d %q", qr.Code, qr.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(qr.Body.String(), "\x89PNG\r\n\x1a\n") {
		t.Fatal("QR response is not a PNG")
	}

	approvalPath := "/api/auth/sign-in-approvals/" + request.approvalToken
	if rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, approvalPath, nil), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated approval status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	login := serveRequest(
		handler,
		httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"correct horse"}`)),
		nil,
	)
	if login.Code != http.StatusNoContent || len(login.Result().Cookies()) != 1 {
		t.Fatalf("approver login = %d %q", login.Code, login.Body.String())
	}
	approverSession := login.Result().Cookies()[0]

	details := serveRequest(handler, httptest.NewRequest(http.MethodGet, approvalPath, nil), approverSession)
	if details.Code != http.StatusOK || !strings.Contains(details.Body.String(), `"code":"`+started.Code+`"`) {
		t.Fatalf("approval details = %d %q", details.Code, details.Body.String())
	}

	sessionResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		sessionResult <- serveRequest(
			handler,
			httptest.NewRequest(http.MethodPost, sessionPath(started.DeviceToken), nil),
			nil,
		)
	}()

	allow := serveRequest(
		handler,
		httptest.NewRequest(http.MethodPost, approvalPath, nil),
		approverSession,
	)
	if allow.Code != http.StatusNoContent {
		t.Fatalf("allow status = %d, want %d; body = %q", allow.Code, http.StatusNoContent, allow.Body.String())
	}

	var approved *httptest.ResponseRecorder
	select {
	case approved = <-sessionResult:
	case <-time.After(time.Second):
		t.Fatal("session request did not finish after approval")
	}
	if approved.Code != http.StatusNoContent || len(approved.Result().Cookies()) != 1 {
		t.Fatalf("approved session response = %d %q", approved.Code, approved.Body.String())
	}
	deviceSession := approved.Result().Cookies()[0]
	if deviceSession.Value == approverSession.Value {
		t.Fatal("device reused the approver session")
	}
	app := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), deviceSession)
	if app.Code != http.StatusOK {
		t.Fatalf("device session status = %d, want %d", app.Code, http.StatusOK)
	}

	retry := serveRequest(
		handler,
		httptest.NewRequest(http.MethodPost, sessionPath(started.DeviceToken), nil),
		nil,
	)
	if retry.Code != http.StatusNoContent || len(retry.Result().Cookies()) != 1 {
		t.Fatalf("retried session response = %d %q", retry.Code, retry.Body.String())
	}

	interrupted, interruptedRequest := start(t)
	ctx, cancel := context.WithCancel(context.Background())
	interruptedResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, sessionPath(interrupted.DeviceToken), nil).WithContext(ctx)
		interruptedResult <- serveRequest(handler, req, nil)
	}()
	cancel()
	select {
	case <-interruptedResult:
	case <-time.After(time.Second):
		t.Fatal("session request did not stop after disconnect")
	}
	if !auth.approveSignInRequest(interruptedRequest.approvalToken, time.Now()) {
		t.Fatal("disconnected sign-in request could not be approved")
	}
	reconnected := serveRequest(
		handler,
		httptest.NewRequest(http.MethodPost, sessionPath(interrupted.DeviceToken), nil),
		nil,
	)
	if reconnected.Code != http.StatusNoContent || len(reconnected.Result().Cookies()) != 1 {
		t.Fatalf("reconnected session response = %d %q", reconnected.Code, reconnected.Body.String())
	}

	expired, expiredRequest := start(t)
	auth.signInMu.Lock()
	expiredRequest.expiresAt = time.Now().Add(-time.Second)
	auth.signInMu.Unlock()
	if rec := serveRequest(handler, httptest.NewRequest(http.MethodPost, sessionPath(expired.DeviceToken), nil), nil); rec.Code != http.StatusGone {
		t.Fatalf("expired session status = %d, want %d", rec.Code, http.StatusGone)
	}
}

func TestSignInRequestConcurrentApproval(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword() error = %v", err)
	}
	auth, err := newAuthenticator(string(hash), testSessionSecret)
	if err != nil {
		t.Fatalf("newAuthenticator() error = %v", err)
	}
	server := HttpServer{mgr: fakeTorrentManager{}, settings: settings.NewSettings(), auth: auth}
	handler := server.handler()

	for attempt := 0; attempt < 20; attempt++ {
		deviceToken, request, err := auth.startSignInRequest("https://cinemator.test", time.Now())
		if err != nil {
			t.Fatalf("startSignInRequest() error = %v", err)
		}

		const waiterCount = 4
		start := make(chan struct{})
		responses := make(chan *httptest.ResponseRecorder, waiterCount)
		var wait sync.WaitGroup
		wait.Add(waiterCount)
		for range waiterCount {
			go func() {
				defer wait.Done()
				<-start
				path := "/api/auth/sign-in-requests/" + deviceToken + "/session"
				responses <- serveRequest(handler, httptest.NewRequest(http.MethodPost, path, nil), nil)
			}()
		}
		close(start)

		if !auth.approveSignInRequest(request.approvalToken, time.Now()) {
			t.Fatalf("attempt %d: approval was not recorded", attempt)
		}
		if !auth.approveSignInRequest(request.approvalToken, time.Now()) {
			t.Fatalf("attempt %d: repeated approval was not idempotent", attempt)
		}
		wait.Wait()
		close(responses)

		for response := range responses {
			if response.Code != http.StatusNoContent || len(response.Result().Cookies()) != 1 {
				t.Fatalf("attempt %d: session response = %d %q", attempt, response.Code, response.Body.String())
			}
		}
	}
}

func TestAuthenticationLoginLimits(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword() error = %v", err)
	}

	t.Run("attempt rate", func(t *testing.T) {
		auth, err := newAuthenticator(string(hash), testSessionSecret)
		if err != nil {
			t.Fatalf("newAuthenticator() error = %v", err)
		}
		server := HttpServer{mgr: fakeTorrentManager{}, settings: settings.NewSettings(), auth: auth}
		handler := server.handler()

		for attempt := 0; attempt < loginAttemptBurst; attempt++ {
			rec := serveRequest(
				handler,
				httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"wrong"}`)),
				nil,
			)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("attempt %d status = %d, want %d", attempt+1, rec.Code, http.StatusUnauthorized)
			}
		}

		rec := serveRequest(
			handler,
			httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"wrong"}`)),
			nil,
		)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("limited status = %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
		if got := rec.Header().Get("Retry-After"); got != "10" {
			t.Fatalf("Retry-After = %q, want 10", got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
	})

	t.Run("concurrent bcrypt work", func(t *testing.T) {
		auth, err := newAuthenticator(string(hash), testSessionSecret)
		if err != nil {
			t.Fatalf("newAuthenticator() error = %v", err)
		}
		for range maxConcurrentLoginChecks {
			auth.loginChecks <- struct{}{}
		}
		defer func() {
			for range maxConcurrentLoginChecks {
				<-auth.loginChecks
			}
		}()

		server := HttpServer{mgr: fakeTorrentManager{}, settings: settings.NewSettings(), auth: auth}
		rec := serveRequest(
			server.handler(),
			httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"correct horse"}`)),
			nil,
		)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Fatalf("Retry-After = %q, want 1", got)
		}
	})

	t.Run("bcrypt capacity is released", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			password   string
			wantStatus int
		}{
			{name: "rejected password", password: "wrong", wantStatus: http.StatusUnauthorized},
			{name: "accepted password", password: "correct horse", wantStatus: http.StatusNoContent},
		} {
			t.Run(test.name, func(t *testing.T) {
				auth, err := newAuthenticator(string(hash), testSessionSecret)
				if err != nil {
					t.Fatalf("newAuthenticator() error = %v", err)
				}
				server := HttpServer{mgr: fakeTorrentManager{}, settings: settings.NewSettings(), auth: auth}
				rec := serveRequest(
					server.handler(),
					httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"`+test.password+`"}`)),
					nil,
				)
				if rec.Code != test.wantStatus {
					t.Fatalf("status = %d, want %d", rec.Code, test.wantStatus)
				}
				if got := len(auth.loginChecks); got != 0 {
					t.Fatalf("active bcrypt checks = %d, want 0", got)
				}
			})
		}
	})
}

func TestMissingPasswordHashLeavesApplicationPublic(t *testing.T) {
	server := HttpServer{mgr: fakeTorrentManager{}, settings: settings.NewSettings()}
	handler := server.handler()

	rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}

	status := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/auth/status", nil), nil)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"enabled":false`) {
		t.Fatalf("auth status = %d %q", status.Code, status.Body.String())
	}
}

func serveRequest(handler http.Handler, request *http.Request, cookie *http.Cookie) *httptest.ResponseRecorder {
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
