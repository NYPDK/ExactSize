# Third-party notices

The distributed AppImage bundles FFmpeg and ffprobe static binaries obtained from the `ffmpeg-static` and `ffprobe-static` npm packages. FFmpeg is licensed under the GNU General Public License version 3 or later in the bundled configuration because it includes GPL codecs such as x264 and x265.

- FFmpeg project: <https://ffmpeg.org/>
- FFmpeg legal and source information: <https://ffmpeg.org/legal.html>
- Static build source: <https://johnvansickle.com/ffmpeg/>
- x264: <https://www.videolan.org/developers/x264.html>
- x265: <https://www.videolan.org/developers/x265.html>
- libaom: <https://aomedia.googlesource.com/aom/>
- libvpx: <https://chromium.googlesource.com/webm/libvpx/>

ExactSize invokes FFmpeg as a separate executable and does not link against FFmpeg libraries.
