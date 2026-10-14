package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestFetchFollowsNoRedirect(t *testing.T) {
	client := New(1024)
	calls := 0
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		header := make(http.Header)
		header.Set("Location", "https://elsewhere.example/document.json")
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})
	if _, _, err := client.Fetch(t.Context(), "https://upstream.example/start"); err == nil {
		t.Fatal("a redirect was taken for the document")
	}
	if calls != 1 {
		t.Fatalf("the redirect was followed: %d calls", calls)
	}
}

func TestValidateURLRejectsPrivateAddresses(t *testing.T) {
	for _, rawURL := range []string{
		"http://[64:ff9b::a9fe:a9fe]/latest/meta-data",
		"http://[2002:a9fe:a9fe::1]/resource",
		"http://localhost/resource",
		"http://service.localhost/resource",
		"http://127.0.0.1/resource",
		"http://[::1]/resource",
		"http://10.0.0.1/resource",
		"http://100.64.0.1/resource",
		"http://172.16.0.1/resource",
		"http://192.168.0.1/resource",
		"http://198.18.0.1/resource",
		"http://240.0.0.1/resource",
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1/",
	} {
		request, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateURL(request.URL); err == nil {
			t.Errorf("private URL %q was accepted", rawURL)
		}
	}
}

func TestValidateURLAcceptsPublicAddresses(t *testing.T) {
	for _, rawURL := range []string{
		"https://example.com/resource",
		"http://203.0.113.10:8080/resource",
	} {
		request, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateURL(request.URL); err != nil {
			t.Errorf("public URL %q was rejected: %v", rawURL, err)
		}
	}
}

func TestFetchRejectsPrivateAddress(t *testing.T) {
	client := New(1024)
	if _, _, err := client.Fetch(t.Context(), "http://127.0.0.1/resource"); err == nil {
		t.Fatal("client accepted a loopback upstream")
	}
}

func TestFetchRejectsRedirectToPrivateAddress(t *testing.T) {
	client := New(1024)
	calls := 0
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		header := make(http.Header)
		header.Set("Location", "http://127.0.0.1/private")
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})
	if _, _, err := client.Fetch(t.Context(), "https://upstream.example/start"); err == nil {
		t.Fatal("client followed a redirect to a loopback upstream")
	}
	if calls != 1 {
		t.Fatalf("private redirect reached the transport: %d calls", calls)
	}
}
