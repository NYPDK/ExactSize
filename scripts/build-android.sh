#!/bin/sh
# Build a directly installable arm64-v8a Android APK without Gradle. The
# Android SDK platform/build tools, NDK, FFmpeg, x264, and dav1d
# are pinned, downloaded into build/android/cache, and verified before use.
set -eu

PROJECT_ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
BUILD_ROOT="${ANDROID_BUILD_ROOT:-$PROJECT_ROOT/build/android}"
CACHE_DIR="$BUILD_ROOT/cache"
SDK_ROOT="${ANDROID_SDK_ROOT:-$BUILD_ROOT/sdk}"
WORK_DIR="$BUILD_ROOT/work"
OUTPUT_DIR="${ANDROID_OUTPUT_DIR:-$PROJECT_ROOT/build}"
MIN_SDK=28
TARGET_SDK=35
BUILD_TOOLS_VERSION=35.0.1
NDK_VERSION=27.2.12479018

JDK_ARCHIVE=OpenJDK17U-jdk_x64_linux_hotspot_17.0.19_10.tar.gz
JDK_URL="https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.19%2B10/$JDK_ARCHIVE"
JDK_SHA256=d8afc263758141a66e0e3aafc321e783f7016696f4eaea067d340a269037d331
PLATFORM_ARCHIVE=platform-35_r02.zip
PLATFORM_URL="https://dl.google.com/android/repository/$PLATFORM_ARCHIVE"
PLATFORM_SHA1=0bb560a90a7a2cbd0dd8348224d518b638fe7949
BUILD_TOOLS_ARCHIVE=build-tools_r35.0.1_linux.zip
BUILD_TOOLS_URL="https://dl.google.com/android/repository/$BUILD_TOOLS_ARCHIVE"
BUILD_TOOLS_SHA1=e009a9b188cfeb1d2b4c318ab5cb4f1ddc368861
NDK_ARCHIVE=android-ndk-r27c-linux.zip
NDK_URL="https://dl.google.com/android/repository/$NDK_ARCHIVE"
NDK_SHA1=090e8083a715fdb1a3e402d0763c388abb03fb4e
FFMPEG_VERSION=8.1.2
FFMPEG_ARCHIVE="ffmpeg-$FFMPEG_VERSION.tar.xz"
FFMPEG_URL="https://ffmpeg.org/releases/$FFMPEG_ARCHIVE"
FFMPEG_SHA256=464beb5e7bf0c311e68b45ae2f04e9cc2af88851abb4082231742a74d97b524c
X264_COMMIT=b35605ace3ddf7c1a5d67a2eb553f034aef41d55
X264_ARCHIVE="x264-$X264_COMMIT.tar.gz"
X264_URL="https://code.videolan.org/videolan/x264/-/archive/$X264_COMMIT/$X264_ARCHIVE"
X264_SHA256=cd71a7515b0e9a012e1ac9b1f8415bebcaf6fc97d4db32286642ac4c0fbe24f9
DAV1D_VERSION=1.5.3
DAV1D_ARCHIVE="dav1d-$DAV1D_VERSION.tar.gz"
DAV1D_URL="https://code.videolan.org/videolan/dav1d/-/archive/$DAV1D_VERSION/$DAV1D_ARCHIVE"
DAV1D_SHA256=cbe212b02faf8c6eed5b6d55ef8a6e363aaab83f15112e960701a9c3df813686
MESON_VERSION=1.10.0
MESON_ARCHIVE="meson-$MESON_VERSION.tar.gz"
MESON_URL="https://github.com/mesonbuild/meson/releases/download/$MESON_VERSION/$MESON_ARCHIVE"
MESON_SHA256=8071860c1f46a75ea34801490fd1c445c9d75147a65508cd3a10366a7006cc1c
NINJA_VERSION=1.13.2
NINJA_ARCHIVE="ninja-linux-$NINJA_VERSION.zip"
NINJA_URL="https://github.com/ninja-build/ninja/releases/download/v$NINJA_VERSION/ninja-linux.zip"
NINJA_SHA256=5749cbc4e668273514150a80e387a957f933c6ed3f5f11e03fb30955e2bbead6

die() {
  printf 'build-android: %s\n' "$1" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

download() {
  url="$1"
  output="$2"
  [ -s "$output" ] && return
  printf 'Downloading %s\n' "$url" >&2
  curl -fL --retry 3 --progress-bar -o "$output.part" "$url" || die "download failed: $url"
  mv "$output.part" "$output"
}

verify_sha1() {
  expected="$1"
  file="$2"
  actual="$(sha1sum "$file" | awk '{print $1}')"
  [ "$actual" = "$expected" ] || die "SHA-1 mismatch for $file"
}

verify_sha256() {
  expected="$1"
  file="$2"
  actual="$(sha256sum "$file" | awk '{print $1}')"
  [ "$actual" = "$expected" ] || die "SHA-256 mismatch for $file"
}

clean_dir() {
  dir="$1"
  case "$dir" in
    "$BUILD_ROOT"/*) ;;
    *) die "refusing to clean unexpected path: $dir" ;;
  esac
  # FUSE workspaces can race `find -delete` while traversing FFmpeg's very
  # large source tree (temporary compiler files disappear between readdir and
  # unlink). Remove the already-validated build directory as one operation so
  # a failed incremental cleanup cannot leave a mixed source/object tree.
  if [ -e "$dir" ] || [ -L "$dir" ]; then
    rm -rf -- "$dir"
  fi
  mkdir -p "$dir"
}

install_android_archive() {
  archive_name="$1"
  archive_url="$2"
  archive_sha1="$3"
  destination="$4"
  marker="$5"
  [ -e "$destination/$marker" ] && return

  archive="$CACHE_DIR/$archive_name"
  download "$archive_url" "$archive"
  verify_sha1 "$archive_sha1" "$archive"
  unpack="$BUILD_ROOT/unpack-${archive_name%.zip}"
  clean_dir "$unpack"
  unzip -q "$archive" -d "$unpack"
  package_root="$(find "$unpack" -mindepth 1 -maxdepth 1 -type d | sed -n '1p')"
  [ -n "$package_root" ] || die "unexpected archive layout: $archive_name"
  clean_dir "$destination"
  cp -a "$package_root/." "$destination/"
  [ -e "$destination/$marker" ] || die "$archive_name did not provide $marker"
}

need curl
need unzip
need zip
need go
need make
need tar
need python3
need pkg-config
mkdir -p "$CACHE_DIR" "$SDK_ROOT" "$OUTPUT_DIR"

# FUSE-mounted workspaces often report the host's full logical CPU count even
# when the container has a much smaller memory budget. Keep native builds
# bounded to avoid hundreds of simultaneous clang processes exhausting memory;
# callers can raise this deliberately for a larger build host.
BUILD_JOBS="${EXACTSIZE_BUILD_JOBS:-$(getconf _NPROCESSORS_ONLN 2>/dev/null || printf 2)}"
case "$BUILD_JOBS" in
  ''|*[!0-9]*) BUILD_JOBS=2 ;;
esac
[ "$BUILD_JOBS" -gt 8 ] && BUILD_JOBS=8
[ "$BUILD_JOBS" -gt 0 ] || BUILD_JOBS=2

# Several upstream autotools-style configure scripts do not quote compiler and
# prefix paths correctly. Give those tools a stable, space-free view of this
# mounted workspace while retaining every real build file under BUILD_ROOT.
SHORT_ID="$(printf '%s' "$BUILD_ROOT" | cksum | awk '{print $1}')"
SHORT_ROOT="/tmp/exactsize-android-$SHORT_ID"
if [ -L "$SHORT_ROOT" ]; then
  [ "$(readlink "$SHORT_ROOT")" = "$BUILD_ROOT" ] || die "unexpected short-path link: $SHORT_ROOT"
elif [ -e "$SHORT_ROOT" ]; then
  die "short-path location already exists and is not a symlink: $SHORT_ROOT"
else
  ln -s "$BUILD_ROOT" "$SHORT_ROOT"
fi

# The Android tools and Java source compiler are pinned to an LTS JDK rather than
# relying on whichever (possibly JRE-only) Java happens to be installed.
JDK_HOME="${ANDROID_JAVA_HOME:-$BUILD_ROOT/jdk17}"
if [ ! -x "$JDK_HOME/bin/javac" ]; then
  JDK_TARBALL="$CACHE_DIR/$JDK_ARCHIVE"
  download "$JDK_URL" "$JDK_TARBALL"
  verify_sha256 "$JDK_SHA256" "$JDK_TARBALL"
  clean_dir "$JDK_HOME"
  tar -xzf "$JDK_TARBALL" -C "$JDK_HOME" --strip-components=1
fi
for file in "$JDK_HOME/bin/java" "$JDK_HOME/bin/javac" "$JDK_HOME/bin/keytool"; do
  [ -x "$file" ] || die "JDK tool is missing: $file"
done
export JAVA_HOME="$JDK_HOME"
export PATH="$JDK_HOME/bin:$PATH"

export ANDROID_SDK_ROOT="$SDK_ROOT"
export ANDROID_HOME="$SDK_ROOT"
# Package archive hashes are from Google's repository2-3.xml metadata. Direct
# extraction avoids sdkmanager/Android CLI rename failures on FUSE workspaces.
install_android_archive "$PLATFORM_ARCHIVE" "$PLATFORM_URL" "$PLATFORM_SHA1" \
  "$SDK_ROOT/platforms/android-$TARGET_SDK" android.jar
install_android_archive "$BUILD_TOOLS_ARCHIVE" "$BUILD_TOOLS_URL" "$BUILD_TOOLS_SHA1" \
  "$SDK_ROOT/build-tools/$BUILD_TOOLS_VERSION" aapt2
install_android_archive "$NDK_ARCHIVE" "$NDK_URL" "$NDK_SHA1" \
  "$SDK_ROOT/ndk/$NDK_VERSION" toolchains/llvm/prebuilt/linux-x86_64/bin/clang

BUILD_TOOLS="$SDK_ROOT/build-tools/$BUILD_TOOLS_VERSION"
ANDROID_JAR="$SDK_ROOT/platforms/android-$TARGET_SDK/android.jar"
NDK_ROOT="$SDK_ROOT/ndk/$NDK_VERSION"
TOOLCHAIN="$SHORT_ROOT/sdk/ndk/$NDK_VERSION/toolchains/llvm/prebuilt/linux-x86_64"
CC="$TOOLCHAIN/bin/aarch64-linux-android${MIN_SDK}-clang"
AR="$TOOLCHAIN/bin/llvm-ar"
RANLIB="$TOOLCHAIN/bin/llvm-ranlib"
STRIP="$TOOLCHAIN/bin/llvm-strip"
for file in "$BUILD_TOOLS/aapt2" "$BUILD_TOOLS/d8" "$BUILD_TOOLS/apksigner" \
  "$BUILD_TOOLS/zipalign" "$ANDROID_JAR" "$CC" "$AR" "$RANLIB" "$STRIP"; do
  [ -e "$file" ] || die "Android tool is missing after SDK install: $file"
done

FFMPEG_TARBALL="$CACHE_DIR/$FFMPEG_ARCHIVE"
X264_TARBALL="$CACHE_DIR/$X264_ARCHIVE"
DAV1D_TARBALL="$CACHE_DIR/$DAV1D_ARCHIVE"
MESON_TARBALL="$CACHE_DIR/$MESON_ARCHIVE"
NINJA_ZIP="$CACHE_DIR/$NINJA_ARCHIVE"
download "$FFMPEG_URL" "$FFMPEG_TARBALL"
download "$X264_URL" "$X264_TARBALL"
download "$DAV1D_URL" "$DAV1D_TARBALL"
download "$MESON_URL" "$MESON_TARBALL"
download "$NINJA_URL" "$NINJA_ZIP"
verify_sha256 "$FFMPEG_SHA256" "$FFMPEG_TARBALL"
verify_sha256 "$X264_SHA256" "$X264_TARBALL"
verify_sha256 "$DAV1D_SHA256" "$DAV1D_TARBALL"
verify_sha256 "$MESON_SHA256" "$MESON_TARBALL"
verify_sha256 "$NINJA_SHA256" "$NINJA_ZIP"

# dav1d uses Meson and Ninja. Pin local copies so Android builds do not depend
# on the host distribution's Python packages or build-tool versions.
MESON_HOME="$BUILD_ROOT/host-tools/meson-$MESON_VERSION"
if [ ! -f "$MESON_HOME/meson.py" ]; then
  clean_dir "$MESON_HOME"
  tar -xzf "$MESON_TARBALL" -C "$MESON_HOME" --strip-components=1
fi
NINJA_HOME="$BUILD_ROOT/host-tools/ninja-$NINJA_VERSION"
if [ ! -x "$NINJA_HOME/ninja" ]; then
  clean_dir "$NINJA_HOME"
  unzip -q "$NINJA_ZIP" -d "$NINJA_HOME"
  chmod 0755 "$NINJA_HOME/ninja"
fi
export PATH="$NINJA_HOME:$PATH"

run_meson() {
  python3 "$MESON_HOME/meson.py" "$@"
}

NATIVE_PREFIX_REAL="$BUILD_ROOT/native-prefix"
X264_SOURCE_REAL="$BUILD_ROOT/src/x264"
DAV1D_SOURCE_REAL="$BUILD_ROOT/src/dav1d"
DAV1D_BUILD_REAL="$BUILD_ROOT/src/dav1d-build"
FFMPEG_SOURCE_REAL="$BUILD_ROOT/src/ffmpeg"
NATIVE_PREFIX="$SHORT_ROOT/native-prefix"
X264_SOURCE="$SHORT_ROOT/src/x264"
DAV1D_SOURCE="$SHORT_ROOT/src/dav1d"
DAV1D_BUILD="$SHORT_ROOT/src/dav1d-build"
FFMPEG_SOURCE="$SHORT_ROOT/src/ffmpeg"
NATIVE_MARKER="$FFMPEG_SOURCE_REAL/.exactsize-$FFMPEG_VERSION-$X264_COMMIT-dav1d-$DAV1D_VERSION-$NDK_VERSION-mediacodec-pages16"
mkdir -p "$BUILD_ROOT/src"
if [ ! -f "$NATIVE_MARKER" ] || [ ! -f "$NATIVE_PREFIX_REAL/lib/libdav1d.a" ] || \
    [ ! -x "$FFMPEG_SOURCE_REAL/ffmpeg" ] || [ ! -x "$FFMPEG_SOURCE_REAL/ffprobe" ]; then
  clean_dir "$NATIVE_PREFIX_REAL"
  clean_dir "$X264_SOURCE_REAL"
  clean_dir "$DAV1D_SOURCE_REAL"
  clean_dir "$DAV1D_BUILD_REAL"
  clean_dir "$FFMPEG_SOURCE_REAL"
  tar -xzf "$X264_TARBALL" -C "$X264_SOURCE_REAL" --strip-components=1
  tar -xzf "$DAV1D_TARBALL" -C "$DAV1D_SOURCE_REAL" --strip-components=1
  tar -xJf "$FFMPEG_TARBALL" -C "$FFMPEG_SOURCE_REAL" --strip-components=1

  printf 'Building x264 for arm64-v8a\n' >&2
  (
    cd "$X264_SOURCE"
    CC="$CC" AR="$AR" RANLIB="$RANLIB" STRIP="$STRIP" ./configure \
      --host=aarch64-linux \
      --sysroot="$TOOLCHAIN/sysroot" \
      --prefix="$NATIVE_PREFIX" \
      --enable-static \
      --enable-pic \
      --disable-cli \
      --disable-opencl
    make -j"$BUILD_JOBS"
    make install
  )

  printf 'Building dav1d AV1 software decoder for arm64-v8a\n' >&2
  DAV1D_CROSS_FILE="$DAV1D_BUILD_REAL/android-arm64.ini"
  printf '%s\n' \
    '[binaries]' \
    "c = '$CC'" \
    "ar = '$AR'" \
    "strip = '$STRIP'" \
    '' \
    '[built-in options]' \
    "c_args = ['-fPIC']" \
    "c_link_args = ['-Wl,-z,max-page-size=16384']" \
    '' \
    '[properties]' \
    'needs_exe_wrapper = true' \
    '' \
    '[host_machine]' \
    "system = 'android'" \
    "cpu_family = 'aarch64'" \
    "cpu = 'armv8-a'" \
    "endian = 'little'" \
    > "$DAV1D_CROSS_FILE"
  run_meson setup "$DAV1D_BUILD" "$DAV1D_SOURCE" \
    --cross-file "$DAV1D_CROSS_FILE" \
    --prefix "$NATIVE_PREFIX" \
    --libdir lib \
    --buildtype release \
    --default-library static \
    -Denable_tools=false \
    -Denable_examples=false \
    -Denable_tests=false \
    -Denable_docs=false
  ninja -C "$DAV1D_BUILD" -j "$BUILD_JOBS"
  ninja -C "$DAV1D_BUILD" install

  printf 'Building FFmpeg and ffprobe for arm64-v8a\n' >&2
  # FFmpeg's MediaCodec encoders use the NDK API when no JVM is attached
  # (the ExactSize backend is an isolated child process). This exposes the
  # device's H.264/HEVC/VP8/VP9/AV1 hardware or vendor codec without a
  # Java-side frame-copy bridge.
  (
    cd "$FFMPEG_SOURCE"
    PKG_CONFIG_PATH="$NATIVE_PREFIX/lib/pkgconfig" ./configure \
      --prefix="$NATIVE_PREFIX" \
      --target-os=android \
      --arch=aarch64 \
      --enable-cross-compile \
      --cc="$CC" \
      --ar="$AR" \
      --ranlib="$RANLIB" \
      --strip="$STRIP" \
      --sysroot="$TOOLCHAIN/sysroot" \
      --extra-cflags="-I$NATIVE_PREFIX/include -fPIC" \
      --extra-ldflags="-L$NATIVE_PREFIX/lib -Wl,-z,max-page-size=16384" \
      --extra-libs="-landroid -lmediandk -llog" \
      --enable-gpl \
      --enable-jni \
      --enable-mediacodec \
      --enable-libx264 \
      --enable-libdav1d \
      --enable-static \
      --disable-shared \
      --disable-doc \
      --disable-debug \
      --disable-avdevice \
      --disable-network \
      --disable-sdl2 \
      --disable-vulkan \
      --disable-xlib \
      --disable-zlib \
      --disable-bzlib \
      --disable-lzma \
      --disable-iconv \
      --disable-securetransport \
      --disable-videotoolbox \
      --disable-audiotoolbox
    make -j"$BUILD_JOBS" ffmpeg ffprobe
  )
  touch "$NATIVE_MARKER"
fi

VERSION="$(sed -n 's/^const version = "\(.*\)"$/\1/p' "$PROJECT_ROOT/main.go")"
[ -n "$VERSION" ] || die "could not read the version from main.go"
VERSION_CODE="$(printf '%s' "$VERSION" | awk -F. '{printf "%d", ($1 * 10000) + ($2 * 100) + $3}')"
OUTPUT="$OUTPUT_DIR/ExactSize-$VERSION-android-arm64.apk"

clean_dir "$WORK_DIR"
mkdir -p "$WORK_DIR/resources" "$WORK_DIR/generated" "$WORK_DIR/classes" \
  "$WORK_DIR/dex" "$WORK_DIR/apk/lib/arm64-v8a"

printf 'Building Go backend for Android arm64\n' >&2
mkdir -p "$BUILD_ROOT/go-cache"
(
  cd "$PROJECT_ROOT"
  GOCACHE="$BUILD_ROOT/go-cache" CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build \
    -buildvcs=false -trimpath -ldflags="-s -w" \
    -o "$WORK_DIR/apk/lib/arm64-v8a/libexactsize.so" .
)
cp "$FFMPEG_SOURCE_REAL/ffmpeg" "$WORK_DIR/apk/lib/arm64-v8a/libffmpeg.so"
cp "$FFMPEG_SOURCE_REAL/ffprobe" "$WORK_DIR/apk/lib/arm64-v8a/libffprobe.so"
chmod 0755 "$WORK_DIR/apk/lib/arm64-v8a/"*.so

"$BUILD_TOOLS/aapt2" compile --dir "$PROJECT_ROOT/android/res" \
  -o "$WORK_DIR/resources/resources.zip"
"$BUILD_TOOLS/aapt2" link \
  -I "$ANDROID_JAR" \
  --manifest "$PROJECT_ROOT/android/AndroidManifest.xml" \
  --min-sdk-version "$MIN_SDK" \
  --target-sdk-version "$TARGET_SDK" \
  --version-code "$VERSION_CODE" \
  --version-name "$VERSION" \
  --java "$WORK_DIR/generated" \
  -R "$WORK_DIR/resources/resources.zip" \
  -o "$WORK_DIR/unsigned-base.apk"

"$JDK_HOME/bin/javac" --release 8 -encoding UTF-8 \
  -classpath "$ANDROID_JAR" \
  -d "$WORK_DIR/classes" \
  "$WORK_DIR/generated/io/exactsize/app/R.java" \
  "$PROJECT_ROOT/android/src/io/exactsize/app/"*.java
(
  cd "$WORK_DIR/classes"
  zip -q -r "$WORK_DIR/classes.jar" .
)
"$BUILD_TOOLS/d8" --min-api "$MIN_SDK" --lib "$ANDROID_JAR" \
  --output "$WORK_DIR/dex" "$WORK_DIR/classes.jar"
cp "$WORK_DIR/dex/classes.dex" "$WORK_DIR/apk/classes.dex"
cp "$WORK_DIR/unsigned-base.apk" "$WORK_DIR/unsigned.apk"
(
  cd "$WORK_DIR/apk"
  zip -q -0 -u "$WORK_DIR/unsigned.apk" classes.dex lib/arm64-v8a/libexactsize.so \
    lib/arm64-v8a/libffmpeg.so lib/arm64-v8a/libffprobe.so
)
"$BUILD_TOOLS/zipalign" -P 16 -f 4 "$WORK_DIR/unsigned.apk" "$WORK_DIR/aligned.apk"

KEYSTORE="${ANDROID_KEYSTORE:-$BUILD_ROOT/keys/exactsize-android.keystore}"
KEY_ALIAS="${ANDROID_KEY_ALIAS:-exactsize}"
STORE_PASSWORD="${ANDROID_STORE_PASSWORD:-changeit}"
KEY_PASSWORD="${ANDROID_KEY_PASSWORD:-$STORE_PASSWORD}"
if [ ! -f "$KEYSTORE" ]; then
  mkdir -p "$(dirname "$KEYSTORE")"
  "$JDK_HOME/bin/keytool" -genkeypair -noprompt \
    -keystore "$KEYSTORE" \
    -storepass "$STORE_PASSWORD" \
    -keypass "$KEY_PASSWORD" \
    -alias "$KEY_ALIAS" \
    -keyalg RSA -keysize 3072 -validity 10000 \
    -dname "CN=ExactSize Android, OU=Local Build, O=ExactSize, C=US"
fi

"$BUILD_TOOLS/apksigner" sign \
  --ks "$KEYSTORE" \
  --ks-key-alias "$KEY_ALIAS" \
  --ks-pass "pass:$STORE_PASSWORD" \
  --key-pass "pass:$KEY_PASSWORD" \
  --v3-signing-enabled true \
  --out "$OUTPUT" \
  "$WORK_DIR/aligned.apk"
"$BUILD_TOOLS/apksigner" verify --verbose --print-certs "$OUTPUT"
"$BUILD_TOOLS/zipalign" -c -P 16 4 "$OUTPUT"
printf '%s\n' "$OUTPUT"
