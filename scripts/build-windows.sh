#!/bin/sh
# Cross-builds the portable Windows x86-64 distribution. The BtbN GPL FFmpeg
# archive is downloaded and checksum-verified when it is not already cached.
#
# Overrides:
#   GO_BIN, FFMPEG_ZIP_URL, FFMPEG_CHECKSUMS_URL  select build inputs
#   BUILD_ROOT, OUTPUT_DIR, OUTPUT_ARCHIVE         select output paths
set -eu

PROJECT_ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
BUILD_ROOT="${BUILD_ROOT:-$PROJECT_ROOT/build}"
CACHE_DIR="$BUILD_ROOT/cache"
GO_BIN="${GO_BIN:-go}"
FFMPEG_ZIP_URL="${FFMPEG_ZIP_URL:-https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip}"
FFMPEG_CHECKSUMS_URL="${FFMPEG_CHECKSUMS_URL:-https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/checksums.sha256}"

die() {
  printf 'build-windows: %s\n' "$1" >&2
  exit 1
}

download() {
  command -v curl >/dev/null 2>&1 || die "curl is required to download $1"
  printf 'Downloading %s\n' "$1" >&2
  curl -fL --retry 3 --progress-bar -o "$2.part" "$1" || die "download failed: $1"
  mv "$2.part" "$2"
}

command -v "$GO_BIN" >/dev/null 2>&1 || die "$GO_BIN was not found on PATH"
command -v unzip >/dev/null 2>&1 || die "unzip is required"
command -v strings >/dev/null 2>&1 || die "strings is required to inspect ffmpeg.exe"
if command -v zip >/dev/null 2>&1; then
  ZIP_BIN=zip
elif command -v python >/dev/null 2>&1 || command -v python3 >/dev/null 2>&1; then
  ZIP_BIN=
else
  die "zip or python is required to create the portable archive"
fi

VERSION="$(sed -n 's/^const version = "\(.*\)"$/\1/p' "$PROJECT_ROOT/main.go")"
[ -n "$VERSION" ] || die "could not read the version from main.go"
OUTPUT_DIR="${OUTPUT_DIR:-$BUILD_ROOT/ExactSize-$VERSION-windows-x86_64}"
OUTPUT_ARCHIVE="${OUTPUT_ARCHIVE:-$BUILD_ROOT/ExactSize-$VERSION-windows-x86_64.zip}"
GOCACHE="${GOCACHE:-$CACHE_DIR/go-build-windows}"
export GOCACHE

mkdir -p "$CACHE_DIR" "$GOCACHE"

CHECKSUMS="$CACHE_DIR/checksums.sha256"
download "$FFMPEG_CHECKSUMS_URL" "$CHECKSUMS"
ZIP_NAME="${FFMPEG_ZIP_URL##*/}"
FFMPEG_ZIP="$CACHE_DIR/$ZIP_NAME"
CHECKSUM_LINE="$(grep "[[:space:]]$ZIP_NAME\$" "$CHECKSUMS" || true)"
[ -n "$CHECKSUM_LINE" ] || die "$ZIP_NAME is not listed in the release checksums"

verify_ffmpeg_zip() {
  (cd "$CACHE_DIR" && printf '%s\n' "$CHECKSUM_LINE" | sha256sum -c - >/dev/null 2>&1)
}

if [ ! -f "$FFMPEG_ZIP" ] || ! verify_ffmpeg_zip; then
  download "$FFMPEG_ZIP_URL" "$FFMPEG_ZIP"
  verify_ffmpeg_zip || die "checksum verification failed for $ZIP_NAME"
fi
printf 'Windows FFmpeg archive checksum OK\n' >&2

if [ -d "$OUTPUT_DIR" ]; then
  find "$OUTPUT_DIR" -mindepth 1 -delete || die "could not clean $OUTPUT_DIR"
fi
mkdir -p "$OUTPUT_DIR"

cd "$PROJECT_ROOT"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build \
  -buildvcs=false -trimpath -ldflags="-s -w -H=windowsgui" \
  -o "$OUTPUT_DIR/ExactSize.exe" .

unzip -j -o "$FFMPEG_ZIP" '*/bin/ffmpeg.exe' '*/bin/ffprobe.exe' -d "$OUTPUT_DIR" >/dev/null
[ -s "$OUTPUT_DIR/ffmpeg.exe" ] || die "ffmpeg.exe is missing after extraction"
[ -s "$OUTPUT_DIR/ffprobe.exe" ] || die "ffprobe.exe is missing after extraction"

# The release archive is checksum-pinned, and these string-table checks catch
# accidental selection of a reduced FFmpeg flavor before it is packaged.
for encoder in libx264 libx265 libvvenc libaom-av1 libvpx-vp9 aac libopus libvorbis libmp3lame h264_nvenc h264_qsv h264_amf; do
  strings "$OUTPUT_DIR/ffmpeg.exe" | grep -F "$encoder" >/dev/null || die "ffmpeg.exe lacks $encoder"
done

cp "$PROJECT_ROOT/LICENSE" "$PROJECT_ROOT/README.md" "$PROJECT_ROOT/THIRD_PARTY_NOTICES.md" "$OUTPUT_DIR/"
unzip -p "$FFMPEG_ZIP" '*/LICENSE.txt' >"$OUTPUT_DIR/FFMPEG-LICENSE.txt"
[ -s "$OUTPUT_DIR/FFMPEG-LICENSE.txt" ] || die "FFmpeg license was not extracted"

rm -f "$OUTPUT_ARCHIVE"
if [ -n "${ZIP_BIN:-}" ]; then
  (cd "$(dirname -- "$OUTPUT_DIR")" && zip -X -q -r "$OUTPUT_ARCHIVE" "$(basename -- "$OUTPUT_DIR")")
else
  PYTHON_BIN="$(command -v python3 || command -v python)"
  "$PYTHON_BIN" - "$OUTPUT_DIR" "$OUTPUT_ARCHIVE" <<'PY'
import os, sys, zipfile
source, archive = sys.argv[1], sys.argv[2]
root_name = os.path.basename(source)
with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as bundle:
    for dirpath, _, filenames in os.walk(source):
        for filename in filenames:
            path = os.path.join(dirpath, filename)
            arcname = os.path.join(root_name, os.path.relpath(path, source)).replace("\\", "/")
            bundle.write(path, arcname)
PY
fi
[ -s "$OUTPUT_ARCHIVE" ] || die "portable archive was not created"

printf '%s\n%s\n' "$OUTPUT_DIR/ExactSize.exe" "$OUTPUT_ARCHIVE"
