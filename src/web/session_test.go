package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"cinemator/config"

	"golang.org/x/crypto/bcrypt"
)

func TestSessionRenewalWindow(t *testing.T) {
	for _, test := range []struct {
		name      string
		remaining time.Duration
		version   string
		renew     bool
	}{
		{"before window", sessionTTL/3 + time.Second, "v2", false},
		{"at window", sessionTTL / 3, "v2", true},
		{"inside window", time.Hour, "v2", true},
		{"legacy cookie", time.Hour, "v1", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, handler := sessionServerForTest(t)
				cookie := signedSessionCookieForTest(test.version, time.Now().Add(test.remaining), "1")
				rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), cookie)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				if !test.renew {
					if len(rec.Result().Cookies()) != 0 {
						t.Fatal("session renewed outside its renewal window or from a legacy token")
					}
					return
				}
				renewed := responseSessionForTest(t, rec)
				assertSessionForTest(t, renewed, time.Now().Add(sessionTTL), 2)
				if renewed.Value == cookie.Value {
					t.Fatal("renewal reused the previous token")
				}
				if renewed.MaxAge != int(sessionTTL/time.Second) || !renewed.HttpOnly || !renewed.Secure || renewed.SameSite != http.SameSiteStrictMode {
					t.Fatal("renewed cookie lost its lifetime or security attributes")
				}
			})
		})
	}
}

func TestQRSessionInheritsLegacyExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		auth, handler := sessionServerForTest(t)
		deviceToken, request, err := auth.startSignInRequest("https://cinemator.test", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		expiresAt := time.Now().Add(time.Hour)
		parent := signedSessionCookieForTest("v1", expiresAt, "")
		approved := serveRequest(handler, httptest.NewRequest(http.MethodPost, "/api/auth/sign-in-approvals/"+request.approvalToken, nil), parent)
		if approved.Code != http.StatusNoContent {
			t.Fatalf("approval status = %d, want 204", approved.Code)
		}
		rec := serveRequest(handler, httptest.NewRequest(http.MethodPost, "/api/auth/sign-in-requests/"+deviceToken+"/session", nil), nil)
		cookie := responseSessionForTest(t, rec)
		assertSessionForTest(t, cookie, expiresAt, maxSessionRenewals)
		if !cookie.Expires.Equal(expiresAt) || cookie.MaxAge != int(time.Hour/time.Second) {
			t.Fatalf("QR cookie expires %v with MaxAge %d, want %v and 3600", cookie.Expires, cookie.MaxAge, expiresAt)
		}
		app := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), cookie)
		if app.Code != http.StatusOK || len(app.Result().Cookies()) != 0 {
			t.Fatalf("legacy QR session status = %d, renewal cookies = %d", app.Code, len(app.Result().Cookies()))
		}
	})
}

func TestSessionRenewalLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, handler := sessionServerForTest(t)
		login := func() *http.Cookie {
			rec := serveRequest(handler, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"correct horse"}`)), nil)
			cookie := responseSessionForTest(t, rec)
			assertSessionForTest(t, cookie, time.Now().Add(sessionTTL), 0)
			return cookie
		}
		request := func(cookie *http.Cookie) *httptest.ResponseRecorder {
			return serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), cookie)
		}
		cookie := login()
		for count := 1; count <= maxSessionRenewals; count++ {
			time.Sleep(sessionTTL * 2 / 3)
			previous := cookie
			cookie = responseSessionForTest(t, request(previous))
			assertSessionForTest(t, cookie, time.Now().Add(sessionTTL), count)

			// A lost renewal response must not strand the previous, unexpired cookie.
			retried := responseSessionForTest(t, request(previous))
			assertSessionForTest(t, retried, cookie.Expires, count)
			if retried.Value == cookie.Value {
				t.Fatal("separate renewals reused the same token")
			}
			if rec := request(cookie); rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 {
				t.Fatal("freshly renewed session was rejected or renewed again")
			}
		}
		time.Sleep(sessionTTL * 2 / 3)
		if rec := request(cookie); rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 {
			t.Fatal("exhausted session must remain usable without further renewal")
		}
		time.Sleep(sessionTTL / 3)
		if rec := request(cookie); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
			t.Fatal("expired session was accepted or renewed")
		}
		wrongPassword := serveRequest(handler, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"wrong"}`)), cookie)
		if wrongPassword.Code != http.StatusUnauthorized || len(wrongPassword.Result().Cookies()) != 0 {
			t.Fatal("incorrect password reset the session")
		}
		cookie = login()
		if request(cookie).Code != http.StatusOK {
			t.Fatal("password login did not restore access")
		}
	})
}

func TestSessionRejectsInvalidTokens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, handler := sessionServerForTest(t)
		expiresAt := time.Now().Add(time.Hour)
		tampered := signedSessionCookieForTest("v2", expiresAt, "1")
		parts := strings.Split(tampered.Value, ".")
		parts[3] = "0"
		tampered.Value = strings.Join(parts, ".")
		for _, test := range []struct {
			name   string
			cookie *http.Cookie
		}{
			{"missing", nil},
			{"expired", signedSessionCookieForTest("v2", time.Now().Add(-time.Second), "0")},
			{"at expiry", signedSessionCookieForTest("v2", time.Now(), "0")},
			{"expired legacy", signedSessionCookieForTest("v1", time.Now(), "")},
			{"negative count", signedSessionCookieForTest("v2", expiresAt, "-1")},
			{"excess count", signedSessionCookieForTest("v2", expiresAt, strconv.Itoa(maxSessionRenewals+1))},
			{"missing count", signedSessionCookieForTest("v2", expiresAt, "")},
			{"invalid count", signedSessionCookieForTest("v2", expiresAt, "abc")},
			{"overflow count", signedSessionCookieForTest("v2", expiresAt, "99999999999999999999999999999999")},
			{"unknown version", signedSessionCookieForTest("v3", expiresAt, "0")},
			{"tampered count", tampered},
		} {
			rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), test.cookie)
			if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
				t.Errorf("%s: status = %d with %d cookies, want 401 without cookies", test.name, rec.Code, len(rec.Result().Cookies()))
			}
		}
	})
}

func TestSessionLogoutDoesNotRenew(t *testing.T) {
	_, handler := sessionServerForTest(t)
	cookie := signedSessionCookieForTest("v2", time.Now().Add(time.Hour), "0")
	rec := serveRequest(handler, httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil), cookie)
	if responseSessionForTest(t, rec).MaxAge >= 0 {
		t.Fatal("logout did not remove the session cookie")
	}
}

func TestQRSessionInheritsRenewalBudget(t *testing.T) {
	for _, count := range []int{0, 1, maxSessionRenewals} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				auth, handler := sessionServerForTest(t)
				expiresAt := time.Now().Add(time.Hour)
				parent := signedSessionCookieForTest("v2", expiresAt, strconv.Itoa(count))
				for range 2 {
					deviceToken, request, err := auth.startSignInRequest("https://cinemator.test", time.Now())
					if err != nil {
						t.Fatal(err)
					}
					approvalPath := "/api/auth/sign-in-approvals/" + request.approvalToken
					approved := serveRequest(handler, httptest.NewRequest(http.MethodPost, approvalPath, nil), parent)
					if approved.Code != http.StatusNoContent || len(approved.Result().Cookies()) != 0 {
						t.Fatal("QR approval failed or renewed the approving session")
					}
					// Repeated approval by a fresh session cannot overwrite the first approval.
					fresh := signedSessionCookieForTest("v2", time.Now().Add(sessionTTL), "0")
					if serveRequest(handler, httptest.NewRequest(http.MethodPost, approvalPath, nil), fresh).Code != http.StatusNoContent {
						t.Fatal("repeated approval was not idempotent")
					}
					for range 2 {
						time.Sleep(time.Minute)
						rec := serveRequest(handler, httptest.NewRequest(http.MethodPost, "/api/auth/sign-in-requests/"+deviceToken+"/session", nil), nil)
						child := responseSessionForTest(t, rec)
						assertSessionForTest(t, child, expiresAt, count)
						if child.Value == parent.Value || child.MaxAge != int(time.Until(expiresAt)/time.Second) {
							t.Fatal("QR session reused a token or reset the cookie lifetime")
						}
						parent = child
					}
				}
				rec := serveRequest(handler, httptest.NewRequest(http.MethodGet, "/api/downloads", nil), parent)
				if rec.Code != http.StatusOK {
					t.Fatalf("QR session rejected: %d", rec.Code)
				}
				if count == maxSessionRenewals {
					if len(rec.Result().Cookies()) != 0 {
						t.Fatal("QR sign-in bypassed the renewal limit")
					}
				} else {
					assertSessionForTest(t, responseSessionForTest(t, rec), time.Now().Add(sessionTTL), count+1)
				}
			})
		})
	}
}

func TestQRSessionCannotOutliveApproval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		auth, handler := sessionServerForTest(t)
		deviceToken, request, err := auth.startSignInRequest("https://cinemator.test", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		parent := signedSessionCookieForTest("v2", time.Now().Add(time.Second), "1")
		approvalPath := "/api/auth/sign-in-approvals/" + request.approvalToken
		if serveRequest(handler, httptest.NewRequest(http.MethodPost, approvalPath, nil), parent).Code != http.StatusNoContent {
			t.Fatal("approval failed")
		}
		time.Sleep(time.Second)
		if rec := serveRequest(handler, httptest.NewRequest(http.MethodPost, approvalPath, nil), parent); rec.Code != http.StatusUnauthorized {
			t.Fatal("expired session was allowed to approve QR sign-in")
		}
		rec := serveRequest(handler, httptest.NewRequest(http.MethodPost, "/api/auth/sign-in-requests/"+deviceToken+"/session", nil), nil)
		if rec.Code != http.StatusGone || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("expired approving session produced status %d with %d cookies, want 410 without cookies", rec.Code, len(rec.Result().Cookies()))
		}
	})
}

func sessionServerForTest(t *testing.T) (*authenticator, http.Handler) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := newAuthenticator(string(hash), testSessionSecret)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{auth: auth, mgr: fakeTorrentManager{}, cfg: config.Load()}
	return auth, server.handler()
}

func signedSessionCookieForTest(version string, expiresAt time.Time, renewals string) *http.Cookie {
	parts := []string{version, strconv.FormatInt(expiresAt.Unix(), 10), base64.RawURLEncoding.EncodeToString(make([]byte, sessionNonceBytes))}
	if version != "v1" {
		parts = append(parts, renewals)
	}
	payload := strings.Join(parts, ".")
	mac := hmac.New(sha256.New, []byte(testSessionSecret))
	mac.Write([]byte(payload))
	return &http.Cookie{Name: sessionCookieName, Value: payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}
}

func responseSessionForTest(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("session response = %d with %d cookies: %s", rec.Code, len(cookies), rec.Body.String())
	}
	return cookies[0]
}

func assertSessionForTest(t *testing.T, cookie *http.Cookie, expiresAt time.Time, renewals int) {
	t.Helper()
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 5 || parts[0] != "v2" || parts[1] != strconv.FormatInt(expiresAt.Unix(), 10) || parts[3] != strconv.Itoa(renewals) || !cookie.Expires.Equal(expiresAt) {
		t.Errorf("session does not have expected expiry %v and renewal count %d", expiresAt, renewals)
	}
}
