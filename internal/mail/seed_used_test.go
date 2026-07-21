package mail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSeedUsedMarksBaseAndAliases(t *testing.T) {
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
	// Seed base → all aliases burned.
	n := pool.SeedUsed([]string{"a@outlook.com"})
	if n < 1 {
		t.Fatalf("want marks, got %d", n)
	}
	if _, err := pool.Acquire(); err == nil {
		t.Fatal("expected pool exhausted after seeding base")
	}
}

func TestSeedUsedMarksExactAliasOnly(t *testing.T) {
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
	// Grab first alias address.
	mb, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(mb)

	n := pool.SeedUsed([]string{mb.Address})
	if n < 1 {
		t.Fatalf("want at least 1 mark, got %d", n)
	}
	// Sibling aliases of same base must still be free.
	mb2, err := pool.Acquire()
	if err != nil {
		t.Fatalf("sibling should still be free: %v", err)
	}
	if strings.EqualFold(mb2.Address, mb.Address) {
		t.Fatal("acquired the seeded alias again")
	}
	if mailboxBase(mb2) != mailboxBase(mb) {
		t.Fatalf("want same base, got %s vs %s", mailboxBase(mb2), mailboxBase(mb))
	}
}

func TestSeedUsedScalesWithLargePool(t *testing.T) {
	dir := t.TempDir()
	mailFile := filepath.Join(dir, "pool.txt")
	longRT := "M.C" + strings.Repeat("x", 120)
	var b strings.Builder
	const bases = 2000
	for i := 0; i < bases; i++ {
		email := fmt.Sprintf("user%d@outlook.com", i)
		cid := fmt.Sprintf("%08x-1111-1111-1111-111111111111", i)
		b.WriteString(email + "----pw----" + longRT + "----" + cid + "\n")
	}
	if err := os.WriteFile(mailFile, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadPool(mailFile, filepath.Join(dir, "state.json"), 5)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a few hundred registered addresses (mix of bases + noise).
	emails := make([]string, 0, 400)
	for i := 0; i < 200; i++ {
		emails = append(emails, fmt.Sprintf("user%d@outlook.com", i))
	}
	for i := 0; i < 200; i++ {
		emails = append(emails, fmt.Sprintf("other%d@gmail.com", i))
	}
	start := time.Now()
	n := pool.SeedUsed(emails)
	elapsed := time.Since(start)
	if n < 200 {
		t.Fatalf("expected many marks, got %d", n)
	}
	// Old O(emails×items) was ~300ms on similar size; indexed path should be well under 50ms.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("SeedUsed too slow: %v (marks=%d items=%d)", elapsed, n, len(pool.items))
	}
	t.Logf("SeedUsed marks=%d items=%d in %v", n, len(pool.items), elapsed)
}

func TestDebouncedSaveFlushes(t *testing.T) {
	dir := t.TempDir()
	mailFile := filepath.Join(dir, "pool.txt")
	statePath := filepath.Join(dir, "state.json")
	longRT := "M.C" + strings.Repeat("x", 120)
	line := "a@outlook.com----pw----" + longRT + "----11111111-1111-1111-1111-111111111111\n"
	if err := os.WriteFile(mailFile, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadPool(mailFile, statePath, 1)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	pool.Mark(mb, true)
	pool.FlushState()

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), strings.ToLower(mb.Address)) {
		t.Fatalf("state file missing address after FlushState: %s", raw)
	}
	// Compact JSON (no pretty indent).
	if strings.Contains(string(raw), "\n  ") {
		t.Fatal("expected compact JSON without indent")
	}
}

func TestBaseBusyO1AfterAcquire(t *testing.T) {
	dir := t.TempDir()
	mailFile := filepath.Join(dir, "pool.txt")
	longRT := "M.C" + strings.Repeat("x", 120)
	content := ""
	for i, e := range []string{"a@outlook.com", "b@outlook.com", "c@outlook.com"} {
		cid := fmt.Sprintf("%08x-1111-1111-1111-111111111111", i+1)
		content += e + "----pw----" + longRT + "----" + cid + "\n"
	}
	if err := os.WriteFile(mailFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadPool(mailFile, filepath.Join(dir, "state.json"), 2)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	base := mailboxBase(mb)
	pool.mu.Lock()
	busy := pool.baseBusyLocked(base)
	cnt := pool.inUseByBase[base]
	pool.mu.Unlock()
	if !busy || cnt != 1 {
		t.Fatalf("want base busy count=1, busy=%v count=%d", busy, cnt)
	}
	pool.Mark(mb, true)
	pool.mu.Lock()
	busy = pool.baseBusyLocked(base)
	cnt = pool.inUseByBase[base]
	pool.mu.Unlock()
	if busy || cnt != 0 {
		t.Fatalf("after Mark success want idle, busy=%v count=%d", busy, cnt)
	}
}
