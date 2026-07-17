package memory

import (
	"context"
	"os"
	"testing"
)

func TestMemoryUpdateDeleteAndSecret(t *testing.T) {
	root := t.TempDir()
	m := New(root)
	e := Entry{ID: "m1", Type: "project", Content: "Go", Summary: "language"}
	if err := m.Upsert(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	e.Content = "Go 1.21"
	if err := m.Upsert(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	x, _ := m.List(context.Background())
	if x[0].Version != 2 {
		t.Fatalf("version=%d", x[0].Version)
	}
	if err := m.Upsert(context.Background(), Entry{Type: "user", Content: "password=secret", Summary: "x"}); err == nil {
		t.Fatal("secret accepted")
	}
	if err := m.Delete(context.Background(), "m1"); err != nil {
		t.Fatal(err)
	}
	x, _ = m.Retrieve(context.Background(), "language", 5)
	if len(x) != 0 {
		t.Fatal("deleted memory retrieved")
	}
	_ = os.ErrNotExist
}
