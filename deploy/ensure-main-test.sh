#!/usr/bin/env bash
# Tests deploy.sh's ensure_main in a throwaway git repository.
#
#   deploy/ensure-main-test.sh deploy/deploy.sh
#
# It never touches a server, and cannot: the copy under test has its dispatcher
# replaced by a single call to ensure_main, so nothing after that function
# exists to run. Everything else happens in a mktemp directory with its own
# origin, and the environment deploy.sh insists on is supplied with values that
# point nowhere.
#
# This exists because deploy.sh is the most dangerous file in the repository
# and ensure_main is the part of it that moves somebody's working tree. It was
# written after a real incident (see the comment on hand_over_if_changed) and
# then again after the guard was changed to switch branches rather than refuse,
# which is exactly the kind of change that is easy to get subtly wrong: the
# switch can rewrite the running script, and bash reads a script incrementally.
set -uo pipefail

REAL="$1"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

export DEPLOY_SSH_HOST=example.test DEPLOY_SSH_USER=u \
	EMBED_PORT=8412 ADMIN_PORT=8413 \
	EMBED_HOST=f.example.test ADMIN_HOST=forms.example.test \
	EMBED_DOCROOT=public_html/f/public ADMIN_DOCROOT=public_html/forms/public
unset CI DEPLOY_ALLOW_BRANCH DEPLOY_SELF_UPDATED 2>/dev/null || true

pass=0 fail=0
ok()   { printf '  \033[32mok\033[0m   %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n     %s\n' "$1" "${2:-}"; fail=$((fail+1)); }

# build_script writes the script under test, with the dispatcher replaced.
# $2, if given, is an extra marker line so a test can tell two versions apart.
build_script() {
	local dest="$1" marker="${2:-}"
	mkdir -p "$(dirname "$dest")"
	# The LAST dispatcher, not the first: there is an earlier
	# `case "${1:-}" in` for -h/--help near the top, and cutting at that one
	# produced a 43-line script with no functions in it at all.
	local last
	last=$(grep -nF 'case "${1:-}" in' "$REAL" | tail -1 | cut -d: -f1)
	head -n "$((last - 1))" "$REAL" > "$dest"
	[ -z "$marker" ] || printf '# VERSION: %s\n' "$marker" >> "$dest"
	printf 'ensure_main\n' >> "$dest"
	chmod +x "$dest"
}

setup() {
	rm -rf "$WORK/origin.git" "$WORK/repo" "$WORK/other" "$WORK/other2"
	git init -q --bare "$WORK/origin.git"
	git init -q -b main "$WORK/repo"
	git -C "$WORK/repo" config user.email t@example.test
	git -C "$WORK/repo" config user.name Test
	build_script "$WORK/repo/deploy/deploy.sh" "${1:-one}"
	echo seed > "$WORK/repo/README"
	git -C "$WORK/repo" add -A
	git -C "$WORK/repo" commit -qm "seed"
	git -C "$WORK/repo" remote add origin "$WORK/origin.git"
	git -C "$WORK/repo" push -q -u origin main
}

run() { (cd "$WORK/repo" && bash deploy/deploy.sh deploy 2>&1); }
branch_now() { git -C "$WORK/repo" rev-parse --abbrev-ref HEAD; }

printf '\n1. a clean checkout on a branch is switched to main\n'
setup
git -C "$WORK/repo" switch -q -c feature/x
out=$(run); rc=$?
[ $rc -eq 0 ] && ok "exit 0" || bad "exit $rc" "$out"
[ "$(branch_now)" = main ] && ok "now on main" || bad "still on $(branch_now)" "$out"
grep -q "switching to main" <<<"$out" && ok "says it switched" || bad "no switch line" "$out"
grep -q "git switch feature/x" <<<"$out" && ok "names the way back" || bad "no way back" "$out"

printf '\n2. a dirty tree cancels, and does NOT switch\n'
setup
git -C "$WORK/repo" switch -q -c feature/y
echo changed >> "$WORK/repo/README"
out=$(run); rc=$?
[ $rc -ne 0 ] && ok "cancelled (exit $rc)" || bad "exit 0" "$out"
[ "$(branch_now)" = feature/y ] && ok "left on feature/y" || bad "switched to $(branch_now)" "$out"
grep -q "not in git" <<<"$out" && ok "says why" || bad "no reason" "$out"

printf '\n3. DEPLOY_ALLOW_BRANCH=1 stays on the branch\n'
setup
git -C "$WORK/repo" switch -q -c feature/z
out=$( (cd "$WORK/repo" && DEPLOY_ALLOW_BRANCH=1 bash deploy/deploy.sh deploy 2>&1) ); rc=$?
[ $rc -eq 0 ] && ok "exit 0" || bad "exit $rc" "$out"
[ "$(branch_now)" = feature/z ] && ok "still on feature/z" || bad "switched to $(branch_now)" "$out"

printf '\n4. on main and behind origin: fast-forwards\n'
setup
# Move origin/main ahead by committing in a second clone.
git clone -q -b main "$WORK/origin.git" "$WORK/other"
git -C "$WORK/other" config user.email t@example.test
git -C "$WORK/other" config user.name Test
echo more > "$WORK/other/NEW"
git -C "$WORK/other" add -A && git -C "$WORK/other" commit -qm "ahead"
git -C "$WORK/other" push -q origin main
out=$(run); rc=$?
[ $rc -eq 0 ] && ok "exit 0" || bad "exit $rc" "$out"
[ -f "$WORK/repo/NEW" ] && ok "fast-forwarded" || bad "did not move" "$out"

printf '\n5. a switch that changes deploy.sh hands over\n'
setup one
# main gets a different deploy.sh from the feature branch, so switching to
# main rewrites the running script.
git -C "$WORK/repo" switch -q -c feature/w
build_script "$WORK/repo/deploy/deploy.sh" two
git -C "$WORK/repo" commit -qam "a different deploy.sh on the branch"
out=$(run); rc=$?
[ $rc -eq 0 ] && ok "exit 0" || bad "exit $rc" "$out"
grep -q "handing over" <<<"$out" && ok "handed over" || bad "no hand-over" "$out"
[ "$(branch_now)" = main ] && ok "ended on main" || bad "on $(branch_now)" "$out"
[ "$(grep -c "; switching to main," <<<"$out")" -eq 1 ] && ok "switched once, did not loop" || bad "switched more than once" "$out"

printf '\n5b. the switch does not leak git chatter into the log\n'
setup
git -C "$WORK/repo" switch -q -c feature/quiet
out=$(run); rc=$?
grep -q "Your branch is up to date" <<<"$out" && bad "git chatter in the log" "$out" || ok "quiet"

printf '\n5c. a branch AND a behind main: two hand-overs in one run\n'
# The case that reached a real deploy and cancelled it. Three different
# deploy.sh versions: origin has v3, the local branch has v2, and local main is
# still v1 and behind. Switching to main replaces v2 with v1 -- one hand-over
# -- and the fast-forward then replaces v1 with v3, which is a second. A flag
# instead of a count stopped here with "changed again after re-running once",
# which was accurate and was not a loop.
setup one
git clone -q -b main "$WORK/origin.git" "$WORK/other3"
git -C "$WORK/other3" config user.email t@example.test
git -C "$WORK/other3" config user.name Test
build_script "$WORK/other3/deploy/deploy.sh" three
git -C "$WORK/other3" commit -qam "v3 on origin"
git -C "$WORK/other3" push -q origin main

git -C "$WORK/repo" switch -q -c feature/v2
build_script "$WORK/repo/deploy/deploy.sh" two
git -C "$WORK/repo" commit -qam "v2 on the branch"

out=$(run); rc=$?
[ $rc -eq 0 ] && ok "exit 0" || bad "exit $rc" "$out"
[ "$(branch_now)" = main ] && ok "ended on main" || bad "on $(branch_now)" "$out"
[ "$(grep -c 'handing over' <<<"$out")" -eq 2 ] && ok "handed over twice" \
	|| bad "handed over $(grep -c 'handing over' <<<"$out") time(s), want 2" "$out"
grep -q "will not stop changing" <<<"$out" && bad "hit the loop guard" "$out" || ok "did not hit the loop guard"
[ "$(git -C "$WORK/repo" rev-parse HEAD)" = "$(git -C "$WORK/repo" rev-parse origin/main)" ] \
	&& ok "ended at origin/main" || bad "not at origin/main" "$out"
grep -q "building from main @" <<<"$out" && ok "got as far as the build" || bad "never reached the build" "$out"

printf '\n5d. a third change does stop it\n'
# The bound still bounds. Pretending two hand-overs have already happened,
# a script that changes again must stop rather than loop.
setup one
git -C "$WORK/repo" switch -q -c feature/loop
build_script "$WORK/repo/deploy/deploy.sh" different
git -C "$WORK/repo" commit -qam "a different deploy.sh"
out=$( (cd "$WORK/repo" && DEPLOY_SELF_UPDATED=2 bash deploy/deploy.sh deploy 2>&1) ); rc=$?
[ $rc -ne 0 ] && ok "cancelled (exit $rc)" || bad "exit 0" "$out"
grep -q "will not stop changing" <<<"$out" && ok "says why" || bad "no reason" "$out"

printf '\n5e. a non-numeric counter from the environment does not crash bash\n'
setup one
git -C "$WORK/repo" switch -q -c feature/junk
build_script "$WORK/repo/deploy/deploy.sh" other
git -C "$WORK/repo" commit -qam "a different deploy.sh"
out=$( (cd "$WORK/repo" && DEPLOY_SELF_UPDATED=yes bash deploy/deploy.sh deploy 2>&1) ); rc=$?
grep -qi "integer expression\|unary operator" <<<"$out" && bad "bash error, not a sentence" "$out" || ok "no bash error"

printf '\n6. a diverged main cancels\n'
setup
git clone -q -b main "$WORK/origin.git" "$WORK/other2"
git -C "$WORK/other2" config user.email t@example.test
git -C "$WORK/other2" config user.name Test
echo theirs > "$WORK/other2/THEIRS"
git -C "$WORK/other2" add -A && git -C "$WORK/other2" commit -qm theirs
git -C "$WORK/other2" push -q origin main
echo mine > "$WORK/repo/MINE"
git -C "$WORK/repo" add -A && git -C "$WORK/repo" commit -qm mine
out=$(run); rc=$?
[ $rc -ne 0 ] && ok "cancelled (exit $rc)" || bad "exit 0" "$out"
grep -q "diverged" <<<"$out" && ok "says diverged" || bad "no reason" "$out"

printf '\n7. CI deploys the checked-out commit and never switches\n'
setup
git -C "$WORK/repo" switch -q -c release/1
out=$( (cd "$WORK/repo" && CI=1 bash deploy/deploy.sh deploy 2>&1) ); rc=$?
[ $rc -eq 0 ] && ok "exit 0" || bad "exit $rc" "$out"
[ "$(branch_now)" = release/1 ] && ok "left the checkout alone" || bad "switched to $(branch_now)" "$out"

printf '\n%d passed, %d failed\n\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
