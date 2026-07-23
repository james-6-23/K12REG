package workspace

import "testing"

func TestEmailMatchPlusAlias(t *testing.T) {
	cases := []struct {
		want, got string
		ok        bool
	}{
		{"a+tag1@outlook.com", "a+tag1@outlook.com", true},
		{"a+tag1@outlook.com", "a@outlook.com", true},
		{"a@outlook.com", "a+xyz@outlook.com", true},
		{"JenniferSutton44391P+6fe02@outlook.com", "jennifersutton44391p+6fe02@outlook.com", true},
		{"foo@outlook.com", "bar@outlook.com", false},
		{"a@outlook.com", "a@gmail.com", false},
		{"", "a@outlook.com", false},
	}
	for _, tc := range cases {
		if got := emailMatch(tc.want, tc.got); got != tc.ok {
			t.Errorf("emailMatch(%q,%q)=%v want %v", tc.want, tc.got, got, tc.ok)
		}
	}
}

func TestMatchInviteID(t *testing.T) {
	items := []map[string]any{
		{"id": "1", "email_address": "other@outlook.com"},
		{"id": "2", "email": "user+abc@outlook.com"},
	}
	id, em := matchInviteID(items, "user+xyz@outlook.com")
	if id != "2" {
		t.Fatalf("want id=2 got %s email=%s", id, em)
	}
}
