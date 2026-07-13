package mail

import (
	"testing"
	"time"
)

func TestRecipientMatchesMailbox(t *testing.T) {
	base := Mailbox{
		Address:   "user@outlook.com",
		BaseEmail: "user@outlook.com",
	}
	alias := Mailbox{
		Address:   "user+ab12@outlook.com",
		BaseEmail: "user@outlook.com",
	}
	sibling := graphMsg{
		Subject:    "Your OpenAI code",
		From:       "noreply@tm.openai.com",
		Recipients: []string{"user+cd34@outlook.com"},
		Received:   time.Now(),
	}
	mine := graphMsg{
		Subject:    "Your OpenAI code",
		From:       "noreply@tm.openai.com",
		Recipients: []string{"user+ab12@outlook.com"},
		Received:   time.Now(),
	}
	emptyTo := graphMsg{
		Subject:  "Your OpenAI code",
		From:     "noreply@tm.openai.com",
		Received: time.Now(),
	}
	tagOnly := graphMsg{
		Subject:    "code",
		From:       "noreply@tm.openai.com",
		Recipients: []string{`"User" <user+ab12@outlook.com>`},
		Received:   time.Now(),
	}

	if !recipientMatchesMailbox(base, sibling) {
		t.Fatal("base mailbox should accept any mail")
	}
	if recipientMatchesMailbox(alias, sibling) {
		t.Fatal("alias must not accept sibling To")
	}
	if !recipientMatchesMailbox(alias, mine) {
		t.Fatal("alias must accept own To")
	}
	if !recipientMatchesMailbox(alias, emptyTo) {
		t.Fatal("empty recipients should not deadlock")
	}
	if !recipientMatchesMailbox(alias, tagOnly) {
		t.Fatal("should match +tag in recipient blob")
	}
}

func TestMessageOTPRefStable(t *testing.T) {
	m := graphMsg{ID: "AAA", Subject: "x", Received: time.Unix(1, 0)}
	if messageOTPRef(m, "123456") != "id:AAA" {
		t.Fatal("prefer graph id")
	}
	m2 := graphMsg{Subject: "s", Received: time.Unix(2, 0), Recipients: []string{"a@b.com"}}
	r1 := messageOTPRef(m2, "111111")
	r2 := messageOTPRef(m2, "111111")
	if r1 != r2 || r1 == "" {
		t.Fatalf("fingerprint unstable: %q %q", r1, r2)
	}
}

func TestClaimOTPMessage(t *testing.T) {
	ref := "id:test-claim-" + time.Now().Format("150405.000")
	if !claimOTPMessage(ref, "999001") {
		t.Fatal("first claim")
	}
	if claimOTPMessage(ref, "999001") {
		t.Fatal("second claim should fail")
	}
	UnclaimOTPCode("999001")
	if !claimOTPMessage(ref, "999001") {
		t.Fatal("after unclaim should succeed")
	}
}
