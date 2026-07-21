#!/bin/sh
# Publishes the version in main.go with its complete AppImage update pair.
# The tag must already exist and point at the version being released.
set -eu

PROJECT_ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
VERSION="$(sed -n 's/^const version = "\(.*\)"$/\1/p' "$PROJECT_ROOT/main.go")"
[ -n "$VERSION" ] || { printf 'publish-release: could not read main.go version\n' >&2; exit 1; }

TAG="v$VERSION"
APPIMAGE="$PROJECT_ROOT/build/ExactSize-$VERSION-x86_64.AppImage"
ZSYNC="$APPIMAGE.zsync"

command -v gh >/dev/null 2>&1 || { printf 'publish-release: gh is required\n' >&2; exit 1; }
git -C "$PROJECT_ROOT" rev-parse --verify --quiet "refs/tags/$TAG" >/dev/null || {
  printf 'publish-release: tag %s does not exist locally\n' "$TAG" >&2
  exit 1
}
[ -s "$APPIMAGE" ] || { printf 'publish-release: missing %s\n' "$APPIMAGE" >&2; exit 1; }
[ -s "$ZSYNC" ] || { printf 'publish-release: missing %s\n' "$ZSYNC" >&2; exit 1; }

gh release create "$TAG" "$APPIMAGE" "$ZSYNC" \
  --repo NYPDK/ExactSize \
  --verify-tag \
  --latest \
  --title "ExactSize $VERSION" \
  --generate-notes

ASSETS="$(gh release view "$TAG" --repo NYPDK/ExactSize --json assets --jq '.assets[].name')"
printf '%s\n' "$ASSETS" | grep -Fx "$(basename "$APPIMAGE")" >/dev/null || {
  printf 'publish-release: GitHub release is missing %s\n' "$(basename "$APPIMAGE")" >&2
  exit 1
}
printf '%s\n' "$ASSETS" | grep -Fx "$(basename "$ZSYNC")" >/dev/null || {
  printf 'publish-release: GitHub release is missing %s\n' "$(basename "$ZSYNC")" >&2
  exit 1
}

printf 'Published %s with AppImage and zsync assets.\n' "$TAG"
