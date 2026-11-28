package vol_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/vol/objecttest"
)

func TestValidAppPrefix(t *testing.T) {
	for _, ok := range []string{"kypost", "kydrive", "ky-post.2"} {
		if !vol.ValidAppPrefix(ok) {
			t.Error("refused", ok)
		}
	}
	for _, bad := range []string{"", "/kypost", "kypost/", "ky/post", "..", "ky..post", "KyPost", ".kypost"} {
		if vol.ValidAppPrefix(bad) {
			t.Error("accepted", bad)
		}
	}
}

// Two apps keep the same org and cell in one bucket. Purging the org in KyPost must leave
// the other app's objects, and a pre-prefix app's unprefixed keys, untouched; KyPost cannot
// even address them.
func TestObjectPurgeLeavesAnotherAppsObjectsInASharedBucket(t *testing.T) {
	cell := objecttest.Cell()
	c := objecttest.Client(t)
	open := func(app string) *vol.Object {
		o, err := vol.NewObject(c, app, []string{cell}, objecttest.Key)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	post, drive := open("kypost"), open("kydrive")
	id, err := vol.NewObjectID()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	legacy := vol.BodyPrefix("x", cell, orgA, "mb1")[len("x/"):] + id // cells/<cell>/bodies/...
	t.Cleanup(func() {
		_ = drive.RemoveAll(ctx, drive.BodyPrefix(cell, orgA))
		_ = post.RemoveAll(ctx, post.BodyPrefix(cell, orgA))
		_ = c.DeleteAllVersions(ctx, legacy)
	})
	for _, o := range []*vol.Object{post, drive} {
		if _, err := o.Write(ctx, o.BodyPrefix(cell, orgA, "mb1")+id, []byte("body"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Put(ctx, legacy, bytes.NewReader([]byte("legacy")), 6); err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []string{drive.BodyPrefix(cell, orgA), "cells/" + cell + "/bodies/" + orgA + "/"} {
		if err := post.RemoveAll(ctx, foreign); !errors.Is(err, vol.ErrPath) {
			t.Fatalf("KyPost addressed %s: %v", foreign, err)
		}
	}
	if err := post.RemoveAll(ctx, post.BodyPrefix(cell, orgA)); err != nil {
		t.Fatal(err)
	}
	if keys, err := post.List(ctx, post.BodyPrefix(cell, orgA)); err != nil || len(keys) != 0 {
		t.Fatal("purge kept KyPost's own body", keys, err)
	}
	if body, _, err := drive.Read(ctx, drive.BodyPrefix(cell, orgA, "mb1")+id); err != nil || string(body) != "body" {
		t.Fatal("KyPost's purge deleted another app's object", err)
	}
	if _, err := c.Head(ctx, legacy, ""); err != nil {
		t.Fatal("KyPost's purge deleted an unprefixed key", err)
	}
}
