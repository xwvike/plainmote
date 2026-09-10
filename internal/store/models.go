package store

import "time"

type User struct {
	ID        string
	GitHubID  string
	Login     string
	Name      string
	AvatarURL string
}

type Resource struct {
	ID          string
	OwnerID     string
	Name        string
	Filename    string
	ContentKey  string
	ContentSize int64
	ContentType string
	// ContentEncoding is empty for opaque resources and the canonical source
	// encoding for text. The editor uses it for lossless decode/edit/encode
	// round-trips; it is separate from the HTTP media type on purpose.
	ContentEncoding string
	OriginURL       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LiveShares      int
}

func (r Resource) Remote() bool { return r.OriginURL != "" }

// Editable reports whether the content can be shown in a textarea. Bytes that
// are not text must never go through a form: the browser rewrites whatever it
// cannot represent, so saving would overwrite the file with a broken copy.
func (r Resource) Editable() bool {
	return !r.Remote() && TextLike(r.ContentType)
}

type AccessLog struct {
	ID             string
	ResourceID     string
	ResourceName   string
	ResourceFile   string
	LinkID         string
	LinkName       string
	Outcome        string
	RemoteIP       string
	RemoteAddr     string
	Host           string
	Query          string
	Proto          string
	UserAgent      string
	Referer        string
	Forwarded      string
	XForwardedFor  string
	CFConnectingIP string
	CFRay          string
	ContentLength  string
	TLS            bool
	Method         string
	Path           string
	Status         int
	Detail         string
	OccurredAt     time.Time
}

type RequestMeta struct {
	RemoteIP       string
	RemoteAddr     string
	Host           string
	Query          string
	Proto          string
	UserAgent      string
	Referer        string
	Forwarded      string
	XForwardedFor  string
	CFConnectingIP string
	CFRay          string
	ContentLength  string
	TLS            bool
	Method         string
	Path           string
}

type ConsumeResult struct {
	Allowed  bool
	Resource Resource
	LinkID   string
	LinkName string
	Reason   string
}
