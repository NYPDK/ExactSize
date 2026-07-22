# Recompress-the-Output Fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When the correction ladder gives up with a completed over-target output, automatically recompress that output (one extra generation) instead of failing the job.

**Architecture:** The attempt ladder inside `Job.runEncode` (encode.go) is extracted into `Job.runAttemptLadder`, parameterized by a `ladderOptions` struct. `runEncode` gains an outer two-generation loop: generation 1 is today's ladder except its final attempt always runs to completion (size monitor off) and give-up sites return the completed over-target artifact instead of an error; generation 2 renames that artifact to a chain input inside the same work dir, re-probes it, copies audio instead of re-encoding, seeds its bitrate from the artifact's measured stream breakdown, and runs the same ladder once more.

**Tech Stack:** Go (single `main` package, flat repo), table-driven tests, fake ffmpeg/ffprobe shell scripts in `encode_test.go` (pattern: `TestEarlySizeCorrectionCancelsCleansAndRetries`).

**Spec:** `docs/superpowers/specs/2026-07-22-recompress-fallback-design.md`

## Global Constraints

- All work lands directly on `main` — no feature branches.
- Comment style: full-sentence "why" doc comments directly above every production top-level declaration (no blank line between). Test functions carry NO doc comments.
- Commit messages: short imperative subject, no body (plus the Claude trailers used in this repo's recent commits).
- Tests are table-driven where natural; run with `go test ./...` from the repo root.
- Failure-suffix copy, verbatim: `, even after recompressing the output`
- Recompression opening message, verbatim: `The encoder could not reach the target from the source. Recompressing the compressed output — quality may be reduced.`
- Version bump lands in Task 5 only: `1.13.0` → `1.14.0` in exactly three places (main.go `version` const, README "Run it" block, new `<release>` line atop `packaging/io.exactsize.ExactSize.metainfo.xml`).

---

### Task 1: AudioCopy request flag and args emission

**Files:**
- Modify: `encode.go` (EncodeRequest struct at ~line 72; `audioEncoderArgs` at ~line 1706)
- Test: `encode_test.go`

**Interfaces:**
- Produces: `EncodeRequest.AudioCopy bool` (json:"-") — later tasks set it for generation 2; `audioEncoderArgs` returns `["-c:a", "copy"]` when set (unless AudioCodec is "none", which still wins with `-an`).

- [ ] **Step 1: Write the failing test** — add to `encode_test.go` (no doc comment, repo convention):

```go
func TestAudioCopyEmitsCopyArgs(t *testing.T) {
	request := validTestRequest()
	request.AudioCopy = true
	args := audioEncoderArgs(request, VideoInfo{})
	if len(args) != 2 || args[0] != "-c:a" || args[1] != "copy" {
		t.Fatalf("audio copy args = %v; want [-c:a copy]", args)
	}

	request.AudioCodec = "none"
	args = audioEncoderArgs(request, VideoInfo{})
	if len(args) != 1 || args[0] != "-an" {
		t.Fatalf("audio none with copy flag = %v; want [-an]", args)
	}

	request = validTestRequest()
	request.AudioCopy = true
	request.TwoPass = false
	full := buildFFmpegArgs(request, VideoInfo{}, 1000, "/tmp/out.mp4", "/tmp/pass", 1, false)
	joined := strings.Join(full, " ")
	if !strings.Contains(joined, "-c:a copy") || strings.Contains(joined, "-b:a") {
		t.Fatalf("full args = %v; want -c:a copy and no -b:a", full)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestAudioCopyEmitsCopyArgs ./...`
Expected: FAIL — `request.AudioCopy undefined (type EncodeRequest has no field or method AudioCopy)`

- [ ] **Step 3: Implement.** In the `EncodeRequest` struct, after the `VAAPIDevice` line, add:

```go
	AudioCopy        bool    `json:"-"`
```

In `audioEncoderArgs`, immediately after the existing `if request.AudioCodec == "none" { return []string{"-an"} }` block, add:

```go
	// The recompression generation's audio already went through the requested
	// codec and bitrate once; copying it avoids a second lossy pass and keeps
	// the audio byte count identical to the measured intermediate.
	if request.AudioCopy {
		return []string{"-c:a", "copy"}
	}
```

- [ ] **Step 4: Run the full suite**

Run: `go test ./...`
Expected: PASS (all packages)

- [ ] **Step 5: Commit**

```bash
git add encode.go encode_test.go
git commit -m "Add internal audio-copy mode to encode args"
```

---

### Task 2: Extract the attempt ladder into runAttemptLadder (pure refactor)

**Files:**
- Modify: `encode.go` (`runEncode`, ~lines 427–805)

**Interfaces:**
- Produces (used by Tasks 3–4):

```go
type ladderOptions struct {
	startVideoKbps int
	chainCandidate bool
	openingMessage string
	failureSuffix  string
}

type overTargetArtifact struct {
	actualBytes  int64
	breakdown    OutputBreakdown
	hasBreakdown bool
}

func (j *Job) runAttemptLadder(ffmpeg, ffprobe string, encoder EncoderInfo, info VideoInfo, tempOutput, passLog string, opts ladderOptions) (*overTargetArtifact, error)
```

This task introduces the types and the extraction only. With a zero-value `ladderOptions`, behavior is bit-for-bit today's; the artifact return is always nil. The options take effect in Task 3.

- [ ] **Step 1: Add the two new types** directly above `func (j *Job) runEncode` with why-comments:

```go
// ladderOptions parameterizes one generation of the correction ladder. The
// zero value reproduces the classic single-generation behavior; generation 1
// of the recompression fallback sets chainCandidate, and generation 2 sets
// the remaining fields.
type ladderOptions struct {
	startVideoKbps int
	chainCandidate bool
	openingMessage string
	failureSuffix  string
}

// overTargetArtifact describes a completed encode that missed the strict
// target: the recompression fallback feeds it back in as the next
// generation's input, budgeted from its measured stream breakdown when the
// probe succeeded.
type overTargetArtifact struct {
	actualBytes  int64
	breakdown    OutputBreakdown
	hasBreakdown bool
}
```

- [ ] **Step 2: Perform the extraction.** In `runEncode`, everything from the line

```go
	startWidth, startHeight, autoResolution, encoderLimited := startingResolution(j.request, info)
```

through the function's final

```go
	return errors.New("compression ended unexpectedly")
```

moves verbatim into a new method placed directly after `runEncode`:

```go
// runAttemptLadder runs one generation of encode attempts with bitrate,
// frame-rate, and resolution corrections. It returns a nil artifact on
// success (the output was published), an artifact when opts.chainCandidate
// is set and the ladder gave up holding a completed over-target output, and
// an error otherwise.
func (j *Job) runAttemptLadder(ffmpeg, ffprobe string, encoder EncoderInfo, info VideoInfo, tempOutput, passLog string, opts ladderOptions) (*overTargetArtifact, error) {
```

Three mechanical edits to the moved body:

1. Every `return <expr>` becomes `return nil, <expr>` (this covers `return err`, `return errors.New(...)`, `return fmt.Errorf(...)`, `return j.ctx.Err()`), and the success-path `return nil` after the "completed" snapshot becomes `return nil, nil`.
2. The work-dir block does NOT move — delete it from the moved body and leave it in `runEncode` (Step 3). That block is:

```go
	outputDir := filepath.Dir(j.request.Output)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output folder: %w", err)
	}
	tempDir, err := os.MkdirTemp(outputDir, ".exactsize-work-")
	if err != nil {
		return fmt.Errorf("create temporary work folder: %w", err)
	}
	defer os.RemoveAll(tempDir)
	tempOutput := filepath.Join(tempDir, "output."+containerExtension(j.request.Container))
	passLog := filepath.Join(tempDir, "pass")
```

3. Because the ladder now receives `tempOutput`/`passLog` as parameters, deleting the work-dir block leaves the rest of the moved body contiguous and in its original order. No other statements move or change.

- [ ] **Step 3: Rewrite the tail of `runEncode`.** Everything after the encoder-compatibility check (`return errors.New("the selected video encoder is not compatible with the selected codec")` block) becomes:

```go
	outputDir := filepath.Dir(j.request.Output)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output folder: %w", err)
	}
	tempDir, err := os.MkdirTemp(outputDir, ".exactsize-work-")
	if err != nil {
		return fmt.Errorf("create temporary work folder: %w", err)
	}
	defer os.RemoveAll(tempDir)
	tempOutput := filepath.Join(tempDir, "output."+containerExtension(j.request.Container))
	passLog := filepath.Join(tempDir, "pass")

	_, err = j.runAttemptLadder(ffmpeg, ffprobe, encoder, info, tempOutput, passLog, ladderOptions{})
	return err
```

(The `// ResolutionHeight selects the starting size...` comment travels with the `startingResolution` line into the ladder body; nothing of it remains in `runEncode`.)

- [ ] **Step 4: Run the full suite — this is the refactor gate**

Run: `go test ./...`
Expected: PASS with zero test changes. If anything fails, the extraction changed behavior — fix before proceeding.

- [ ] **Step 5: Commit**

```bash
git add encode.go
git commit -m "Extract encode attempt ladder into runAttemptLadder"
```

---

### Task 3: Chain decision helpers and ladder option behavior

**Files:**
- Modify: `encode.go` (`runAttemptLadder` body; two new top-level funcs near `outputVideoBudgetKbps`)
- Test: `encode_test.go`

**Interfaces:**
- Consumes: `ladderOptions`, `overTargetArtifact` (Task 2).
- Produces (used by Task 4):

```go
func chainOrFail(opts ladderOptions, artifact *overTargetArtifact, failure error) (*overTargetArtifact, error)
func chainStartKbps(targetBytes int64, duration float64, artifact *overTargetArtifact, hardware bool) (int, error)
```

- [ ] **Step 1: Write the failing unit tests** in `encode_test.go`:

```go
func TestChainOrFailRoutesArtifactsAndSuffixes(t *testing.T) {
	artifact := &overTargetArtifact{actualBytes: 12}
	got, err := chainOrFail(ladderOptions{chainCandidate: true}, artifact, errors.New("boom"))
	if err != nil || got != artifact {
		t.Fatalf("chain candidate with artifact = %v, %v; want artifact, nil", got, err)
	}
	got, err = chainOrFail(ladderOptions{chainCandidate: true}, nil, errors.New("boom"))
	if got != nil || err == nil || err.Error() != "boom" {
		t.Fatalf("chain candidate without artifact = %v, %v; want nil, boom", got, err)
	}
	got, err = chainOrFail(ladderOptions{failureSuffix: ", even after recompressing the output"}, nil, errors.New("boom"))
	if got != nil || err == nil || err.Error() != "boom, even after recompressing the output" {
		t.Fatalf("suffixed failure = %v, %v", got, err)
	}
}

func TestChainStartKbpsUsesMeasuredBreakdown(t *testing.T) {
	noBreakdown := &overTargetArtifact{actualBytes: 12_000_000}
	kbps, err := chainStartKbps(10_000_000, 10, noBreakdown, false)
	if err != nil || kbps != 0 {
		t.Fatalf("no breakdown = %d, %v; want 0, nil", kbps, err)
	}

	measured := &overTargetArtifact{
		actualBytes:  12_000_000,
		hasBreakdown: true,
		breakdown:    OutputBreakdown{AudioBytes: 1_000_000, MuxBytes: 200_000},
	}
	want := outputVideoBudgetKbps(10_000_000, 10, measured.breakdown)
	kbps, err = chainStartKbps(10_000_000, 10, measured, false)
	if err != nil || kbps != want {
		t.Fatalf("software budget = %d, %v; want %d", kbps, err, want)
	}
	kbps, err = chainStartKbps(10_000_000, 10, measured, true)
	if err != nil || kbps != hardwareSafeBitrate(want) {
		t.Fatalf("hardware budget = %d, %v; want %d", kbps, err, hardwareSafeBitrate(want))
	}

	tiny := &overTargetArtifact{
		actualBytes:  1_100_000,
		hasBreakdown: true,
		breakdown:    OutputBreakdown{AudioBytes: 990_000},
	}
	if _, err = chainStartKbps(1_000_000, 100, tiny, false); err == nil {
		t.Fatal("expected too-small error for sub-minimum video budget")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestChainOrFail|TestChainStartKbps' ./...`
Expected: FAIL — `undefined: chainOrFail` / `undefined: chainStartKbps`

- [ ] **Step 3: Implement the helpers**, placed directly after `outputVideoBudgetKbps` in encode.go:

```go
// chainOrFail is the single decision point every ladder give-up site funnels
// through: generation 1 hands a completed over-target output to the
// recompression fallback instead of failing, and generation 2 fails with the
// ladder's error suffixed so the user knows the last resort already ran.
func chainOrFail(opts ladderOptions, artifact *overTargetArtifact, failure error) (*overTargetArtifact, error) {
	if opts.chainCandidate && artifact != nil {
		return artifact, nil
	}
	if opts.failureSuffix != "" {
		return nil, errors.New(failure.Error() + opts.failureSuffix)
	}
	return nil, failure
}

// chainStartKbps seeds the recompression generation's bitrate from the
// intermediate's measured stream breakdown; a zero result tells the ladder to
// fall back to its own calculateVideoBitrate estimate because the breakdown
// probe failed.
func chainStartKbps(targetBytes int64, duration float64, artifact *overTargetArtifact, hardware bool) (int, error) {
	if !artifact.hasBreakdown {
		return 0, nil
	}
	kbps := outputVideoBudgetKbps(targetBytes, duration, artifact.breakdown)
	if hardware {
		kbps = hardwareSafeBitrate(kbps)
	}
	if kbps < minimumVideoBitrateKbps {
		return 0, errors.New("the target is too small after measured audio and container overhead")
	}
	return kbps, nil
}
```

- [ ] **Step 4: Wire the options into `runAttemptLadder`.** Five focused edits:

(a) Seed bitrate override — the prelude currently reads:

```go
	videoKbps, err := calculateVideoBitrate(j.request, info)
	if err != nil {
		return nil, err
	}
```

becomes:

```go
	videoKbps, err := calculateVideoBitrate(j.request, info)
	if opts.startVideoKbps > 0 {
		// The recompression generation budgets from the intermediate's
		// measured breakdown; the nominal estimate only matters when that
		// probe failed.
		videoKbps = opts.startVideoKbps
	} else if err != nil {
		return nil, err
	}
```

and the `plannedFPS` recompute a few lines below,

```go
	if plannedFPS {
		if videoKbps, err = calculateVideoBitrate(j.request, info); err != nil {
			return nil, err
		}
	}
```

becomes:

```go
	if plannedFPS && opts.startVideoKbps == 0 {
		if videoKbps, err = calculateVideoBitrate(j.request, info); err != nil {
			return nil, err
		}
	}
```

(b) Opening message — the `attemptMessage := ""` initialization becomes:

```go
	attemptMessage := opts.openingMessage
```

and in the `if plannedFPS || plannedResolution` block just below, the assignment `attemptMessage = fmt.Sprintf(...)` becomes:

```go
		attemptMessage = strings.TrimSpace(opts.openingMessage + " " + fmt.Sprintf(
```

(keeping the existing format string and arguments, and adding the matching closing parenthesis).

(c) Final-attempt monitor disable — directly after

```go
		timeBoundedProjection := encoder.Hardware && needsTimeBoundedHardwareProjection(j.request, info, videoKbps)
```

add:

```go
		// The recompression fallback needs a complete file to feed back in,
		// so generation 1's final attempt runs without the size monitor: the
		// output either fits or becomes the chain input.
		monitorTargetBytes := j.request.TargetBytes
		if opts.chainCandidate && attempt == maximumAttempts {
			monitorTargetBytes = 0
			timeBoundedProjection = false
		}
```

and in the two monitored `runFFmpeg` calls (the two-pass second-pass call and the single-pass call), replace the `j.request.TargetBytes` argument with `monitorTargetBytes`.

(d) Capture the breakdown per attempt — before the `if earlyCorrection == nil && actualBytes <= j.request.TargetBytes` success check, the measurement block currently ends with the `else` branch assigning `actualVideoKbps`/`availableVideoKbps` from `breakdown`. Hoist storage above the measurement chain:

```go
		measuredBreakdown := OutputBreakdown{}
		haveBreakdown := false
```

and inside the successful-probe `else` branch add, before the existing assignments:

```go
			measuredBreakdown = breakdown
			haveBreakdown = true
```

(e) Route the give-up sites through `chainOrFail` — three sites. First build the artifact value the sites share, placed immediately before the `if attempt == maximumAttempts` block:

```go
		// Only a completed attempt leaves a full file behind; an
		// early-corrected attempt was deliberately killed and deleted.
		var completedArtifact *overTargetArtifact
		if earlyCorrection == nil {
			completedArtifact = &overTargetArtifact{
				actualBytes:  actualBytes,
				breakdown:    measuredBreakdown,
				hasBreakdown: haveBreakdown,
			}
		}
```

Then:

```go
		if attempt == maximumAttempts {
			if encoder.Hardware {
				return chainOrFail(opts, completedArtifact, fmt.Errorf("could not bring the output below the strict target after %d attempts; a software encoder, a lower resolution, or a different video codec may reach targets the GPU cannot", maximumAttempts))
			}
			return chainOrFail(opts, completedArtifact, fmt.Errorf("could not bring the output below the strict target after %d attempts", maximumAttempts))
		}
```

and the two GPU give-up returns inside the hardware-correction block:

```go
				if !autoResolution {
					return chainOrFail(opts, completedArtifact, errors.New("the GPU encoder cannot reach this target at the selected resolution and frame-rate range; lower the minimum FPS or resolution, switch to a software encoder, or raise the target"))
				}
```

```go
				if !ok {
					return chainOrFail(opts, completedArtifact, errors.New("the GPU encoder cannot reach this target even at reduced resolution; switch to a software encoder, try a different video codec, or raise the target"))
				}
```

The remaining error returns (too-small errors, ffmpeg failures, context cancellation, `could not exhaust the hardware encoder's bitrate options`) stay plain — recompressing cannot fix an infeasible target, and invariant failures should surface as-is.

- [ ] **Step 5: Run tests**

Run: `go test ./...`
Expected: PASS (new unit tests plus the untouched suite — `runEncode` still passes `ladderOptions{}`, so nothing chains yet).

- [ ] **Step 6: Commit**

```bash
git add encode.go encode_test.go
git commit -m "Teach the attempt ladder to hand back over-target artifacts"
```

---

### Task 4: Generation loop in runEncode with end-to-end tests

**Files:**
- Modify: `encode.go` (`runEncode` tail from Task 2 Step 3)
- Test: `encode_test.go`

**Interfaces:**
- Consumes: `runAttemptLadder`, `chainStartKbps`, `ladderOptions`, `overTargetArtifact`, `EncodeRequest.AudioCopy`.

- [ ] **Step 1: Write the two failing integration tests** in `encode_test.go`, modeled on `TestEarlySizeCorrectionCancelsCleansAndRetries` (fake ffprobe prints a fixed probe document; fake ffmpeg counts invocations in `EXACTSIZE_TEST_ATTEMPT_FILE` and appends its argv to `EXACTSIZE_TEST_ARGS_LOG`, one line per invocation; libx264 is a software encoder, so the ladder caps at 3 attempts per generation):

```go
func TestRecompressionFallbackReachesTarget(t *testing.T) {
	tempDir := t.TempDir()
	input := filepath.Join(tempDir, "input.mp4")
	output := filepath.Join(tempDir, "output.mp4")
	if err := os.WriteFile(input, []byte("fake input"), 0o644); err != nil {
		t.Fatal(err)
	}

	ffprobe := filepath.Join(tempDir, "fake-ffprobe")
	probeDocument := `{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"avg_frame_rate":"30/1","pix_fmt":"yuv420p"}],"format":{"duration":"10","size":"10","format_name":"mov,mp4"}}`
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nprintf '%s\\n' '"+probeDocument+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	attemptFile := filepath.Join(tempDir, "attempt")
	argsLog := filepath.Join(tempDir, "args.log")
	t.Setenv("EXACTSIZE_TEST_ATTEMPT_FILE", attemptFile)
	t.Setenv("EXACTSIZE_TEST_ARGS_LOG", argsLog)
	ffmpeg := filepath.Join(tempDir, "fake-ffmpeg")
	ffmpegScript := `#!/bin/sh
attempt=0
if [ -f "$EXACTSIZE_TEST_ATTEMPT_FILE" ]; then
  read attempt < "$EXACTSIZE_TEST_ATTEMPT_FILE"
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" > "$EXACTSIZE_TEST_ATTEMPT_FILE"
printf '%s\n' "$*" >> "$EXACTSIZE_TEST_ARGS_LOG"
for output do :; done
if [ "$attempt" -lt 3 ]; then
  truncate -s 11000000 "$output"
  printf 'out_time_us=10000000\nprogress=end\n'
elif [ "$attempt" -eq 3 ]; then
  truncate -s 12000000 "$output"
  printf 'out_time_us=1000000\nprogress=end\n'
else
  truncate -s 9000000 "$output"
  printf 'out_time_us=10000000\nprogress=end\n'
fi
`
	if err := os.WriteFile(ffmpeg, []byte(ffmpegScript), 0o755); err != nil {
		t.Fatal(err)
	}

	request := validTestRequest()
	request.Input = input
	request.Output = output
	request.TargetBytes = 10_000_000
	request.AudioCodec = "none"
	request.AudioBitrateKbps = 0
	request.TwoPass = false
	job := newJob(request)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job.ctx = ctx
	job.cancel = cancel

	if err := job.runEncode(ffmpeg, ffprobe); err != nil {
		t.Fatalf("fallback run failed: %v", err)
	}
	status := job.snapshot()
	if status.State != "completed" || status.Attempt != 1 {
		t.Fatalf("expected generation 2 to complete on its first attempt, got %+v", status)
	}
	if stat, err := os.Stat(output); err != nil || stat.Size() != 9_000_000 {
		t.Fatalf("published output = %v, %v; want 9000000 bytes", stat, err)
	}
	logBytes, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 ffmpeg invocations, got %d:\n%s", len(lines), logBytes)
	}
	for _, line := range lines[:3] {
		if !strings.Contains(line, input) {
			t.Fatalf("generation 1 should encode the original input: %s", line)
		}
	}
	if !strings.Contains(lines[3], "chain-input.mp4") {
		t.Fatalf("generation 2 should encode the chain input: %s", lines[3])
	}
	if !strings.Contains(lines[3], "-an") {
		t.Fatalf("audio none must stay -an in generation 2: %s", lines[3])
	}
	workDirs, err := filepath.Glob(filepath.Join(tempDir, ".exactsize-work-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workDirs) != 0 {
		t.Fatalf("work directory (and chain input) not cleaned up: %v", workDirs)
	}
}

func TestRecompressionFallbackStillOverTargetFails(t *testing.T) {
	tempDir := t.TempDir()
	input := filepath.Join(tempDir, "input.mp4")
	output := filepath.Join(tempDir, "output.mp4")
	if err := os.WriteFile(input, []byte("fake input"), 0o644); err != nil {
		t.Fatal(err)
	}

	ffprobe := filepath.Join(tempDir, "fake-ffprobe")
	probeDocument := `{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"avg_frame_rate":"30/1","pix_fmt":"yuv420p"}],"format":{"duration":"10","size":"10","format_name":"mov,mp4"}}`
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nprintf '%s\\n' '"+probeDocument+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	attemptFile := filepath.Join(tempDir, "attempt")
	t.Setenv("EXACTSIZE_TEST_ATTEMPT_FILE", attemptFile)
	ffmpeg := filepath.Join(tempDir, "fake-ffmpeg")
	ffmpegScript := `#!/bin/sh
attempt=0
if [ -f "$EXACTSIZE_TEST_ATTEMPT_FILE" ]; then
  read attempt < "$EXACTSIZE_TEST_ATTEMPT_FILE"
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" > "$EXACTSIZE_TEST_ATTEMPT_FILE"
for output do :; done
truncate -s 11000000 "$output"
printf 'out_time_us=10000000\nprogress=end\n'
`
	if err := os.WriteFile(ffmpeg, []byte(ffmpegScript), 0o755); err != nil {
		t.Fatal(err)
	}

	request := validTestRequest()
	request.Input = input
	request.Output = output
	request.TargetBytes = 10_000_000
	request.AudioCodec = "none"
	request.AudioBitrateKbps = 0
	request.TwoPass = false
	job := newJob(request)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job.ctx = ctx
	job.cancel = cancel

	err := job.runEncode(ffmpeg, ffprobe)
	if err == nil || !strings.Contains(err.Error(), ", even after recompressing the output") {
		t.Fatalf("expected suffixed give-up error, got %v", err)
	}
	attemptBytes, readErr := os.ReadFile(attemptFile)
	if readErr != nil || strings.TrimSpace(string(attemptBytes)) != "6" {
		t.Fatalf("expected 3+3 ffmpeg invocations, got %q (%v)", attemptBytes, readErr)
	}
	if _, statErr := os.Stat(output); statErr == nil {
		t.Fatal("an over-target output must never be published")
	}
}
```

In the first test's script, attempt 3 (generation 1's final attempt) reports only 1 of 10 seconds encoded while already 2 MB over target — exactly the shape the output-size monitor kills. Reaching invocation 4 therefore proves the monitor is disabled on the final chain-candidate attempt; if it were active, the attempt would early-correct, leave no artifact behind, and the run would fail with today's un-suffixed error. The 12 MB attempt-3 output also fails the fake ffprobe's breakdown parse, so generation 2 exercises the `chainStartKbps == 0` → `calculateVideoBitrate` fallback path from the spec.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestRecompressionFallback ./...`
Expected: FAIL — first test errors with today's give-up message after 3 invocations (no generation 2 runs), second test's error lacks the suffix.

- [ ] **Step 3: Implement the generation loop.** Replace the two-line tail of `runEncode` from Task 2 Step 3 (`_, err = j.runAttemptLadder(...)` / `return err`) with:

```go
	artifact, err := j.runAttemptLadder(ffmpeg, ffprobe, encoder, info, tempOutput, passLog, ladderOptions{chainCandidate: true})
	if err != nil || artifact == nil {
		return err
	}

	// Last resort: the ladder gave up from the source, but its completed
	// over-target output is a simpler input the same settings can usually
	// squeeze under the ceiling. One extra generation, then give up for real.
	chainInput := filepath.Join(tempDir, "chain-input."+containerExtension(j.request.Container))
	if err := os.Rename(tempOutput, chainInput); err != nil {
		return fmt.Errorf("stash recompression input: %w", err)
	}
	chainInfo, err := probeVideo(j.ctx, ffprobe, chainInput)
	if err != nil {
		return err
	}
	startKbps, err := chainStartKbps(j.request.TargetBytes, chainInfo.Duration, artifact, encoder.Hardware)
	if err != nil {
		return err
	}
	j.request.Input = chainInput
	if j.request.AudioCodec != "none" {
		j.request.AudioCopy = true
	}
	opening := "The encoder could not reach the target from the source. Recompressing the compressed output — quality may be reduced."
	j.set(func(status *JobSnapshot) {
		status.Phase = "Correcting"
		status.Message = opening
	})
	_, err = j.runAttemptLadder(ffmpeg, ffprobe, encoder, chainInfo, tempOutput, passLog, ladderOptions{
		startVideoKbps: startKbps,
		openingMessage: opening,
		failureSuffix:  ", even after recompressing the output",
	})
	return err
```

- [ ] **Step 4: Run the full suite**

Run: `go test ./...`
Expected: PASS — both new tests plus the whole existing suite (in particular `TestEarlySizeCorrectionCancelsCleansAndRetries`, which now runs with `chainCandidate: true` and must still complete on its second attempt without chaining).

- [ ] **Step 5: Commit**

```bash
git add encode.go encode_test.go
git commit -m "Recompress the over-target output as a last resort"
```

---

### Task 5: Version bump and release notes

**Files:**
- Modify: `main.go:27`, `README.md:12-13`, `packaging/io.exactsize.ExactSize.metainfo.xml:26`

- [ ] **Step 1: Bump the three sites.**

main.go:

```go
const version = "1.14.0"
```

README.md "Run it" block:

```bash
chmod +x ExactSize-1.14.0-x86_64.AppImage
./ExactSize-1.14.0-x86_64.AppImage
```

packaging/io.exactsize.ExactSize.metainfo.xml — add above the existing 1.13.0 line, matching its exact formatting:

```xml
    <release version="1.14.0" date="2026-07-22"/>
```

- [ ] **Step 2: Run the full suite** (server_test.go contract tests assert version consistency patterns)

Run: `go test ./...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add main.go README.md packaging/io.exactsize.ExactSize.metainfo.xml
git commit -m "Bump version to 1.14.0"
```
