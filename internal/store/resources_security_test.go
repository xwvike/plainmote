package store

import "testing"

func TestNormalizeResourceInputRejectsPrivateUpstream(t *testing.T) {
	for _, originURL := range []string{
		"http://127.0.0.1/resource",
		"http://10.0.0.1/resource",
		"http://100.64.0.1/resource",
		"http://169.254.169.254/latest/meta-data",
	} {
		if _, err := normalizeResourceInput("remote", "remote.txt", nil, "", originURL); err == nil {
			t.Errorf("private upstream %q was accepted", originURL)
		}
	}
}

func TestNormalizeResourceInputAcceptsPublicUpstream(t *testing.T) {
	input, err := normalizeResourceInput("remote", "remote.txt", nil, "", "https://upstream.example/resource")
	if err != nil {
		t.Fatal(err)
	}
	if input.OriginURL != "https://upstream.example/resource" || len(input.Content) != 0 {
		t.Fatalf("unexpected remote resource input: %+v", input)
	}
}
