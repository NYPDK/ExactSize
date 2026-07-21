#!/bin/sh
# Builds the ExactSize AppImage. Runs with no arguments: FFmpeg (a static
# build with the software, GPU, and H.266 encoders the app offers) and
# appimagetool are downloaded and cached under build/cache when not provided.
#
# Overrides:
#   GO_BIN, FFMPEG_BIN, FFPROBE_BIN, APPIMAGETOOL_BIN  use these binaries
#   FFMPEG_TARBALL_URL, FFMPEG_CHECKSUMS_URL           pin an FFmpeg release
#   BUILD_ROOT, OUTPUT                                 build and output paths
set -eu

PROJECT_ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
BUILD_ROOT="${BUILD_ROOT:-$PROJECT_ROOT/build}"
CACHE_DIR="$BUILD_ROOT/cache"
APPDIR="$BUILD_ROOT/ExactSize.AppDir"
GO_BIN="${GO_BIN:-go}"
FFMPEG_BIN="${FFMPEG_BIN:-}"
FFPROBE_BIN="${FFPROBE_BIN:-}"
APPIMAGETOOL_BIN="${APPIMAGETOOL_BIN:-}"
UPDATE_INFORMATION="${UPDATE_INFORMATION:-gh-releases-zsync|NYPDK|ExactSize|latest|ExactSize-*-x86_64.AppImage.zsync}"

FFMPEG_TARBALL_URL="${FFMPEG_TARBALL_URL:-https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-linux64-gpl.tar.xz}"
FFMPEG_CHECKSUMS_URL="${FFMPEG_CHECKSUMS_URL:-https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/checksums.sha256}"
APPIMAGETOOL_URL="https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage"

die() {
  printf 'build-appimage: %s\n' "$1" >&2
  exit 1
}

download() {
  # download <url> <destination>
  command -v curl >/dev/null 2>&1 || die "curl is required to download $1"
  printf 'Downloading %s\n' "$1" >&2
  curl -fL --retry 3 --progress-bar -o "$2.part" "$1" || die "download failed: $1"
  mv "$2.part" "$2"
}

resolve_tool() {
  # resolve_tool <name-or-path> -> absolute path on stdout
  if [ -f "$1" ]; then
    printf '%s\n' "$1"
  else
    command -v "$1" || die "$1 was not found on PATH"
  fi
}

if [ "$(uname -m)" != "x86_64" ] && [ -z "$FFMPEG_BIN" ]; then
  die "the default FFmpeg download targets x86_64; set FFMPEG_BIN and FFPROBE_BIN for $(uname -m)"
fi

VERSION="$(sed -n 's/^const version = "\(.*\)"$/\1/p' "$PROJECT_ROOT/main.go")"
[ -n "$VERSION" ] || die "could not read the version from main.go"
OUTPUT="${OUTPUT:-$BUILD_ROOT/ExactSize-$VERSION-x86_64.AppImage}"

mkdir -p "$CACHE_DIR"

# --- FFmpeg -----------------------------------------------------------------
if [ -z "$FFMPEG_BIN" ] || [ -z "$FFPROBE_BIN" ]; then
  TARBALL_NAME="${FFMPEG_TARBALL_URL##*/}"
  TARBALL="$CACHE_DIR/$TARBALL_NAME"
  EXTRACT_DIR="$CACHE_DIR/${TARBALL_NAME%.tar.xz}"

  download "$FFMPEG_CHECKSUMS_URL" "$CACHE_DIR/checksums.sha256"
  CHECKSUM_LINE="$(grep "[[:space:]]$TARBALL_NAME\$" "$CACHE_DIR/checksums.sha256" || true)"
  [ -n "$CHECKSUM_LINE" ] || die "$TARBALL_NAME is not listed in the release checksums"

  verify_tarball() {
    ( cd "$CACHE_DIR" && printf '%s\n' "$CHECKSUM_LINE" | sha256sum -c - >/dev/null 2>&1 )
  }

  if [ ! -f "$TARBALL" ] || ! verify_tarball; then
    download "$FFMPEG_TARBALL_URL" "$TARBALL"
    verify_tarball || die "checksum verification failed for $TARBALL_NAME"
  fi
  printf 'FFmpeg tarball checksum OK\n' >&2

  rm -rf "$EXTRACT_DIR"
  tar -xf "$TARBALL" -C "$CACHE_DIR"
  [ -d "$EXTRACT_DIR" ] || die "unexpected tarball layout: $EXTRACT_DIR is missing"
  FFMPEG_BIN="$EXTRACT_DIR/bin/ffmpeg"
  FFPROBE_BIN="$EXTRACT_DIR/bin/ffprobe"
else
  FFMPEG_BIN="$(resolve_tool "$FFMPEG_BIN")"
  FFPROBE_BIN="$(resolve_tool "$FFPROBE_BIN")"
fi

[ -f "$FFMPEG_BIN" ] || die "FFmpeg binary is missing: $FFMPEG_BIN"
[ -f "$FFPROBE_BIN" ] || die "ffprobe binary is missing: $FFPROBE_BIN"

# The bundled FFmpeg must provide every encoder the app offers. Hardware
# encoders only need to be compiled in; the app tests them on the user's
# machine at launch.
ENCODERS="$("$FFMPEG_BIN" -hide_banner -encoders 2>/dev/null)" || die "could not run $FFMPEG_BIN"
for encoder in libx264 libx265 libvvenc libaom-av1 libvpx-vp9 aac libopus libvorbis libmp3lame; do
  printf '%s\n' "$ENCODERS" | grep -q "[[:space:]]$encoder[[:space:]]" || die "the selected FFmpeg build lacks the $encoder encoder"
done
for encoder in h264_nvenc h264_qsv h264_vaapi h264_amf; do
  printf '%s\n' "$ENCODERS" | grep -q "[[:space:]]$encoder[[:space:]]" || printf 'warning: FFmpeg build lacks %s; that GPU family will be unavailable\n' "$encoder" >&2
done

# --- appimagetool -----------------------------------------------------------
if [ -z "$APPIMAGETOOL_BIN" ]; then
  # The official AppImage bundles zsyncmake. An arbitrary system
  # appimagetool may not, in which case it only warns and silently omits the
  # release metadata file. Use the cached official tool unless explicitly
  # overridden so both build artifacts are deterministic.
  APPIMAGETOOL_BIN="$CACHE_DIR/appimagetool-x86_64.AppImage"
  [ -f "$APPIMAGETOOL_BIN" ] || download "$APPIMAGETOOL_URL" "$APPIMAGETOOL_BIN"
  chmod +x "$APPIMAGETOOL_BIN"
else
  APPIMAGETOOL_BIN="$(resolve_tool "$APPIMAGETOOL_BIN")"
fi

# --- AppDir -----------------------------------------------------------------
# A running instance keeps files in the AppDir busy; on FUSE filesystems the
# deletions then leave .fuse_hidden files behind and the cleanup fails.
if pgrep -f "$APPDIR/usr/bin" >/dev/null 2>&1; then
  die "ExactSize is running from $APPDIR; close it and rerun the build"
fi
if [ -d "$APPDIR" ]; then
  find "$APPDIR" -mindepth 1 -delete || die "could not clean $APPDIR (files may still be in use)"
fi

mkdir -p "$APPDIR/usr/bin" "$APPDIR/usr/share/applications" "$APPDIR/usr/share/metainfo" "$APPDIR/usr/share/icons/hicolor/scalable/apps" "$APPDIR/usr/share/icons/hicolor/256x256/apps" "$APPDIR/usr/share/doc/exactsize"

cd "$PROJECT_ROOT"
CGO_ENABLED=0 "$GO_BIN" build -buildvcs=false -trimpath -ldflags="-s -w" -o "$APPDIR/usr/bin/exactsize" .

cp "$FFMPEG_BIN" "$APPDIR/usr/bin/ffmpeg"
cp "$FFPROBE_BIN" "$APPDIR/usr/bin/ffprobe"
cp packaging/AppRun "$APPDIR/AppRun"
cp packaging/exactsize.desktop "$APPDIR/exactsize.desktop"
cp packaging/exactsize.desktop "$APPDIR/usr/share/applications/exactsize.desktop"
cp packaging/io.exactsize.ExactSize.metainfo.xml "$APPDIR/usr/share/metainfo/io.exactsize.ExactSize.metainfo.xml"
cp packaging/io.exactsize.ExactSize.metainfo.xml "$APPDIR/usr/share/metainfo/io.exactsize.ExactSize.appdata.xml"
# The PNG is the AppImage's own icon: appimagetool embeds the root icon and
# .DirIcon, and file managers thumbnail PNG far more reliably than SVG.
cp packaging/exactsize-256.png "$APPDIR/exactsize.png"
cp packaging/exactsize-256.png "$APPDIR/.DirIcon"
cp packaging/exactsize-256.png "$APPDIR/usr/share/icons/hicolor/256x256/apps/exactsize.png"
cp web/icon.svg "$APPDIR/usr/share/icons/hicolor/scalable/apps/exactsize.svg"
cp LICENSE README.md THIRD_PARTY_NOTICES.md "$APPDIR/usr/share/doc/exactsize/"

chmod +x "$APPDIR/AppRun" "$APPDIR/usr/bin/exactsize" "$APPDIR/usr/bin/ffmpeg" "$APPDIR/usr/bin/ffprobe"

# APPIMAGE_EXTRACT_AND_RUN lets the appimagetool AppImage run without FUSE;
# a natively installed appimagetool ignores it.
ZSYNC_OUTPUT="$OUTPUT.zsync"
GENERATED_ZSYNC="$PROJECT_ROOT/$(basename "$OUTPUT").zsync"
rm -f "$ZSYNC_OUTPUT"
if [ "$GENERATED_ZSYNC" != "$ZSYNC_OUTPUT" ]; then
  rm -f "$GENERATED_ZSYNC"
fi
ARCH=x86_64 APPIMAGE_EXTRACT_AND_RUN=1 "$APPIMAGETOOL_BIN" \
  --updateinformation "$UPDATE_INFORMATION" \
  --file-url "$(basename "$OUTPUT")" \
  "$APPDIR" "$OUTPUT"
chmod 0755 "$OUTPUT"
# appimagetool writes the zsync file to its working directory when DESTINATION
# is absolute. Normalize it beside OUTPUT so release tooling has one stable
# artifact location regardless of the caller's current directory.
if [ "$GENERATED_ZSYNC" != "$ZSYNC_OUTPUT" ] && [ -s "$GENERATED_ZSYNC" ]; then
  mv "$GENERATED_ZSYNC" "$ZSYNC_OUTPUT"
fi
[ -s "$ZSYNC_OUTPUT" ] || die "appimagetool did not generate $ZSYNC_OUTPUT; install zsyncmake or use the downloaded appimagetool"
chmod 0644 "$ZSYNC_OUTPUT"

# The sealed AppImage keeps the spec-compliant Exec=exactsize. The on-disk
# AppDir copy points at its own AppRun instead, so double-clicking the desktop
# file in a file manager launches the app rather than failing on a program
# name that is not on PATH.
sed -i "s|^Exec=.*|Exec=\"$APPDIR/AppRun\"|" "$APPDIR/exactsize.desktop"

printf '%s\n%s\n' "$OUTPUT" "$ZSYNC_OUTPUT"
