# Third-party notices

The Linux AppImage and Windows portable ZIP bundle FFmpeg and ffprobe static binaries from BtbN's FFmpeg-Builds project under GNU GPL version 3 or later terms. The Android APK bundles FFmpeg and ffprobe built by `scripts/build-android.sh` from pinned FFmpeg, x264, and dav1d source archives under GNU GPL version 2 or later terms. Its MediaCodec encoders use the Android platform codec service; dav1d supplies the software AV1 input decoder. The GPL configurations are required by codecs such as x264 and x265.

- FFmpeg project: <https://ffmpeg.org/>
- FFmpeg legal and source information: <https://ffmpeg.org/legal.html>
- Desktop static build source and recipes: <https://github.com/BtbN/FFmpeg-Builds>
- FFmpeg source releases: <https://ffmpeg.org/releases/>
- x264: <https://www.videolan.org/developers/x264.html>
- dav1d: <https://code.videolan.org/videolan/dav1d>
- x265: <https://www.videolan.org/developers/x265.html>
- libaom: <https://aomedia.googlesource.com/aom/>
- libvpx: <https://chromium.googlesource.com/webm/libvpx/>

ExactSize invokes FFmpeg as a separate executable and does not link against FFmpeg libraries. Exact source versions, download URLs, and checksums used by the Android build are pinned in `scripts/build-android.sh`.
