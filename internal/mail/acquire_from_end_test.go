package mail

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireFromEnd(t *testing.T) {
	dir := t.TempDir()
	mailFile := filepath.Join(dir, "pool.txt")
	// email----password----refresh----client_id (refresh long enough for looksRefresh)
	longRT := "M.C"
	for len(longRT) < 120 {
		longRT += "x"
	}
	line := func(email, cid string) string {
		return email + "----pw----" + longRT + "----" + cid + "\n"
	}
	content := line("a@hotmail.com", "11111111-1111-1111-1111-111111111111") +
		line("b@hotmail.com", "22222222-2222-2222-2222-222222222222") +
		line("c@hotmail.com", "33333333-3333-3333-3333-333333333333")
	if err := os.WriteFile(mailFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadPool(mailFile, filepath.Join(dir, "state.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := pool.AcquireFromEnd()
	if err != nil {
		t.Fatal(err)
	}
	if mb.Address != "c@hotmail.com" {
		t.Fatalf("want c from end, got %s", mb.Address)
	}
	pool.Mark(mb, true)
	mb2, err := pool.AcquireFromEnd()
	if err != nil {
		t.Fatal(err)
	}
	if mb2.Address != "b@hotmail.com" {
		t.Fatalf("want b next from end, got %s", mb2.Address)
	}
}
