#!/usr/bin/env bash
# Renders one cut of the video from a recording of measure.py: the caption
# check, the stills sheet and claims, the video, verify.py and a 1 fps
# contact sheet, into ${RENDER_OUT:-race-render}:
#
#   check.<lang>.txt, frames.<lang>.png, claims.<lang>.json,
#   moejs-race.<lang>.mp4, verify.<lang>.json, contact.<lang>.png
#
# RACE_OUT is the recording (default race-out); RENDER_CPUS optionally pins
# the render (taskset list). Needs uv; the Python packages are pinned below.
#
# Usage: bench/race/render.sh zh|en
set -euo pipefail

L=${1:?usage: render.sh zh|en}
HERE=$(cd "$(dirname "$0")" && pwd)
R=$(cd "${RACE_OUT:-race-out}" && pwd)
mkdir -p "${RENDER_OUT:-race-render}"
OUT=$(cd "${RENDER_OUT:-race-render}" && pwd)
V=$OUT/moejs-race.$L.mp4
py() {
	${RENDER_CPUS:+taskset -c "$RENDER_CPUS"} uv run -q --no-project --with pillow==13.0.0 --with numpy==2.4.6 \
		--with imageio-ffmpeg==0.7.0 python "$@"
}

py "$HERE/video.py" "$R" "$V" --lang "$L" --check 0.1 | tee "$OUT/check.$L.txt"
grep -qx 'shrunk \[\]' "$OUT/check.$L.txt" && grep -qx 'short \[\]' "$OUT/check.$L.txt" ||
	{ echo "captions shrunk or too short" >&2; exit 1; }
py "$HERE/video.py" "$R" "$V" --lang "$L" --stills "$OUT/frames.$L.png" --claims "$OUT/claims.$L.json"
py "$HERE/video.py" "$R" "$V" --lang "$L"
rm -f "$V.audio.wav"
py "$HERE/verify.py" "$V" "$OUT/claims.$L.json" "$R" | tee "$OUT/verify.$L.json"
FF=$(py -c 'import imageio_ffmpeg; print(imageio_ffmpeg.get_ffmpeg_exe())')
${RENDER_CPUS:+taskset -c "$RENDER_CPUS"} "$FF" -v error -y -i "$V" \
	-vf 'fps=1,scale=320:180:flags=area,tile=10x12' -frames:v 1 "$OUT/contact.$L.png"
sha256sum "$V"
