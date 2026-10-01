package web

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

type versionClient struct {
	t       *testing.T
	app     *App
	session string
	csrf    string
	asParts bool
}

func newVersionClient(t *testing.T, db *store.Store, user User) *versionClient {
	t.Helper()
	session, csrf, _, err := db.CreateSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &versionClient{t: t, app: newTestApp(db, user.GitHubID), session: session, csrf: csrf}
}

func (c *versionClient) do(method, target string, form url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	var request *http.Request
	if form == nil {
		request = httptest.NewRequest(method, "https://cfg.test"+target, nil)
	} else {
		form.Set("csrf", c.csrf)
		request = httptest.NewRequest(method, "https://cfg.test"+target, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	request.Header.Set("Accept-Language", "en")
	if c.asParts {
		request.Header.Set(partsHeader, "1")
	}
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: c.csrf})
	response := httptest.NewRecorder()
	c.app.handler.ServeHTTP(response, request)
	return response
}

// parts makes a request the way parts.js does.
func (c *versionClient) parts(method, target string, form url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	c.asParts = true
	defer func() { c.asParts = false }()
	response := c.do(method, target, form)
	if !strings.Contains(strings.Join(response.Header().Values("Vary"), ","), partsHeader) {
		c.t.Fatalf("%s %s (parts) does not vary on the parts header", method, target)
	}
	return response
}

// partsPage is a GET through parts that must succeed.
func (c *versionClient) partsPage(target string) string {
	c.t.Helper()
	response := c.parts(http.MethodGet, target, nil)
	if response.Code != http.StatusOK {
		c.t.Fatalf("GET %s (parts): %d %s", target, response.Code, response.Body.String())
	}
	return response.Body.String()
}

func (c *versionClient) page(target string) string {
	c.t.Helper()
	response := c.do(http.MethodGet, target, nil)
	if response.Code != http.StatusOK {
		c.t.Fatalf("GET %s: %d %s", target, response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "template error") {
		c.t.Fatalf("GET %s rendered a template error", target)
	}
	return response.Body.String()
}

func (c *versionClient) save(resource Resource, body string, base int) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.do(http.MethodPost, "/resources/"+resource.ID, url.Values{
		"name": {resource.Name}, "filename": {resource.Filename}, "content": {body},
		"content_encoding": {"utf-8"}, "content_eol": {"lf"}, "base_version": {strconv.Itoa(base)},
	})
}

// TestVersionPagesThroughRouter walks the history the way the owner does:
// save, see the list, open the old version, compare, restore, undo.
func TestVersionPagesThroughRouter(t *testing.T) {
	db, user, resource := testDatabase(t)
	client := newVersionClient(t, db, user)
	base := "/resources/" + resource.ID

	page := client.page(base)
	if !strings.Contains(page, `name="base_version" value="1"`) {
		t.Fatal("the edit form must carry the version it starts from")
	}
	if !strings.Contains(page, base+"/versions") || !strings.Contains(page, "Versions<b>0</b>") {
		t.Fatal("the resource page must lead to its history, count and all")
	}

	if saved := client.save(resource, "answer=43\n", 1); saved.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", saved.Code, saved.Body.String())
	}
	list := client.page(base + "/versions")
	for _, want := range []string{">v2<", `href="` + base + `/versions/1"`, "compare?from=1&amp;to=2", "Current"} {
		if !strings.Contains(list, want) {
			t.Fatalf("the list is missing %q", want)
		}
	}

	view := client.page(base + "/versions/1")
	if !strings.Contains(view, "answer=42") || !strings.Contains(view, "data-preview") || !strings.Contains(view, "Share links currently return v2") {
		t.Fatal("the version page shows the old content read-only and says what the links return")
	}
	if dialog := client.page(base + "/versions/1?restore=1"); !strings.Contains(dialog, `<button type="submit" class="pri">Restore v1</button>`) || !strings.Contains(dialog, "new version, v3") {
		t.Fatal("the restore button names the version picked, and the dialog says it becomes v3")
	}

	compare := client.page(base + "/versions/compare?from=1&to=2")
	if !strings.Contains(compare, `class="row del"`) || !strings.Contains(compare, "answer=42") || !strings.Contains(compare, "answer=43") {
		t.Fatal("the comparison shows the line that changed on both sides")
	}

	restored := client.do(http.MethodPost, base+"/versions/1", url.Values{"action": {"restore"}})
	if restored.Code != http.StatusSeeOther || restored.Header().Get("Location") != base+"?restored=1" {
		t.Fatalf("restore lands on the resource with a notice: %d %q", restored.Code, restored.Header().Get("Location"))
	}
	notice := client.page(base + "?restored=1")
	if !strings.Contains(notice, "Restored the content of v1 as v3") || !strings.Contains(notice, `action="`+base+`/versions/2"`) {
		t.Fatal("the notice says what happened and offers the way back")
	}
	if current, _ := db.ResourceForOwner(context.Background(), user.ID, resource.ID); current.Version != 3 {
		t.Fatalf("restore made v%d", current.Version)
	}

	// The current version has no page of its own: it is the resource.
	if moved := client.do(http.MethodGet, base+"/versions/3", nil); moved.Code != http.StatusSeeOther {
		t.Fatalf("the current version redirects to the resource, got %d", moved.Code)
	}
	if missing := client.do(http.MethodGet, base+"/versions/9", nil); missing.Code != http.StatusNotFound {
		t.Fatalf("an unknown version is not found, got %d", missing.Code)
	}
}

func TestStaleSaveComesBackAsAConflict(t *testing.T) {
	db, user, resource := testDatabase(t)
	client := newVersionClient(t, db, user)
	base := "/resources/" + resource.ID

	client.save(resource, "answer=43\n", 1) // saved elsewhere: now v2
	stale := client.save(resource, "answer=44\n", 1)
	if stale.Code != http.StatusConflict {
		t.Fatalf("a save against v1 over v2 is refused as a conflict, got %d", stale.Code)
	}
	body := stale.Body.String()
	for _, want := range []string{"answer=44", "Not saved.", "Save anyway as v3", `name="base_version" value="2"`, "compare?from=1&amp;to=2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the conflict page is missing %q", want)
		}
	}
	if current, _ := db.ResourceForOwner(context.Background(), user.ID, resource.ID); current.Version != 2 {
		t.Fatalf("a conflict writes nothing, current is v%d", current.Version)
	}
	// Saving anyway is saving again with the base the page now carries.
	if again := client.save(resource, "answer=44\n", 2); again.Code != http.StatusSeeOther {
		t.Fatalf("saving over the reported version succeeds, got %d", again.Code)
	}
	if list := client.page(base + "/versions"); !strings.Contains(list, ">v3<") {
		t.Fatal("the forced save is v3")
	}
}

// TestCreateAndSaveSayWhatHappened checks the confirmations: a new resource
// says it was made and offers the next one, a save names the version it made,
// and a notice that is no longer true is not shown.
func TestCreateAndSaveSayWhatHappened(t *testing.T) {
	db, user, resource := testDatabase(t)
	client := newVersionClient(t, db, user)
	base := "/resources/" + resource.ID

	created := client.do(http.MethodPost, "/resources/new", url.Values{
		"kind": {"local"}, "name": {"Second"}, "filename": {"second.conf"}, "content": {"a=1\n"},
		"content_encoding": {"utf-8"}, "content_eol": {"lf"},
	})
	location := created.Header().Get("Location")
	if created.Code != http.StatusSeeOther || !strings.HasSuffix(location, "?created=1") {
		t.Fatalf("create lands on the resource with a notice: %d %q", created.Code, location)
	}
	page := client.page(location)
	for _, want := range []string{"Created “Second”.", `href="/resources/new">Create another`, `href="/resources/">Back to resources`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the created notice is missing %q", want)
		}
	}

	saved := client.save(resource, "answer=43\n", 1)
	if saved.Header().Get("Location") != base+"?saved=2" {
		t.Fatalf("a save that changes the content names its version, got %q", saved.Header().Get("Location"))
	}
	if page := client.page(base + "?saved=2"); !strings.Contains(page, "Saved as v2.") {
		t.Fatal("the save notice names the version")
	}
	resource.Name = "Renamed"
	renamed := client.save(resource, "answer=43\n", 2)
	if renamed.Header().Get("Location") != base+"?saved=0" {
		t.Fatalf("a rename makes no version, got %q", renamed.Header().Get("Location"))
	}
	if page := client.page(base + "?saved=0"); !strings.Contains(page, "Saved.") || strings.Contains(page, "Saved as") {
		t.Fatal("a rename is confirmed without a version")
	}

	client.save(resource, "answer=44\n", 2)
	stale := client.page(base + "?saved=2&created=1")
	if strings.Contains(stale, "Saved as v2") || strings.Contains(stale, "Created “") {
		t.Fatal("notices that are no longer true are not shown")
	}
}

// TestDialogsOpenAsParts checks every dialog link answers parts.js with the
// dialog alone, while the same address loaded plainly still shows it on the
// page for a browser without the script.
func TestDialogsOpenAsParts(t *testing.T) {
	db, user, resource := testDatabase(t)
	client := newVersionClient(t, db, user)
	base := "/resources/" + resource.ID
	client.save(resource, "answer=43\n", 1) // v1 is now an earlier version

	for _, tc := range []struct{ target, want string }{
		{base + "?delete=1", `name="action" value="delete"`},
		{base + "/versions?remove=1", "Delete v1"},
		{base + "/versions/1?restore=1", "Restore v1"},
	} {
		parts := client.partsPage(tc.target)
		if strings.Contains(parts, "<html") || !strings.Contains(parts, `data-part="dialog"`) || !strings.Contains(parts, tc.want) || !strings.Contains(parts, "data-close") {
			t.Fatalf("%s: the parts are not the dialog: %q", tc.target, parts)
		}
		if strings.Contains(parts, "scrim") {
			t.Fatalf("%s: a modal dialog brings its own backdrop", tc.target)
		}
		page := client.page(tc.target)
		if !strings.Contains(page, `<div class="scrim"></div>`) || !strings.Contains(page, `<dialog class="dlg" open data-part="dialog">`) || !strings.Contains(page, "/static/parts.js") {
			t.Fatalf("%s: the plain page still shows the dialog", tc.target)
		}
	}
	// Deleting the account: the typed name is checked through parts too, and
	// answers with a message or with the last confirmation.
	mismatch := client.parts(http.MethodPost, "/account/delete", url.Values{"confirm": {"nobody"}})
	if body := mismatch.Body.String(); mismatch.Code != http.StatusBadRequest || strings.Contains(body, "<html") || !strings.Contains(body, `<div data-part="flash"><p class="err">`) {
		t.Fatalf("a wrong name answers with the message: %d %q", mismatch.Code, body)
	}
	confirm := client.parts(http.MethodPost, "/account/delete", url.Values{"confirm": {user.Login}})
	if body := confirm.Body.String(); confirm.Code != http.StatusOK || !strings.Contains(body, `data-part="dialog"`) || !strings.Contains(body, `name="final" value="1"`) {
		t.Fatalf("the right name answers with the confirmation: %d %q", confirm.Code, body)
	}
	if page := client.page("/account"); !strings.Contains(page, "data-parts") || !strings.Contains(page, "/static/parts.js") {
		t.Fatal("the account form is sent through parts.js")
	}

	for _, target := range []string{base, base + "/versions", base + "/versions/1", base + "/versions/compare?from=1&to=2"} {
		if page := client.page(target); !strings.Contains(page, "data-dialog") {
			t.Fatalf("%s has no dialog links", target)
		}
	}
}

func TestDeletingAndCopyingVersions(t *testing.T) {
	db, user, resource := testDatabase(t)
	client := newVersionClient(t, db, user)
	base := "/resources/" + resource.ID
	client.save(resource, "answer=43\n", 1)
	client.save(resource, "answer=44\n", 2)

	if dialog := client.page(base + "/versions?remove=1"); !strings.Contains(dialog, "Delete v1 permanently") {
		t.Fatal("deleting a version asks first")
	}
	copied := client.do(http.MethodPost, base+"/versions/2", url.Values{"action": {"copy"}})
	if copied.Code != http.StatusSeeOther || !strings.HasPrefix(copied.Header().Get("Location"), "/resources/") || copied.Header().Get("Location") == base {
		t.Fatalf("a copy lands on the new resource: %d %q", copied.Code, copied.Header().Get("Location"))
	}
	if page := client.page(copied.Header().Get("Location")); !strings.Contains(page, "Example (v2)") || !strings.Contains(page, "answer=43") {
		t.Fatal("the copy carries the version's content and says where it came from")
	}

	deleted := client.do(http.MethodPost, base+"/versions/1", url.Values{"action": {"delete"}})
	if deleted.Code != http.StatusSeeOther || deleted.Header().Get("Location") != base+"/versions" {
		t.Fatalf("delete lands on the list: %d %q", deleted.Code, deleted.Header().Get("Location"))
	}
	list := client.page(base + "/versions")
	if strings.Contains(list, `href="`+base+`/versions/1"`) || !strings.Contains(list, "v1 has been removed") {
		t.Fatal("a deleted version leaves the list and its number is accounted for")
	}
}

func TestLogsAndStorageShowVersions(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	client := newVersionClient(t, db, user)

	link, err := db.CreateShare(ctx, user.ID, resource.ID, "web-01", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	fetch := httptest.NewRecorder()
	client.app.handler.ServeHTTP(fetch, httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+link.Token+"/example.conf", nil))
	if fetch.Code != http.StatusOK {
		t.Fatalf("delivery: %d", fetch.Code)
	}
	client.save(resource, "answer=43\n", 1)

	logs := client.page("/logs")
	if !strings.Contains(logs, `<span class="vc old">v1</span>`) || !strings.Contains(logs, "current v2") {
		t.Fatal("the access history names the version delivered and marks it behind")
	}
	if list := client.page("/resources/" + resource.ID + "/versions"); !strings.Contains(list, `<span class="tag">web-01</span>`) {
		t.Fatal("the list shows which version web-01 last received")
	}

	account := client.page("/account")
	for _, want := range []string{"meter-bar", "Earlier versions (borrowed)", "Available for saving"} {
		if !strings.Contains(account, want) {
			t.Fatalf("the account page is missing %q", want)
		}
	}
	if dashboard := client.page("/resources/"); !strings.Contains(dashboard, "meter-bar mini") {
		t.Fatal("the resource list carries the small storage bar")
	}
}

func pngOf(t *testing.T, width, height int, shade uint8) []byte {
	t.Helper()
	picture := image.NewGray(image.Rect(0, 0, width, height))
	for i := range picture.Pix {
		picture.Pix[i] = shade
	}
	var out bytes.Buffer
	if err := png.Encode(&out, picture); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestFileVersionsAreShownNotDiffed covers the other half of what resources
// hold: an image replaced by another keeps the first, which is looked at,
// streamed in ranges and compared side by side.
func TestFileVersionsAreShownNotDiffed(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	client := newVersionClient(t, db, user)
	first, second := pngOf(t, 12, 8, 40), pngOf(t, 30, 20, 200)
	logo, err := db.CreateResource(ctx, user.ID, "logo", "logo.png", first, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveResource(ctx, user.ID, logo.ID, store.ResourceEdit{Name: "logo", Filename: "logo.png", Content: second, BaseVersion: 1}); err != nil {
		t.Fatal(err)
	}
	base := "/resources/" + logo.ID

	view := client.page(base + "/versions/1")
	if !strings.Contains(view, `<img class="resource-media resource-image" src="`+base+`/versions/1/raw"`) || strings.Contains(view, "data-preview") {
		t.Fatal("an earlier image is shown as an image, not as text")
	}

	ranged := httptest.NewRequest(http.MethodGet, "https://cfg.test"+base+"/versions/1/raw", nil)
	ranged.Header.Set("Range", "bytes=0-9")
	ranged.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
	response := httptest.NewRecorder()
	client.app.handler.ServeHTTP(response, ranged)
	if response.Code != http.StatusPartialContent || !bytes.Equal(response.Body.Bytes(), first[:10]) ||
		response.Header().Get("Content-Range") != "bytes 0-9/"+strconv.Itoa(len(first)) || response.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("an earlier version is served in ranges, as what it is: %d %q %q", response.Code, response.Header().Get("Content-Range"), response.Header().Get("Content-Type"))
	}

	compare := client.page(base + "/versions/compare?from=1&to=2")
	for _, want := range []string{"The two versions differ", `class="cmp-sides"`, "12 × 8", "30 × 20", `src="` + base + `/raw"`, `src="` + base + `/versions/1/raw"`} {
		if !strings.Contains(compare, want) {
			t.Fatalf("the image comparison is missing %q", want)
		}
	}
	if strings.Contains(compare, `class="diff"`) {
		t.Fatal("images are not diffed line by line")
	}

	restored := client.do(http.MethodPost, base+"/versions/1", url.Values{"action": {"restore"}})
	if restored.Code != http.StatusSeeOther {
		t.Fatalf("an image version restores like any other: %d", restored.Code)
	}
	current, err := db.ResourceForOwner(ctx, user.ID, logo.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := db.ReadContent(ctx, current)
	if !bytes.Equal(body, first) || current.ContentType != "image/png" || current.Version != 3 {
		t.Fatalf("restoring brings the first image back as v3: v%d %s", current.Version, current.ContentType)
	}

	// Text against a file: each side as itself, nothing diffed.
	mixed, err := db.CreateResource(ctx, user.ID, "mixed", "", []byte("plain: text\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveResource(ctx, user.ID, mixed.ID, store.ResourceEdit{Name: "mixed", Content: second}); err != nil {
		t.Fatal(err)
	}
	page := client.page("/resources/" + mixed.ID + "/versions/compare?from=1&to=2")
	if !strings.Contains(page, `class="cmp-sides"`) || !strings.Contains(page, `href="/resources/`+mixed.ID+`/versions/1">View</a>`) {
		t.Fatal("text beside an image is shown as a card that opens it")
	}
}

func TestImageSizeReadsWebP(t *testing.T) {
	// A VP8X header for a 640 × 480 image.
	header := []byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x00\x00\x00\x00\x7f\x02\x00\xdf\x01\x00")
	if width, height := imageSize(header); width != 640 || height != 480 {
		t.Fatalf("got %d × %d", width, height)
	}
	if width, height := imageSize([]byte("not an image")); width != 0 || height != 0 {
		t.Fatalf("unknown bytes have no size, got %d × %d", width, height)
	}
}

// TestReplacingAVideoWithAnImage is the file that changed kind: a video
// replaced by an image must become an image - type and name - and restoring
// the video must bring both back.
func TestReplacingAVideoWithAnImage(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	client := newVersionClient(t, db, user)
	video := []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\x00\x00\x08free")
	picture := pngOf(t, 16, 9, 90)
	clip, err := db.CreateResource(ctx, user.ID, "clip", "movie.mp4", video, "", "")
	if err != nil || clip.ContentType != "video/mp4" {
		t.Fatalf("a video starts as a video: %q %v", clip.ContentType, err)
	}

	upload := func(filename, uploadedAs string, content []byte, base int) {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for field, value := range map[string]string{"csrf": client.csrf, "name": "clip", "filename": filename, "base_version": strconv.Itoa(base)} {
			_ = form.WriteField(field, value)
		}
		part, _ := form.CreateFormFile("upload", uploadedAs)
		_, _ = part.Write(content)
		_ = form.Close()
		request := httptest.NewRequest(http.MethodPost, "https://cfg.test/resources/"+clip.ID, &body)
		request.Header.Set("Content-Type", form.FormDataContentType())
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
		response := httptest.NewRecorder()
		client.app.handler.ServeHTTP(response, request)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("replace with %s: %d %s", uploadedAs, response.Code, response.Body.String())
		}
	}

	upload("movie.mp4", "photo.png", picture, 1)
	current, _ := db.ResourceForOwner(ctx, user.ID, clip.ID)
	if current.ContentType != "image/png" || current.Filename != "photo.png" {
		t.Fatalf("the image is an image, named as one: %q %q", current.ContentType, current.Filename)
	}
	if page := client.page("/resources/" + clip.ID); !strings.Contains(page, `<img class="resource-media resource-image"`) || strings.Contains(page, "<video") {
		t.Fatal("the resource page previews the image as an image")
	}
	versions, _ := db.ListVersions(ctx, user.ID, clip.ID)
	if len(versions) != 1 || versions[0].ContentType != "video/mp4" || versions[0].Filename != "movie.mp4" {
		t.Fatalf("the video is kept as it was, name and all: %+v", versions)
	}
	if view := client.page("/resources/" + clip.ID + "/versions/1"); !strings.Contains(view, "<video") {
		t.Fatal("the earlier version is still previewed as a video")
	}

	if _, err := db.RestoreVersion(ctx, user.ID, clip.ID, 1); err != nil {
		t.Fatal(err)
	}
	current, _ = db.ResourceForOwner(ctx, user.ID, clip.ID)
	if current.ContentType != "video/mp4" || current.Filename != "movie.mp4" {
		t.Fatalf("restoring the video brings back its type and name: %q %q", current.ContentType, current.Filename)
	}

	// A replacement of the same kind keeps the owner's name.
	upload("movie.mp4", "take-2.mp4", append(bytes.Clone(video), 0), 3)
	if current, _ = db.ResourceForOwner(ctx, user.ID, clip.ID); current.Filename != "movie.mp4" {
		t.Fatalf("a video replacing a video keeps the name, got %q", current.Filename)
	}
}

// TestImagesOpenCentred: an image opened on its own is laid out by the
// browser's image viewer with inline styles, which the delivery policy has to
// let through or Chrome leaves it in the top left corner. Only images get
// them, and scripts stay forbidden for everything.
func TestImagesOpenCentred(t *testing.T) {
	image := deliveredPolicy("image/png")
	if !strings.Contains(image, "style-src 'unsafe-inline'") || !strings.Contains(image, "sandbox") || strings.Contains(image, "script-src") {
		t.Fatalf("an image keeps the sandbox and gains only inline styles: %q", image)
	}
	for _, other := range []string{"text/plain; charset=utf-8", "application/json", "video/mp4", "application/octet-stream"} {
		if got := deliveredPolicy(other); got != deliveredContentSecurityPolicy {
			t.Fatalf("%s keeps the strict policy, got %q", other, got)
		}
	}
}
