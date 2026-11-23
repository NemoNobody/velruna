#!/usr/bin/env bash
# CLI smoke over one live velruna store. Exit 0 = all checks
# passed. Runs against VELRUNA_CONF (or the default config lookup), exercising
# the paths whose OpenSearch calls changed across versions: index creation,
# composable templates + ingest pipeline (journal), scripted corrections,
# optimistic concurrency, search, prune scripts.
#
# Usage: VELRUNA_CONF=<conf> [VELRUNA_BIN=<path>] hack/cli-smoke.sh
# The run writes throwaway facts under a timestamped "smoke:<runid>:" prefix,
# so it is safe on any store you are willing to pollute a few KB of.
set -uo pipefail
BIN=${VELRUNA_BIN:-velruna}
K="smoke:$(date +%s%N)"
FAILS=0
PASSES=0

step() { # step <desc> <cmd...> — pass = exit 0
  local desc=$1; shift
  if LAST=$("$@" 2>&1); then
    echo "  PASS  $desc"; PASSES=$((PASSES+1))
  else
    echo "  FAIL  $desc (exit $?)"; echo "$LAST" | sed 's/^/        /'; FAILS=$((FAILS+1))
  fi
}
neg() { # neg <desc> <cmd...> — pass = non-zero exit (rejection expected)
  local desc=$1; shift
  if LAST=$("$@" 2>&1); then
    echo "  FAIL  $desc (expected a rejection, got success)"; FAILS=$((FAILS+1))
  else
    echo "  PASS  $desc (rejected as expected)"; PASSES=$((PASSES+1))
  fi
}
check() { # check <desc> <pattern> — pattern must appear in the last output
  local desc=$1 pat=$2
  if printf '%s' "$LAST" | grep -qi -- "$pat"; then
    echo "  PASS  $desc"; PASSES=$((PASSES+1))
  else
    echo "  FAIL  $desc (no match for: $pat)"; echo "$LAST" | sed 's/^/        /'; FAILS=$((FAILS+1))
  fi
}

step "status"                  $BIN status
step "add (config)"            $BIN add "$K:one" "widget alpha value" config --project smoke --source tool:smoke
step "add (behavior)"          $BIN add "$K:two" "widget beta value" behavior --project smoke
step "get"                     $BIN get "$K:one";                    check "get returns the value" "widget alpha value"
step "correction (re-add)"     $BIN add "$K:one" "widget alpha value updated v2" config --project smoke
step "journal has writes"      $BIN journal "$K:one";                check "journal op=write" "write"
step "history-info"            $BIN history-info "$K:one"
step "list"                    $BIN list smoke;                      check "list shows :two" ":two"
step "keys"                    $BIN keys "$K:";                      check "keys finds :one" ":one"
step "search"                  $BIN search "widget";                 check "search hits :one" ":one"
step "obsolete"                $BIN obsolete "$K:two" "smoke cleanup"
neg  "secret guard rejects"    $BIN add "$K:sec" 'aws access key AKIA1234567890ABCDEF leaked' config --project smoke
step "secret guard redacts"    $BIN add "$K:hex" 'deployment id 0123456789abcdef0123456789abcdef0123456789abcdef' config --project smoke
check "the write is stored" "added fact"
step "redacted value stored"   $BIN get "$K:hex";                  check "marker in the value" "velruna-redacted:long-hex"
check "audit on the fact" "redacted: value=long-hex"
step "prune dry run"           $BIN prune "$K:one" --keep 1;         check "dry run prints a plan" "plan"
step "prune --yes"             $BIN prune "$K:one" --keep 1 --yes
step "journal after prune"     $BIN journal "$K:one"
step "migrate-history"         $BIN migrate-history
step "docs search (empty ok)"  $BIN docs "definitely-nosuch-term-xyz"

WS="smoke$(date +%s)$$"
step "workset create"        $BIN workset create "$WS" --project smoke
step "workset list"          $BIN workset list --project smoke;     check "list shows the workset" "$WS"
step "workset rename"        $BIN workset rename "$WS" "${WS}x"
step "workset old slug"      $BIN workset show "$WS";             check "alias still resolves" "${WS}x"
neg  "workset delete needs --yes" $BIN workset delete "${WS}x"
step "workset delete"        $BIN workset delete "${WS}x" --yes
step "exec (capture)"        $BIN exec --force -- echo smoke exec output;  check "exec reports changed" "status: changed"
step "exec (dedupe)"         $BIN exec -- echo smoke exec output;  check "exec reports unchanged" "status: unchanged"

echo "smoke: $PASSES passed, $FAILS failed (run prefix $K)"
[ "$FAILS" -eq 0 ]
