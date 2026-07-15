package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cinemator/presentation/settings"

	"golang.org/x/crypto/bcrypt"
)

func TestNewAuthenticatorIsOptionalAndValidatesHash(t *testing.T) {
	auth, err := newAuthenticator("")
	if err != nil {
		t.Fatalf("newAuthenticator(empty) error = %v", err)
	}
	if auth != nil {
		t.Fatal("newAuthenticator(empty) did not disable authentication")
	}

	if _, err := newAuthenticator("not-a-bcrypt-hash"); err == nil {
		t.Fatal("newAuthenticator(invalid) error = nil, want error")
	}
}

func TestAuthenticationFlowProtectsApplication(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword() error = %v", err)
	}
	auth, err := newAuthenticator(string(hash))
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

	reusedSession := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), session)
	if reusedSession.Code != http.StatusUnauthorized {
		t.Fatalf("reused session status = %d, want %d", reusedSession.Code, http.StatusUnauthorized)
	}
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
