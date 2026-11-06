package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenUsesLiteralDatabasePath(t *testing.T) {
	for _, name := range []string{"bot#state.db", "bot?state.db"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			db, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database not created at the requested literal path: %v", err)
			}
			reader, err := OpenReadOnly(context.Background(), path)
			if err != nil {
				t.Fatalf("read-only client cannot open writer's database: %v", err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
