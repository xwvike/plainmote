package web

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"plainmote/internal/store"
)

// The admin interface: signed JSON endpoints and no pages. The page that
// calls them lives elsewhere - any static site, or a file opened locally -
// and holds the private key; this service holds only public keys, so its
// environment leaking is not enough to act as the operator. Anything that
// fails to authenticate is answered exactly like a path that does not exist.
const adminPrefix = "/_admin/v1/"

const (
	adminSignatureLabel = "PLAINMOTE-ADMIN-V1"
	adminClockSkew      = 5 * time.Minute
	adminNonceWindow    = 10 * time.Minute
	adminBodyLimit      = 64 << 10
	adminReasonMaxRunes = 500
	// Failed attempts per source address per minute before it is refused
	// without the signature even being checked.
	adminFailureLimit = 30
)

// AdminKeyID names a public key: the first 16 hex digits of its SHA-256.
func AdminKeyID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:16]
}

// AdminSigningString is what a request's signature covers.
func AdminSigningString(method, target, timestamp, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{adminSignatureLabel, strings.ToUpper(method), target, timestamp, nonce, hex.EncodeToString(sum[:])}, "\n")
}

// adminGate holds what verification remembers between requests: nonces seen
// inside the window, and recent failures by source address.
type adminGate struct {
	keys    map[string]ed25519.PublicKey
	origins []string

	mu       sync.Mutex
	nonces   map[string]time.Time
	failures map[string]*adminFailures
}

type adminFailures struct {
	count int
	since time.Time
}

func newAdminGate(keys []ed25519.PublicKey, origins []string) *adminGate {
	gate := &adminGate{keys: map[string]ed25519.PublicKey{}, origins: origins,
		nonces: map[string]time.Time{}, failures: map[string]*adminFailures{}}
	for _, key := range keys {
		gate.keys[AdminKeyID(key)] = key
	}
	return gate
}

// throttled reports a source that has failed too often in the last minute.
func (g *adminGate) throttled(ip string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.failures[ip]
	return f != nil && now.Sub(f.since) < time.Minute && f.count >= adminFailureLimit
}

func (g *adminGate) fail(ip string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.failures[ip]
	if f == nil || now.Sub(f.since) >= time.Minute {
		if len(g.failures) > 10000 {
			clear(g.failures)
		}
		f = &adminFailures{since: now}
		g.failures[ip] = f
	}
	f.count++
}

// useNonce records a nonce, and reports false if it was already used.
func (g *adminGate) useNonce(nonce string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for seen, at := range g.nonces {
		if now.Sub(at) > adminNonceWindow {
			delete(g.nonces, seen)
		}
	}
	if _, used := g.nonces[nonce]; used {
		return false
	}
	g.nonces[nonce] = now
	return true
}

func (g *adminGate) allowedOrigin(origin string) bool {
	return origin != "" && slices.Contains(g.origins, strings.ToLower(origin))
}

// verify checks a request's signature and returns the key that made it and
// the body it covered.
func (g *adminGate) verify(r *http.Request, now time.Time) (string, []byte, error) {
	keyID := r.Header.Get("X-PlainMote-Key")
	key, ok := g.keys[keyID]
	if !ok {
		return "", nil, errors.New("unknown key")
	}
	timestamp := r.Header.Get("X-PlainMote-Timestamp")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return "", nil, errors.New("bad timestamp")
	}
	if skew := now.Sub(time.Unix(seconds, 0)); skew > adminClockSkew || skew < -adminClockSkew {
		return "", nil, errors.New("stale timestamp")
	}
	nonce := r.Header.Get("X-PlainMote-Nonce")
	if raw, err := base64.RawURLEncoding.DecodeString(nonce); err != nil || len(raw) < 16 || len(nonce) > 128 {
		return "", nil, errors.New("bad nonce")
	}
	signature, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-PlainMote-Signature"))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return "", nil, errors.New("bad signature encoding")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, adminBodyLimit+1))
	if err != nil || len(body) > adminBodyLimit {
		return "", nil, errors.New("body too large")
	}
	if !ed25519.Verify(key, []byte(AdminSigningString(r.Method, r.URL.RequestURI(), timestamp, nonce, body)), signature) {
		return "", nil, errors.New("bad signature")
	}
	// Only a valid request spends its nonce, so a forged one cannot be used to
	// burn nonces a real client is about to send.
	if !g.useNonce(nonce, now) {
		return "", nil, errors.New("replayed nonce")
	}
	return keyID, body, nil
}

const adminAllowHeaders = "Content-Type, X-PlainMote-Key, X-PlainMote-Timestamp, X-PlainMote-Nonce, X-PlainMote-Signature"

func (a *App) handleAdmin(w http.ResponseWriter, r *http.Request) {
	gate := a.admin
	now := time.Now().UTC()
	ip := a.clientIP(r)
	origin := r.Header.Get("Origin")
	if gate.allowedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", adminAllowHeaders)
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	notFound := func() { writePlainError(w, http.StatusNotFound, "not found") }
	if gate.throttled(ip, now) {
		notFound()
		return
	}
	keyID, body, err := gate.verify(r, now)
	if err != nil {
		gate.fail(ip, now)
		notFound()
		return
	}
	request := adminRequest{app: a, w: w, r: r, body: body, now: now,
		actor: store.AdminActor{KeyID: keyID, RemoteIP: ip}}
	request.route(strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, adminPrefix), "/"), "/"))
}

type adminRequest struct {
	app   *App
	w     http.ResponseWriter
	r     *http.Request
	body  []byte
	now   time.Time
	actor store.AdminActor
}

func (q adminRequest) json(status int, value any) {
	q.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	q.w.Header().Set("Cache-Control", "no-store")
	q.w.Header().Set("X-Content-Type-Options", "nosniff")
	q.w.WriteHeader(status)
	_ = json.NewEncoder(q.w).Encode(value)
}

func (q adminRequest) fail(status int, code, message string) {
	q.json(status, map[string]string{"error": code, "message": message})
}

// failWith maps a store error to a response. What the service itself got
// wrong stays in the process log.
func (q adminRequest) failWith(err error) {
	var quota *store.QuotaError
	switch {
	case errors.Is(err, store.ErrNotFound):
		q.fail(http.StatusNotFound, "not_found", "no such object")
	case errors.Is(err, store.ErrInternal) || errors.As(err, &quota):
		fmt.Fprintf(os.Stderr, "admin %s %s: %v\n", q.r.Method, q.r.URL.Path, err)
		q.fail(http.StatusInternalServerError, "internal", "the service could not complete the request")
	default:
		q.fail(http.StatusConflict, err.Error(), err.Error())
	}
}

// reason reads the reason every change must carry.
func (q adminRequest) reason(into any) (string, bool) {
	var fields struct {
		Reason string `json:"reason"`
	}
	if into == nil {
		into = &fields
	}
	if len(bytes.TrimSpace(q.body)) == 0 || json.Unmarshal(q.body, into) != nil {
		q.fail(http.StatusBadRequest, "bad_request", "the body must be a JSON object with a reason")
		return "", false
	}
	if into != &fields {
		_ = json.Unmarshal(q.body, &fields)
	}
	reason := strings.TrimSpace(fields.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > adminReasonMaxRunes {
		q.fail(http.StatusBadRequest, "bad_reason", "reason must be 1 to 500 characters")
		return "", false
	}
	return reason, true
}

func (q adminRequest) page() (limit, offset int) {
	query := q.r.URL.Query()
	limit, _ = strconv.Atoi(query.Get("size"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	page, _ := strconv.Atoi(query.Get("page"))
	if page < 1 {
		page = 1
	}
	return limit, (page - 1) * limit
}

func (q adminRequest) route(parts []string) {
	method := q.r.Method
	is := func(want string, n int) bool { return method == want && len(parts) == n }
	switch {
	case is(http.MethodGet, 1) && parts[0] == "overview":
		q.overview()
	case is(http.MethodGet, 1) && parts[0] == "users":
		q.users()
	case is(http.MethodGet, 2) && parts[0] == "users":
		q.user(parts[1])
	case is(http.MethodPost, 3) && parts[0] == "users" && (parts[2] == "suspend" || parts[2] == "unsuspend"):
		q.suspend(parts[1], parts[2] == "suspend")
	case is(http.MethodPost, 3) && parts[0] == "users" && parts[2] == "plans":
		q.grantPlan(parts[1])
	case is(http.MethodDelete, 4) && parts[0] == "users" && parts[2] == "plans":
		q.revokePlan(parts[1], parts[3])
	case is(http.MethodGet, 1) && parts[0] == "plans":
		q.plans()
	case is(http.MethodPost, 1) && parts[0] == "plans":
		q.createPlan()
	case is(http.MethodGet, 1) && parts[0] == "resources":
		q.resources()
	case is(http.MethodGet, 2) && parts[0] == "resources":
		q.resource(parts[1])
	case is(http.MethodPost, 3) && parts[0] == "resources" && parts[2] == "takedown":
		q.takedown(parts[1])
	case is(http.MethodPost, 3) && parts[0] == "resources" && parts[2] == "restore":
		q.restore(parts[1])
	case is(http.MethodDelete, 2) && parts[0] == "resources":
		q.deleteResource(parts[1])
	case is(http.MethodPost, 3) && parts[0] == "links" && parts[2] == "revoke":
		q.revokeLink(parts[1])
	case is(http.MethodPost, 1) && parts[0] == "lookup":
		q.lookup()
	case is(http.MethodGet, 1) && parts[0] == "audit":
		q.audit()
	default:
		q.fail(http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func (q adminRequest) overview() {
	a := q.app
	overview, err := a.db.AdminOverview(q.r.Context(), q.now)
	if err != nil {
		q.failWith(err)
		return
	}
	database, objects := a.db.Health(q.r.Context())
	prune, pruneAt := a.db.LastPrune()
	var lastPrune any
	if !pruneAt.IsZero() {
		lastPrune = map[string]any{
			"last_run_at": pruneAt, "access_logs": prune.AccessLogs, "sessions": prune.Sessions,
			"pastes": prune.Pastes, "versions": prune.Versions,
		}
	}
	q.json(http.StatusOK, map[string]any{
		"version":    a.cfg.Version,
		"revision":   a.cfg.Revision,
		"started_at": a.cfg.StartedAt,
		"config": map[string]any{
			"public_url":             a.cfg.PublicURL,
			"registration_mode":      a.cfg.RegistrationMode,
			"anonymous":              a.cfg.AnonymousEnabled,
			"max_content_bytes":      a.cfg.MaxContent,
			"log_retention_hours":    int(a.cfg.LogRetention / time.Hour),
			"history_keep":           store.HistoryKeep,
			"history_retention_days": int(store.HistoryRetention / (24 * time.Hour)),
		},
		"health":    map[string]string{"database": database, "object_storage": objects},
		"users":     overview.Users,
		"resources": overview.Resources,
		"links":     overview.Links,
		"access":    overview.Access,
		"anonymous": overview.Anonymous,
		"prune":     lastPrune,
	})
}

func (q adminRequest) users() {
	limit, offset := q.page()
	query := q.r.URL.Query()
	status := query.Get("status")
	if status != "" && status != "active" && status != "suspended" {
		q.fail(http.StatusBadRequest, "bad_request", "status must be active or suspended")
		return
	}
	users, total, err := q.app.db.AdminListUsers(q.r.Context(), query.Get("q"), status, limit, offset, q.now)
	if err != nil {
		q.failWith(err)
		return
	}
	q.json(http.StatusOK, map[string]any{"items": users, "total": total})
}

func (q adminRequest) user(id string) {
	user, err := q.app.db.AdminGetUser(q.r.Context(), id, q.now)
	if err != nil {
		q.failWith(err)
		return
	}
	q.json(http.StatusOK, user)
}

func (q adminRequest) suspend(id string, suspend bool) {
	reason, ok := q.reason(nil)
	if !ok {
		return
	}
	var err error
	if suspend {
		err = q.app.db.AdminSuspendUser(q.r.Context(), q.actor, id, reason, q.now)
	} else {
		err = q.app.db.AdminUnsuspendUser(q.r.Context(), q.actor, id, reason, q.now)
	}
	if err != nil {
		q.failWith(err)
		return
	}
	q.user(id)
}

func (q adminRequest) plans() {
	plans, err := q.app.db.AdminListPlans(q.r.Context())
	if err != nil {
		q.failWith(err)
		return
	}
	q.json(http.StatusOK, map[string]any{"items": plans, "total": len(plans)})
}

func (q adminRequest) createPlan() {
	var fields struct {
		Name         string `json:"name"`
		MaxResources int64  `json:"max_resources"`
		MaxStorage   int64  `json:"max_storage"`
	}
	reason, ok := q.reason(&fields)
	if !ok {
		return
	}
	plan, err := q.app.db.AdminCreatePlan(q.r.Context(), q.actor, fields.Name, fields.MaxResources, fields.MaxStorage, reason, q.now)
	if err != nil {
		q.failWith(err)
		return
	}
	q.json(http.StatusCreated, plan)
}

func (q adminRequest) grantPlan(userID string) {
	var fields struct {
		PlanID    string     `json:"plan_id"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	reason, ok := q.reason(&fields)
	if !ok {
		return
	}
	if err := q.app.db.AdminGrantPlan(q.r.Context(), q.actor, userID, fields.PlanID, fields.ExpiresAt, reason, q.now); err != nil {
		q.failWith(err)
		return
	}
	q.user(userID)
}

func (q adminRequest) revokePlan(userID, planID string) {
	reason, ok := q.reason(nil)
	if !ok {
		return
	}
	if err := q.app.db.AdminRevokePlan(q.r.Context(), q.actor, userID, planID, reason, q.now); err != nil {
		q.failWith(err)
		return
	}
	q.user(userID)
}

func (q adminRequest) resources() {
	limit, offset := q.page()
	query := q.r.URL.Query()
	status := query.Get("status")
	if status != "" && status != "active" && status != "taken_down" {
		q.fail(http.StatusBadRequest, "bad_request", "status must be active or taken_down")
		return
	}
	resources, total, err := q.app.db.AdminListResources(q.r.Context(), query.Get("owner"), query.Get("q"), status, limit, offset, q.now)
	if err != nil {
		q.failWith(err)
		return
	}
	q.json(http.StatusOK, map[string]any{"items": resources, "total": total})
}

func (q adminRequest) resource(id string) {
	resource, err := q.app.db.AdminGetResource(q.r.Context(), id, q.now)
	if err != nil {
		q.failWith(err)
		return
	}
	q.json(http.StatusOK, resource)
}

func (q adminRequest) takedown(id string) {
	reason, ok := q.reason(nil)
	if !ok {
		return
	}
	deleted, err := q.app.db.AdminTakedown(q.r.Context(), q.actor, id, reason, q.now)
	if err != nil {
		q.failWith(err)
		return
	}
	if deleted {
		q.json(http.StatusOK, map[string]any{"id": id, "deleted": true})
		return
	}
	q.resource(id)
}

func (q adminRequest) restore(id string) {
	reason, ok := q.reason(nil)
	if !ok {
		return
	}
	if err := q.app.db.AdminRestoreResource(q.r.Context(), q.actor, id, reason, q.now); err != nil {
		q.failWith(err)
		return
	}
	q.resource(id)
}

func (q adminRequest) deleteResource(id string) {
	reason, ok := q.reason(nil)
	if !ok {
		return
	}
	if err := q.app.db.AdminDeleteResource(q.r.Context(), q.actor, id, reason, q.now); err != nil {
		q.failWith(err)
		return
	}
	q.json(http.StatusOK, map[string]any{"id": id, "deleted": true})
}

func (q adminRequest) revokeLink(id string) {
	reason, ok := q.reason(nil)
	if !ok {
		return
	}
	if err := q.app.db.AdminRevokeLink(q.r.Context(), q.actor, id, reason, q.now); err != nil {
		q.failWith(err)
		return
	}
	q.json(http.StatusOK, map[string]any{"id": id, "revoked": true})
}

// lookup takes the address quoted in a report - whole, as a path, or as the
// bare token - and says what it opens. The token never comes back.
func (q adminRequest) lookup() {
	var fields struct {
		Link string `json:"link"`
	}
	if json.Unmarshal(q.body, &fields) != nil {
		q.fail(http.StatusBadRequest, "bad_request", "the body must be a JSON object with a link")
		return
	}
	token := strings.TrimSpace(fields.Link)
	if at := strings.Index(token, deliveryPrefix); at >= 0 {
		token, _ = splitDeliveryPath(token[at:])
	}
	resource, link, err := q.app.db.AdminLookup(q.r.Context(), token, q.now)
	if err != nil {
		q.failWith(err)
		return
	}
	q.json(http.StatusOK, map[string]any{"resource": resource, "link": link})
}

func (q adminRequest) audit() {
	limit, offset := q.page()
	entries, total, err := q.app.db.AdminAudit(q.r.Context(), q.r.URL.Query().Get("target"), limit, offset)
	if err != nil {
		q.failWith(err)
		return
	}
	q.json(http.StatusOK, map[string]any{"items": entries, "total": total})
}
