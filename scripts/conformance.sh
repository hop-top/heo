#!/usr/bin/env bash
# Run the 12-factor AI-CLI conformance gate the way hop-top/spec-12fc does
# (its scripts/run.sh), and leave the evidence in docs/conformance/.
#
#   verify-no-leak   repo-wide scan for scenario rubrics (F10)
#   verify-stories   e2e/stories (F1, F2, F9, F12)
#   grade            the recorded cassette, against a local grading service
#                    (F3-F8, F11); needs the private scenarios, see below
#
# Environment:
#   KIT             kit binary (default: kit on PATH); build it from the
#                   same source go.mod resolves hop.top/kit to
#   SPEC_12FC_DIR   checkout of hop-top/spec-12fc (default: a shallow clone
#                   of SPEC_12FC_REF into bin/conformance/spec-12fc)
#   HEO_SCENARIOS   root of the private scenario library, holding
#                   scenarios/<ns>/<id>/<version>/scenario.yaml. Scenarios
#                   are rubrics and never live in this repository
#                   (verify-no-leak fails on them). Unset: grade is skipped,
#                   its factors report skip, and the badge says so.
#   HEO_SCENARIO    scenario to record and grade (default
#                   heo/local-install-walkthrough@1.0.0)
#
# Nothing is pushed or uploaded: the grading service listens on loopback
# for the length of the run.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$PWD

KIT=${KIT:-kit}
SPEC_12FC_REF=${SPEC_12FC_REF:-fbd05973d8c0ec1acd9b20994d3db9bbfc3ac141}
HEO_SCENARIO=${HEO_SCENARIO:-heo/local-install-walkthrough@1.0.0}
WORK=$ROOT/bin/conformance
EVIDENCE=$ROOT/docs/conformance
CASSETTE=$ROOT/tests/conformance/cassette
# The recorded steps run in this tree, so the paths a command prints
# (config files) are the same on every recording.
SANDBOX=/tmp/heo-12fc

die() {
  echo "conformance: $*" >&2
  exit 1
}

command -v "$KIT" >/dev/null 2>&1 || die "kit not found (set KIT)"
command -v jq >/dev/null 2>&1 || die "jq is required"
command -v python3 >/dev/null 2>&1 || die "python3 is required (spec-12fc's run.sh uses it)"

mkdir -p "$WORK" "$EVIDENCE"
SVC_PID=""
# shellcheck disable=SC2317 # reached through the EXIT trap
cleanup() { [ -z "$SVC_PID" ] || kill "$SVC_PID" 2>/dev/null || true; }
trap cleanup EXIT

if [ -z "${SPEC_12FC_DIR:-}" ]; then
  SPEC_12FC_DIR=$WORK/spec-12fc
  if [ ! -d "$SPEC_12FC_DIR/.git" ]; then
    git clone --quiet https://github.com/hop-top/spec-12fc.git "$SPEC_12FC_DIR"
  fi
  git -C "$SPEC_12FC_DIR" checkout --quiet --detach "$SPEC_12FC_REF"
fi
[ -x "$SPEC_12FC_DIR/scripts/run.sh" ] || die "$SPEC_12FC_DIR/scripts/run.sh not found"

echo "== build heo"
go build -buildvcs=false -o bin/heo .

# verify-stories scans every YAML file below its path as a story, so the
# gate gets the stories directory, not the repo root. The leak scan is
# the repo-wide one.
echo "== verify-no-leak (repo)"
"$KIT" conformance verify-no-leak --paths=. --format=json >"$WORK/verify-no-leak-repo.json"
jq -e '.findings | length == 0' "$WORK/verify-no-leak-repo.json" >/dev/null ||
  die "scenario-shaped content in the repository: $WORK/verify-no-leak-repo.json"

GRADE_ENV=()
if [ -n "${HEO_SCENARIOS:-}" ]; then
  scenario_file=$HEO_SCENARIOS/scenarios/${HEO_SCENARIO%@*}/${HEO_SCENARIO#*@}/scenario.yaml
  [ -f "$scenario_file" ] || die "scenario not found: $scenario_file"
  story=$(sed -n 's/^ *story_path: *//p' "$scenario_file" | head -n 1)
  [ -f "$ROOT/$story" ] || die "story $story named by the scenario is not in this repository"

  echo "== record $HEO_SCENARIO"
  case $SANDBOX in /tmp/heo-12fc) rm -rf "$SANDBOX" ;; *) die "refusing to clear $SANDBOX" ;; esac
  mkdir -p "$SANDBOX/home" "$SANDBOX/cwd"
  rm -rf "$CASSETTE"
  mkdir -p "$(dirname "$CASSETTE")"
  # A clean environment: nothing of the caller's HOME, XDG tree or
  # HEO_* settings reaches the recorded commands.
  env -i PATH="$(dirname "$(command -v "$KIT")"):/usr/bin:/bin" \
    HOME="$SANDBOX/home" XDG_CONFIG_HOME="$SANDBOX/cfg" XDG_DATA_HOME="$SANDBOX/data" \
    XDG_STATE_HOME="$SANDBOX/state" XDG_CACHE_HOME="$SANDBOX/cache" \
    XDG_RUNTIME_DIR="$SANDBOX/run" APS_DATA_PATH="$SANDBOX/aps" TMPDIR=/tmp \
    "$KIT" conformance harness record --scenario "$scenario_file" --binary "$ROOT/bin/heo" \
    --story "$ROOT/$story" --out "$CASSETTE" --workdir "$SANDBOX/cwd" \
    --scenario-ref "$HEO_SCENARIO" --binary-version "$(git rev-parse --short HEAD)" \
    --format json >"$WORK/record.json"
  rm -rf "$SANDBOX"

  echo "== grading service (loopback)"
  rm -f "$WORK/claims.db"
  token=$("$KIT" conformance svc token mint --claims-db "$WORK/claims.db" --scope grade:heo \
    --tenant heo --tier-max 2 --description "local conformance run" --format json |
    grep -o 'kit-conf-[A-Za-z0-9_-]*' | head -n 1)
  [ -n "$token" ] || die "no token minted"
  "$KIT" conformance svc serve --claims-db "$WORK/claims.db" --scenarios-root "$HEO_SCENARIOS" \
    --addr 127.0.0.1 --port 0 >"$WORK/svc.log" 2>&1 &
  SVC_PID=$!
  port=""
  for _ in $(seq 1 50); do
    port=$(jq -r '.port // empty' "$WORK/svc.log" 2>/dev/null | head -n 1 || true)
    [ -n "$port" ] && break
    sleep 0.2
  done
  [ -n "$port" ] || die "grading service did not start: $WORK/svc.log"
  GRADE_ENV=(INPUT_CASSETTE_DIR="$CASSETTE" INPUT_GRADE_SERVICE="http://127.0.0.1:$port"
    INPUT_GRADE_TOKEN="$token" INPUT_GRADE_TIER=2)
else
  echo "== grade skipped: HEO_SCENARIOS is not set"
fi

echo "== spec-12fc run.sh"
set +e
env PATH="$(dirname "$(command -v "$KIT")"):$PATH" \
  INPUT_COMMAND=all INPUT_PATHS=e2e/stories INPUT_DIFF="" \
  INPUT_REPORT_PATH="$EVIDENCE/12fc-report.json" INPUT_MATRIX_PATH="$EVIDENCE/12fc-matrix.json" \
  INPUT_SARIF_PATH="$EVIDENCE/12fc.sarif" INPUT_BADGE_PATH="$ROOT/.12fc.json" \
  GITHUB_SHA="$(git rev-parse HEAD)" GITHUB_OUTPUT="$WORK/github-output" \
  GITHUB_STEP_SUMMARY="$WORK/step-summary.md" ${GRADE_ENV[@]+"${GRADE_ENV[@]}"} \
  "$SPEC_12FC_DIR/scripts/run.sh" >"$WORK/run.log" 2>&1
rc=$?
set -e
tail -n 20 "$WORK/run.log"
echo "== run.sh exit $rc; verdict: $(sed -n 's/^verdict=//p' "$WORK/github-output" | tail -n 1)"
jq -r '.factors[] | "F\(.n) \(.name): \(.status)"' "$EVIDENCE/12fc-matrix.json"
exit "$rc"
