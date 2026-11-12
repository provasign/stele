#!/usr/bin/env bash
# Build Z3 as a static library for Stele's release binaries, which link it in
# (internal/solver/embedded.go, build tag z3static) so users install nothing
# besides stele. Z3's own releases ship only a shared library.
#
# Usage: scripts/build-z3.sh <version> <prefix> [darwin-arch]
#   version      a Z3 release, e.g. 5.3.1
#   prefix       where include/ and lib/libz3.a are installed
#   darwin-arch  arm64 or x86_64 on macOS (default: this machine's)
#
# Then build stele with:
#   CGO_ENABLED=1 CGO_CFLAGS="-I<prefix>/include" CGO_LDFLAGS="-L<prefix>/lib" \
#     go build -tags z3static ./cmd/stele
set -euo pipefail

ver="${1:?usage: $0 <version> <prefix> [darwin-arch]}"
prefix="$(mkdir -p "$2" && cd "$2" && pwd)"
arch="${3:-}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL "https://github.com/Z3Prover/z3/archive/refs/tags/z3-${ver}.tar.gz" | tar xz -C "$work"
src="$work/z3-z3-${ver}"

extra=()
if [ "$(uname -s)" = Darwin ]; then
  # Z3 5.3 needs std::to_chars, first in macOS 13.3; Z3's own release targets it too.
  extra+=(-DCMAKE_OSX_DEPLOYMENT_TARGET=13.3)
  [ -n "$arch" ] && extra+=("-DCMAKE_OSX_ARCHITECTURES=${arch}")
fi

jobs="$(getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu)"
cmake -S "$src" -B "$work/build" \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_INSTALL_PREFIX="$prefix" \
  -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
  -DZ3_BUILD_LIBZ3_SHARED=OFF \
  -DZ3_BUILD_EXECUTABLE=OFF \
  -DZ3_BUILD_TEST_EXECUTABLES=OFF \
  -DZ3_ENABLE_EXAMPLE_TARGETS=OFF \
  -DZ3_BUILD_DOCUMENTATION=OFF \
  "${extra[@]}" >/dev/null
cmake --build "$work/build" -j "$jobs"
cmake --install "$work/build" >/dev/null
# Z3 installs its library to lib/ or lib64/; Stele's build looks in lib/.
if [ ! -f "$prefix/lib/libz3.a" ] && [ -f "$prefix/lib64/libz3.a" ]; then
  mkdir -p "$prefix/lib" && cp "$prefix/lib64/libz3.a" "$prefix/lib/"
fi
cp "$src/LICENSE.txt" "$prefix/LICENSE-z3.txt"
ls -l "$prefix/lib/libz3.a"
