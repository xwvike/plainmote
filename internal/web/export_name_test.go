package web

import "testing"

// The owner's filename is the file. Only a resource with none gets one made up.
func TestExportKeepsTheOwnersFilename(t *testing.T) {
	for _, tc := range []struct {
		resource Resource
		want     string
	}{
		{Resource{ID: "a", Filename: "Dockerfile", ContentType: "text/plain; charset=utf-8"}, "files/a/Dockerfile"},
		{Resource{ID: "b", Filename: ".gitignore", ContentType: "text/plain; charset=utf-8"}, "files/b/.gitignore"},
		{Resource{ID: "c", Filename: ".env", ContentType: "text/plain; charset=utf-8"}, "files/c/.env"},
		{Resource{ID: "d", Filename: "config.yaml", ContentType: "text/plain; charset=utf-8"}, "files/d/config.yaml"},
		{Resource{ID: "e", Name: "我的笔记", ContentType: "text/markdown; charset=utf-8"}, "files/e/我的笔记.md"},
		{Resource{ID: "f", ContentType: "text/plain; charset=utf-8"}, "files/f/content.txt"},
		{Resource{ID: "g", Name: "../../etc/passwd", ContentType: "text/plain"}, "files/g/_.._etc_passwd.txt"},
		{Resource{ID: "h", Filename: "..", Name: "notes", ContentType: "text/plain"}, "files/h/notes.txt"},
		{Resource{ID: "i", Filename: "remote.txt", OriginURL: "https://example.com/remote.txt"}, "files/i/remote.txt.url"},
		{Resource{ID: "j", Name: "Upstream", OriginURL: "https://example.com/x"}, "files/j/Upstream.url"},
	} {
		if got := exportBodyPath(tc.resource); got != tc.want {
			t.Errorf("%q / %q: got %q, want %q", tc.resource.Filename, tc.resource.Name, got, tc.want)
		}
	}
}
