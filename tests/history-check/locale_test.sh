#!/bin/sh
# Regression test for the LC_ALL=C on `cut` in the Makefile target history-check.
#
# Why: BSD cut (macOS) under a UTF-8 locale prints "cut: stdin: Illegal byte sequence" for a line
# with an invalid byte, writes nothing and the pipeline carries on, so an author or committer email
# with an invalid byte was listed by grep but dropped by cut and history-check printed "ok"
# (fail open). Under LC_ALL=C cut works on bytes and keeps the hash.
#
# This script needs no GNU grep: it runs on macOS developer machines (where it proves the fix) and
# in CI on glibc (where cut was never affected, so it is a plain regression check there). It is the
# counterpart of gnu_grep_test.sh, which can only run with GNU grep.
#
# sed and sort in the same pipeline only ever see hex hashes (cut has already removed the email), so
# their LC_ALL=C is not locale-sensitive and no test can tell it apart: it is kept for consistency.
#
# What: for C and every UTF-8 locale installed (C.UTF-8, en_US.UTF-8), assert that a commit whose
# author email, or committer email, holds an invalid byte (not a noreply form) makes history-check
# exit non-zero printing only the hash, and that a clean history passes.
#
# Usage: sh tests/history-check/locale_test.sh   (or: make test-history-check)
set -u
# Repo-selecting git variables from the caller must not leak into the throwaway repos: with GIT_DIR
# set, `git update-ref` below would move the caller's HEAD. Unset them all, then always name the
# repository explicitly (cd into it or git -C).
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES \
  GIT_COMMON_DIR GIT_NAMESPACE GIT_CEILING_DIRECTORIES GIT_PREFIX GIT_CONFIG GIT_CONFIG_COUNT \
  GIT_CONFIG_PARAMETERS GIT_SHALLOW_FILE GIT_GRAFT_FILE GIT_REPLACE_REF_BASE GIT_NO_REPLACE_OBJECTS \
  GIT_INDEX_VERSION GIT_DISCOVERY_ACROSS_FILESYSTEM GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_AUTHOR_DATE \
  GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL GIT_COMMITTER_DATE

# HC_INNER is only valid when the parent script set it (see the token below).
if [ -n "${HC_INNER:-}" ] && ! { case $HC_INNER in /?*) [ -f "$HC_INNER/inner-token" ];; *) false;; esac; }; then
  echo "FAIL: HC_INNER is reserved for the script's own self-checks"; exit 1
fi

HERE=$(cd "$(dirname "$0")" && pwd) || exit 2
ROOT=$(cd "$HERE/../.." && pwd) || exit 2
[ -f "$ROOT/Makefile" ] || { echo "FAIL: no Makefile in $ROOT"; exit 2; }
MK=${MAKEFILE:-$ROOT/Makefile}
T=$(mktemp -d) || exit 2
case $T in /?*) ;; *) echo "FAIL: mktemp did not return an absolute path: $T"; exit 2;; esac
# Separate traps: a trap on "EXIT INT TERM" that does not exit would run rm and then carry on, so an
# interrupted run printed PASS and exited 0. An interrupted run must never print PASS: it exits 130
# (INT), 143 (TERM) or 129 (HUP), and cleanup is idempotent.
# shellcheck disable=SC2329  # invoked through trap
cleanup() { if [ -n "$T" ]; then rm -rf "$T"; T=; fi; }
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
trap 'cleanup; exit 129' HUP
# HC_INNER marks the script's own isolation self-checks (the parent re-runs itself). Its value is the
# parent's temp dir and the parent creates $T/inner-token in it: an HC_INNER set by anyone else (a
# stray export, CI config) has no such file and is refused, never silently skipping the self-checks.
: > "$T/inner-token" || exit 2
SELF=$HERE/${0##*/}

# A UTF-8 locale is the whole point of the proof: not having one is a failure, not a pass.
ALL=$(locale -a 2>/dev/null) || { echo "FAIL: no UTF-8 locale installed (locale -a failed)"; exit 1; }
LOCALES=C; UTF=
for l in $ALL; do
  case $l in C.UTF-8|C.utf8|en_US.UTF-8|en_US.utf8) LOCALES="$LOCALES $l"; UTF=1;; esac
done
[ -n "$UTF" ] || { echo "FAIL: no UTF-8 locale installed"; exit 1; }

BAD=$(printf '\377')                  # never valid UTF-8
GOOD=12345+someone@users.noreply.github.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

mk() { # name author-email committer-email
  # The second commit is written with hash-object: git commit re-encodes invalid bytes in the
  # ident to valid UTF-8, but commits made by other tools can carry them raw.
  d=$T/$1; git init -q "$d" || exit 2
  ( cd "$d" || exit 2
    echo a > f; git add f
    GIT_AUTHOR_NAME=A GIT_AUTHOR_EMAIL=$GOOD GIT_COMMITTER_NAME=A GIT_COMMITTER_EMAIL=$GOOD \
      git -c commit.gpgsign=false commit -q -m base || exit 2
    parent=$(git rev-parse HEAD) || exit 2
    echo b > f; git add f; tree=$(git write-tree) || exit 2
    printf 'tree %s\nparent %s\nauthor A <%s> 1700000000 +0000\ncommitter A <%s> 1700000000 +0000\n\nsubj\n' \
      "$tree" "$parent" "$2" "$3" > "$T/$1.obj"
    c=$(git hash-object -t commit -w --literally "$T/$1.obj") || exit 2
    git update-ref HEAD "$c" ) || exit 2
}
mk clean "$GOOD" "$GOOD"
mk author "${BAD}x@example.invalid" "$GOOD"
mk committer "$GOOD" "${BAD}x@example.invalid"

fail=0; n=0; loc=C
ok() { n=$((n+1)); echo "ok   [$loc] $1"; }
no() { n=$((n+1)); echo "FAIL [$loc] $1 (rc=$rc out=[$out])"; fail=1; }

hist() { # fixture -> rc, out
  out=$(LC_ALL=$loc make -s -f "$MK" -C "$ROOT" history-check GIT="git --git-dir=$T/$1/.git" REV=HEAD 2>&1); rc=$?
  out=$(printf '%s\n' "$out" | sed '/^make.*Error/d')
}
expect_pass() {
  hist "$1"
  if [ $rc -eq 0 ] && [ "$out" = "history-check: ok" ]; then ok "$1 passes"; else no "$1 should pass"; fi
}
expect_flagged() {
  hist "$1"
  h=$(git -C "$T/$1" log -1 --format=%h)
  last=$(printf '%s\n' "$out" | tail -n 1)
  case $out in *@*|*example*|*Illegal*) leak=1;; *) leak=0;; esac
  if [ $rc -ne 0 ] && [ "$last" = "$h" ] && [ $leak -eq 0 ] && [ "$(printf '%s\n' "$out" | wc -l | tr -d ' ')" -eq 2 ]
  then ok "$1 flagged, only the hash printed"; else no "$1 should be flagged"; fi
}

for loc in $LOCALES; do
  expect_pass clean
  expect_flagged author
  expect_flagged committer
done

# --- self-checks of the isolation, run once (inner runs get HC_INNER=<parent temp dir>) ---------
if [ -z "${HC_INNER:-}" ]; then
  loc=-
  # a caller's GIT_DIR must stay untouched: HEAD and index identical afterwards
  dd=$T/decoy; git init -q "$dd" || exit 2
  echo d > "$dd/f"; git -C "$dd" add f
  GIT_AUTHOR_NAME=A GIT_AUTHOR_EMAIL=$GOOD GIT_COMMITTER_NAME=A GIT_COMMITTER_EMAIL=$GOOD \
    git -C "$dd" -c commit.gpgsign=false commit -q -m decoy || exit 2
  s0=$(git -C "$dd" rev-parse HEAD; git -C "$dd" ls-files -s)
  HC_INNER=$T GIT_DIR=$dd/.git GIT_WORK_TREE=$dd GIT_INDEX_FILE=$dd/.git/index sh "$SELF" >/dev/null 2>&1
  irc=$?
  s1=$(git -C "$dd" rev-parse HEAD; git -C "$dd" ls-files -s)
  rc=$irc; out=-
  if [ "$s0" = "$s1" ] && [ "$irc" -eq 0 ]; then ok "GIT_DIR in the caller's environment: script passes, HEAD and index untouched"; else no "GIT_DIR leaked into the script"; fi
  # a locale binary that fails must make the script fail, not pass without the UTF-8 proof
  mkdir "$T/shadow" && printf '#!/bin/sh\nexit 127\n' > "$T/shadow/locale" && chmod +x "$T/shadow/locale" || exit 2
  out=$(HC_INNER=$T PATH=$T/shadow:$PATH sh "$SELF" 2>&1); rc=$?
  if [ $rc -eq 1 ]; then ok "no usable locale -a is a failure"; else no "no usable locale -a must fail"; fi
  # HC_INNER set from outside (no token) must be refused, not skip the self-checks
  out=$(HC_INNER=1 sh "$SELF" 2>&1); rc=$?
  case $out in *"HC_INNER is reserved"*) m=1;; *) m=0;; esac
  if [ $rc -eq 1 ] && [ $m -eq 1 ]; then ok "HC_INNER from the environment is refused"; else no "HC_INNER from outside must fail"; fi
fi

echo "$n checks, locales: $LOCALES"
[ $fail -eq 0 ] && echo "PASS" || echo "FAILED"
exit $fail
