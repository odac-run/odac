#!/usr/bin/env bash
# mTLS end-to-end test (MTLS_PLAN.md Faz 10): build, start, run the scenario
# suites, grep the logs for leaks, tear down. One command, re-runnable.
#
#   ./run.sh                 run every suite, then tear down
#   ./run.sh custom          run only the named suites
#   KEEP=1 ./run.sh          leave the stack up afterwards (debugging)
#   ./run.sh --down          tear down a stack left by KEEP=1 or a crash
#
# Agent source (pick one):
#   AGENT_SRC=/path/to/odac                       a source tree (go.mod, cmd/, internal/)
#   AGENT_REPO=/path/to/odac AGENT_REF=feat/mtls  a git ref, exported with
#                                                 `git archive` (the repo is not touched)
#   neither: the git checkout this script lives in, when it is the agent repo
# Reserved for the edge suites: TUNNEL_SRC / TUNNEL_REPO + TUNNEL_REF (odac.cc).
#
# Isolation contract: everything is compose project "mtls-e2e" on an internal
# network with no published ports, images are tagged mtls-e2e/*, and teardown
# removes exactly those (compose down -v + the mtls-e2e/* tags). The run ends by
# diffing `docker ps -a`, networks and volumes against the state it started in.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
export HERE
# shellcheck source=lib/common.sh
. "$HERE/lib/common.sh"
# shellcheck source=lib/leak.sh
. "$HERE/lib/leak.sh"
# shellcheck source=scenarios/custom.sh
. "$HERE/scenarios/custom.sh"

ALL_SUITES="custom"
IMAGES="mtls-e2e/node:local mtls-e2e/hub:local mtls-e2e/client:local mtls-e2e/origin:local"

WORK=${E2E_WORK:-$(mktemp -d "${TMPDIR:-/tmp}/mtls-e2e.XXXXXX")}
mkdir -p "$WORK"
chmod 700 "$WORK"
RUNLOG="$WORK/run.log"

snapshot() {
	{
		echo "## containers"
		docker ps -a --format '{{.ID}} {{.Names}} {{.Image}}' | sort
		echo "## networks"
		docker network ls --format '{{.ID}} {{.Name}}' | sort
		echo "## volumes"
		docker volume ls --format '{{.Name}}' | sort
	} >"$1"
}

teardown() {
	log "teardown: compose down -v (project $PROJECT) + mtls-e2e/* images"
	AGENT_SRC=${AGENT_SRC:-/nonexistent} dc --profile multi-edge down -v --remove-orphans --timeout 10 >/dev/null 2>&1 || true
	local img
	for img in $IMAGES; do
		docker image inspect "$img" >/dev/null 2>&1 && docker image rm "$img" >/dev/null 2>&1 || true
	done
}

compare_snapshots() {
	snapshot "$WORK/docker-after.txt"
	if diff -u "$WORK/docker-before.txt" "$WORK/docker-after.txt" >"$WORK/docker-diff.txt"; then
		log "docker state after teardown is identical to before the run (containers, networks, volumes)"
	else
		log "WARNING: docker state differs from before the run:"
		sed -n '3,$p' "$WORK/docker-diff.txt"
	fi
}

# isolate_docker_config points this run's docker CLI at a private, credential-
# less config. A developer's credential helper (Docker Desktop: the macOS
# keychain) blocks forever once the screen is locked, which hung an unattended
# build at "load metadata"; every image this harness pulls is public, so it
# needs no credentials. The user's own config is only read (endpoint, plugin
# dirs), never written. E2E_USER_DOCKER_CONFIG=1 keeps the user's config.
isolate_docker_config() {
	[ "${E2E_USER_DOCKER_CONFIG:-0}" = 1 ] && return 0
	local host user_cfg=${DOCKER_CONFIG:-$HOME/.docker} dir="$WORK/docker-config"
	host=${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)}
	[ -n "$host" ] || die "could not resolve the docker endpoint"
	mkdir -p "$dir"
	printf '{"cliPluginsExtraDirs":["%s","/Applications/Docker.app/Contents/Resources/cli-plugins","/usr/local/lib/docker/cli-plugins"]}\n' \
		"$user_cfg/cli-plugins" >"$dir/config.json"
	export DOCKER_CONFIG="$dir" DOCKER_HOST="$host"
	docker compose version >/dev/null 2>&1 || die "docker compose not found with the private config (E2E_USER_DOCKER_CONFIG=1 to use yours)"
}

resolve_agent_src() {
	if [ -n "${AGENT_SRC:-}" ]; then
		:
	elif [ -n "${AGENT_REF:-}" ]; then
		local repo=${AGENT_REPO:-$(git -C "$HERE" rev-parse --show-toplevel 2>/dev/null || true)}
		[ -n "$repo" ] || die "AGENT_REF needs AGENT_REPO (or run from inside the agent repo)"
		AGENT_SRC="$WORK/src/agent"
		mkdir -p "$AGENT_SRC"
		git -C "$repo" archive "$AGENT_REF" | tar -x -C "$AGENT_SRC"
		AGENT_DESC="$(git -C "$repo" rev-parse --short "$AGENT_REF") ($AGENT_REF via git archive)"
	else
		AGENT_SRC=$(git -C "$HERE" rev-parse --show-toplevel 2>/dev/null || true)
	fi
	[ -n "${AGENT_SRC:-}" ] && [ -f "$AGENT_SRC/go.mod" ] && [ -d "$AGENT_SRC/cmd/odac-server" ] ||
		die "no agent source: set AGENT_SRC, or AGENT_REPO + AGENT_REF (see the header of $0)"
	AGENT_SRC=$(cd "$AGENT_SRC" && pwd)
	AGENT_DESC=${AGENT_DESC:-$(git -C "$AGENT_SRC" describe --always --dirty 2>/dev/null || echo "tree at $AGENT_SRC")}
	export AGENT_SRC
}

main() {
	local suites=${*:-$ALL_SUITES} s
	for s in $suites; do
		declare -F "suite_$s" >/dev/null || die "unknown suite: $s (known: $ALL_SUITES)"
	done
	command -v docker >/dev/null || die "docker not found"
	docker compose version >/dev/null 2>&1 || die "docker compose plugin not found"

	isolate_docker_config
	resolve_agent_src
	log "work dir: $WORK"
	log "agent: $AGENT_DESC"

	# Leftovers of an earlier crashed run of THIS project only.
	if [ -n "$(dc --profile multi-edge ps -a -q 2>/dev/null)" ]; then
		log "found containers of project $PROJECT from an earlier run, removing them"
		teardown
	fi
	snapshot "$WORK/docker-before.txt"

	LOGS_DONE=0
	[ "${KEEP:-0}" = 1 ] || trap '[ "$LOGS_DONE" = 1 ] || collect_logs "$WORK/logs"; teardown; compare_snapshots' EXIT

	log "build: images (host engine; the only phase with network access)"
	if ! { dc --progress plain build hub edge-a client &&
		docker build --progress plain -t mtls-e2e/origin:local "$HERE/images/origin"; } >"$WORK/build.log" 2>&1; then
		tail -n 30 "$WORK/build.log"
		die "image build failed (full log: $WORK/build.log)"
	fi

	log "up: hub edge-a client"
	dc up -d --quiet-pull hub edge-a client >/dev/null 2>&1
	wait_healthy edge-a 240
	info "edge-a healthy at $(node_ip edge-a)"
	info "client curl: $(client_sh 'curl -V | head -1 | cut -d" " -f1-2'), wss: $(client_sh 'curl -V | grep -q wss && echo yes || echo no')"

	log "load: origin image into edge-a's engine"
	docker save mtls-e2e/origin:local | dc exec -T edge-a docker load >/dev/null

	for s in $suites; do
		"suite_$s"
	done

	log "--- logs ---"
	collect_logs "$WORK/logs"
	LOGS_DONE=1
	leak_check "$WORK/logs" "$RUNLOG"

	summary
	if [ "${KEEP:-0}" = 1 ]; then
		log "KEEP=1: stack left running. Remove it with: $0 --down"
	fi
	# An explicit exit: bash 3.2 (macOS) skips the EXIT trap of a pipeline
	# subshell that just runs off its end, which left the stack up after a pass.
	if [ "$FAILED" = 0 ]; then exit 0; fi
	exit 1
}

if [ "${1:-}" = "--down" ]; then
	AGENT_SRC=${AGENT_SRC:-/nonexistent}
	export AGENT_SRC
	isolate_docker_config
	teardown
	exit 0
fi

main "$@" 2>&1 | tee "$RUNLOG"
exit "${PIPESTATUS[0]}"
