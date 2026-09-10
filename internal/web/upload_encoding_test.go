package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"plainmote/internal/store"
)

func TestEditorContentUsesSelectedEncoding(t *testing.T) {
	values := url.Values{
		"content":          {"name: café\n"},
		"content_encoding": {"windows-1252"},
	}
	request := httptest.NewRequest(http.MethodPost, "/resources/new", bytes.NewBufferString(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	form, err := readResourceForm(request, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{'n', 'a', 'm', 'e', ':', ' ', 'c', 'a', 'f', 0xe9, '\n'}
	if form.Uploaded || form.ContentEncoding != "windows-1252" || !bytes.Equal(form.Content, want) {
		t.Fatalf("encoded form = uploaded:%v encoding:%q content:%x", form.Uploaded, form.ContentEncoding, form.Content)
	}
}

func TestEditorRejectsCharactersTheSelectedEncodingCannotRepresent(t *testing.T) {
	values := url.Values{
		"content":          {"emoji: 😀\n"},
		"content_encoding": {"gbk"},
	}
	request := httptest.NewRequest(http.MethodPost, "/resources/new", bytes.NewBufferString(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := readResourceForm(request, 1<<20); err == nil {
		t.Fatal("GBK save must reject a character it cannot represent")
	}
}

func TestUploadedBytesAreNotReencoded(t *testing.T) {
	original, encodingName, err := store.EncodeText("名称: 上海节点\n", "gb18030")
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("content", "this preview must not win")
	_ = writer.WriteField("content_encoding", encodingName)
	part, err := writer.CreateFormFile("upload", "legacy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(original); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/resources/new", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	form, err := readResourceForm(request, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !form.Uploaded || form.ContentEncoding != "gb18030" || !bytes.Equal(form.Content, original) {
		t.Fatalf("upload changed bytes: uploaded:%v encoding:%q got:%x want:%x", form.Uploaded, form.ContentEncoding, form.Content, original)
	}
}
