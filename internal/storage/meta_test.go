package storage

import (
	"context"
	"testing"
)

func TestMeta(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if v, err := db.GetMeta(ctx, "announced_version"); err != nil || v != "" {
		t.Fatalf("empty: v=%q err=%v, want \"\"", v, err)
	}
	if err := db.SetMeta(ctx, "announced_version", "v1.6.0"); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.GetMeta(ctx, "announced_version"); v != "v1.6.0" {
		t.Fatalf("got %q, want v1.6.0", v)
	}
	// Upsert перезаписывает.
	_ = db.SetMeta(ctx, "announced_version", "v1.7.0")
	if v, _ := db.GetMeta(ctx, "announced_version"); v != "v1.7.0" {
		t.Fatalf("got %q, want v1.7.0", v)
	}
}
