package store

import (
	"bytes"
	"strings"
	"testing"
)

// TestDetectContentType covers the shapes this service actually hosts. Nobody
// types a content type any more, so this table is the whole contract.
func TestDetectContentType(t *testing.T) {
	clash := "port: 7890\nmode: rule\nproxies:\n  - name: hk-01\n    type: trojan\n"
	pngBytes := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, make([]byte, 32)...)
	jpegBytes := append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 32)...)
	gifBytes := append([]byte("GIF89a"), make([]byte, 32)...)
	pdfBytes := append([]byte("%PDF-1.7"), make([]byte, 32)...)
	singbox := "{\n  \"log\": {\"level\": \"info\"},\n  \"outbounds\": [{\"type\": \"direct\"}]\n}\n"

	for _, tc := range []struct{ name, path, body, want string }{
		{"clash 订阅", "clash.yaml", clash, typeYAML},
		{"yml 后缀", "/configs/compose.yml", clash, typeYAML},
		{"yaml 文档标记，无后缀", "/configs/anything", "---\nfoo: bar\n", typeYAML},
		{"sing-box", "/configs/sing-box.json", singbox, typeJSON},
		{"json 内容，无后缀", "/configs/singbox", singbox, typeJSON},
		{"json 数组", "/data/list", "[1, 2, 3]\n", typeJSON},
		{"xml 后缀", "/configs/config.xml", "<?xml version=\"1.0\"?>\n<c/>\n", typeXML},
		{"xml 内容，无后缀", "/configs/cfg", "<?xml version=\"1.0\"?>\n<c/>\n", typeXML},
		{"csv", "/data/rows.csv", "a,b\n1,2\n", typeCSV},
		{"toml", "/configs/app.toml", "[server]\nport = 80\n", typeTOML},
		{"nginx 片段", "/snippets/proxy.conf", "location / { proxy_pass http://x; }\n", typeText},
		{"authorized_keys", "/keys/authorized_keys", "ssh-ed25519 AAAA alice\n", typeText},
		{"hosts", "/snippets/hosts.txt", "127.0.0.1 localhost\n", typeText},
		{"dotenv", "/snippets/backup.env", "TOKEN=abc\n", typeText},
		{"wireguard", "/configs/wg0.conf", "[Interface]\nAddress = 10.0.0.2/32\n", typeText},
		{"半截 json 不算 json", "/data/broken", "{\"a\": ", typeText},
		{"二进制", "/blobs/thing", "\x00\xff\xfe\x01", typeBinary},
		{"后缀优先于内容", "/configs/a.json", clash, typeJSON},
		{"txt 后缀压过 json 内容", "/configs/a.txt", singbox, typeText},
		{"conf 后缀压过 json 内容", "/configs/a.conf", singbox, typeText},
		{"大小写后缀", "/configs/A.JSON", clash, typeJSON},
		{"png 带后缀", "logo.png", string(pngBytes), "image/png"},
		{"png 无后缀", "logo", string(pngBytes), "image/png"},
		{"jpeg 无后缀", "photo", string(jpegBytes), "image/jpeg"},
		{"gif 无后缀", "anim", string(gifBytes), "image/gif"},
		{"mp3 带后缀", "song.mp3", "whatever", "audio/mpeg"},
		{"mp4 带后缀", "clip.mp4", "whatever", "video/mp4"},
		{"pdf 无后缀", "doc", string(pdfBytes), "application/pdf"},
		{"未知二进制", "blob", "\x00\x01\xff\xfe乱", typeBinary},
	} {
		if got := DetectContentType(tc.path, []byte(tc.body)); got != tc.want {
			t.Errorf("%s (%s): got %q, want %q", tc.name, tc.path, got, tc.want)
		}
	}
}

func TestLegacyEncodedTextRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		encoding string
		content  string
	}{
		{"gb18030", "名称: 上海😀节点\n说明: 中文配置文件\n"},
		{"gbk", "名称: 上海节点\n说明: 中文配置文件\n"},
		{"big5", "名稱: 台北節點\n說明: 中文設定檔\n"},
		{"shift_jis", "名前: 東京ノード\n説明: 日本語設定ファイル\n"},
		{"windows-1252", "name: café\ndescription: déjà vu\n"},
		{"utf-16le-bom", "name: UTF-16 配置\n"},
		{"utf-32be-bom", "name: UTF-32 配置\n"},
	} {
		encoded, canonical, err := EncodeText(tc.content, tc.encoding)
		if err != nil {
			t.Fatalf("encode %s: %v", tc.encoding, err)
		}
		contentType, detected := DetectContent("config.yaml", encoded, canonical)
		if contentType != typeYAML || detected != canonical {
			t.Errorf("%s classified as %q / %q", tc.encoding, contentType, detected)
		}
		decoded, decodedAs, err := DecodeText(encoded, detected)
		if err != nil || decodedAs != canonical || decoded != tc.content {
			t.Errorf("%s decoded as %q: %v / %q", tc.encoding, decodedAs, err, decoded)
		}
		reencoded, _, err := EncodeText(decoded, decodedAs)
		if err != nil || !bytes.Equal(reencoded, encoded) {
			t.Errorf("%s did not round-trip: %v", tc.encoding, err)
		}
	}
}

func TestLegacyEncodingCanBeAutoDetected(t *testing.T) {
	content := "名称: 上海节点\n说明: 这是一个用于自动识别字符编码的中文配置文件。\n"
	encoded, _, err := EncodeText(content, "gb18030")
	if err != nil {
		t.Fatal(err)
	}
	decoded, encodingName, err := DecodeText(encoded, "")
	if err != nil || decoded != content || encodingName != "gb18030" {
		t.Fatalf("GB18030 auto-detection: encoding=%q err=%v content=%q", encodingName, err, decoded)
	}
}

func TestUnicodeWithoutBOMCanBeAutoDetected(t *testing.T) {
	content := "name: 中文 config file\n"
	for _, encodingName := range []string{"utf-16le", "utf-16be", "utf-32le", "utf-32be"} {
		encoded, _, err := EncodeText(content, encodingName)
		if err != nil {
			t.Fatal(err)
		}
		decoded, detected, err := DecodeText(encoded, "")
		if err != nil || decoded != content || detected != encodingName {
			t.Errorf("%s auto-detection: detected=%q err=%v content=%q", encodingName, detected, err, decoded)
		}
	}
}

func TestMalformedLegacyTextIsNeverDecodedWithReplacementCharacters(t *testing.T) {
	for _, tc := range []struct {
		encoding string
		content  []byte
	}{
		{"big5", []byte{0x81, 0x30}},
		{"gb18030", []byte{0x81, 0x20}},
	} {
		decoded, err := decodeWithEncoding(tc.content, tc.encoding)
		if err == nil {
			t.Errorf("%s malformed bytes decoded as %q", tc.encoding, decoded)
		}
	}
}

// Detection must never hand back a type a browser will execute: a resource
// living on this origin must not be able to become a script.
func TestDetectNeverReturnsAnExecutableType(t *testing.T) {
	bodies := []string{
		"<html><body><script>alert(1)</script></body></html>",
		"<!DOCTYPE html><script>alert(1)</script>",
		"<svg xmlns=\"http://www.w3.org/2000/svg\"><script>alert(1)</script></svg>",
		"alert(1)",
		"<?xml version=\"1.0\"?><svg xmlns=\"http://www.w3.org/2000/svg\"/>",
	}
	paths := []string{"/x", "/x.html", "/x.htm", "/x.svg", "/x.js", "/x.xhtml", "/x.json", "/x.yaml", "/x.png", "/x.mp4"}
	banned := []string{"html", "javascript", "ecmascript", "svg", "xhtml"}
	for _, path := range paths {
		for _, body := range bodies {
			got := DetectContentType(path, []byte(body))
			for _, word := range banned {
				if strings.Contains(got, word) {
					t.Fatalf("%s with %q produced an executable type %q", path, body[:min(20, len(body))], got)
				}
			}
		}
	}
}

func TestSafeContentTypeRejectsExecutableUpstreamTypes(t *testing.T) {
	for _, unsafe := range []string{
		"text/html; charset=utf-8",
		"image/svg+xml",
		"application/javascript",
		"application/xhtml+xml",
		"",
	} {
		if got := SafeContentType(unsafe); got != typeBinary {
			t.Errorf("SafeContentType(%q) = %q, want %q", unsafe, got, typeBinary)
		}
	}
	for _, safe := range []string{typeJSON, typeYAML, typeText, "image/png", "application/pdf"} {
		if got := SafeContentType(safe); got != safe {
			t.Errorf("SafeContentType(%q) = %q", safe, got)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TextLike decides whether the editor may show a resource at all, so the line
// it draws has to be exact: anything it lets through will be round-tripped
// through a form.
func TestTextLike(t *testing.T) {
	for _, editable := range []string{typeText, typeJSON, typeYAML, typeTOML, typeXML, typeCSV, "text/markdown"} {
		if !TextLike(editable) {
			t.Errorf("%q should be editable", editable)
		}
	}
	for _, opaque := range []string{typeBinary, "image/png", "audio/mpeg", "video/mp4", "application/pdf", "application/zip"} {
		if TextLike(opaque) {
			t.Errorf("%q must not be offered to the text editor", opaque)
		}
	}
}
