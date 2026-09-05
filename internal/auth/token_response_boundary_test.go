package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenLifetimeCannotWrapIntoAnAcceptedDuration(t *testing.T) {
	// Multiplication by time.Second wraps this 584-year lifetime to one hour.
	const expiresIn int64 = 18446747674
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"test-access","refresh_token":"test-refresh","token_type":"bearer","expires_in":%d}`, expiresIn)
	}))
	defer srv.Close()
	_, err := newTokenClient(nil).Refresh(context.Background(), &Manifest{TokenEndpoint: srv.URL}, "test-refresh")
	if err == nil {
		t.Fatal("accepted an overflowing token lifetime")
	}
}

func TestProviderOutageCannotRevokeARefreshGrant(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
			}))
			defer srv.Close()
			_, err := newTokenClient(nil).Refresh(context.Background(), &Manifest{TokenEndpoint: srv.URL}, "test-refresh")
			if CodeOf(err) != CodeRefreshFailed {
				t.Fatalf("transient HTTP %d must preserve the session; got %v", status, err)
			}
		})
	}
}
