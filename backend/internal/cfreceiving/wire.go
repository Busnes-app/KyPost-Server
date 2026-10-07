// Package cfreceiving is KyPost's side of the continuous Cloudflare receiving
// wire contract (docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md#wire-contract):
// pickup credentials, the two signed documents, a bounded Worker client and
// the local pickup state. It decides nothing about mail ownership; the app
// receiving runtime does.
package cfreceiving

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	routesContext = "kypost-cf-routes/1\n"
	rotateContext = "kypost-cf-rotate/1\n"
	// MaxMessageBytes is the Worker's per-route maxBytes ceiling (25 MiB).
	MaxMessageBytes = 25 << 20
	maxRoutes       = 5000
	maxBlocks       = 5000
	maxTableBytes   = 1 << 20
	maxRoutesBody   = 2*maxTableBytes + 4096
	maxEnvelope     = 2000
)

var (
	// ErrUnauthorized is the Worker refusing this bearer (401): another
	// instance rotated the credentials, so this one is fenced.
	ErrUnauthorized = errors.New("the Worker refused this instance's pickup bearer; another instance holds receiving")
	// ErrConflict is 409: a stale revision, a concurrent install, or a
	// provider object whose digest differs from the local record.
	ErrConflict = errors.New("the Worker refused a conflicting change")
	// ErrInvalid is a provider object that can never be held locally.
	ErrInvalid = errors.New("provider object is malformed or fails its digest")
)

// Material is one epoch's bearer and signing key. Never log it.
type Material struct {
	Epoch int64  `json:"epoch"`
	Token string `json:"token"`
	Seed  []byte `json:"seed"`
}

// NewMaterial generates a fresh 256-bit bearer and Ed25519 key.
func NewMaterial(epoch int64) (Material, error) {
	token, seed := make([]byte, 32), make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(token); err != nil {
		return Material{}, err
	}
	if _, err := rand.Read(seed); err != nil {
		return Material{}, err
	}
	return Material{Epoch: epoch, Token: hex.EncodeToString(token), Seed: seed}, nil
}

func (m Material) valid() bool {
	raw, err := hex.DecodeString(m.Token)
	return m.Epoch >= 1 && err == nil && len(raw) == 32 && hex.EncodeToString(raw) == m.Token && len(m.Seed) == ed25519.SeedSize
}

// TokenSHA256 is the Worker's PICKUP_TOKEN_SHA256 for this bearer.
func (m Material) TokenSHA256() string {
	sum := sha256.Sum256([]byte(m.Token))
	return hex.EncodeToString(sum[:])
}

// PublicKey is the Worker's ROUTING_PUBLIC_KEY for this key.
func (m Material) PublicKey() string {
	return base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(m.Seed).Public().(ed25519.PublicKey))
}

// signed is the contract's document: the signature covers exactly
// UTF-8(context) || UTF-8(payload), and the payload travels as a JSON string.
func (m Material) signed(field, context string, payload []byte) ([]byte, error) {
	signature := ed25519.Sign(ed25519.NewKeyFromSeed(m.Seed), append([]byte(context), payload...))
	return json.Marshal(map[string]string{field: string(payload), "signature": base64.StdEncoding.EncodeToString(signature)})
}

// Route is one published address with the owner it was frozen to. Only
// address, generation and maxBytes reach the Worker.
type Route struct {
	Address    string `json:"address"`
	Generation int64  `json:"generation"`
	MaxBytes   int64  `json:"maxBytes"`
	Issuer     string `json:"issuer"`
	Subject    string `json:"subject"`
	Mailbox    string `json:"mailbox"`
}

// Block is a blocked sender address or domain; Until nil never expires.
type Block struct {
	Address string `json:"address,omitempty"`
	Domain  string `json:"domain,omitempty"`
	Until   *int64 `json:"until"`
}

// ValidAddress is the Worker's route rule: lowercase printable ASCII
// local@domain without a second '@', at most 254 characters with a local part
// of at most 64. Internationalized domains must already be A-labels.
func ValidAddress(a string) bool {
	local, domain, ok := strings.Cut(a, "@")
	if !ok || len(a) > 254 || len(local) == 0 || len(local) > 64 || domain == "" || a != strings.ToLower(a) {
		return false
	}
	for _, c := range []byte(local + domain) {
		if c < 0x21 || c > 0x7e || c == '@' {
			return false
		}
	}
	return true
}

// LowerASCII lowercases A-Z only. Both matchers compare senders this way:
// Unicode case mapping differs between Go and JavaScript (U+0130) and folds
// some non-ASCII into ASCII (the Kelvin sign to k), so non-ASCII compares
// exactly.
func LowerASCII(s string) string {
	return strings.Map(func(c rune) rune {
		if c >= 'A' && c <= 'Z' {
			return c + 'a' - 'A'
		}
		return c
	}, s)
}

func atoms(s string, ok func(rune) bool) bool {
	for atom := range strings.SplitSeq(s, ".") {
		if atom == "" || strings.ContainsFunc(atom, func(c rune) bool { return !ok(c) }) {
			return false
		}
	}
	return true
}

func atext(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+/=?^_`{|}~-", c)
}

// ValidSender is the sender rule both receiving profiles enforce (the
// Worker's senderOk, fixture receiving-worker/senders.json): the null
// sender, or at most 320 UTF-8 bytes with a dot-atom local part of ASCII
// atext or any non-ASCII character, and an ASCII dot-atom domain (compared
// after LowerASCII). Quoted local parts, domain literals and U-label domains
// are refused: no block could match them on both sides.
func ValidSender(s string) bool {
	if s == "" {
		return true
	}
	at := strings.LastIndexByte(s, '@')
	return utf8.ValidString(s) && len(s) <= 320 && at > 0 &&
		atoms(s[:at], func(c rune) bool { return atext(c) || c >= 0x80 }) &&
		atoms(LowerASCII(s[at+1:]), func(c rune) bool { return atext(c) && (c < 'A' || c > 'Z') })
}

// ValidBlock is the Worker's blockedSenders rule: exactly one of a non-null
// sender address or a sender domain, already LowerASCII, and until null or
// at least 1.
func ValidBlock(b Block) bool {
	value, sender := b.Address, b.Address
	if b.Domain != "" {
		value, sender = b.Domain, "x@"+b.Domain
	}
	return (b.Address == "") != (b.Domain == "") && value == LowerASCII(value) && sender != "" && ValidSender(sender) && (b.Until == nil || *b.Until >= 1)
}

type wireRoute struct {
	Address    string `json:"address"`
	Generation int64  `json:"generation"`
	MaxBytes   int64  `json:"maxBytes"`
}

type wireTable struct {
	Revision       int64       `json:"revision"`
	IssuedAt       int64       `json:"issuedAt"`
	Routes         []wireRoute `json:"routes"`
	BlockedSenders []Block     `json:"blockedSenders"`
}

func newWireTable(revision, issuedAt int64, routes []Route) (wireTable, error) {
	table := wireTable{revision, issuedAt, make([]wireRoute, 0, len(routes)), []Block{}}
	seen := map[string]bool{}
	for _, r := range routes {
		if !ValidAddress(r.Address) || seen[r.Address] || r.Generation < 1 || r.MaxBytes < 1 || r.MaxBytes > MaxMessageBytes {
			return table, errors.New("invalid route for the Worker")
		}
		seen[r.Address] = true
		table.Routes = append(table.Routes, wireRoute{r.Address, r.Generation, r.MaxBytes})
	}
	if len(routes) > maxRoutes {
		return table, errors.New("routing table exceeds the Worker's limits")
	}
	return table, nil
}

// BlockWireBytes is a block's exact size in the signed table, with the
// separating comma, measured by the encoder SignTable uses (json.Marshal
// escapes &, < and > to six bytes each).
func BlockWireBytes(b Block) int {
	raw, _ := json.Marshal(b)
	return len(raw) + 1
}

// FitBlocks returns the longest prefix of blocks that fits the Worker's
// table beside routes (at most 5000 blocks and 1 MiB in all), so blocks can
// never stop route publication; callers order blocks by priority first. An
// error means the routes alone cannot be published.
func FitBlocks(routes []Route, blocks []Block) ([]Block, error) {
	const safe = 1<<53 - 1 // widest revision and issuedAt the Worker accepts
	table, err := newWireTable(safe, safe, routes)
	if err != nil {
		return nil, err
	}
	base, err := json.Marshal(table)
	if err != nil || len(base) > maxTableBytes {
		return nil, errors.New("routing table exceeds the Worker's limits")
	}
	size := len(base) - 1 // the first block has no comma
	for i, b := range blocks {
		if size += BlockWireBytes(b); i == maxBlocks || size > maxTableBytes {
			return blocks[:i], nil
		}
	}
	return blocks, nil
}

// SignTable is the PUT /routes body for routes and blocks.
func SignTable(m Material, revision, issuedAt int64, routes []Route, blocks []Block) ([]byte, error) {
	table, err := newWireTable(revision, issuedAt, routes)
	if err != nil {
		return nil, err
	}
	for _, b := range blocks {
		if !ValidBlock(b) {
			return nil, errors.New("invalid sender block for the Worker")
		}
	}
	table.BlockedSenders = append(table.BlockedSenders, blocks...)
	payload, err := json.Marshal(table)
	if err != nil || revision < 1 || issuedAt < 1 || len(blocks) > maxBlocks || len(payload) > maxTableBytes {
		return nil, errors.New("routing table exceeds the Worker's limits")
	}
	return m.signed("table", routesContext, payload)
}

// SignRotation is the POST /rotate body moving from current to next.
func SignRotation(current, next Material) ([]byte, error) {
	payload, err := json.Marshal(struct {
		Epoch       int64  `json:"epoch"`
		TokenSHA256 string `json:"tokenSha256"`
		PublicKey   string `json:"publicKey"`
	}{next.Epoch, next.TokenSHA256(), next.PublicKey()})
	if err != nil {
		return nil, err
	}
	return current.signed("rotation", rotateContext, payload)
}

// Item is one listed provider object.
type Item struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// Envelope is the Worker's frozen capture record.
type Envelope struct {
	ID            string `json:"id"`
	Sender        string `json:"sender"`
	Recipient     string `json:"recipient"`
	Generation    int64  `json:"generation"`
	TableRevision int64  `json:"tableRevision"`
	CapturedAt    int64  `json:"capturedAt"`
	Size          int64  `json:"size"`
	Digest        string `json:"digest"`
}

// ValidKey is the Worker's lowercase UUIDv7 key.
func ValidKey(k string) bool {
	if len(k) != 36 || k[14] != '7' || !strings.ContainsRune("89ab", rune(k[19])) {
		return false
	}
	for i, c := range []byte(k) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// KeyTime is a valid key's capture millisecond (its first 48 bits).
func KeyTime(k string) int64 {
	raw, _ := hex.DecodeString(k[:8] + k[9:13])
	var ms int64
	for _, b := range raw {
		ms = ms<<8 | int64(b)
	}
	return ms
}

func validDigest(d string) bool {
	raw, err := hex.DecodeString(d)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == d
}

// Client speaks the pickup API. HTTP must refuse redirects and proxies; use
// NewHTTPClient.
type Client struct {
	Origin string
	HTTP   *http.Client
}

// NewHTTPClient is outbound HTTPS with no proxy, no redirects and bounded
// headers; bodies are bounded per request.
func NewHTTPClient() *http.Client {
	transport := &http.Transport{Proxy: nil, MaxResponseHeaderBytes: 16 << 10, TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: time.Minute}
	return &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

// do returns the status and at most limit body bytes. Error bodies are empty
// by contract and never surface in errors.
func (c Client) do(ctx context.Context, method, path, token string, body []byte, limit int64) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.Origin+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, errors.New("cloudflare Worker unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, nil, nil, ErrUnauthorized
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return resp.StatusCode, nil, nil, errors.New("cloudflare Worker response unreadable or oversized")
	}
	return resp.StatusCode, resp.Header, data, nil
}

func unexpected(status int) error {
	if status == http.StatusConflict {
		return ErrConflict
	}
	return fmt.Errorf("cloudflare Worker answered HTTP %d", status)
}

func strict(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

// List reads one page after key ("" for the start).
func (c Client) List(ctx context.Context, token, after string) ([]Item, bool, error) {
	path := "/messages?limit=100"
	if after != "" {
		path += "&after=" + url.QueryEscape(after)
	}
	status, _, data, err := c.do(ctx, http.MethodGet, path, token, nil, 64<<10)
	if err != nil {
		return nil, false, err
	}
	if status != http.StatusOK {
		return nil, false, unexpected(status)
	}
	var page struct {
		Messages  []Item `json:"messages"`
		Truncated bool   `json:"truncated"`
	}
	if strict(data, &page) != nil || len(page.Messages) > 100 {
		return nil, false, errors.New("cloudflare Worker listing malformed")
	}
	for _, it := range page.Messages {
		if !ValidKey(it.Key) || it.Size < 1 || !validDigest(it.Digest) {
			return nil, false, errors.New("cloudflare Worker listing malformed")
		}
	}
	return page.Messages, page.Truncated, nil
}

// Fetch reads one object and verifies it against its envelope. maxBytes
// bounds the read; ErrInvalid means it can never be held locally.
func (c Client) Fetch(ctx context.Context, token, key string, maxBytes int64) (Envelope, []byte, error) {
	var e Envelope
	status, header, raw, err := c.do(ctx, http.MethodGet, "/messages/"+key, token, nil, maxBytes)
	if err != nil {
		return e, nil, err
	}
	if status != http.StatusOK {
		return e, nil, unexpected(status)
	}
	encoded := header.Get("X-KyPost-Envelope")
	envelope, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if len(encoded) > 2700 || err != nil || len(envelope) > maxEnvelope || strict(envelope, &e) != nil {
		return e, nil, ErrInvalid
	}
	sum := sha256.Sum256(raw)
	if e.ID != key || e.Size != int64(len(raw)) || e.Digest != hex.EncodeToString(sum[:]) || !ValidAddress(e.Recipient) || e.Generation < 1 || e.TableRevision < 1 || e.CapturedAt < 1 || len(e.Sender) > 512 {
		return e, nil, ErrInvalid
	}
	return e, raw, nil
}

// Delete removes key only while its digest matches; absent is success.
func (c Client) Delete(ctx context.Context, token, key, digest string) error {
	status, _, _, err := c.do(ctx, http.MethodDelete, "/messages/"+key+"?digest="+digest, token, nil, 0)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return unexpected(status)
	}
	return nil
}

// Revision reads the stored table's revision; 0 when none is installed.
func (c Client) Revision(ctx context.Context, token string) (int64, error) {
	status, _, data, err := c.do(ctx, http.MethodGet, "/routes", token, nil, maxRoutesBody)
	if err != nil || status == http.StatusNotFound {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, unexpected(status)
	}
	var doc struct {
		Table     string `json:"table"`
		Signature string `json:"signature"`
	}
	var table struct {
		Revision int64 `json:"revision"`
	}
	if strict(data, &doc) != nil || json.Unmarshal([]byte(doc.Table), &table) != nil || table.Revision < 1 {
		return 0, errors.New("cloudflare Worker routes malformed")
	}
	return table.Revision, nil
}

// PutRoutes installs a signed table.
func (c Client) PutRoutes(ctx context.Context, token string, body []byte) error {
	status, _, _, err := c.do(ctx, http.MethodPut, "/routes", token, body, 0)
	if err == nil && status != http.StatusNoContent {
		err = unexpected(status)
	}
	return err
}

// Rotate posts a signed rotation and returns the status (0 when no response
// arrived, which says nothing about whether it was installed).
func (c Client) Rotate(ctx context.Context, token string, body []byte) (int, error) {
	status, _, _, err := c.do(ctx, http.MethodPost, "/rotate", token, body, 0)
	return status, err
}

// Authenticated reports whether token is the Worker's current bearer.
func (c Client) Authenticated(ctx context.Context, token string) (bool, error) {
	_, err := c.Revision(ctx, token)
	if errors.Is(err, ErrUnauthorized) {
		return false, nil
	}
	return err == nil, err
}
