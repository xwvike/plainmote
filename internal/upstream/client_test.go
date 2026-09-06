package upstream

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestFetchAllowsThreeRedirects(t *testing.T) {
	server := redirectServer(t)
	client := New(true, 1024)
	body, contentType, err := client.Fetch(t.Context(), server.URL+"/3")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "done" || contentType != "text/plain; charset=utf-8" {
		t.Fatalf("unexpected response: %q %q", body, contentType)
	}
}

func TestFetchRejectsFourthRedirect(t *testing.T) {
	server := redirectServer(t)
	client := New(true, 1024)
	if _, _, err := client.Fetch(t.Context(), server.URL+"/4"); err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("four redirects returned %v", err)
	}
}

func TestValidateURLRejectsPrivateAddressesByDefault(t *testing.T) {
	for _, rawURL := range []string{
		"http://127.0.0.1/resource",
		"http://[::1]/resource",
		"http://169.254.169.254/latest/meta-data",
	} {
		request, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateURL(request.URL, false); err == nil {
			t.Errorf("private URL %q was accepted", rawURL)
		}
		if err := ValidateURL(request.URL, true); err != nil {
			t.Errorf("private URL %q was rejected after opt-in: %v", rawURL, err)
		}
	}
}

func TestFetchCannotConnectToPrivateAddressWithoutOptIn(t *testing.T) {
	server := redirectServer(t)
	client := New(false, 1024)
	if _, _, err := client.Fetch(t.Context(), server.URL+"/0"); err == nil {
		t.Fatal("client connected to a loopback upstream without opt-in")
	}
}

func redirectServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remaining, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if remaining > 0 {
			http.Redirect(w, r, "/"+strconv.Itoa(remaining-1), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("done"))
	}))
	t.Cleanup(server.Close)
	return server
}
