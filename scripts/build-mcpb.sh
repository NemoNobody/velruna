#!/usr/bin/env bash
# Build the MCPB bundle: one zip that a desktop client (Claude Desktop and
# others speaking the MCPB format) can install without a package manager.
#
# Layout inside the bundle:
#   manifest.json            metadata, user_config and the launch command
#   server/velruna-launch.sh picks the platform/arch binary and execs it
#   server/velruna-<os>-<arch>  every release build
#
# One bundle carries all platforms because the MCPB manifest can express
# per-OS commands (Windows uses platform_overrides) but not per-architecture
# ones — the launcher fills that gap on darwin/linux.
#
# Usage: scripts/build-mcpb.sh <version-without-v> <dist-dir> <output.mcpb>
set -euo pipefail

version=${1:?usage: build-mcpb.sh <version> <dist-dir> <output.mcpb>}
dist=${2:?usage: build-mcpb.sh <version> <dist-dir> <output.mcpb>}
out=${3:?usage: build-mcpb.sh <version> <dist-dir> <output.mcpb>}

here=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if [ ! -f "$here/packaging/mcpb/manifest.json" ]; then
	echo "build-mcpb: run from the repository (missing packaging/mcpb/manifest.json)" >&2
	exit 2
fi

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/server"

# The manifest version is plain semver; the tag keeps the "v".
sed "s/__VERSION__/${version}/" "$here/packaging/mcpb/manifest.json" >"$stage/manifest.json"
install -m 0755 "$here/packaging/mcpb/velruna-launch.sh" "$stage/server/velruna-launch.sh"

count=0
for bin in "$dist"/velruna-*; do
	[ -f "$bin" ] || continue
	install -m 0755 "$bin" "$stage/server/$(basename "$bin")"
	count=$((count + 1))
done
if [ "$count" -eq 0 ]; then
	echo "build-mcpb: no velruna-* binaries in $dist (build them first)" >&2
	exit 2
fi

# zip runs from the staging dir, so the output path must be absolute first.
case "$out" in
	/*) ;;
	*) out="$(pwd)/$out" ;;
esac
rm -f "$out"
(cd "$stage" && zip -q -r -X "$out" manifest.json server)
echo "built $out with $count binaries (version $version)"
