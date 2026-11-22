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
# Usage: scripts/build-mcpb.sh <version-without-v> <dist-dir> <output.mcpb> [platform]
#
# The optional platform (darwin | linux | windows) narrows the bundle to that
# OS's builds and declares only that platform in the manifest: the same server,
# a third of the download. The universal bundle (no platform) is what the MCP
# registry points at, because a registry package entry has no platform field —
# clients could not choose between several artifacts.
set -euo pipefail

version=${1:?usage: build-mcpb.sh <version> <dist-dir> <output.mcpb> [platform]}
dist=${2:?usage: build-mcpb.sh <version> <dist-dir> <output.mcpb> [platform]}
out=${3:?usage: build-mcpb.sh <version> <dist-dir> <output.mcpb> [platform]}
platform=${4:-}
# The spec names platforms darwin/linux/win32; the CLI accepts "windows".
spec_platform=$platform
if [ "$platform" = "windows" ]; then spec_platform=win32; fi

here=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if [ ! -f "$here/packaging/mcpb/manifest.json" ]; then
	echo "build-mcpb: run from the repository (missing packaging/mcpb/manifest.json)" >&2
	exit 2
fi

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/server"

# The manifest version is plain semver; the tag keeps the "v" and both the
# Makefile and the release workflow pass it through, so strip it here.
version=${version#v}
sed "s/__VERSION__/${version}/" "$here/packaging/mcpb/manifest.json" >"$stage/manifest.json"
if [ "$platform" != "windows" ]; then
	install -m 0755 "$here/packaging/mcpb/velruna-launch.sh" "$stage/server/velruna-launch.sh"
fi

count=0
for bin in "$dist"/velruna-*; do
	[ -f "$bin" ] || continue
	if [ -n "$platform" ] && [ "$platform" != "windows" ]; then
		case "$(basename "$bin")" in
			velruna-"$platform"-*) ;;
			*) continue ;;
		esac
	elif [ "$platform" = "windows" ]; then
		case "$(basename "$bin")" in
			velruna-windows-*) ;;
			*) continue ;;
		esac
	fi
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
if [ -n "$platform" ]; then
	python3 - "$stage/manifest.json" "$platform" "$spec_platform" <<'PY'
import json, sys
path, platform, spec_platform = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, encoding="utf-8") as fh:
    manifest = json.load(fh)
manifest.setdefault("compatibility", {})["platforms"] = [spec_platform]
config = manifest.get("server", {}).setdefault("mcp_config", {})
if spec_platform == "win32":
    # There is no POSIX shell on Windows: point the bundle straight at the exe.
    # entry_point names the launched file too — the manifest is a contract, and
    # a missing file there reads like a packaging bug.
    config["command"] = "${__dirname}/server/velruna-windows-amd64.exe"
    config.pop("platform_overrides", None)
    manifest["server"]["entry_point"] = "server/velruna-windows-amd64.exe"
else:
    # Drop launch overrides for OSes this bundle no longer carries: a command
    # pointing at an absent file reads like a packaging bug.
    overrides = config.get("platform_overrides")
    if isinstance(overrides, dict):
        for os_name in list(overrides):
            if os_name != spec_platform:
                del overrides[os_name]
        if not overrides:
            del config["platform_overrides"]
with open(path, "w", encoding="utf-8") as fh:
    json.dump(manifest, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
PY
fi

rm -f "$out"
(cd "$stage" && zip -q -r -X "$out" manifest.json server)
echo "built $out with $count binaries (version $version)"
