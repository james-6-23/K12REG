package register

import (
	"fmt"
	"testing"
)

func TestIsEmailPermanentlyUnusable(t *testing.T) {
	cases := []struct {
		msg       string
		permanent bool
		exists    bool
	}{
		{
			msg: `validate_otp HTTP 403: { "error": { "message": "You do not have an account because it has been deleted or deactivated. If you believe this was an error, please contact us through our help center at help.openai.com." } }`,
			permanent: true,
		},
		{
			msg:       `create_account HTTP 400: user_already_exists`,
			permanent: true,
			exists:    true,
		},
		{
			msg:       `validate_otp HTTP 409: invalid_state session is no longer valid`,
			permanent: false,
		},
		{
			msg:       `chatgpt_authorize HTTP 502: bad gateway`,
			permanent: false,
		},
		{
			msg:       `registration_disallowed for this domain`,
			permanent: true,
		},
	}
	for _, tc := range cases {
		err := fmt.Errorf("%s", tc.msg)
		if got := IsEmailPermanentlyUnusable(err); got != tc.permanent {
			t.Errorf("permanent(%q) = %v want %v", tc.msg[:min(60, len(tc.msg))], got, tc.permanent)
		}
		if got := IsEmailAlreadyRegistered(err); got != tc.exists {
			t.Errorf("exists(%q) = %v want %v", tc.msg[:min(40, len(tc.msg))], got, tc.exists)
		}
	}
	if IsEmailPermanentlyUnusable(nil) {
		t.Fatal("nil should not be permanent")
	}
}
