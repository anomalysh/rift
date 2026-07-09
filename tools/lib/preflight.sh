#!/usr/bin/env bash
# Preflight guards: refuse to start an expensive operation the machine cannot
# finish.
#
# Compiling Caddy, cross-building seven CLI binaries, or bringing up the e2e
# stack all consume gigabytes. When they run out of disk or memory they do not
# fail cleanly: the Go linker is OOM-killed, or Docker leaves a half-written
# layer and a corrupt build cache. The error you eventually see describes the
# symptom, never the cause. These checks turn that into one sentence, before
# anything is written.
#
# Source this after tools/lib/common.sh; it uses log_info/log_warn/die.
#
# Every threshold is a caller-supplied argument, not a constant here: only the
# caller knows what its own build costs.

# Guard against double-sourcing, since several scripts source both libs.
if [ -n "${RIFT_PREFLIGHT_SOURCED:-}" ]; then
	return 0 2>/dev/null || true
fi
RIFT_PREFLIGHT_SOURCED=1

# RIFT_SKIP_PREFLIGHT=1 bypasses every check. Deliberately loud: a CI runner
# that lies about its resources is a real situation, but a silent bypass would
# turn these guards into decoration.
rift_preflight_skipped() {
	if [ "${RIFT_SKIP_PREFLIGHT:-0}" = "1" ]; then
		log_warn "RIFT_SKIP_PREFLIGHT=1: resource guards bypassed"
		return 0
	fi
	return 1
}

# ---------------------------------------------------------------- disk

# preflight_disk_free_mb [PATH] -> MiB free on the filesystem holding PATH.
preflight_disk_free_mb() {
	local path="${1:-.}"
	# Resolve to an existing ancestor: df on a not-yet-created directory fails.
	while [ ! -e "$path" ] && [ "$path" != "/" ]; do
		path="$(dirname "$path")"
	done
	df -Pm "$path" 2>/dev/null | awk 'NR==2 {print $4}'
}

# require_disk_free MB [PATH] [WHAT]
require_disk_free() {
	local need="$1" path="${2:-.}" what="${3:-this operation}"
	rift_preflight_skipped && return 0

	local have
	have="$(preflight_disk_free_mb "$path")"
	if [ -z "$have" ]; then
		log_warn "could not determine free space on $path; continuing"
		return 0
	fi
	if [ "$have" -lt "$need" ]; then
		die "not enough disk for $what: $(rift_mib "$have") free on $path, need $(rift_mib "$need"). Free some space, or set RIFT_SKIP_PREFLIGHT=1 to override."
	fi
	log_info "disk ok: $(rift_mib "$have") free on $path (need $(rift_mib "$need"))"
}

# Docker writes layers under its data-root, which is frequently a different
# filesystem from the repo. Checking the repo's free space would prove nothing
# about whether an image build can finish.
preflight_docker_root() {
	docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker
}

# require_docker_disk MB [WHAT]
require_docker_disk() {
	local need="$1" what="${2:-this image build}"
	rift_preflight_skipped && return 0
	require_disk_free "$need" "$(preflight_docker_root)" "$what"
}

# ---------------------------------------------------------------- memory

# preflight_mem_available_mb -> MiB the kernel believes is available now.
#
# MemAvailable, not MemFree: page cache is reclaimable, and MemFree on a busy
# host reads near zero while gigabytes are in fact available.
preflight_mem_available_mb() {
	if [ -r /proc/meminfo ]; then
		awk '/^MemAvailable:/ {printf "%d", $2/1024; found=1} END {if (!found) exit 1}' /proc/meminfo && return 0
	fi
	# macOS and anything else: fall back to physical memory, which overstates
	# what is available but is better than refusing to run at all.
	if command -v sysctl >/dev/null 2>&1; then
		local bytes
		bytes="$(sysctl -n hw.memsize 2>/dev/null || true)"
		[ -n "$bytes" ] && echo $((bytes / 1024 / 1024)) && return 0
	fi
	return 1
}

# require_memory MB [WHAT]
require_memory() {
	local need="$1" what="${2:-this operation}"
	rift_preflight_skipped && return 0

	local have
	if ! have="$(preflight_mem_available_mb)"; then
		log_warn "could not determine available memory; continuing"
		return 0
	fi

	if [ "$have" -lt "$need" ]; then
		local swap=""
		if [ -r /proc/meminfo ]; then
			swap="$(awk '/^SwapFree:/ {printf "%d", $2/1024}' /proc/meminfo)"
		fi
		if [ -n "$swap" ] && [ "$swap" -gt 0 ] && [ $((have + swap)) -ge "$need" ]; then
			# Swap will keep the linker alive, slowly. Warn rather than refuse:
			# the operation will finish, and the operator should know why it
			# crawled.
			log_warn "low memory for $what: $(rift_mib "$have") available, need $(rift_mib "$need"); $(rift_mib "$swap") swap will be used and this will be slow"
			return 0
		fi
		die "not enough memory for $what: $(rift_mib "$have") available, need $(rift_mib "$need"). The linker is usually the first thing the OOM killer takes. Set RIFT_SKIP_PREFLIGHT=1 to override."
	fi
	log_info "memory ok: $(rift_mib "$have") available (need $(rift_mib "$need"))"
}

# ---------------------------------------------------------------- cpu

preflight_cpus() {
	if command -v nproc >/dev/null 2>&1; then
		nproc
	elif command -v sysctl >/dev/null 2>&1; then
		sysctl -n hw.ncpu 2>/dev/null || echo 1
	else
		echo 1
	fi
}

# require_cpus N [WHAT] -- advisory only: a slow build is not a failed build.
require_cpus() {
	local need="$1" what="${2:-this operation}"
	rift_preflight_skipped && return 0

	local have
	have="$(preflight_cpus)"
	if [ "$have" -lt "$need" ]; then
		log_warn "only $have CPU(s) for $what; expected $need or more, this will be slow"
		return 0
	fi
	log_info "cpu ok: $have core(s)"
}

# ---------------------------------------------------------------- files

# require_file_smaller_than PATH MB [WHAT]
#
# Guards against shipping or loading something absurd: a docker image tarball
# that will not fit on the remote host, an artifact that ballooned because a
# build accidentally embedded a cache.
require_file_smaller_than() {
	local path="$1" limit="$2" what="${3:-$1}"
	rift_preflight_skipped && return 0
	[ -e "$path" ] || die "$what does not exist: $path"

	local size
	size="$(du -Pm "$path" 2>/dev/null | awk 'NR==1 {print $1}')"
	[ -n "$size" ] || return 0
	if [ "$size" -gt "$limit" ]; then
		die "$what is $(rift_mib "$size"), over the $(rift_mib "$limit") limit: $path"
	fi
	log_info "size ok: $what is $(rift_mib "$size") (limit $(rift_mib "$limit"))"
}

# require_docker -- docker exists AND the daemon answers. `command -v docker`
# alone passes on a machine whose daemon is dead, and the real error then
# arrives several hundred lines into a build.
require_docker() {
	command -v docker >/dev/null 2>&1 || die "docker is required but not installed"
	docker info >/dev/null 2>&1 || die "the docker daemon is not reachable (is it running, and are you in the docker group?)"
}

# ---------------------------------------------------------------- reporting

rift_mib() {
	local mb="$1"
	if [ "$mb" -ge 1024 ]; then
		awk -v m="$mb" 'BEGIN { printf "%.1f GiB", m/1024 }'
	else
		printf '%d MiB' "$mb"
	fi
}

# preflight_report -- one line describing the machine, for a build log.
preflight_report() {
	local path="${1:-.}"
	local mem
	mem="$(preflight_mem_available_mb 2>/dev/null || echo '?')"
	log_info "host: $(preflight_cpus) cpu, ${mem} MiB mem available, $(preflight_disk_free_mb "$path") MiB free on $path"
}
