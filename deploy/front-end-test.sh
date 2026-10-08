#!/usr/bin/env bash
# Tests deploy.sh's install_front_end against a directory standing in for the
# server.
#
#   deploy/front-end-test.sh deploy/deploy.sh
#
# It never touches a server, and cannot: the copy under test has its
# dispatcher cut off, and remote, remote_script and push -- the only three
# functions in deploy.sh that open a connection -- are replaced by versions
# that act on a temporary directory. What is left is the real install_front_end,
# restore_front_end and forget_front_end_prev, run as written.
#
# This exists because a .htaccess is the one file in a deploy whose mistakes
# take every page down at once, and because the rule this script now follows
# -- install only what changed, keep the outgoing one, put it back when the
# public URLs stop answering -- is easy to get subtly wrong in a way nothing
# else here would catch until a real deploy.
set -uo pipefail

REAL="$1"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

export DEPLOY_SSH_HOST=example.test DEPLOY_SSH_USER=u \
	EMBED_PORT=8412 ADMIN_PORT=8413 \
	EMBED_HOST=f.example.test ADMIN_HOST=forms.example.test \
	EMBED_DOCROOT=embed ADMIN_DOCROOT=admin \
	SECRETS_ENV=/nonexistent
unset CI 2>/dev/null || true

pass=0 fail=0
ok()   { printf '  \033[32mok\033[0m   %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n     %s\n' "$1" "${2:-}"; fail=$((fail+1)); }

# The script under test, with its dispatcher replaced by the stand-ins for
# the server and a call to whatever the case asks for. The LAST dispatcher,
# for the reason ensure-main-test.sh gives.
mkdir -p "$WORK/repo/deploy" "$WORK/server/embed" "$WORK/server/admin"
cp "$(dirname "$REAL")/htaccess.template" "$WORK/repo/deploy/htaccess.template"

last=$(grep -nF 'case "${1:-}" in' "$REAL" | tail -1 | cut -d: -f1)
head -n "$((last - 1))" "$REAL" > "$WORK/repo/deploy/deploy.sh"
cat >> "$WORK/repo/deploy/deploy.sh" <<'DRIVER'
SERVER="${SERVER:?}"
remote() { (cd "$SERVER" && bash -c "$*"); }
remote_script() { (cd "$SERVER" && bash -s); }
push() { cp "$1" "$SERVER/$2"; }

for step in "$@"; do
	case "$step" in
	install) install_front_end ;;
	restore) restore_front_end ;;
	forget) forget_front_end_prev ;;
	esac
done

printf 'CHANGED=%s\n' "${FRONT_END_CHANGED[*]+${FRONT_END_CHANGED[*]}}"
DRIVER

run() { SERVER="$WORK/server" bash "$WORK/repo/deploy/deploy.sh" "$@" 2>&1; }
rendered() { sed "s/__APP_PORT__/$1/" "$WORK/repo/deploy/htaccess.template"; }
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

printf '\n1. a server with no .htaccess gets both, readable by Apache\n'
out=$(run install); rc=$?
[ $rc -eq 0 ] && ok "exit 0" || bad "exit $rc" "$out"
diff -q <(rendered 8412) "$WORK/server/embed/.htaccess" >/dev/null && ok "embed is the template at its port" || bad "embed differs" "$out"
diff -q <(rendered 8413) "$WORK/server/admin/.htaccess" >/dev/null && ok "admin is the template at its port" || bad "admin differs" "$out"
[ "$(mode "$WORK/server/admin/.htaccess")" = 644 ] && ok "mode 644" || bad "mode $(mode "$WORK/server/admin/.htaccess")"
[ ! -e "$WORK/server/admin/.htaccess.new" ] && ok "no .htaccess.new left behind" || bad ".htaccess.new left behind"
grep -q "CHANGED=embed admin" <<<"$out" && ok "reports both changed" || bad "changed list" "$out"

printf '\n2. run again: both current, nothing touched\n'
before=$(stat -c %Y "$WORK/server/admin/.htaccess" 2>/dev/null || stat -f %m "$WORK/server/admin/.htaccess")
sleep 1
out=$(run install); rc=$?
[ $rc -eq 0 ] && ok "exit 0" || bad "exit $rc" "$out"
grep -q "already current" <<<"$out" && ok "says it is current" || bad "no current line" "$out"
grep -q "^CHANGED=$" <<<"$out" && ok "reports nothing changed" || bad "changed list" "$out"
after=$(stat -c %Y "$WORK/server/admin/.htaccess" 2>/dev/null || stat -f %m "$WORK/server/admin/.htaccess")
[ "$before" = "$after" ] && ok "the file was not rewritten" || bad "rewritten"

printf '\n3. an old .htaccess is replaced, kept as .prev, and can be put back\n'
echo "# the old rules" > "$WORK/server/admin/.htaccess"
out=$(run install restore); rc=$?
[ $rc -eq 0 ] && ok "exit 0" || bad "exit $rc" "$out"
grep -q "CHANGED=admin$" <<<"$out" && ok "only admin changed" || bad "changed list" "$out"
[ "$(cat "$WORK/server/admin/.htaccess")" = "# the old rules" ] && ok "restore put the old one back" || bad "not restored" "$(cat "$WORK/server/admin/.htaccess")"
[ ! -e "$WORK/server/admin/.htaccess.prev" ] && ok "no .prev left after restoring" || bad ".prev left"

printf '\n4. after a good deploy the .prev is tidied away\n'
out=$(run install forget); rc=$?
[ $rc -eq 0 ] && ok "exit 0" || bad "exit $rc" "$out"
diff -q <(rendered 8413) "$WORK/server/admin/.htaccess" >/dev/null && ok "admin is the new one" || bad "admin differs"
[ ! -e "$WORK/server/admin/.htaccess.prev" ] && ok "no .prev left" || bad ".prev left"

printf '\n5. the renewal path is still Apache'"'"'s, and only the OAuth documents are let through\n'
t="$WORK/server/admin/.htaccess"
grep -qxF 'RewriteCond %{REQUEST_URI} !^/\.well-known/oauth-(authorization-server|protected-resource)(/|$)' "$t" && ok "the exception names exactly the two documents" || bad "exception line"
first_wk=$(grep -n 'well-known' "$t" | grep -v '^[0-9]*:#' | head -1 | cut -d: -f1)
first_proxy=$(grep -n '\[P' "$t" | grep -v '^[0-9]*:#' | head -1 | cut -d: -f1)
[ -n "$first_wk" ] && [ -n "$first_proxy" ] && [ "$first_wk" -lt "$first_proxy" ] && ok "the .well-known rule comes before the proxy" || bad "rule order" "well-known at $first_wk, proxy at $first_proxy"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
