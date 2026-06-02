"use strict";

const TRANSPORTS = ["tcp", "uds", "shm"];
const TLABEL = { tcp: "TCP", uds: "UDS", shm: "Shared Memory" };

// Each metric defines how to pick its value, the number of decimal digits, and a
// ladder of units ordered small -> large. rescaleMetric picks one unit per group
// based on the largest value so all three bars share a unit and large payloads
// don't render as huge raw numbers. Bar length is always proportional to the
// value; whether lower or higher is better is conveyed by the card's hint text.
const METRICS = {
  latency: {
    pick: (r) => r.latencyP50Us, // microseconds
    digits: 1,
    units: [ { suffix: "µs", div: 1 }, { suffix: "ms", div: 1e3 }, { suffix: "s", div: 1e6 } ],
  },
  throughput: {
    pick: (r) => r.mbPerSec,
    digits: 0,
    units: [ { suffix: "MB/s", div: 1 }, { suffix: "GB/s", div: 1024, digits: 2 } ],
  },
  cpu: {
    pick: (r) => r.cpuSecPer1M, // CPU-seconds per 1M messages
    digits: 2,
    units: [ { suffix: "", div: 1 }, { suffix: "k", div: 1e3 }, { suffix: "M", div: 1e6 } ],
  },
};

// pickUnit returns the largest unit whose divisor keeps maxVal >= 1 (so the
// displayed magnitude is small), defaulting to the smallest unit.
function pickUnit(units, maxVal) {
  let chosen = units[0];
  for (const u of units) {
    if (maxVal / u.div >= 1) chosen = u;
  }
  return chosen;
}

// formatValue renders v in the given unit. A unit MAY override the metric's
// digit count (e.g. GB/s needs decimals so sub-1 GB/s values don't round to 0).
function formatValue(cfg, unit, v) {
  const scaled = v / unit.div;
  const digits = unit.digits != null ? unit.digits : cfg.digits;
  const s = scaled.toFixed(digits);
  return unit.suffix ? `${s} ${unit.suffix}` : s;
}

// CODE drives the "only thing that changes" panel. Each entry is either a
// [class, text] header/blank line, or a [class, code, comment] line whose
// trailing comment is aligned automatically by renderCode (see below) — never
// hand-pad with spaces, or the columns drift whenever a line is edited.
const CODE = {
  go: [
    ["dim", "// server"],
    ["", "lis, _ := net.Listen(\"tcp\", \"127.0.0.1:0\")", "// TCP"],
    ["", "lis, _ := net.Listen(\"unix\", sockPath)", "// UDS"],
    ["add", "lis, _ := shm.NewListener(segmentName)", "// Shared Memory"],
    ["dim", ""],
    ["dim", "// client"],
    ["", "conn, _ := grpc.NewClient(target, insecure)", "// TCP / UDS"],
    ["add", "conn, _ := grpc.NewClient(target, shm.WithTransport())", "// Shared Memory"],
  ],
  dotnet: [
    ["dim", "// server"],
    ["", "builder.WebHost.UseKestrel(o => o.ListenLocalhost(0));", "// TCP"],
    ["", "builder.WebHost.UseKestrel(o => o.ListenUnixSocket(p));", "// UDS"],
    ["add", "builder.WebHost.UseSharedMemory(segmentName);", "// Shared Memory"],
    ["dim", ""],
    ["dim", "// client"],
    ["", "GrpcChannel.ForAddress(\"http://localhost:port\");", "// TCP / UDS"],
    ["add", "GrpcChannel.ForAddress(\"shm://\" + segmentName);", "// Shared Memory"],
  ],
};

let state = {
  lang: "go",
  running: false,
  results: {}, // transport -> result event (current view)
  cache: {},   // "lang:payload:profile" -> { transport -> result event }
  runKey: null,
  es: null,        // active EventSource
  presence: null,  // long-lived presence connection (keeps backend alive)
  watchdog: null,  // hang-detection timer
  hung: false,     // set when the watchdog fired for this run
  sawDone: false,  // set when the engine reported completion
  hadError: false, // set when any transport reported an error this run
  errors: [],      // transports that errored this run
  hangTimeoutMs: 0,// hang threshold for the current run (scales with payload)
};

// HANG_BASE_MS is the minimum gap (no server event) before declaring a run
// hung. Large payloads legitimately spend longer between events — a 256 MiB
// ping-pong moves a quarter-gigabyte per round trip and its phases take many
// seconds — so the effective threshold scales with payload size in
// hangTimeoutMs() rather than using a single fixed constant that would
// false-positive on the largest sizes.
const HANG_BASE_MS = 20000;

// hangTimeoutMs returns the no-event threshold for the currently selected
// payload: a 20 s floor plus one second per MiB, capped at 90 s.
function hangTimeoutMs() {
  const bytes = Number($("#payload").value) || 4096;
  const mib = bytes / (1024 * 1024);
  return Math.min(90000, HANG_BASE_MS + mib * 1000);
}

// The page holds a long-lived presence connection (an EventSource to
// /api/presence) for its entire lifetime. The backend counts these and shuts
// down only once every page has closed its connection. Unlike a polled
// heartbeat, a held connection survives a locked screen, hibernation, and
// background-tab timer throttling — it freezes with the machine and is still
// there on wake — so the backend is never killed out from under an open page.

const $ = (sel) => document.querySelector(sel);

// currentKey identifies a result set by the language + payload + profile combination.
function currentKey() {
  return `${state.lang}:${$("#payload").value}:${$("#profile").value}`;
}

function buildBars() {
  for (const metric of Object.keys(METRICS)) {
    const host = $(`#bars-${metric}`);
    host.innerHTML = "";
    for (const t of TRANSPORTS) {
      const row = document.createElement("div");
      row.className = "bar-row";
      row.dataset.t = t;
      row.dataset.metric = metric;
      row.innerHTML = `
        <div class="bar-label"><span class="name">${TLABEL[t]}</span><span class="val">—</span></div>
        <div class="bar-track"><div class="bar-fill"></div></div>`;
      host.appendChild(row);
    }
  }
}

function renderCode() {
  const block = $("#codeBlock");
  const rows = CODE[state.lang];
  // Align trailing comments to one column: pad every code part to the longest
  // code part (plus a two-space gutter) so the // comments line up regardless of
  // how the individual lines were edited.
  const codeWidth = Math.max(
    0,
    ...rows.filter((r) => r.length === 3).map((r) => r[1].length)
  );
  block.innerHTML = rows
    .map((row) => {
      const cls = row[0];
      let txt;
      if (row.length === 3) {
        txt = row[1].padEnd(codeWidth + 2) + row[2];
      } else {
        txt = row[1];
      }
      return `<span class="${cls}">${escapeHtml(txt) || "&nbsp;"}</span>`;
    })
    .join("\n");
}

function escapeHtml(s) {
  return s.replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c]));
}

function setStatus(text, warn) {
  const el = $("#status");
  el.textContent = text;
  el.classList.toggle("warn", !!warn);
}

function resetCharts() {
  state.results = {};
  buildBars();
  setStatus("Ready.");
}

// showResults renders an already-computed result set (e.g. from cache).
function showResults(results) {
  buildBars();
  state.results = { ...results };
  for (const [metric, cfg] of Object.entries(METRICS)) rescaleMetric(metric, cfg);
}

// loadOrClear shows the cached result for the current lang+payload if one
// exists, otherwise clears the charts.
function loadOrClear() {
  const cached = state.cache[currentKey()];
  if (cached && Object.keys(cached).length > 0) {
    showResults(cached);
    setStatus("Cached result \u00b7 click Run to re-measure.");
  } else {
    resetCharts();
  }
}

// hardReset discards every cached result and clears the charts.
function hardReset() {
  state.cache = {};
  resetCharts();
}

function markPending(transport) {
  for (const metric of Object.keys(METRICS)) {
    const row = document.querySelector(`#bars-${metric} .bar-row[data-t="${transport}"]`);
    if (row) row.classList.add("pending");
  }
}

function applyResult(ev) {
  state.results[ev.transport] = ev;
  for (const [metric, cfg] of Object.entries(METRICS)) {
    const row = document.querySelector(`#bars-${metric} .bar-row[data-t="${ev.transport}"]`);
    if (row) row.classList.remove("pending");
    rescaleMetric(metric, cfg);
  }
}

function rescaleMetric(metric, cfg) {
  const entries = Object.entries(state.results);
  const values = entries.map(([, r]) => cfg.pick(r)).filter((v) => v > 0);
  if (values.length === 0) return;
  const max = Math.max(...values);
  // One unit for the whole group so the three bars are directly comparable and
  // large payloads don't show enormous raw numbers.
  const unit = pickUnit(cfg.units, max);
  for (const [t, r] of entries) {
    const v = cfg.pick(r);
    const row = document.querySelector(`#bars-${metric} .bar-row[data-t="${t}"]`);
    if (!row) continue;
    const fill = row.querySelector(".bar-fill");
    const valEl = row.querySelector(".val");
    // Bar length is always proportional to the value. Whether lower or higher is
    // better is conveyed by the metric's label, not by inverting the bars.
    const pct = max > 0 && v > 0 ? Math.max(4, (v / max) * 100) : 0;
    fill.style.width = `${pct}%`;
    valEl.textContent = formatValue(cfg, unit, v);
  }
}

// checkHealth pings the backend. Resolves true if the shell is alive, false if
// it is unreachable (process died, port closed, etc.).
async function checkHealth() {
  try {
    const r = await fetch("/api/health", { cache: "no-store" });
    return r.ok;
  } catch {
    return false;
  }
}

// backendDown is shown when the shell process is no longer reachable. The
// backend self-exits a few seconds after every page disconnects (keepalive by
// open-page count), so the usual cause is that all tabs were closed — or the
// machine slept long enough that the page lost its presence connection. The
// page cannot relaunch the backend itself (the page *is* served by it), so the
// message spells out the manual recovery: close every demo tab, then re-run
// demo.exe to start a fresh backend.
function backendDown() {
  setStatus(
    "\u26a0 Backend not reachable \u2014 the demo process has exited. " +
      "Close ALL demo browser tabs, then re-run demo.exe to start it again, " +
      "and open the page it prints.",
    true
  );
}

async function run() {
  if (state.running) return;
  if (!(await checkHealth())) { backendDown(); return; }
  resetCharts();
  state.running = true;
  state.hung = false;
  state.sawDone = false;
  state.hadError = false;
  state.errors = [];
  state.hangTimeoutMs = hangTimeoutMs();
  state.runKey = currentKey();
  $("#runBtn").disabled = true;
  $("#payload").disabled = true;
  $("#profile").disabled = true;

  const payload = $("#payload").value;
  const profile = $("#profile").value;
  const lang = state.lang;
  setStatus(`Running ${lang === "go" ? "Go" : ".NET"} benchmark…`);

  const es = new EventSource(`/api/run?lang=${lang}&payload=${payload}&profile=${profile}`);
  state.es = es;
  petWatchdog();

  es.onmessage = (msg) => {
    petWatchdog();
    let ev;
    try { ev = JSON.parse(msg.data); } catch { return; }
    switch (ev.type) {
      case "progress":
        if (ev.phase === "connect") markPending(ev.transport);
        // When a transport is measured over several rounds (median wins), the
        // engine tags each phase with round/rounds so the status line shows
        // progress through the repeated measurements (e.g. "round 2/3").
        const round = ev.rounds > 1 ? ` · round ${ev.round}/${ev.rounds}` : "";
        setStatus(`${TLABEL[ev.transport] || ev.transport} · ${ev.phase}${round}…`);
        break;
      case "result":
        applyResult(ev);
        break;
      case "error":
        state.hadError = true;
        if (ev.transport && !state.errors.includes(ev.transport)) {
          state.errors.push(ev.transport);
        }
        setStatus(`Error (${ev.transport || "?"}): ${ev.error}`, true);
        break;
      case "done":
        state.sawDone = true;
        es.close();
        finishRun();
        break;
    }
  };
  es.onerror = async () => {
    es.close();
    // A stream error before "done" means the backend dropped the connection.
    // Distinguish a crashed shell from a normal hiccup with a health probe.
    if (state.running && !state.sawDone && !state.hung) {
      const alive = await checkHealth();
      finishRun();
      if (!alive) { backendDown(); return; }
      if (Object.keys(state.results).length === 0) {
        setStatus("\u26a0 Connection lost before any result. Aborted.", true);
      }
      return;
    }
    finishRun();
  };
}

// petWatchdog (re)arms the hang-detection timer. Called on every server event;
// if the timer ever fires, the run is stuck and we surface a clear warning
// instead of leaving the UI spinning forever.
function petWatchdog() {
  clearTimeout(state.watchdog);
  if (!state.running) return;
  state.watchdog = setTimeout(onHang, state.hangTimeoutMs || HANG_BASE_MS);
}

function onHang() {
  state.hung = true;
  const pending = TRANSPORTS.filter((t) => !state.results[t]);
  const stuck = pending.length ? pending.map((t) => TLABEL[t]).join(", ") : "the run";
  if (state.es) state.es.close();
  finishRun();
  const secs = Math.round((state.hangTimeoutMs || HANG_BASE_MS) / 1000);
  setStatus(`\u26a0 Hang detected \u2014 no response for ${secs}s on: ${stuck}. Aborted.`, true);
}

function finishRun() {
  state.running = false;
  clearTimeout(state.watchdog);
  state.es = null;
  $("#runBtn").disabled = false;
  $("#payload").disabled = false;
  $("#profile").disabled = false;
  document.querySelectorAll(".bar-row.pending").forEach((r) => r.classList.remove("pending"));
  if (Object.keys(state.results).length > 0) {
    if (state.runKey) state.cache[state.runKey] = { ...state.results };
  }
  if (state.hung) return; // onHang already set a warning status.
  if (state.hadError) {
    const failed = state.errors.length
      ? state.errors.map((t) => TLABEL[t] || t).join(", ")
      : "one or more transports";
    setStatus(`\u26a0 Completed with errors on: ${failed}.`, true);
  } else if (Object.keys(state.results).length > 0) {
    setStatus("Done.");
  }
}

function init() {
  buildBars();
  renderCode();

  $("#langToggle").addEventListener("click", (e) => {
    const btn = e.target.closest(".seg-btn");
    if (!btn || state.running) return;
    document.querySelectorAll("#langToggle .seg-btn").forEach((b) => b.classList.remove("active"));
    btn.classList.add("active");
    state.lang = btn.dataset.lang;
    renderCode();
    loadOrClear();
  });

  $("#payload").addEventListener("change", () => { if (!state.running) loadOrClear(); });
  $("#profile").addEventListener("change", () => { if (!state.running) loadOrClear(); });

  $("#runBtn").addEventListener("click", run);
  $("#resetBtn").addEventListener("click", async () => {
    if (state.running) return;
    if (!(await checkHealth())) { backendDown(); return; }
    hardReset();
  });

  startPresence();
}

// startPresence opens a long-lived connection that keeps the backend alive
// while this page is open and lets it exit once the page is gone. The
// EventSource auto-reconnects after transient drops (and after a refresh the
// fresh page opens a new one), so only a truly closed tab stops it for good.
// Because it is a held connection rather than a timer, a locked screen or
// hibernation does not end it: it freezes with the machine and resumes on wake.
function startPresence() {
  try {
    state.presence = new EventSource("/api/presence");
  } catch (e) {
    // EventSource unavailable; the backend's startup grace still bounds it.
  }
}

init();
