#!/bin/sh
# Builds the npm packages under npm/ for both Mac architectures into
# dist/npm/, stamped with one version and pointed at the deployed room-be,
# then packs them into dist/ or, with --publish, publishes them.
#
#   scripts/npm-release.sh 0.1.0             # build + npm pack (inspect, or npm i -g dist/*.tgz)
#   scripts/npm-release.sh 0.1.0 --publish   # build + npm publish (needs `npm login`)
#
# ROOM_RELEASE_SERVER overrides the room-be the release logs in to by default.
# ROOM_CODESIGN_IDENTITY signs the binaries, so macOS keeps their screen
# permissions across upgrades on every Mac that installs them.
set -eu

usage() { echo "usage: $0 <version, e.g. 0.1.0> [--publish]" >&2; exit 2; }
version=${1:-}
case $version in [0-9]*.[0-9]*.[0-9]*) ;; *) usage ;; esac
case ${2:-} in
  '') npm_cmd="npm pack --pack-destination ../.." ;;
  --publish) npm_cmd="npm publish --access public" ;;
  *) usage ;;
esac

cd "$(dirname "$0")/.."
server=${ROOM_RELEASE_SERVER:-https://room-be-production-7c2a536d.up.railway.app}
identity=${ROOM_CODESIGN_IDENTITY:-}
ldflags="-s -w -X $(go list -m)/internal/config.DefaultBaseURL=$server -X main.version=$version"

rm -rf dist
mkdir -p dist/npm
cp -R npm/roomd npm/roomd-darwin-arm64 npm/roomd-darwin-x64 dist/npm/

# goarch npm-arch (go passes clang the matching -arch for cgo)
for target in "arm64 arm64" "amd64 x64"; do
  set -- $target
  out=dist/npm/roomd-darwin-$2/bin
  mkdir -p "$out"
  echo "building darwin/$1"
  # roomd stays cgo-free (CLAUDE.md: roomd links no media framework).
  CGO_ENABLED=0 GOOS=darwin GOARCH=$1 \
    go build -trimpath -ldflags "$ldflags" -o "$out/roomd" ./cmd/roomd
  CGO_ENABLED=1 GOOS=darwin GOARCH=$1 \
    go build -trimpath -ldflags "-s -w" -o "$out/roomd-screen" ./cmd/roomd-screen
  if [ -n "$identity" ]; then
    # The identifiers macOS ties the screen permissions to: keep them the
    # same as installBinaries' in cmd/roomd/install.go.
    codesign -f -s "$identity" --identifier com.stonksfi.roomd "$out/roomd"
    codesign -f -s "$identity" --identifier com.stonksfi.roomd-screen "$out/roomd-screen"
  fi
done

# Every package's version, and the main package's pins on the platform
# packages, are the placeholder "0.0.0" in npm/.
sed -i '' "s/\"0\.0\.0\"/\"$version\"/" dist/npm/*/package.json

# The platform packages first: the main package names them by exact version.
for pkg in roomd-darwin-arm64 roomd-darwin-x64 roomd; do
  (cd "dist/npm/$pkg" && $npm_cmd)
done
