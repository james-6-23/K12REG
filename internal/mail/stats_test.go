package mail

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildPoolReport(t *testing.T) {
	dir := t.TempDir()
	mailPath := filepath.Join(dir, "pool.txt")
	statePath := filepath.Join(dir, "state.json")
	// two bases
	body := "a@outlook.com----pw----M.Crefresh_token_here_padding_to_be_long_enough_for_parser_check_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx----11111111-1111-1111-1111-111111111111\n" +
		"b@outlook.com----pw----M.Crefresh_token_here_padding_to_be_long_enough_for_parser_check_yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy----22222222-2222-2222-2222-222222222222\n"
	if err := os.WriteFile(mailPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"a@outlook.com":{"state":"used"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := BuildPoolReport(mailPath, statePath, 2, false)
	if rep.Error != "" {
		t.Fatal(rep.Error)
	}
	if rep.BaseTotal != 2 {
		t.Fatalf("bases %d", rep.BaseTotal)
	}
	if rep.SlotTotal != 4 {
		t.Fatalf("slots %d", rep.SlotTotal)
	}
	// a used → both aliases counted used; b free → 2 free
	if rep.Free != 2 {
		t.Fatalf("free %d want 2", rep.Free)
	}
	if rep.Used < 2 {
		t.Fatalf("used %d", rep.Used)
	}
}
