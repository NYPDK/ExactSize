# Recompress-the-output last-resort fallback

## Problem

When the correction ladder in `encode.go` exhausts its attempt limit
(`correctionAttemptLimit` scales with the adaptive fps/resolution rungs for hardware
encoders; software ladders are capped at 3) and the output is still over the strict
target, the job fails with "could not bring the output below the
strict target after N attempts" (encode.go:660-664). On hardware encoders whose rate
control overshoots low bitrate requests (Pixel 7 Pro MediaCodec H.264 delivers ~2.5×
a 1.2 Mbps request), this failure is reachable in normal use.

A recompression pass over the already-compressed output usually lands where the
source could not: the intermediate has lower complexity, so the same encoder's rate
control lands lower, and the planner can budget from the intermediate's *measured*
stream sizes instead of estimates.

## Decision summary

- Trigger: automatic, when generation 1 (the existing ladder) gives up while a
  completed over-target output exists. Give-up means attempt exhaustion (where the
  final-attempt monitor disable guarantees the artifact) or the two GPU
  cannot-reach-target returns (where the artifact exists whenever the just-measured
  attempt ran to completion rather than being early-stopped). Infeasible-target
  errors ("target too small…") never chain — recompression cannot fix them. No new
  UI, setting, or API field.
- Depth: exactly one recompression generation. If generation 2 also fails, the job
  fails with the current error text plus a note that recompression was attempted.
- Scope: compress jobs only (`TargetBytes > 0`). The remux path is untouched.

## Flow

Wrap the existing attempt loop in an outer generation loop (max 2 generations),
inside the same job, work dir, and context:

1. **Generation 1** runs today's ladder with one change: on its *final* permitted
   attempt, `runFFmpeg` is called with `targetBytes = 0` (and no time-bounded
   projection), so the output-size monitor cannot early-kill it. The final attempt
   therefore always runs to completion and leaves a complete file at `tempOutput`.
   Earlier attempts keep the monitor exactly as today.
2. If that completed output is ≤ target, publish and complete as today (no behavior
   change on the success path).
3. If it is over target, do not return the failure error. Instead:
   - rename `tempOutput` to `chain-input.<container ext>` inside the same
     `.exactsize-work-*` dir (the "tmp save");
   - re-probe the chain input (duration is unchanged; resolution/fps reflect
     generation 1's output);
   - run the same ladder again as **generation 2** with the chain input as the
     ffmpeg input: fresh attempt counter, attempt limit recomputed via
     `correctionAttemptLimit` against the chain input's probe info, size monitor
     enabled on *all* attempts (no generation 3 needs an artifact).
4. Generation 2's success path (≤ target → publish → "completed") and verification
   are identical to today's.

Generation 2 inherits the request state as mutated by generation 1's corrections
(resolution/fps rungs already walked down). It starts from the operating point
generation 1 ended at, not from scratch.

## Generation-2 planning

- **Video budget:** generation 1's final attempt already produced a
  `probeOutputBreakdown` of the intermediate. Generation 2's starting video bitrate
  is `outputVideoBudgetKbps(TargetBytes, duration, breakdown)` — measured audio and
  container overhead, not the nominal estimate. `hardwareSafeBitrate` still applies
  for hardware encoders. If the breakdown probe failed, fall back to
  `calculateVideoBitrate` against the chain input's probe info.
- **Audio:** copied, not re-encoded (`-c:a copy`). The audio was already transcoded
  to the requested codec/bitrate in generation 1; copying avoids double degradation
  and makes the audio byte count exact. Implemented as an internal job flag consumed
  by the encode args builder (reusing the copy emission the remux path already has,
  encode.go:893). The flag is never serialized into the request JSON. Audio filters,
  bitrate, and channel-layout arguments are skipped when the flag is set. With audio
  "none", generation 1 already stripped audio and the flag changes nothing.
- **Two-pass:** generation 2 honors the request's two-pass setting unchanged.

## Status, cancellation, failure

- Same job, same `JobSnapshot` fields. The web UI and the Android
  `EncodingService` poller keep working with no changes.
- Generation 2 announces itself via the existing `attemptMessage` mechanism:
  "The encoder could not reach the target from the source. Recompressing the
  compressed output — quality may be reduced." Subsequent corrections within
  generation 2 use the existing correction messages.
- `Attempt` restarts at 1 for generation 2 (per-attempt progress/ETA semantics are
  unchanged).
- Cancellation needs no new code: every ffmpeg run and the loop head already check
  `j.ctx`.
- If generation 2 exhausts its ladder: return the existing error with the suffix
  ", even after recompressing the output" (both the hardware and software variants).

## Cleanup and bounds

- The chain input lives inside the existing `.exactsize-work-*` dir, so the existing
  `defer os.RemoveAll(tempDir)` removes it on every exit path. No files are written
  outside the work dir. (The separately-tracked storage-GC bug — uploads never
  deleted, no startup sweep — is out of scope here.)
- Disk overhead: at most one extra completed output at the final attempt's corrected
  bitrate, held only for the duration of generation 2.
- Wall clock: worst case roughly doubles (two ladders). Accepted as the cost of a
  last resort that replaces a hard failure.

## Testing

Table-driven where natural; test functions carry no doc comments (repo convention).

- Fallback triggers only when generation 1 exhausts attempts with a completed
  over-target output; a within-target final attempt completes as today.
- The size monitor is disabled only on generation 1's final attempt (first
  generation earlier attempts and all generation 2 attempts keep it).
- Generation 2's starting bitrate comes from the measured breakdown when available,
  and from the chain input's probe otherwise.
- The args builder emits `-c:a copy` and omits audio bitrate/filter args when the
  internal flag is set; emits today's audio args when it is not; emits `-an`
  handling unchanged for audio "none".
- Generation 2 failure returns the amended error text.
- The chain input is created inside the work dir (cleanup is covered by the existing
  `RemoveAll`).

## Release

Feature release: bump 1.13.0 → 1.14.0 in the three places — `version` const in
main.go, the AppImage filename in README's "Run it" block, and a new `<release>`
line atop `packaging/io.exactsize.ExactSize.metainfo.xml`.
