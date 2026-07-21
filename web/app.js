"use strict";

const token = new URLSearchParams(window.location.search).get("token") || "";

const $ = (id) => document.getElementById(id);

const elements = {
  appHeader: $("appHeader"),
  appVersion: $("appVersion"),
  runtimeStatus: $("runtimeStatus"),
  runtimeLabel: $("runtimeLabel"),
  updateButton: $("updateButton"),
  updateVersion: $("updateVersion"),
  updateOverlay: $("updateOverlay"),
  updateCloseButton: $("updateCloseButton"),
  updateCurrentVersion: $("updateCurrentVersion"),
  updateLatestVersion: $("updateLatestVersion"),
  updateSummary: $("updateSummary"),
  updateNote: $("updateNote"),
  updateDownloadProgress: $("updateDownloadProgress"),
  updateStatusMessage: $("updateStatusMessage"),
  updateProgressTrack: $("updateProgressTrack"),
  updateProgressFill: $("updateProgressFill"),
  updateProgressPercent: $("updateProgressPercent"),
  updateOpenAssetButton: $("updateOpenAssetButton"),
  updateInstallButton: $("updateInstallButton"),
  themeColor: $("themeColor"),
  themeToggle: $("themeToggle"),
  minimizeButton: $("minimizeButton"),
  quitButton: $("quitButton"),
  resizeGrip: $("resizeGrip"),
  resolution: $("resolution"),
  autoResolution: $("autoResolution"),
  frameRateRow: $("frameRateRow"),
  frameRateSlider: $("frameRateSlider"),
  frameRateMinimum: $("frameRateMinimum"),
  frameRateMaximum: $("frameRateMaximum"),
  frameRateValue: $("frameRateValue"),
  dropZone: $("dropZone"),
  fileInput: $("fileInput"),
  emptyInputCopy: $("emptyInputCopy"),
  fileSummary: $("fileSummary"),
  fileName: $("fileName"),
  fileMeta: $("fileMeta"),
  changeFile: $("changeFile"),
  sizePresets: $("sizePresets"),
  targetValue: $("targetValue"),
  targetUnit: $("targetUnit"),
  container: $("container"),
  videoCodec: $("videoCodec"),
  encoder: $("encoder"),
  preset: $("preset"),
  twoPass: $("twoPass"),
  twoPassRow: $("twoPassRow"),
  audioCodec: $("audioCodec"),
  audioBitrate: $("audioBitrate"),
  audioBitrateRow: $("audioBitrateRow"),
  audioChannels: $("audioChannels"),
  audioChannelsRow: $("audioChannelsRow"),
  audioInfo: $("audioInfo"),
  bitrateEstimate: $("bitrateEstimate"),
  outputPath: $("outputPath"),
  browseOutput: $("browseOutput"),
  compressButton: $("compressButton"),
  remuxButton: $("remuxButton"),
  progressPanel: $("progressPanel"),
  progressTrack: $("progressTrack"),
  progressFill: $("progressFill"),
  progressPercent: $("progressPercent"),
  progressEyeline: $("progressEyeline"),
  progressHeading: $("progressHeading"),
  progressPassLabel: $("progressPassLabel"),
  progressPass: $("progressPass"),
  progressSize: $("progressSize"),
  progressElapsed: $("progressElapsed"),
  progressRemainingStat: $("progressRemainingStat"),
  progressRemaining: $("progressRemaining"),
  progressMessage: $("progressMessage"),
  cancelButton: $("cancelButton"),
  showOutputButton: $("showOutputButton"),
  confirmOverlay: $("confirmOverlay"),
  confirmStay: $("confirmStay"),
  confirmQuit: $("confirmQuit"),
  errorFloat: $("errorFloat"),
  errorDismiss: $("errorDismiss"),
  errorMessage: $("errorMessage"),
  toast: $("toast"),
};

const state = {
  appStatus: null,
  input: null,
  inputDisplayName: "",
  inputIsTemp: false,
  encoding: false,
  pollingTimer: null,
  toastTimer: null,
  availableUpdate: null,
  updatePollingTimer: null,
  updateInstalling: false,
  notifiedCorrectionAttempt: 0,
};

const containerCodecs = {
  mp4: ["h264", "h265", "h266", "av1"],
  mkv: ["h264", "h265", "h266", "av1", "av2", "vp9"],
  webm: ["av1", "vp9"],
  mov: ["h264", "h265", "av1"],
};

const containerAudio = {
  mp4: ["aac", "mp3", "none"],
  mkv: ["aac", "opus", "vorbis", "mp3", "none"],
  webm: ["opus", "vorbis", "none"],
  mov: ["aac", "mp3", "none"],
};

const codecLabels = {
  h264: "H.264 / AVC",
  h265: "H.265 / HEVC",
  h266: "H.266 / VVC",
  av1: "AV1",
  av2: "AV2",
  vp9: "VP9",
};

const audioLabels = {
  aac: "AAC",
  opus: "Opus",
  vorbis: "Vorbis",
  mp3: "MP3",
  none: "No audio",
};

const audioBitrates = {
  aac: [16, 24, 32, 48, 64, 96, 128, 160, 192, 256, 320],
  opus: [6, 8, 12, 16, 24, 32, 48, 64, 96, 128, 160, 192, 256, 320],
  vorbis: [48, 64, 96, 128, 160, 192, 256, 320],
  mp3: [32, 48, 64, 96, 128, 160, 192, 256, 320],
};

const extensions = {
  mp4: "mp4",
  mkv: "mkv",
  webm: "webm",
  mov: "mov",
};

async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  headers.set("X-ExactSize-Token", token);
  if (options.body && !(options.body instanceof FormData)) {
    headers.set("Content-Type", "application/json");
  }
  const response = await fetch(path, { ...options, headers });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(payload.error || `Request failed (${response.status})`);
  }
  return payload;
}

async function initialize() {
  applyTheme("dark");
  bindEvents();
  setRuntime("Starting…", "busy");
  try {
    state.appStatus = await api("/api/status");
    if (state.appStatus.version) {
      elements.appVersion.textContent = `v${state.appStatus.version}`;
      elements.appVersion.hidden = false;
    }
    if (!state.appStatus.encoders?.length) {
      throw new Error("The bundled FFmpeg build has no supported video encoders.");
    }
    refreshCompatibility();
    refreshResolutionOptions();
    refreshFrameRateControl(true);
    setupFramelessWindow();
    updateSizePresetSelection();
    setRuntime("Ready", "ready");
    updateFormState();
    void checkForUpdates();
  } catch (error) {
    setRuntime("FFmpeg error", "error");
    showError(error);
  }
}

function applyTheme(theme) {
  const isLight = theme === "light";
  document.documentElement.dataset.theme = isLight ? "light" : "dark";
  elements.themeToggle.setAttribute("aria-pressed", String(isLight));
  const label = isLight ? "Switch to dark mode" : "Switch to light mode";
  elements.themeToggle.setAttribute("aria-label", label);
  elements.themeToggle.title = label;
  elements.themeColor.content = isLight ? "#f7f8f4" : "#1b1c19";
}

function toggleTheme() {
  applyTheme(document.documentElement.dataset.theme === "light" ? "dark" : "light");
}

async function checkForUpdates() {
  try {
    const update = await api("/api/update/check");
    if (!update.updateAvailable) return;
    state.availableUpdate = update;
    showUpdateAvailable(update);
  } catch {
    // Update checks are opportunistic; offline use must stay fully functional.
  }
}

function showUpdateAvailable(update) {
  const label = `v${update.latestVersion}`;
  elements.updateVersion.textContent = label;
  elements.updateButton.setAttribute("aria-label", `Update available: ExactSize ${label}. Review update options.`);
  elements.updateButton.title = `Review the ExactSize ${label} update`;
  elements.updateButton.hidden = false;
}

function openUpdateDialog() {
  const update = state.availableUpdate;
  if (!update) return;
  elements.updateCurrentVersion.textContent = `v${update.currentVersion}`;
  elements.updateLatestVersion.textContent = `v${update.latestVersion}`;
  elements.updateSummary.textContent = `${update.assetName || "The new AppImage"}${update.assetSize ? ` · ${formatBytes(update.assetSize)}` : ""}`;
  elements.updateDownloadProgress.hidden = true;
  elements.updateOpenAssetButton.disabled = !update.assetName;
  elements.updateInstallButton.disabled = !update.canSelfUpdate;
  elements.updateInstallButton.textContent = "Download & install";
  elements.updateNote.textContent = update.canSelfUpdate
    ? "The AppImage is downloaded beside the running copy and replaces it only after its size, SHA-256 digest, and format are verified."
    : (update.installReason || "Automatic installation is unavailable, but the exact release asset can still be opened.");
  elements.updateOverlay.hidden = false;
  (update.canSelfUpdate ? elements.updateInstallButton : elements.updateOpenAssetButton).focus();
}

function closeUpdateDialog() {
  if (state.updateInstalling) return;
  elements.updateOverlay.hidden = true;
}

async function openUpdateAsset() {
  elements.updateOpenAssetButton.disabled = true;
  try {
    await api("/api/update/open-asset", { method: "POST" });
    showToast(`Opened ${state.availableUpdate?.assetName || "the exact AppImage asset"}`);
  } catch (error) {
    showToast(error.message, true);
  } finally {
    elements.updateOpenAssetButton.disabled = false;
  }
}

async function installUpdate() {
  state.updateInstalling = true;
  elements.updateInstallButton.disabled = true;
  elements.updateOpenAssetButton.disabled = true;
  elements.updateCloseButton.disabled = true;
  elements.updateDownloadProgress.hidden = false;
  elements.updateStatusMessage.textContent = "Preparing verified download…";
  renderUpdateProgress(0, 1);
  try {
    const status = await api("/api/update/install", { method: "POST" });
    renderUpdateStatus(status);
    pollUpdateStatus();
  } catch (error) {
    state.updateInstalling = false;
    elements.updateStatusMessage.textContent = error.message;
    elements.updateNote.textContent = "Nothing was replaced. You can retry or open the exact AppImage asset instead.";
    elements.updateInstallButton.disabled = false;
    elements.updateOpenAssetButton.disabled = false;
    elements.updateCloseButton.disabled = false;
  }
}

function pollUpdateStatus() {
  window.clearTimeout(state.updatePollingTimer);
  state.updatePollingTimer = window.setTimeout(async () => {
    state.updatePollingTimer = null;
    try {
      const status = await api("/api/update/status");
      renderUpdateStatus(status);
      if (status.state === "downloading") {
        pollUpdateStatus();
        return;
      }
      if (status.state === "installed") {
        elements.updateInstallButton.textContent = "Installed";
        elements.updateVersion.textContent = "Restart required";
        elements.updateNote.textContent = "Close and reopen ExactSize when convenient. The current session can continue running safely.";
      } else {
        elements.updateInstallButton.disabled = false;
        elements.updateOpenAssetButton.disabled = false;
        elements.updateNote.textContent = "Nothing was replaced. You can retry or open the exact AppImage asset instead.";
      }
      state.updateInstalling = false;
      elements.updateCloseButton.disabled = false;
    } catch (error) {
      state.updateInstalling = false;
      elements.updateStatusMessage.textContent = error.message;
      elements.updateInstallButton.disabled = false;
      elements.updateOpenAssetButton.disabled = false;
      elements.updateCloseButton.disabled = false;
    }
  }, 450);
}

function renderUpdateStatus(status) {
  elements.updateStatusMessage.textContent = status.message || "Downloading the verified AppImage…";
  renderUpdateProgress(status.downloadedBytes || 0, status.totalBytes || 1);
}

function renderUpdateProgress(downloaded, total) {
  const percent = Math.max(0, Math.min(100, Math.round((downloaded / total) * 100)));
  elements.updateProgressFill.style.width = `${percent}%`;
  elements.updateProgressPercent.textContent = `${percent}%`;
  elements.updateProgressTrack.setAttribute("aria-valuenow", String(percent));
}

const resolutionLadder = [2160, 1440, 1080, 720, 540, 480, 360];

function updateSizePresetSelection() {
  const isMB = elements.targetUnit.value === "MB";
  const value = elements.targetValue.value.trim();
  for (const button of elements.sizePresets.querySelectorAll("button[data-mb]")) {
    button.classList.toggle("active", isMB && value === button.dataset.mb);
  }
}

function refreshResolutionOptions() {
  const sourceHeight = state.input?.height || 0;
  const options = [{ value: "0", label: sourceHeight ? `Source (${state.input.width} × ${sourceHeight})` : "Source resolution" }];
  for (const height of resolutionLadder) {
    if (sourceHeight && height >= sourceHeight) continue;
    options.push({ value: String(height), label: `${height}p` });
  }
  replaceOptions(elements.resolution, options, elements.resolution.value || "0");
}

function refreshFrameRateControl(resetToSource = false) {
  const sourceFPS = Number(state.input?.fps || 0);
  const available = Number.isFinite(sourceFPS) && sourceFPS > 0;
  const controls = [elements.frameRateMinimum, elements.frameRateMaximum];
  if (!available) {
    controls.forEach((control) => {
      control.min = "5";
      control.max = "5";
      control.value = "5";
      control.disabled = true;
      control.setAttribute("aria-valuetext", "Select a video first");
    });
    elements.frameRateRow.classList.add("unavailable");
    elements.frameRateSlider.classList.add("strict");
    elements.frameRateSlider.style.setProperty("--range-start", "0%");
    elements.frameRateSlider.style.setProperty("--range-end", "0%");
    elements.frameRateValue.textContent = "Select video";
    elements.frameRateValue.title = "Select a video to configure its frame rate";
    return;
  }

  const minimum = Math.min(5, Math.max(1, Math.floor(sourceFPS)));
  const maximum = Math.max(5, Math.ceil(sourceFPS));
  controls.forEach((control) => {
    control.min = String(minimum);
    control.max = String(maximum);
  });
  const currentMinimum = Number(elements.frameRateMinimum.value);
  const currentMaximum = Number(elements.frameRateMaximum.value);
  if (resetToSource || !Number.isFinite(currentMinimum) || !Number.isFinite(currentMaximum)) {
    elements.frameRateMinimum.value = String(minimum);
    elements.frameRateMaximum.value = String(maximum);
  } else {
    elements.frameRateMaximum.value = String(Math.max(minimum, Math.min(maximum, currentMaximum)));
    const clampedMinimum = Math.max(minimum, Math.min(maximum, currentMinimum));
    elements.frameRateMinimum.value = String(clampedMinimum >= Number(elements.frameRateMaximum.value) ? minimum : clampedMinimum);
  }
  const reducible = sourceFPS > 5;
  controls.forEach((control) => { control.disabled = state.encoding || !reducible; });
  elements.frameRateRow.classList.toggle("unavailable", !reducible);
  updateFrameRateLabel();
}

function updateFrameRateLabel(activeHandle = "") {
  const sourceFPS = Number(state.input?.fps || 0);
  const selectedMinimum = Number(elements.frameRateMinimum.value || 0);
  const selectedMaximum = Number(elements.frameRateMaximum.value || 0);
  if (!sourceFPS || !selectedMinimum || !selectedMaximum) return;
  const absoluteMinimum = Number(elements.frameRateMinimum.min || 5);
  const sourcePosition = Number(elements.frameRateMaximum.max || Math.ceil(sourceFPS));
  const atSource = selectedMaximum >= sourcePosition;
  const maximumFPS = atSource ? sourceFPS : Math.round(selectedMaximum);
  const strict = selectedMinimum <= absoluteMinimum || selectedMinimum >= selectedMaximum;
  const minimumFPS = strict ? maximumFPS : Math.round(selectedMinimum);
  const span = sourcePosition - absoluteMinimum;
  const toProgress = (value) => span > 0 ? ((value - absoluteMinimum) / span) * 100 : 100;
  const endProgress = Math.max(0, Math.min(100, toProgress(selectedMaximum)));
  // The floor position is a fixed-rate sentinel, not the start of a visible
  // range. Render that state like a normal single-value slider: filled from
  // the beginning through the selected maximum. The gray floor handle stays
  // available as the affordance for opening an adaptive range.
  const startProgress = strict ? 0 : Math.max(0, Math.min(endProgress, toProgress(selectedMinimum)));
  elements.frameRateSlider.style.setProperty("--range-start", `${startProgress}%`);
  elements.frameRateSlider.style.setProperty("--range-end", `${endProgress}%`);
  elements.frameRateSlider.classList.toggle("strict", strict);
  elements.frameRateSlider.classList.toggle("minimum-active", activeHandle === "minimum");
  const label = strict
    ? `Fixed · ${trimNumber(maximumFPS, 2)} fps`
    : `${trimNumber(minimumFPS, 2)}–${trimNumber(maximumFPS, 2)} fps`;
  const description = strict
    ? `Fixed at ${trimNumber(maximumFPS, 2)} fps. Move the minimum handle above ${absoluteMinimum} fps to allow adaptive reduction.`
    : `Adaptive range from ${trimNumber(minimumFPS, 2)} to ${trimNumber(maximumFPS, 2)} fps.`;
  elements.frameRateValue.textContent = label;
  elements.frameRateValue.title = description;
  elements.frameRateMinimum.setAttribute("aria-valuetext", strict ? `Lock position; ${description}` : `Minimum ${trimNumber(minimumFPS, 2)} fps`);
  elements.frameRateMaximum.setAttribute("aria-valuetext", `Maximum ${trimNumber(maximumFPS, 2)} fps${atSource ? ", source frame rate" : ""}`);
}

function requestedOutputFPS() {
  const sourceFPS = Number(state.input?.fps || 0);
  const selected = Number(elements.frameRateMaximum.value || 0);
  const maximum = Number(elements.frameRateMaximum.max || 0);
  if (!sourceFPS || !selected || selected >= maximum) return 0;
  return Math.round(selected);
}

function requestedMinimumOutputFPS() {
  const selectedMinimum = Number(elements.frameRateMinimum.value || 0);
  const selectedMaximum = Number(elements.frameRateMaximum.value || 0);
  const absoluteMinimum = Number(elements.frameRateMinimum.min || 5);
  if (!selectedMinimum || selectedMinimum <= absoluteMinimum || selectedMinimum >= selectedMaximum) return 0;
  return Math.round(selectedMinimum);
}

function handleFrameRateInput(activeHandle) {
  let minimum = Number(elements.frameRateMinimum.value || 0);
  let maximum = Number(elements.frameRateMaximum.value || 0);
  const absoluteMinimum = Number(elements.frameRateMinimum.min || 5);
  if (activeHandle === "minimum" && minimum >= maximum) {
    minimum = Math.max(absoluteMinimum, maximum - 1);
    elements.frameRateMinimum.value = String(minimum);
  } else if (activeHandle === "maximum" && maximum <= minimum) {
    minimum = absoluteMinimum;
    elements.frameRateMinimum.value = String(minimum);
  }
  updateFrameRateLabel(activeHandle);
  updateEstimate();
}

function setupFramelessWindow() {
  const frameless = Boolean(state.appStatus?.frameless);
  document.documentElement.classList.toggle("frameless", frameless);
  elements.minimizeButton.hidden = !frameless;
  elements.resizeGrip.hidden = !frameless;
  if (!frameless) return;

  const windowAction = (action) => api(`/api/window/${action}`, { method: "POST" }).catch(() => {});
  // The compositor-side follower tracks the cursor between press and release,
  // so the pointer keeps its grab offset instead of warping to the center.
  const beginFollow = (element, action) => (event) => {
    if (event.button !== 0) return;
    if (action === "move" && event.target.closest("button, select, input, a")) return;
    event.preventDefault();
    element.setPointerCapture(event.pointerId);
    windowAction(`${action}-start`);
    const finish = () => {
      element.removeEventListener("pointerup", finish);
      element.removeEventListener("pointercancel", finish);
      windowAction(`${action}-end`);
    };
    element.addEventListener("pointerup", finish);
    element.addEventListener("pointercancel", finish);
  };
  elements.appHeader.addEventListener("pointerdown", beginFollow(elements.appHeader, "move"));
  elements.resizeGrip.addEventListener("pointerdown", beginFollow(elements.resizeGrip, "resize"));
  elements.minimizeButton.addEventListener("click", () => windowAction("minimize"));
}

function bindEvents() {
  elements.dropZone.addEventListener("click", chooseInput);
  elements.fileInput.addEventListener("change", () => {
    const [file] = elements.fileInput.files;
    if (file) uploadInput(file);
  });

  for (const eventName of ["dragenter", "dragover"]) {
    elements.dropZone.addEventListener(eventName, (event) => {
      event.preventDefault();
      if (!state.encoding) elements.dropZone.classList.add("dragging");
    });
  }
  for (const eventName of ["dragleave", "drop"]) {
    elements.dropZone.addEventListener(eventName, (event) => {
      event.preventDefault();
      elements.dropZone.classList.remove("dragging");
    });
  }
  elements.dropZone.addEventListener("drop", (event) => {
    if (state.encoding) return;
    handleDrop(event.dataTransfer);
  });

  elements.container.addEventListener("change", () => {
    const previousExtension = getPathExtension(elements.outputPath.value);
    refreshCompatibility();
    if (!previousExtension || Object.values(extensions).includes(previousExtension)) {
      elements.outputPath.value = replaceExtension(elements.outputPath.value, extensions[elements.container.value]);
    }
    updateEstimate();
    updateFormState();
  });
  elements.videoCodec.addEventListener("change", () => {
    refreshEncoders();
    updateFormState();
  });
  elements.encoder.addEventListener("change", updateTwoPassAvailability);
  elements.audioCodec.addEventListener("change", () => {
    updateAudioFields();
    updateEstimate();
    updateFormState();
  });
  elements.audioBitrate.addEventListener("change", updateEstimate);
  elements.autoResolution.addEventListener("change", updateFormState);
  elements.frameRateMinimum.addEventListener("input", () => handleFrameRateInput("minimum"));
  elements.frameRateMaximum.addEventListener("input", () => handleFrameRateInput("maximum"));
  for (const control of [elements.frameRateMinimum, elements.frameRateMaximum]) {
    control.addEventListener("change", () => updateFrameRateLabel());
    control.addEventListener("blur", () => updateFrameRateLabel());
  }
  elements.sizePresets.addEventListener("click", (event) => {
    const button = event.target.closest("button");
    if (!button || state.encoding) return;
    if (!button.dataset.mb) return;
    elements.targetValue.value = button.dataset.mb;
    elements.targetUnit.value = "MB";
    updateEstimate();
    updateFormState();
    updateSizePresetSelection();
  });
  elements.targetValue.addEventListener("input", () => {
    updateSizePresetSelection();
    updateEstimate();
    updateFormState();
  });
  elements.targetUnit.addEventListener("change", () => {
    updateSizePresetSelection();
    updateEstimate();
    updateFormState();
  });
  elements.outputPath.addEventListener("input", updateFormState);
  elements.browseOutput.addEventListener("click", chooseOutput);
  elements.compressButton.addEventListener("click", startCompression);
  elements.remuxButton.addEventListener("click", startRemux);
  elements.cancelButton.addEventListener("click", cancelCompression);
  elements.showOutputButton.addEventListener("click", showOutput);
  elements.updateButton.addEventListener("click", openUpdateDialog);
  elements.updateCloseButton.addEventListener("click", closeUpdateDialog);
  elements.updateOpenAssetButton.addEventListener("click", openUpdateAsset);
  elements.updateInstallButton.addEventListener("click", installUpdate);
  elements.updateOverlay.addEventListener("click", (event) => {
    if (event.target === elements.updateOverlay) closeUpdateDialog();
  });
  elements.themeToggle.addEventListener("click", toggleTheme);
  elements.quitButton.addEventListener("click", quitApplication);
  elements.errorDismiss.addEventListener("click", clearError);
}

async function chooseInput() {
  if (state.encoding) return;
  try {
    const startDir = state.input?.path ? directoryOf(state.input.path) : "";
    const result = await api("/api/dialog/open", {
      method: "POST",
      body: JSON.stringify({ startDir }),
    });
    if (result.fallback) {
      elements.fileInput.click();
    } else if (!result.canceled && result.path) {
      await loadInputPath(result.path, "");
    }
  } catch (error) {
    showToast(error.message, true);
  }
}

// handleDrop recovers the dropped file's real location when possible: file
// managers may include a file:// URI in the drag data, and otherwise the
// server looks for a same-name same-size file in the usual folders. Only when
// both fail is the content copied to a temporary file.
async function handleDrop(dataTransfer) {
  // Drag data is protected after the drop event's synchronous turn. Capture
  // both the File and every text flavor before the first await so a failed
  // direct-path probe can still fall back to locating or uploading the file.
  const [file] = dataTransfer.files;
  const dragText = [
    dataTransfer.getData("text/uri-list"),
    dataTransfer.getData("text/plain"),
    dataTransfer.getData("x-special/gnome-copied-files"),
  ];
  for (const path of droppedFilePaths(dragText)) {
    if (await loadInputPath(path, "", false)) return;
  }
  if (!file) return;
  try {
    const located = await api("/api/locate", {
      method: "POST",
      body: JSON.stringify({ name: file.name, size: file.size }),
    });
    if (located.path && (await loadInputPath(located.path, "", false))) return;
  } catch {}
  uploadInput(file);
}

function droppedFilePaths(values) {
  const paths = [];
  const seen = new Set();
  for (const value of values) {
    for (const rawLine of (value || "").split(/\r?\n/)) {
      const line = rawLine.trim();
      if (!line || line.startsWith("#") || line === "copy" || line === "cut") continue;
      try {
        const uri = new URL(line);
        if (uri.protocol !== "file:" || (uri.hostname && uri.hostname !== "localhost")) continue;
        const path = decodeURIComponent(uri.pathname);
        if (path && !seen.has(path)) {
          seen.add(path);
          paths.push(path);
        }
      } catch {
        // Malformed or non-URL clipboard text is not a local file path.
      }
    }
  }
  return paths;
}

async function uploadInput(file) {
  if (state.encoding) return;
  setRuntime("Copying video…", "busy");
  setInputLoading(file.name);
  const form = new FormData();
  form.append("video", file, file.name);
  try {
    const uploaded = await api("/api/upload", { method: "POST", body: form });
    await loadInputPath(uploaded.path, uploaded.name || file.name, true);
  } catch (error) {
    state.input = null;
    resetInputDisplay();
    setRuntime("Ready", "ready");
    showError(error);
  } finally {
    elements.fileInput.value = "";
  }
}

async function loadInputPath(path, displayName, isTemp = false) {
  setRuntime("Inspecting video…", "busy");
  setInputLoading(displayName || baseName(path));
  try {
    const info = await api("/api/probe", {
      method: "POST",
      body: JSON.stringify({ path }),
    });
    state.input = info;
    state.inputDisplayName = displayName || info.name;
    state.inputIsTemp = isTemp;
    renderInput();
    chooseSensibleDefaults(info);
    refreshResolutionOptions();
    refreshFrameRateControl(true);
    setSuggestedOutput();
    updateEstimate();
    updateFormState();
    setRuntime("Ready", "ready");
    return true;
  } catch (error) {
    state.input = null;
    resetInputDisplay();
    setRuntime("Ready", "ready");
    showError(error);
    return false;
  }
}

function setInputLoading(name) {
  elements.emptyInputCopy.hidden = true;
  elements.fileSummary.hidden = false;
  elements.changeFile.hidden = true;
  elements.fileName.textContent = name || "Reading video…";
  elements.fileMeta.textContent = "Reading video metadata…";
}

function renderInput() {
  const info = state.input;
  elements.emptyInputCopy.hidden = true;
  elements.fileSummary.hidden = false;
  elements.changeFile.hidden = false;
  elements.fileName.textContent = state.inputDisplayName || info.name;
  const details = [
    `${info.width} × ${info.height}`,
    info.fps ? `${trimNumber(info.fps, 2)} fps` : "Variable fps",
    String(info.videoCodec || "video").toUpperCase(),
    formatDuration(info.duration),
    formatBytes(info.size),
  ];
  if (info.pixelFormat) details.splice(3, 0, formatPixelFormat(info.pixelFormat));
  elements.fileMeta.textContent = details.join(" · ");
  const audioDescription = info.audioTracks
    ? `${info.audioTracks} audio ${info.audioTracks === 1 ? "track" : "tracks"} will be retained and re-encoded.`
    : "This video has no audio tracks.";
  elements.audioInfo.textContent = audioDescription;
}

function resetInputDisplay() {
  elements.emptyInputCopy.hidden = false;
  elements.fileSummary.hidden = true;
  elements.changeFile.hidden = true;
  refreshResolutionOptions();
  refreshFrameRateControl(true);
  updateFormState();
}

function chooseSensibleDefaults(info) {
  const source = String(info.videoCodec).toLowerCase();
  if (source.includes("av1")) setSelectValueIfPresent(elements.videoCodec, "av1");
  else if (source.includes("hevc") || source.includes("265")) setSelectValueIfPresent(elements.videoCodec, "h265");
  else setSelectValueIfPresent(elements.videoCodec, "h264");
  refreshEncoders();
}

function refreshCompatibility() {
  const container = elements.container.value;
  const allowedCodecs = containerCodecs[container];
  const availableCodecs = new Set((state.appStatus?.encoders || []).map((encoder) => encoder.codec));
  replaceOptions(elements.videoCodec, allowedCodecs.map((value) => ({
    value,
    label: availableCodecs.has(value) ? codecLabels[value] : `${codecLabels[value]} — no encoder available`,
    disabled: !availableCodecs.has(value),
  })), elements.videoCodec.value || "h265");
  refreshEncoders();

  const availableAudioIDs = new Set(state.appStatus?.audioEncoders || []);
  const audioEncoderID = { aac: "aac", opus: "libopus", vorbis: "libvorbis", mp3: "libmp3lame", none: "none" };
  const allowedAudio = containerAudio[container].filter((codec) => codec === "none" || availableAudioIDs.has(audioEncoderID[codec]));
  const preferredAudio = container === "webm" ? "opus" : "aac";
  replaceOptions(elements.audioCodec, allowedAudio.map((value) => ({ value, label: audioLabels[value] })), elements.audioCodec.value || preferredAudio);
  if (!allowedAudio.includes(elements.audioCodec.value)) setSelectValueIfPresent(elements.audioCodec, preferredAudio);
  updateAudioFields();
}

function refreshEncoders() {
  const codec = elements.videoCodec.value;
  const encoders = (state.appStatus?.encoders || []).filter((encoder) => encoder.codec === codec);
  const hardware = encoders.filter((encoder) => encoder.hardware);
  const software = encoders.filter((encoder) => !encoder.hardware);
  const previous = elements.encoder.value;
  elements.encoder.replaceChildren();
  const appendGroup = (label, items) => {
    if (!items.length) return;
    const group = document.createElement("optgroup");
    group.label = label;
    for (const encoder of items) {
      const option = document.createElement("option");
      option.value = encoder.id;
      option.textContent = encoder.name;
      group.append(option);
    }
    elements.encoder.append(group);
  };
  appendGroup("GPU accelerated", hardware);
  appendGroup("Software", software);
  if (encoders.some((encoder) => encoder.id === previous)) elements.encoder.value = previous;
  else if (hardware.length) elements.encoder.value = hardware[0].id;
  else if (software.length) elements.encoder.value = software[0].id;
  updateTwoPassAvailability();
}

function updateTwoPassAvailability() {
  const selected = (state.appStatus?.encoders || []).find((encoder) => encoder.id === elements.encoder.value);
  const available = Boolean(selected?.twoPass);
  elements.twoPass.disabled = !available;
  if (!available) elements.twoPass.checked = false;
  elements.twoPassRow.classList.toggle("unavailable", !available);
  elements.twoPassRow.title = available ? "" : "This encoder does not support a reliable two-pass mode.";
}

function updateAudioFields() {
  const enabled = elements.audioCodec.value !== "none";
  if (enabled) refreshAudioBitrates();
  elements.audioBitrate.disabled = !enabled;
  elements.audioChannels.disabled = !enabled;
  elements.audioBitrateRow.hidden = !enabled;
  elements.audioChannelsRow.hidden = !enabled;
}

function refreshAudioBitrates() {
  const values = audioBitrates[elements.audioCodec.value] || [];
  const current = Number(elements.audioBitrate.value || 128);
  const fallback = values.find((value) => value >= current) ?? values.at(-1);
  replaceOptions(elements.audioBitrate, values.map((value) => ({
    value: String(value),
    label: `${value} kbps`,
  })), String(fallback || 128));
}

function setSuggestedOutput() {
  if (!state.input) return;
  let directory = directoryOf(state.input.path);
  if (state.inputIsTemp) {
    // A temporary upload lives in /tmp; suggest the user's video folder.
    directory = state.appStatus?.defaultOutputDir || directory;
  }
  const sourceName = stripExtension(state.inputDisplayName || state.input.name || "video");
  const extension = extensions[elements.container.value];
  elements.outputPath.value = `${directory}/${sourceName}_compressed.${extension}`.replaceAll("//", "/");
}

async function chooseOutput() {
  if (state.encoding) return;
  let suggested = elements.outputPath.value.trim();
  if (!suggested && state.input) {
    setSuggestedOutput();
    suggested = elements.outputPath.value.trim();
  }
  try {
    const result = await api("/api/dialog/save", {
      method: "POST",
      body: JSON.stringify({ suggested, container: elements.container.value }),
    });
    if (result.fallback) {
      showToast("No file picker was found (install kdialog or zenity). Type the destination path instead.", true);
      return;
    }
    if (!result.canceled && result.path) {
      elements.outputPath.value = ensureExtension(result.path, extensions[elements.container.value]);
      updateFormState();
    }
  } catch (error) {
    showToast(error.message, true);
  }
}

async function startCompression() {
  if (state.encoding || !isFormValid()) return;
  const output = ensureExtension(elements.outputPath.value.trim(), extensions[elements.container.value]);
  elements.outputPath.value = output;
  state.notifiedCorrectionAttempt = 0;
  clearError();
  renderJob({
    state: "queued",
    phase: "Preparing",
    message: "Reading video metadata…",
    progress: 0,
    targetBytes: targetBytes(),
    output,
  });
  setEncodingState(true);
  try {
    await api("/api/jobs", {
      method: "POST",
      body: JSON.stringify({
        input: state.input.path,
        output,
        targetBytes: targetBytes(),
        container: elements.container.value,
        videoCodec: elements.videoCodec.value,
        encoder: elements.encoder.value,
        preset: elements.preset.value,
        audioCodec: elements.audioCodec.value,
        audioBitrateKbps: Number(elements.audioBitrate.value),
        audioChannels: elements.audioChannels.value,
        twoPass: elements.twoPass.checked,
        resolutionHeight: Number(elements.resolution.value || 0),
        autoResolution: elements.autoResolution.checked,
        outputFps: requestedOutputFPS(),
        minimumOutputFps: requestedMinimumOutputFPS(),
      }),
    });
    pollJob();
  } catch (error) {
    setEncodingState(false);
    showError(error);
  }
}

async function pollJob() {
  window.clearTimeout(state.pollingTimer);
  try {
    const job = await api("/api/jobs/current");
    renderJob(job);
    if (["completed", "failed", "canceled"].includes(job.state)) {
      setEncodingState(false);
      if (job.state === "completed") showToast(`Saved ${formatBytes(job.encodedBytes)} output`);
      return;
    }
  } catch (error) {
    setEncodingState(false);
    showError(error);
    return;
  }
  state.pollingTimer = window.setTimeout(pollJob, 250);
}

async function cancelCompression() {
  if (!state.encoding) return;
  elements.cancelButton.disabled = true;
  showToast("Stopping FFmpeg and cleaning temporary files…");
  try {
    await api("/api/jobs/current", { method: "DELETE" });
  } catch (error) {
    showToast(error.message, true);
  }
}

function renderJob(job) {
  const progress = Math.max(0, Math.min(100, Number(job.progress || 0)));
  elements.progressPanel.classList.toggle("active", ["queued", "running"].includes(job.state));
  elements.progressPanel.classList.toggle("complete", job.state === "completed");
  elements.progressPanel.classList.toggle("failed", job.state === "failed");
  elements.progressFill.style.width = `${progress}%`;
  elements.progressPercent.textContent = `${Math.round(progress)}%`;
  elements.progressTrack.setAttribute("aria-valuenow", String(Math.round(progress)));
  elements.progressEyeline.textContent = statusEyeline(job);
  elements.progressHeading.textContent = job.phase || "Compression progress";
  const metric = progressMetric(job);
  elements.progressPassLabel.textContent = metric.label;
  elements.progressPass.textContent = metric.value;
  elements.progressSize.textContent = formatBytes(job.encodedBytes || 0);
  elements.progressElapsed.textContent = formatDuration(job.elapsedSeconds || 0);
  elements.progressRemainingStat.hidden = job.state !== "running";
  elements.progressRemaining.textContent = job.remainingSeconds > 0 ? formatDuration(job.remainingSeconds) : "—";
  const active = ["queued", "running"].includes(job.state);
  elements.progressMessage.hidden = active;
  elements.progressMessage.textContent = active ? "" : (job.message || "");
  elements.progressMessage.title = active ? "" : (job.message || "");
  notifyCorrection(job);
  elements.cancelButton.hidden = !["queued", "running"].includes(job.state);
  elements.cancelButton.disabled = job.state !== "running";
  elements.showOutputButton.hidden = job.state !== "completed";
  if (job.error) {
    elements.errorFloat.hidden = false;
    elements.errorMessage.textContent = job.error;
  } else if (["queued", "running"].includes(job.state)) {
    clearError();
  }
  if (job.state === "failed") setRuntime("Compression failed", "error");
  else if (job.state === "completed") setRuntime("Complete", "ready");
  else if (job.state === "canceled") setRuntime("Canceled", "ready");
}

function notifyCorrection(job) {
  const attempt = Number(job.attempt || 0);
  const message = String(job.message || "").trim();
  const encoding = job.state === "running" && String(job.phase || "").startsWith("Encoding");
  if (!encoding || attempt <= 1 || attempt <= state.notifiedCorrectionAttempt || !message) return;
  state.notifiedCorrectionAttempt = attempt;
  showToast(message);
}

function statusEyeline(job) {
  if (job.state === "completed" && String(job.message || "").startsWith("Muxed")) return "Mux complete";
  if (job.state === "completed" && String(job.message || "").startsWith("Remuxed")) return "Remux complete";
  if (job.state === "completed") return "Verified under target";
  if (job.state === "failed") return "Needs attention";
  if (job.state === "canceled") return "Canceled";
  if (job.attempt > 1) return `Correction attempt ${job.attempt}`;
  return job.state === "running" ? "Compression in progress" : "Preparing";
}

function stageLabel(job) {
  if (job.state === "completed") return "Verified";
  if (job.state === "failed") return "Failed";
  if (job.state === "canceled") return "Canceled";
  if (job.phase === "Verifying") return "Size check";
  if (job.passes > 1 && job.pass) return `Pass ${job.pass} of ${job.passes}`;
  return job.phase || "Preparing";
}

function progressMetric(job) {
  if (job.state === "running" && String(job.phase || "").startsWith("Encoding")) {
    const details = [];
    const bitrate = Number(job.videoBitrateKbps || 0);
    const fps = Number(job.outputFps || 0);
    if (bitrate > 0) details.push(formatBitrate(bitrate));
    if (fps > 0) details.push(`${trimNumber(fps, 2)} fps`);
    return { label: "Video", value: details.join(" · ") || "Starting" };
  }
  return { label: "Stage", value: stageLabel(job) };
}

function setEncodingState(encoding) {
  state.encoding = encoding;
  const controls = document.querySelectorAll(".workflow-section input, .workflow-section select, .workflow-section button, #compressButton");
  controls.forEach((control) => { control.disabled = encoding; });
  if (!encoding) {
    elements.cancelButton.disabled = false;
    refreshCompatibility();
    refreshFrameRateControl(false);
    updateFormState();
  }
  setRuntime(encoding ? "Encoding" : "Ready", encoding ? "busy" : "ready");
}

async function showOutput() {
  try {
    await api("/api/reveal", {
      method: "POST",
      body: JSON.stringify({ path: elements.outputPath.value.trim() }),
    });
  } catch (error) {
    showToast(error.message, true);
  }
}

// Themed stand-in for window.confirm: resolves true only when the user
// explicitly chooses to close; Escape and backdrop clicks keep encoding.
function confirmQuitDuringEncode() {
  return new Promise((resolve) => {
    const overlay = elements.confirmOverlay;
    const done = (answer) => {
      overlay.hidden = true;
      elements.confirmStay.removeEventListener("click", onStay);
      elements.confirmQuit.removeEventListener("click", onQuit);
      overlay.removeEventListener("click", onBackdrop);
      document.removeEventListener("keydown", onKey);
      resolve(answer);
    };
    const onStay = () => done(false);
    const onQuit = () => done(true);
    const onBackdrop = (event) => {
      if (event.target === overlay) done(false);
    };
    const onKey = (event) => {
      if (event.key === "Escape") done(false);
    };
    elements.confirmStay.addEventListener("click", onStay);
    elements.confirmQuit.addEventListener("click", onQuit);
    overlay.addEventListener("click", onBackdrop);
    document.addEventListener("keydown", onKey);
    overlay.hidden = false;
    elements.confirmStay.focus();
  });
}

async function quitApplication() {
  if (state.updateInstalling) {
    showToast("Wait for the AppImage update to finish before closing ExactSize.", true);
    return;
  }
  if (state.encoding && !(await confirmQuitDuringEncode())) return;
  try {
    await api("/api/quit", { method: "POST" });
    window.close();
  } catch {
    window.close();
  }
}

function updateEstimate() {
  if (!state.input) {
    elements.bitrateEstimate.textContent = "Select a video to calculate bitrate";
    return;
  }
  const target = targetBytes();
  if (!target || !state.input.duration) {
    elements.bitrateEstimate.textContent = "Enter a valid target size";
    return;
  }
  const reserve = Math.max(64 * 1024, Math.ceil(target * .006));
  const tracks = elements.audioCodec.value === "none" ? 0 : state.input.audioTracks;
  const audioKbps = tracks * Number(elements.audioBitrate.value || 0);
  const videoKbps = Math.floor((((target - reserve) * 8) / state.input.duration / 1000) - audioKbps);
  if (videoKbps < 64) {
    elements.bitrateEstimate.textContent = "Target is too small for this duration";
    return;
  }
  const maximumFPS = requestedOutputFPS() || Number(state.input.fps || 0);
  const minimumFPS = requestedMinimumOutputFPS();
  const frameRateSummary = minimumFPS
    ? ` · FPS: ${trimNumber(minimumFPS, 2)}–${trimNumber(maximumFPS, 2)}`
    : ` · FPS: ${trimNumber(maximumFPS, 2)} fixed`;
  elements.bitrateEstimate.textContent = `Estimated video bitrate: ${formatBitrate(videoKbps)}${tracks ? ` · Audio: ${audioKbps} kbps total` : " · No audio"}${frameRateSummary}`;
}

function isFormValid() {
  return Boolean(
    state.input &&
    targetBytes() >= 256 * 1024 &&
    elements.outputPath.value.trim() &&
    elements.encoder.value &&
    !state.encoding
  );
}

function sourceContainer() {
  if (!state.input) return "";
  const ext = getPathExtension(state.inputDisplayName || state.input.name || state.input.path || "");
  if (ext === "m4v") return "mp4";
  return Object.values(extensions).includes(ext) ? ext : "";
}

// remuxMode decides what the button can do: "remux" copies everything,
// "mux" copies the video and re-encodes only the audio (whose dropdown is
// always container-valid), and "" means even the video cannot be carried.
function remuxMode() {
  if (!state.input) return { mode: "", reason: "Select a video first" };
  if (!elements.outputPath.value.trim()) return { mode: "", reason: "Choose an output destination" };
  const target = elements.container.value;
  if (sourceContainer() === target) {
    return { mode: "", reason: `The source is already ${target.toUpperCase()}` };
  }
  const key = mapProbeCodec(state.input.videoCodec || "");
  const allowed = containerCodecs[target] || [];
  if (key && !allowed.includes(key)) {
    return { mode: "", reason: `${String(state.input.videoCodec).toUpperCase()} video cannot go into ${target.toUpperCase()}` };
  }
  if (!key && target !== "mkv") {
    return { mode: "", reason: "This source video codec is only safe to carry into MKV" };
  }
  if (state.input.audioTracks > 0) {
    const audioKey = mapProbeAudioCodec(state.input.audioCodec || "");
    const allowedAudio = containerAudio[target] || [];
    if ((audioKey && !allowedAudio.includes(audioKey)) || (!audioKey && target !== "mkv")) {
      return { mode: "mux" };
    }
  }
  return { mode: "remux" };
}

function updateRemuxState() {
  const { mode, reason } = remuxMode();
  const label = elements.remuxButton.querySelector("span");
  if (mode === "mux") {
    label.textContent = "Mux";
    const audio = elements.audioCodec.value === "none"
      ? "drop the audio"
      : `re-encode only the audio to ${audioLabels[elements.audioCodec.value] || elements.audioCodec.value}`;
    elements.remuxButton.title = `The source audio cannot be copied into ${elements.container.value.toUpperCase()}: copy the video losslessly and ${audio}`;
  } else {
    label.textContent = "Remux";
    elements.remuxButton.title = reason || "Copy the streams into the selected container without re-encoding";
  }
  elements.remuxButton.disabled = !mode || state.encoding;
}

function mapProbeAudioCodec(name) {
  name = String(name).toLowerCase();
  return ["aac", "opus", "vorbis", "mp3"].includes(name) ? name : "";
}

function mapProbeCodec(name) {
  name = String(name).toLowerCase();
  if (name.includes("av1")) return "av1";
  if (name.includes("hevc") || name.includes("265")) return "h265";
  if (name.includes("h264") || name.includes("avc")) return "h264";
  if (name.includes("vvc") || name.includes("266")) return "h266";
  if (name.includes("vp9")) return "vp9";
  return "";
}

async function startRemux() {
  if (state.encoding || elements.remuxButton.disabled) return;
  const { mode } = remuxMode();
  if (!mode) return;
  const extension = extensions[elements.container.value];
  let output = ensureExtension(elements.outputPath.value.trim(), extension);
  if (output.endsWith(`_compressed.${extension}`)) {
    output = output.replace(/_compressed\.[^.]+$/, `_${mode}ed.${extension}`);
  }
  elements.outputPath.value = output;
  clearError();
  renderJob({
    state: "queued",
    phase: "Preparing",
    message: "Reading video metadata…",
    progress: 0,
    output,
  });
  setEncodingState(true);
  const request = {
    input: state.input.path,
    output,
    container: elements.container.value,
    remux: true,
  };
  if (mode === "mux") {
    request.muxAudio = true;
    request.audioCodec = elements.audioCodec.value;
    request.audioBitrateKbps = Number(elements.audioBitrate.value);
    request.audioChannels = elements.audioChannels.value;
  }
  try {
    await api("/api/jobs", { method: "POST", body: JSON.stringify(request) });
    pollJob();
  } catch (error) {
    setEncodingState(false);
    showError(error);
  }
}

function updateFormState() {
  elements.compressButton.disabled = !isFormValid();
  updateRemuxState();
}

function targetBytes() {
  const value = Number(elements.targetValue.value);
  if (!Number.isFinite(value) || value <= 0) return 0;
  const multiplier = {
    MB: 1_000_000,
    MiB: 1_048_576,
    GB: 1_000_000_000,
    GiB: 1_073_741_824,
  }[elements.targetUnit.value];
  return Math.floor(value * multiplier);
}

function setRuntime(label, type) {
  elements.runtimeLabel.textContent = label;
  elements.runtimeStatus.classList.remove("ready", "busy", "error");
  elements.runtimeStatus.classList.add(type);
}

function showError(error) {
  const message = error instanceof Error ? error.message : String(error);
  elements.errorFloat.hidden = false;
  elements.errorMessage.textContent = message;
  elements.progressPanel.classList.add("failed");
  elements.progressEyeline.textContent = "Needs attention";
  elements.progressHeading.textContent = "Unable to continue";
  elements.progressMessage.hidden = false;
  elements.progressMessage.textContent = "Check the error, adjust the settings, and try again.";
}

function clearError() {
  elements.errorFloat.hidden = true;
  elements.errorMessage.textContent = "";
}

function showToast(message, isError = false) {
  window.clearTimeout(state.toastTimer);
  elements.toast.textContent = message;
  elements.toast.style.borderColor = isError ? "#9e4e4e" : "";
  elements.toast.hidden = false;
  state.toastTimer = window.setTimeout(() => { elements.toast.hidden = true; }, 4200);
}

function replaceOptions(select, options, preferred) {
  const existing = select.value;
  select.replaceChildren(...options.map(({ value, label, disabled }) => {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = label;
    option.disabled = Boolean(disabled);
    return option;
  }));
  const values = options.filter((option) => !option.disabled).map((option) => option.value);
  if (values.includes(existing)) select.value = existing;
  else if (values.includes(preferred)) select.value = preferred;
  else if (values.length) select.value = values[0];
}

function setSelectValueIfPresent(select, value) {
  if ([...select.options].some((option) => option.value === value && !option.disabled)) select.value = value;
}

function ensureExtension(path, extension) {
  if (!path) return path;
  if (getPathExtension(path) === extension) return path;
  return replaceExtension(path, extension);
}

function replaceExtension(path, extension) {
  if (!path) return path;
  const slash = path.lastIndexOf("/");
  const dot = path.lastIndexOf(".");
  const base = dot > slash ? path.slice(0, dot) : path;
  return `${base}.${extension}`;
}

function getPathExtension(path) {
  const slash = path.lastIndexOf("/");
  const dot = path.lastIndexOf(".");
  return dot > slash ? path.slice(dot + 1).toLowerCase() : "";
}

function directoryOf(path) {
  const index = path.lastIndexOf("/");
  return index > 0 ? path.slice(0, index) : ".";
}

function baseName(path) {
  return path.split("/").pop() || path;
}

function stripExtension(name) {
  const dot = name.lastIndexOf(".");
  return dot > 0 ? name.slice(0, dot) : name;
}

function formatBytes(bytes) {
  const value = Number(bytes || 0);
  if (value < 1000) return `${value} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let size = value;
  let unit = -1;
  do {
    size /= 1000;
    unit += 1;
  } while (size >= 1000 && unit < units.length - 1);
  return `${trimNumber(size, size >= 100 ? 0 : size >= 10 ? 1 : 2)} ${units[unit]}`;
}

function formatBitrate(kbps) {
  if (kbps >= 1000) return `${trimNumber(kbps / 1000, 2)} Mbps`;
  return `${kbps} kbps`;
}

function formatDuration(seconds) {
  const total = Math.max(0, Math.round(Number(seconds || 0)));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const remainder = total % 60;
  if (hours) return `${String(hours).padStart(2, "0")}:${String(minutes).padStart(2, "0")}:${String(remainder).padStart(2, "0")}`;
  return `${String(minutes).padStart(2, "0")}:${String(remainder).padStart(2, "0")}`;
}

function formatPixelFormat(pixelFormat) {
  if (pixelFormat.includes("12")) return "12-bit";
  if (pixelFormat.includes("10")) return "10-bit";
  return "8-bit";
}

function trimNumber(number, digits) {
  return Number(number.toFixed(digits)).toString();
}

initialize();
