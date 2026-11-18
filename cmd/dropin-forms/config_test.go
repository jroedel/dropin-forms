package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimal is the least a config needs to load, so that each case below
// differs from it only in the section under test.
const minimal = `[server]
embed_addr = "127.0.0.1:1"
admin_addr = "127.0.0.1:2"
admin_base_url = "https://forms.example.test"

[embed]
grant_signing_key = "q83vEjRWeJCrze8SNFZ4kKvN7xI0VniQq83vEjRWeJCrze8SNFZ4kKvN7xI0VniQ"

[db]
path = "x.db"
`

// writeConfig writes minimal plus extra to a temporary file.
func writeConfig(t *testing.T, extra string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(minimal+"\n"+extra), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestTwilioSettingsStandOrFallTogether(t *testing.T) {
	const sid = "AC" + "00000000000000000000" + "000000000001"

	cases := []struct {
		name    string
		section string
		refused string // a word the refusal must contain; "" means it loads
	}{
		{"none is texting off", "", ""},
		{"all three", `[twilio]
account_sid = "` + sid + `"
auth_token = "t"
from = "+15125550100"`, ""},
		{"a SID alone", `[twilio]
account_sid = "` + sid + `"`, "not all three"},
		{"an API key in the SID's place", `[twilio]
account_sid = "SK123"
auth_token = "t"
from = "+15125550100"`, "begin with AC"},
		{"a number with no country code", `[twilio]
account_sid = "` + sid + `"
auth_token = "t"
from = "512-555-0100"`, "country code"},
	}

	for _, c := range cases {
		cfg, err := loadConfig(writeConfig(t, c.section))

		switch {
		case c.refused == "" && err != nil:
			t.Errorf("%s: %v, want it to load", c.name, err)
		case c.refused != "" && err == nil:
			t.Errorf("%s: loaded, want a refusal mentioning %q", c.name, c.refused)
		case c.refused != "" && !strings.Contains(err.Error(), c.refused):
			t.Errorf("%s: %v, want it to mention %q", c.name, err, c.refused)
		case c.refused == "" && c.section != "" && cfg.Twilio.From != "+15125550100":
			t.Errorf("%s: from = %q, want it normalised to E.164", c.name, cfg.Twilio.From)
		}
	}
}
