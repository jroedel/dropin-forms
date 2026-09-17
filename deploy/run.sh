#!/usr/bin/env bash
# Run the app in the foreground. supervise.sh is what backgrounds it.
#
# The pid is written here, before exec, and that is not a stylistic choice.
# supervise.sh launches this through `setsid nohup`, which forks -- so the pid
# the shell would report as $! is the wrapper, not the process that survives.
# In the sibling project that was observed directly: $! was 9979 while the
# process still running afterwards was 9980. Writing $$ from inside, then
# exec'ing so the pid is kept across the replacement, is the only version that
# records the right number.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

printf '%s\n' "$$" >dropin-forms.pid

exec ./dropin-forms -config config.toml
