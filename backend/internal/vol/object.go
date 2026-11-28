package vol

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/Busnes-app/ky-primitives/s3"
)

// Object keeps message bodies in an S3-compatible bucket, multi mode only
// (docs/superpowers/specs/2026-11-22-multi-host-replicas-design.md, "MH11 object storage,
// amended"). Keys are <app>/cells/<cell>/bodies/<org>/<mailbox>/<object>, where app is the
// per-app prefix (KYPOST_S3_PREFIX), so the suite's apps can share a bucket. Each object is sealed with
// its org's data key, bound to its full key, so the bucket holds no plaintext and a copied
// object never opens. Objects are written once: revision 0 creates, nothing overwrites.
type Object struct {
	c     *s3.Client
	app   string
	cells []string
	key   func(context.Context, string) ([]byte, error)
}

// MaxObject bounds an object's plaintext: the 25 MiB message cap, not MaxBody.
const MaxObject = 25 << 20

const objectVersion = 1

var (
	// ErrCell is a key outside the cells this deployment serves.
	ErrCell = errors.New("vol: object belongs to a cell this deployment does not serve")
	// ErrWriteOnce is a write at a revision other than 0: objects are never replaced.
	ErrWriteOnce = errors.New("vol: objects are written once")
	errObject    = errors.New("vol: object authentication failed")
)

// DefaultAppPrefix is KyPost's first object-key segment (KYPOST_S3_PREFIX).
const DefaultAppPrefix = "kypost"

// appPrefix is one key segment, so no app's prefix can sit inside another app's keys.
var appPrefix = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidAppPrefix reports whether app is a per-app key prefix: one nonempty segment, no slash
// and no "..". Every key, list and purge stays under "<app>/".
func ValidAppPrefix(app string) bool {
	return appPrefix.MatchString(app) && !strings.Contains(app, "..")
}

// NewObject opens bodies over client for cells, under the app prefix. key returns an org's
// 32-byte data key; with none, only the unsealed start probe works.
func NewObject(client *s3.Client, app string, cells []string, key func(context.Context, string) ([]byte, error)) (*Object, error) {
	if client == nil || len(cells) == 0 || !ValidAppPrefix(app) {
		return nil, errors.New("vol: object storage needs a client, cells and a valid app prefix")
	}
	return &Object{c: client, app: app, cells: slices.Clone(cells), key: key}, nil
}

// NewObjectID is 128 random bits as 32 lowercase hex digits.
func NewObjectID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// BodyPrefix is the slash-terminated prefix of a cell's org, or of one mailbox in it.
func (o *Object) BodyPrefix(cell, org string, mailbox ...string) string {
	return BodyPrefix(o.app, cell, org, mailbox...)
}

// BodyPrefix is the same prefix under an explicit app prefix.
func BodyPrefix(app, cell, org string, mailbox ...string) string {
	return app + "/cells/" + cell + "/bodies/" + org + "/" + strings.Join(append(mailbox, ""), "/")
}

// scope checks a key's shape and cell and returns its org. Nothing is sent before it.
func (o *Object) scope(path string) (string, error) {
	if err := CheckPath(path); err != nil {
		return "", err
	}
	p := strings.Split(path, "/")
	switch {
	case len(p) == 7 && p[0] == o.app && p[1] == "cells" && p[3] == "bodies":
	case len(p) == 5 && p[0] == o.app && p[1] == "cells" && p[3] == "probe":
	default:
		return "", ErrPath
	}
	if !slices.Contains(o.cells, p[2]) {
		return "", ErrCell
	}
	if p[3] == "probe" {
		return "", nil
	}
	return p[4], nil
}

func (o *Object) aead(ctx context.Context, org string) (cipher.AEAD, error) {
	if o.key == nil {
		return nil, errObject
	}
	key, err := o.key(ctx, org)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errObject
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func binding(path string) []byte {
	b, _ := json.Marshal([]string{"kypost:body:1", path})
	return b
}

// Read opens a body; revision 1 means present. A missing object is fs.ErrNotExist.
func (o *Object) Read(ctx context.Context, path string) ([]byte, int64, error) {
	org, err := o.scope(path)
	if err != nil {
		return nil, 0, err
	}
	rc, _, err := o.c.Get(ctx, path, "")
	if err != nil {
		return nil, 0, err
	}
	defer rc.Close()
	sealed, err := io.ReadAll(io.LimitReader(rc, MaxObject+64))
	if err != nil {
		return nil, 0, err
	}
	if org == "" { // a probe is not sealed
		return sealed, 1, nil
	}
	aead, err := o.aead(ctx, org)
	if err != nil {
		return nil, 0, err
	}
	if len(sealed) < 1 || sealed[0] != objectVersion {
		return nil, 0, errObject
	}
	body, err := aead.Open(nil, nil, sealed[1:], binding(path))
	if err != nil || len(body) > MaxObject {
		return nil, 0, errObject
	}
	return body, 1, nil
}

// Write creates path with If-None-Match: *; a lost race is ErrStale.
func (o *Object) Write(ctx context.Context, path string, body []byte, rev int64) (int64, error) {
	if rev != 0 {
		return 0, ErrWriteOnce
	}
	if len(body) > MaxObject {
		return 0, ErrTooLarge
	}
	org, err := o.scope(path)
	if err != nil {
		return 0, err
	}
	sealed := body
	if org != "" {
		aead, err := o.aead(ctx, org)
		if err != nil {
			return 0, err
		}
		sealed = aead.Seal([]byte{objectVersion}, nil, body, binding(path))
	}
	_, err = o.c.PutIfAbsent(ctx, path, bytes.NewReader(sealed), int64(len(sealed)))
	if errors.Is(err, s3.ErrAlreadyExists) {
		return 0, ErrStale
	}
	if err != nil {
		return 0, err
	}
	return 1, nil
}

// Remove deletes every version of path, so a versioned bucket keeps no old bytes.
func (o *Object) Remove(ctx context.Context, path string, rev int64) error {
	if _, err := o.scope(path); err != nil {
		return err
	}
	return o.c.DeleteAllVersions(ctx, path)
}

// List returns every key under a slash-terminated prefix, sorted.
func (o *Object) List(ctx context.Context, prefix string) ([]string, error) {
	if err := o.checkPrefix(prefix); err != nil {
		return nil, err
	}
	var keys []string
	next := ""
	for range 1 << 20 { // a bound, not a limit any mailbox reaches
		page, err := o.c.ListObjects(ctx, prefix, next)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Objects {
			keys = append(keys, obj.Key)
		}
		if next = page.Next; next == "" {
			slices.Sort(keys)
			return keys, nil
		}
	}
	return nil, errors.New("vol: object listing did not end")
}

// RemoveAll deletes every version under a slash-terminated prefix (the purge).
func (o *Object) RemoveAll(ctx context.Context, prefix string) error {
	if err := o.checkPrefix(prefix); err != nil {
		return err
	}
	return o.c.DeletePrefix(ctx, prefix)
}

// checkPrefix admits <app>/cells/<cell>/bodies/<org>/ and .../<org>/<mailbox>/ only.
func (o *Object) checkPrefix(prefix string) error {
	trimmed, ok := strings.CutSuffix(prefix, "/")
	if !ok {
		return ErrPath
	}
	if err := CheckPath(trimmed); err != nil {
		return err
	}
	p := strings.Split(trimmed, "/")
	if (len(p) != 5 && len(p) != 6) || p[0] != o.app || p[1] != "cells" || p[3] != "bodies" {
		return ErrPath
	}
	if !slices.Contains(o.cells, p[2]) {
		return ErrCell
	}
	return nil
}

// Probe proves the credentials and conditional creates work in every served cell: it writes
// a random probe key, requires a second create to be refused, reads it back and deletes it.
func (o *Object) Probe(ctx context.Context) error {
	for _, cell := range o.cells {
		id, err := NewObjectID()
		if err != nil {
			return err
		}
		path := o.app + "/cells/" + cell + "/probe/" + id
		if _, err := o.Write(ctx, path, []byte(id), 0); err != nil {
			return fmt.Errorf("object storage probe write: %w", err)
		}
		_, err = o.Write(ctx, path, []byte("x"), 0)
		got, _, readErr := o.Read(ctx, path)
		removeErr := o.Remove(ctx, path, 1)
		switch {
		case !errors.Is(err, ErrStale):
			return errors.New("object storage probe: the provider does not refuse an existing key (If-None-Match)")
		case readErr != nil || string(got) != id:
			return errors.New("object storage probe: read back failed")
		case removeErr != nil:
			return fmt.Errorf("object storage probe delete: %w", removeErr)
		}
	}
	return nil
}

var _ Vol = (*Object)(nil)
