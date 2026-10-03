package store

import (
	"errors"
	"fmt"
	"time"
)

type User struct {
	ID        string
	GitHubID  string
	Login     string
	Name      string
	AvatarURL string
	// SuspendedReason is set, and Suspended true, for an account the
	// operator has suspended. Only GetUser reads them.
	Suspended       bool
	SuspendedReason string
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
	// Version numbers the saves that changed the content, from 1. VersionAt is
	// when the current content was saved; RestoredFrom is the version it was
	// brought back from, or 0. ContentSHA256 is empty for rows written before
	// it was kept.
	Version       int
	VersionAt     time.Time
	RestoredFrom  int
	ContentSHA256 string
	// TakenDown marks a resource the operator has taken down, with the
	// reason its owner is shown.
	TakenDown      bool
	TakedownReason string
}

// Version is content a resource held before its current one. It is read,
// compared and restored as a whole; nothing about it depends on the versions
// around it.
type Version struct {
	ResourceID      string
	Number          int
	ContentKey      string
	ContentSize     int64
	ContentType     string
	ContentEncoding string
	ContentSHA256   string
	RestoredFrom    int
	SavedAt         time.Time
	ReplacedAt      time.Time
	Filename        string
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
	Location       Location
	Status         int
	Detail         string
	Hits           int
	FirstAt        time.Time
	OccurredAt     time.Time
	// Version is the content version delivered, 0 when nothing was. The
	// resource's current version and whether this one can still be opened
	// come from the resource as it is now; both are 0/false once it is gone.
	Version          int
	CurrentVersion   int
	VersionAvailable bool
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
	Location       Location
}

// Location is where a caller was, as estimated from their IP address by the
// proxy in front of the service. Country is an ISO 3166-1 alpha-2 code (or
// T1 for Tor); every field may be empty.
type Location struct {
	Country string
	Region  string
	City    string
}

type ConsumeResult struct {
	Allowed  bool
	Resource Resource
	LinkID   string
	LinkName string
	Reason   string
	// AccessID is the log row an allowed use was recorded under, for the
	// delivery to correct once it knows how it ended.
	AccessID string
}

type QuotaLimit struct {
	Resources    int64
	StorageBytes int64
}

type QuotaUsage struct {
	Resources int64
	// StorageBytes is the content the resources hold now; it is what the limit
	// is enforced on. HistoryBytes is their earlier versions, which live in
	// whatever room that leaves and give it back when a save needs it.
	StorageBytes int64
	HistoryBytes int64
}

type Plan struct {
	ID         string
	Name       string
	Limit      QuotaLimit
	IsDefault  bool
	ValidFrom  *time.Time
	ValidUntil *time.Time
	CreatedAt  *time.Time
	UpdatedAt  *time.Time
}

type ActivePlans struct {
	Plan      Plan
	GrantedAt time.Time
	ExpiresAt *time.Time
}

type UserQuota struct {
	User        User
	Limit       QuotaLimit
	Usage       QuotaUsage
	ActivePlans []ActivePlans
}

// ErrInternal marks the service failing rather than the request being wrong.
// The text behind it names pgx relations and object keys, so a caller can use
// this to keep it in the process log instead of on a page.
var ErrInternal = errors.New("store: internal error")

// ErrQuotaExceeded marks the account reaching its own ceiling rather than the
// service failing. The caller has to be able to tell the two apart: one is a
// page telling the user what to free, the other is a 500.
var ErrQuotaExceeded = refusal("store: quota exceeded")

// QuotaError carries the numbers behind the refusal. "over quota" on its own
// leaves the user guessing which limit they hit and by how much, on a service
// whose whole job is holding their resources.
type QuotaError struct {
	Storage bool
	Limit   int64
	Usage   int64
	Wanted  int64
}

func (e *QuotaError) Is(target error) bool { return target == ErrQuotaExceeded }

func (e *QuotaError) Error() string {
	if e.Storage {
		return fmt.Sprintf("存储空间不足：已用 %s，上限 %s，本次需要 %s。请先删除或缩减已有资源。",
			BytesText(e.Usage), BytesText(e.Limit), BytesText(e.Wanted))
	}
	return fmt.Sprintf("资源数量已达上限：已有 %d 个，上限 %d 个。请先删除不再需要的资源。", e.Usage, e.Limit)
}

func storageQuotaError(limit, usage, wanted int64) error {
	return &QuotaError{Storage: true, Limit: limit, Usage: usage, Wanted: wanted}
}

func resourceQuotaError(limit, usage int64) error {
	return &QuotaError{Limit: limit, Usage: usage}
}

// BytesText renders a size the way the resource pages do, so a limit reads the
// same in an error as it does next to the file it is about.
func BytesText(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}
