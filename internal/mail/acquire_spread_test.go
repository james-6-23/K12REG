package mail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquirePrefersDifferentBases(t *testing.T) {
	dir := t.TempDir()
	mailFile := filepath.Join(dir, "pool.txt")
	longRT := "M.C" + strings.Repeat("x", 120)
	line := func(email, cid string) string {
		return email + "----pw----" + longRT + "----" + cid + "\n"
	}
	// 3 base inboxes; alias_count=3 → 9 slots, same base shares Graph inbox.
	content := line("a@outlook.com", "11111111-1111-1111-1111-111111111111") +
		line("b@outlook.com", "22222222-2222-2222-2222-222222222222") +
		line("c@outlook.com", "33333333-3333-3333-3333-333333333333")
	if err := os.WriteFile(mailFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadPool(mailFile, filepath.Join(dir, "state.json"), 3)
	if err != nil {
		t.Fatal(err)
	}

	// First 3 acquires should hit 3 different bases (not a+1,a+2,a+3).
	bases := map[string]bool{}
	for i := 0; i < 3; i++ {
		mb, err := pool.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		base := mailboxBase(mb)
		if bases[base] {
			t.Fatalf("acquire %d reused base %s (want spread across bases)", i+1, base)
		}
		bases[base] = true
	}
	if len(bases) != 3 {
		t.Fatalf("want 3 distinct bases, got %v", bases)
	}

	// 4th acquire: all bases busy → may share a base (fallback).
	mb4, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if mailboxBase(mb4) == "" {
		t.Fatal("empty base on 4th acquire")
	}

	// Success marks only alias, not whole base — after Release/finish of one base worker,
	// sibling free aliases of idle bases still preferred.
	pool.Mark(mb4, true)
	// Free one of the first three so its base is no longer busy.
	// We don't have the first three MBs; mark by acquiring state: set one address free via Mark false is hard.
	// Instead: Release isn't public for address — use Mark(false) needs Mailbox.
	// Acquire remaining until we get a free idle base after completing one worker.
}

func TestMarkSuccessDoesNotBurnBase(t *testing.T) {
	dir := t.TempDir()
	mailFile := filepath.Join(dir, "pool.txt")
	longRT := "M.C" + strings.Repeat("x", 120)
	line := "a@outlook.com----pw----" + longRT + "----11111111-1111-1111-1111-111111111111\n"
	if err := os.WriteFile(mailFile, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadPool(mailFile, filepath.Join(dir, "state.json"), 3)
	if err != nil {
		t.Fatal(err)
	}
	mb1, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	pool.Mark(mb1, true)
	// Sibling alias of same base should still be acquirable.
	mb2, err := pool.Acquire()
	if err != nil {
		t.Fatalf("expected sibling alias free after one success: %v", err)
	}
	if mailboxBase(mb2) != mailboxBase(mb1) {
		t.Fatalf("want same base sibling, got %s vs %s", mailboxBase(mb2), mailboxBase(mb1))
	}
	if mb2.Address == mb1.Address {
		t.Fatal("want different alias address")
	}
}
