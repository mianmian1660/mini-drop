package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAuthCheckAcceptsHyphenatedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &APIServer{}
	router := gin.New()
	router.GET("/api/v1/auth/check", s.AuthCheck)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/check", nil)
	req.Header.Set("Drop-User-Uid", "user-1")
	req.Header.Set("Drop-User-Name", "Alice")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"uid":"user-1"`) || !strings.Contains(body, `"user_name":"Alice"`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestAuthCheckAcceptsLegacyUnderscoreHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &APIServer{}
	router := gin.New()
	router.GET("/api/v1/auth/check", s.AuthCheck)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/check", nil)
	req.Header.Set("Drop_user_uid", "legacy-user")
	req.Header.Set("Drop_user_name", "Legacy")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, `"uid":"legacy-user"`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestAuthCheckRejectsMissingIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &APIServer{}
	router := gin.New()
	router.GET("/api/v1/auth/check", s.AuthCheck)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/check", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestAuthCheckAcceptsLoginCookies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &APIServer{}
	router := gin.New()
	router.GET("/api/v1/auth/check", s.AuthCheck)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/check", nil)
	req.AddCookie(&http.Cookie{Name: "drop_user_uid", Value: "cookie-user"})
	req.AddCookie(&http.Cookie{Name: "drop_user_name", Value: "Cookie User"})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"uid":"cookie-user"`) || !strings.Contains(body, `"user_name":"Cookie User"`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestCheckLoginAcceptsHyphenatedHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &APIServer{}
	router := gin.New()
	router.Use(s.CheckLogin)
	router.GET("/protected", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Drop-User-Uid", "user-1")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestCheckLoginAcceptsLoginCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &APIServer{}
	router := gin.New()
	router.Use(s.CheckLogin)
	router.GET("/protected", func(c *gin.Context) {
		c.String(http.StatusOK, getRequestUID(c))
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: "drop_user_uid", Value: "cookie-user"})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "cookie-user" {
		t.Fatalf("body = %q, want cookie-user", got)
	}
}
