package vol_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/ky-primitives/s3"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/vol/objecttest"
)

const orgA, orgB = "0d6c8d5e-5f4e-4c1a-9a43-6f2b1c0e7a01", "0d6c8d5e-5f4e-4c1a-9a43-6f2b1c0e7a02"

func bodyKey(t *testing.T, cell, org, mailbox string) string {
	t.Helper()
	id, err := vol.NewObjectID()
	if err != nil {
		t.Fatal(err)
	}
	return vol.BodyPrefix(cell, org, mailbox) + id
}

func rawObject(t *testing.T, c *s3.Client, key string) []byte {
	t.Helper()
	rc, _, err := c.Get(context.Background(), key, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The bucket holds ciphertext only, and the body reads back.
func TestObjectSealsBodies(t *testing.T) {
	cell := objecttest.Cell()
	o, c := objecttest.New(t, cell)
	ctx := context.Background()
	body := bytes.Repeat([]byte("Subject: secret quarterly plan\r\n\r\nattachment bytes "), 4096)
	key := bodyKey(t, cell, orgA, "mb1")
	if _, err := o.Write(ctx, key, body, 0); err != nil {
		t.Fatal(err)
	}
	stored := rawObject(t, c, key)
	if bytes.Contains(stored, []byte("secret quarterly plan")) || bytes.Contains(stored, []byte("attachment bytes")) {
		t.Fatal("the bucket holds plaintext")
	}
	if len(stored) != len(body)+29 {
		t.Fatalf("sealed size %d, want %d", len(stored), len(body)+29)
	}
	got, rev, err := o.Read(ctx, key)
	if err != nil || rev != 1 || !bytes.Equal(got, body) {
		t.Fatalf("read back: rev %d err %v equal %v", rev, err, bytes.Equal(got, body))
	}
	if _, _, err := o.Read(ctx, bodyKey(t, cell, orgA, "mb1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing object: %v", err)
	}
}

// Ciphertext copied to another org's, mailbox's or cell's key never opens, and neither does
// a tampered or truncated object.
func TestObjectRefusesCopiedOrTamperedCiphertext(t *testing.T) {
	cell, other := objecttest.Cell(), objecttest.Cell()
	o, c := objecttest.New(t, cell, other)
	ctx := context.Background()
	key := bodyKey(t, cell, orgA, "mb1")
	if _, err := o.Write(ctx, key, []byte("Subject: hi\r\n\r\nbody"), 0); err != nil {
		t.Fatal(err)
	}
	sealed := rawObject(t, c, key)
	id := key[strings.LastIndexByte(key, '/')+1:]
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	for name, copy := range map[string]struct {
		key   string
		bytes []byte
	}{
		"other org":     {vol.BodyPrefix(cell, orgB, "mb1") + id, sealed},
		"other mailbox": {vol.BodyPrefix(cell, orgA, "mb2") + id, sealed},
		"other cell":    {vol.BodyPrefix(other, orgA, "mb1") + id, sealed},
		"tampered":      {bodyKey(t, cell, orgA, "mb1"), tampered},
		"truncated":     {bodyKey(t, cell, orgA, "mb1"), sealed[:len(sealed)-1]},
	} {
		if _, err := c.Put(ctx, copy.key, bytes.NewReader(copy.bytes), int64(len(copy.bytes))); err != nil {
			t.Fatal(err)
		}
		if got, _, err := o.Read(ctx, copy.key); err == nil || got != nil {
			t.Errorf("%s: opened (%q, %v)", name, got, err)
		}
	}
}

// A key in a cell this deployment does not serve is refused before any request.
func TestObjectRefusesOtherCells(t *testing.T) {
	cell := objecttest.Cell()
	o, _ := objecttest.New(t, cell)
	ctx := context.Background()
	foreign := bodyKey(t, objecttest.Cell(), orgA, "mb1")
	if _, err := o.Write(ctx, foreign, []byte("x"), 0); !errors.Is(err, vol.ErrCell) {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := o.Read(ctx, foreign); !errors.Is(err, vol.ErrCell) {
		t.Fatalf("read: %v", err)
	}
	if err := o.Remove(ctx, foreign, 1); !errors.Is(err, vol.ErrCell) {
		t.Fatalf("remove: %v", err)
	}
	if err := o.RemoveAll(ctx, vol.BodyPrefix(objecttest.Cell(), orgA)); !errors.Is(err, vol.ErrCell) {
		t.Fatalf("remove all: %v", err)
	}
	for _, bad := range []string{"bodies/" + orgA + "/mb1/x", "cells/" + cell + "/bodies/" + orgA + "/x", "cells/" + cell + "/other/" + orgA + "/mb1/x"} {
		if _, err := o.Write(ctx, bad, []byte("x"), 0); !errors.Is(err, vol.ErrPath) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := o.List(ctx, "cells/"+cell+"/"); !errors.Is(err, vol.ErrPath) {
		t.Fatalf("list outside an org: %v", err)
	}
}

// Objects are written once: of racing creates exactly one wins, and no write replaces it.
func TestObjectWriteOnce(t *testing.T) {
	cell := objecttest.Cell()
	o, _ := objecttest.New(t, cell)
	ctx := context.Background()
	key := bodyKey(t, cell, orgA, "mb1")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { _, errs[i] = o.Write(ctx, key, []byte{byte('a' + i)}, 0) })
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, vol.ErrStale):
			t.Fatal(err)
		}
	}
	if won != 1 {
		t.Fatalf("%d creates won", won)
	}
	if _, err := o.Write(ctx, key, []byte("z"), 1); !errors.Is(err, vol.ErrWriteOnce) {
		t.Fatalf("overwrite: %v", err)
	}
}

// RemoveAll empties a mailbox prefix, then an org prefix, and leaves the neighbours.
func TestObjectRemoveAll(t *testing.T) {
	cell := objecttest.Cell()
	o, _ := objecttest.New(t, cell)
	ctx := context.Background()
	for _, k := range []string{bodyKey(t, cell, orgA, "mb1"), bodyKey(t, cell, orgA, "mb1"), bodyKey(t, cell, orgA, "mb2"), bodyKey(t, cell, orgB, "mb1")} {
		if _, err := o.Write(ctx, k, []byte("x"), 0); err != nil {
			t.Fatal(err)
		}
	}
	count := func(prefix string) int {
		keys, err := o.List(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		return len(keys)
	}
	if n := count(vol.BodyPrefix(cell, orgA)); n != 3 {
		t.Fatalf("org A holds %d", n)
	}
	if err := o.RemoveAll(ctx, vol.BodyPrefix(cell, orgA, "mb1")); err != nil {
		t.Fatal(err)
	}
	if count(vol.BodyPrefix(cell, orgA, "mb1")) != 0 || count(vol.BodyPrefix(cell, orgA, "mb2")) != 1 {
		t.Fatal("mailbox purge")
	}
	if err := o.RemoveAll(ctx, vol.BodyPrefix(cell, orgA)); err != nil {
		t.Fatal(err)
	}
	if count(vol.BodyPrefix(cell, orgA)) != 0 || count(vol.BodyPrefix(cell, orgB)) != 1 {
		t.Fatal("org purge")
	}
}

// The start probe passes on the fixture and fails on wrong credentials.
func TestObjectProbe(t *testing.T) {
	cell := objecttest.Cell()
	o, _ := objecttest.New(t, cell)
	if err := o.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if keys, err := o.List(context.Background(), vol.BodyPrefix(cell, orgA)); err != nil || len(keys) != 0 {
		t.Fatal(keys, err)
	}
	t.Setenv("KY_TEST_S3_SECRET_KEY", "wrong-secret")
	bad, _ := objecttest.New(t, cell)
	err := bad.Probe(context.Background())
	if err == nil || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("wrong secret: %v", err)
	}
}

// ignoringProvider is an S3 endpoint that accepts every PUT, conditional or not.
func ignoringProvider(t *testing.T) *s3.Client {
	var mu sync.Mutex
	objects := map[string][]byte{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = b
			w.Header().Set("ETag", `"x"`)
		case r.Method == http.MethodGet && r.URL.Query().Has("versions"):
			w.Write([]byte(`<ListVersionsResult><IsTruncated>false</IsTruncated></ListVersionsResult>`))
		case r.Method == http.MethodGet:
			w.Write(objects[r.URL.Path])
		}
	}))
	t.Cleanup(srv.Close)
	c, err := s3.New(s3.Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s", HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A provider that ignores If-None-Match fails the start probe: replicas could overwrite.
func TestObjectProbeRefusesUnconditionalProvider(t *testing.T) {
	o, err := vol.NewObject(ignoringProvider(t), []string{"cell-a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "If-None-Match") {
		t.Fatal(err)
	}
}
