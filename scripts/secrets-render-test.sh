#!/usr/bin/env bash
# Tests what `scripts/secrets render` writes into config.toml.
#
#   scripts/secrets-render-test.sh
#
# It never reaches a server and never reaches GitHub, and cannot: `render` is
# the only subcommand it runs, and render's whole contract is that it prints
# the config and touches nothing. `install` and `push` are not called here at
# any point, deliberately -- a test that can deploy is a test nobody dares run.
#
# Everything is read from a fixture in a mktemp directory, through SECRETS_ENV.
# The real secrets.env is never opened, so this passes on a machine that has
# none and cannot print a value that matters.
#
# Two things it exists to hold still.
#
# The first is that the renderer writes *named* settings and never loops over
# the file's keys. That is what makes an extra line in secrets.env inert --
# which matters because loadConfig refuses a config carrying a setting it does
# not understand, so a renderer that copied everything would turn a spare key
# into a service that will not start. STRIPE_TEST_* are exactly such spare
# keys in the default mode, and the assertion below is the reason they are
# safe to keep there.
#
# The second is the mode argument: which pair of Stripe keys lands, and that
# test mode refuses anything that is not a sandbox key. A rehearsal that
# charged a real card would be a bad afternoon.
set -uo pipefail

SELF_DIR=$(cd "$(dirname "$0")" && pwd)
SECRETS="$SELF_DIR/secrets"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass=0 fail=0

ok() {
	printf '  \033[32mok\033[0m   %s\n' "$1"
	pass=$((pass + 1))
}

bad() {
	printf '  \033[31mFAIL\033[0m %s\n' "$1"
	printf '       %s\n' "$2"
	fail=$((fail + 1))
}

# fixture writes a secrets.env holding everything render insists on, plus
# whatever extra lines a case needs.
fixture() {
	local path="$WORK/secrets.env"

	cat > "$path" <<'ENVEOF'
EMBED_HOST=f.example.test
ADMIN_HOST=forms.example.test
EMBED_PORT=8412
ADMIN_PORT=8413
EMBED_ALLOWED_ORIGINS="https://example.test"
APP_DIR=dropin-forms
EMBED_DOCROOT=public_html/f/public
ADMIN_DOCROOT=public_html/forms/public
GRANT_HMAC_KEY=a-fixture-grant-key-long-enough-to-be-accepted
BOOTSTRAP_SIGNIN_SECRET=a-fixture-bootstrap-secret-long-enough-too
KEEP_BACKUPS=14
SMTP_HOST=mail.example.test
SMTP_PORT=587
SMTP_USER=someone@example.test
SMTP_PASSWORD=fixture-not-a-password
MAIL_FROM=someone@example.test
ADMIN_NOTIFY_EMAIL=someone@example.test
ENVEOF

	printf '%s\n' "$@" >> "$path"
	chmod 600 "$path"

	printf '%s' "$path"
}

# render runs the real script against a fixture and prints its output, with
# stderr folded in so a refusal can be asserted on.
render() {
	local path="$1"
	shift

	SECRETS_ENV="$path" bash "$SECRETS" render "$@" 2>&1
}

says() {
	local what="$1" haystack="$2" needle="$3"

	if [[ "$haystack" == *"$needle"* ]]; then
		ok "$what"
	else
		bad "$what" "no $needle in: $(printf '%s' "$haystack" | tr '\n' '|')"
	fi
}

denies() {
	local what="$1" haystack="$2" needle="$3"

	if [[ "$haystack" == *"$needle"* ]]; then
		bad "$what" "found $needle, which should not be there"
	else
		ok "$what"
	fi
}

echo "render, with both pairs of keys present"

both=$(fixture \
	'STRIPE_SECRET_KEY=sk_live_theconfiguredone' \
	'STRIPE_PUBLISHABLE_KEY=pk_live_theconfiguredone' \
	'STRIPE_WEBHOOK_SECRET=whsec_theconfiguredone' \
	'STRIPE_TEST_SECRET_KEY=sk_test_thesandboxone' \
	'STRIPE_TEST_PUBLISHABLE_KEY=pk_test_thesandboxone' \
	'STRIPE_TEST_WEBHOOK_SECRET=whsec_thesandboxone')

out=$(render "$both")
says "the default mode writes the configured key" "$out" 'secret_key = "sk_live_theconfiguredone"'
says "and its webhook secret" "$out" 'webhook_secret = "whsec_theconfiguredone"'
denies "and no sandbox key" "$out" 'sk_test_thesandboxone'
denies "and no sandbox webhook secret" "$out" 'whsec_thesandboxone'

# The assertion that makes a spare key in secrets.env safe to keep. A renderer
# that looped over the file would put these in config.toml, and the binary
# refuses a config with a setting it does not understand.
denies "the extra keys reach the config under no name at all" "$out" 'STRIPE_TEST'
denies "nor does the publishable key, which nothing reads yet" "$out" 'pk_live'

out=$(render "$both" test)
says "test mode says so out loud" "$out" 'no real card will be charged'
says "test mode writes the sandbox key" "$out" 'secret_key = "sk_test_thesandboxone"'
says "and the sandbox webhook secret" "$out" 'webhook_secret = "whsec_thesandboxone"'
denies "and not the configured one" "$out" 'sk_live_theconfiguredone'
denies "nor its webhook secret" "$out" 'whsec_theconfiguredone'

echo
echo "the config is otherwise identical between modes"

# Everything but the [stripe] section has to be the same, or "rehearse in test
# mode" would be rehearsing against a different service.
default_body=$(render "$both" | grep -v 'secret_key\|webhook_secret')
test_body=$(render "$both" test | grep -v 'secret_key\|webhook_secret\|no real card')

if [ "$default_body" = "$test_body" ]; then
	ok "the two modes differ in the Stripe keys and nothing else"
else
	bad "the two modes differ in the Stripe keys and nothing else" \
		"$(diff <(printf '%s' "$default_body") <(printf '%s' "$test_body") | head -5 | tr '\n' '|')"
fi

echo
echo "refusals"

none=$(fixture \
	'STRIPE_SECRET_KEY=sk_live_theconfiguredone' \
	'STRIPE_WEBHOOK_SECRET=whsec_theconfiguredone')

out=$(render "$none" test)
if [ $? -eq 0 ]; then
	bad "test mode with no test keys is refused" "it rendered something instead"
else
	says "test mode with no test keys is refused, by name" "$out" 'STRIPE_TEST_SECRET_KEY'
	says "and says where they come from" "$out" 'secrets.env.example'
fi

half=$(fixture \
	'STRIPE_SECRET_KEY=sk_live_theconfiguredone' \
	'STRIPE_WEBHOOK_SECRET=whsec_theconfiguredone' \
	'STRIPE_TEST_SECRET_KEY=sk_test_thesandboxone')

out=$(render "$half" test)
says "a test key with no test webhook secret is refused" "$out" 'STRIPE_TEST_WEBHOOK_SECRET'

# The one that would cost money. A live key in the test slot means a rehearsal
# on the real page charging a real card.
wrong=$(fixture \
	'STRIPE_SECRET_KEY=sk_live_theconfiguredone' \
	'STRIPE_WEBHOOK_SECRET=whsec_theconfiguredone' \
	'STRIPE_TEST_SECRET_KEY=sk_live_pastedintothewrongslot' \
	'STRIPE_TEST_WEBHOOK_SECRET=whsec_thesandboxone')

out=$(render "$wrong" test)
says "a live key in the test slot is refused" "$out" 'does not begin sk_test_'
denies "and nothing was rendered" "$out" '[stripe]'

# The other direction is deliberately allowed: the STRIPE_* keys are whatever
# this installation runs on, and they were sandbox keys until the day it went
# live. The binary reports which it got rather than this refusing it.
sandbox_default=$(fixture \
	'STRIPE_SECRET_KEY=sk_test_stillrehearsing' \
	'STRIPE_WEBHOOK_SECRET=whsec_stillrehearsing')

out=$(render "$sandbox_default")
says "sandbox keys in the default slot are allowed" "$out" 'secret_key = "sk_test_stillrehearsing"'

echo
echo "payments off"

off=$(fixture)
out=$(render "$off")
denies "no keys at all writes no [stripe] section, rather than an empty one" "$out" '[stripe]'
says "and the rest of the config still renders" "$out" 'admin_base_url'

partly=$(fixture 'STRIPE_SECRET_KEY=sk_live_theconfiguredone')
out=$(render "$partly")
says "a key with no webhook secret is refused, not rendered" "$out" 'STRIPE_WEBHOOK_SECRET'

echo
if [ "$fail" -gt 0 ]; then
	printf '\033[31m%d passed, %d failed\033[0m\n' "$pass" "$fail"
	exit 1
fi

printf '\033[32m%d passed, 0 failed\033[0m\n' "$pass"
