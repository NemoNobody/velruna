#!/bin/sh
# Pick the platform binary shipped in this bundle and hand over to it.
#
# The MCPB manifest can describe per-OS commands but not per-architecture
# ones, so the bundle carries every build and this launcher does the last
# step: macOS and Linux are covered here, Windows uses
# velruna-windows-amd64.exe through the manifest's platform_overrides.
set -eu

dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

os=$(uname -s)
case "$os" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*)
		echo "velruna: unsupported platform $os (the bundle ships darwin and linux builds; use the Windows build on Windows)" >&2
		exit 1
		;;
esac

machine=$(uname -m)
case "$machine" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*)
		echo "velruna: unsupported architecture $machine (supported: amd64, arm64)" >&2
		exit 1
		;;
esac

bin="$dir/velruna-$os-$arch"
if [ ! -x "$bin" ]; then
	echo "velruna: this bundle has no build for $os/$arch (expected $bin)" >&2
	exit 1
fi

exec "$bin" "$@"
