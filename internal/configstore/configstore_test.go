package configstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestConfigStoreRoundTrip(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run configstore tests")
	}
	ctx := context.Background()
	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.pool.Exec(ctx, `DELETE FROM config_fragment WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	// absent row -> (nil, 0, nil)
	doc, ver, err := st.Get(ctx)
	if err != nil || doc != nil || ver != 0 {
		t.Fatalf("empty get: doc=%s ver=%d err=%v", doc, ver, err)
	}
	// first insert (expectedVersion 0) -> version 1
	if err := st.Set(ctx, json.RawMessage(`{"targets":{"children":{}}}`), 0); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if _, ver, _ = st.Get(ctx); ver != 1 {
		t.Fatalf("want version 1, got %d", ver)
	}
	// second insert with expectedVersion 0 -> conflict (row exists)
	if err := st.Set(ctx, json.RawMessage(`{}`), 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict on re-insert, got %v", err)
	}
	// update with correct expectedVersion 1 -> version 2
	if err := st.Set(ctx, json.RawMessage(`{"targets":{"children":{"x":{"probe":"HTTP","host":"a"}}}}`), 1); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, ver, _ = st.Get(ctx); ver != 2 {
		t.Fatalf("want version 2, got %d", ver)
	}
	// stale update expectedVersion 1 -> conflict
	if err := st.Set(ctx, json.RawMessage(`{}`), 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict on stale update, got %v", err)
	}
}
