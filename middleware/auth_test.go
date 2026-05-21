package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bobbyo/ccr/db"
)

func TestAuth_MissingKey(t *testing.T) {
	database, _ := db.Open(":memory:")
	defer database.Close()

	h := Auth(database)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestAuth_ValidKey(t *testing.T) {
	database, _ := db.Open(":memory:")
	defer database.Close()

	_ = db.CreateKey(database, "sk-ccr-abc123", "test")

	h := Auth(database)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("x-api-key", "sk-ccr-abc123")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestAuth_BearerToken(t *testing.T) {
	database, _ := db.Open(":memory:")
	defer database.Close()

	_ = db.CreateKey(database, "sk-ccr-bearer123", "test")

	h := Auth(database)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer sk-ccr-bearer123")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestAuth_RevokedKey(t *testing.T) {
	database, _ := db.Open(":memory:")
	defer database.Close()

	_ = db.CreateKey(database, "sk-ccr-revoked", "test")
	_ = db.RevokeKey(database, "sk-ccr-revoked")

	h := Auth(database)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("x-api-key", "sk-ccr-revoked")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for revoked key, got %d", rr.Code)
	}
}

func TestAuth_UnknownKey(t *testing.T) {
	database, _ := db.Open(":memory:")
	defer database.Close()

	h := Auth(database)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("x-api-key", "sk-ccr-notexist")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unknown key, got %d", rr.Code)
	}
}

func TestBroadcaster_PubSub(t *testing.T) {
	bc := NewBroadcaster()
	ch := bc.Subscribe()

	ev := LogEvent{Status: 200, RoutedModel: "claude"}
	bc.Publish(ev)

	data := <-ch
	if len(data) == 0 {
		t.Error("expected non-empty event data")
	}

	bc.Unsubscribe(ch)
}

func TestBroadcaster_SlowSubscriber(t *testing.T) {
	bc := NewBroadcaster()
	// Fill up the buffer of 64 without reading — Publish should not block.
	ch := bc.Subscribe()
	for i := 0; i < 100; i++ {
		bc.Publish(LogEvent{Status: 200})
	}
	bc.Unsubscribe(ch)
}
