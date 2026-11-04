package cli

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCredentialPayloadRedirectFailsClosed(t *testing.T) {
	var calls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	if response, err := apiRequest(t.Context(), origin.URL, "POST", "/device", nil, map[string]string{"device_code": "fixture"}); err == nil {
		response.Body.Close()
		t.Fatal("credential payload followed cross-origin redirect")
	}
	if calls.Load() != 0 {
		t.Fatal("credential payload reached redirected origin")
	}
}
