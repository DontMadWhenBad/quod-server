// quod prover web client. Plain browser JS, no build step.
// Everything from the server or the model is inserted with textContent,
// never as HTML.
"use strict";

// ------------------------------------------------------------------ settings

const DEFAULTS = {
  server: "", rememberKey: false,
  confirm: false, split: true, explain: true,
  attempts: 0, parallel: 0, budget: 0,
  feedback: false, raw: false,
};
const SETTINGS_KEY = "quod.settings";
const KEY_KEY = "quod.key";

function loadSettings() {
  let s = { ...DEFAULTS };
  try { Object.assign(s, JSON.parse(localStorage.getItem(SETTINGS_KEY) || "{}")); } catch {}
  return s;
}
function saveSettings(s) {
  const { key, ...rest } = s; // never store the key with the settings
  try { localStorage.setItem(SETTINGS_KEY, JSON.stringify(rest)); } catch {}
}
function loadKey() {
  try { return sessionStorage.getItem(KEY_KEY) || localStorage.getItem(KEY_KEY) || ""; } catch { return ""; }
}
function storeKey(key, remember) {
  try {
    sessionStorage.removeItem(KEY_KEY);
    localStorage.removeItem(KEY_KEY);
    if (!key) return;
    (remember ? localStorage : sessionStorage).setItem(KEY_KEY, key);
  } catch {}
}

let settings = loadSettings();
let accessKey = loadKey();

// ------------------------------------------------------------------ DOM helpers

const $ = (sel) => document.querySelector(sel);

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

const oneLine = (s) => String(s ?? "").replace(/\s+/g, " ").trim();
const money = (x) => "$" + (Math.round((x || 0) * 10000) / 10000);

function renderMath(node) {
  if (window.renderMathInElement) {
    try {
      window.renderMathInElement(node, {
        delimiters: [
          { left: "$$", right: "$$", display: true }, { left: "\\[", right: "\\]", display: true },
          { left: "\\(", right: "\\)", display: false }, { left: "$", right: "$", display: false },
        ],
        throwOnError: false,
      });
    } catch {}
  }
}

// ------------------------------------------------------------------ API

class ApiError extends Error {
  constructor(status, code, message, body) {
    super(message);
    this.status = status; this.code = code; this.body = body;
  }
}

function base() {
  return (settings.server || "").replace(/\/+$/, "");
}

function headers(extra = {}) {
  const h = { ...extra };
  if (accessKey) h["Authorization"] = "Bearer " + accessKey;
  return h;
}

async function api(method, path, body) {
  if (!base()) throw new ApiError(0, "not_configured", "Set the server URL in Settings.");
  let res;
  try {
    res = await fetch(base() + path, {
      method,
      headers: headers(body ? { "Content-Type": "application/json" } : {}),
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch (e) {
    throw new ApiError(0, "network", `Cannot reach ${base()} (network error or CORS not allowed for this page).`);
  }
  const text = await res.text();
  let json = null;
  try { json = JSON.parse(text); } catch {}
  if (!res.ok) {
    const err = json?.error || {};
    let msg = err.message || text || res.statusText;
    const retry = res.headers.get("Retry-After");
    if (retry) msg += ` (retry in ${retry}s)`;
    throw new ApiError(res.status, err.code || "http_" + res.status, `HTTP ${res.status}: ${msg}`, text);
  }
  return { json, text };
}

// streamEvents follows a job's SSE stream with fetch (EventSource cannot
// send the Authorization header), resuming with Last-Event-ID on drops.
async function streamEvents(jobId, onEvent, signal) {
  let last = 0, tries = 0;
  const q = settings.feedback ? "?detail=full" : "";
  for (;;) {
    let ended = false;
    try {
      const res = await fetch(`${base()}/v1/jobs/${encodeURIComponent(jobId)}/events${q}`, {
        headers: headers({ "Accept": "text/event-stream", "Last-Event-ID": String(last) }),
        signal,
      });
      if (!res.ok) {
        const t = await res.text();
        let m = t; try { m = JSON.parse(t).error.message; } catch {}
        throw new ApiError(res.status, "stream", `HTTP ${res.status}: ${m}`);
      }
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf("\n\n")) >= 0) {
          const block = buf.slice(0, i);
          buf = buf.slice(i + 2);
          let id = null, type = "message", data = "";
          for (const line of block.split("\n")) {
            if (line.startsWith("id:")) id = line.slice(3).trim();
            else if (line.startsWith("event:")) type = line.slice(6).trim();
            else if (line.startsWith("data:")) data += line.slice(5).trim();
          }
          if (type === "end") { ended = true; break; }
          if (!data) continue;
          if (id) last = Number(id) || last;
          let ev; try { ev = JSON.parse(data); } catch { continue; }
          tries = 0;
          await onEvent(ev, data);
        }
        if (ended) break;
      }
    } catch (e) {
      if (signal?.aborted) return;
      if (e instanceof ApiError && e.status && e.status !== 429 && e.status < 500) throw e;
      if (++tries > 10) throw new ApiError(0, "stream", "Lost the event stream (the job continues on the server).");
    }
    if (ended || signal?.aborted) return;
    logLine("dim", `stream dropped, reconnecting after event ${last}…`);
    await new Promise((r) => setTimeout(r, 2000));
  }
}

// ------------------------------------------------------------------ rendering

const t0 = { at: 0 };
function logLine(cls, text, extra) {
  const secs = t0.at ? Math.round((Date.now() - t0.at) / 1000) : 0;
  const li = el("li", {}, el("span", { class: "t" }, secs + "s"), el("span", { class: "msg " + (cls || "") }, text, extra || null));
  const log = $("#log");
  const stick = log.scrollTop + log.clientHeight >= log.scrollHeight - 8;
  log.append(li);
  if (stick) log.scrollTop = log.scrollHeight;
}

const secs = (ns) => Math.round((ns || 0) / 1e8) / 10;
const ms = (ns) => Math.round((ns || 0) / 1e6);

function describe(ev) {
  const tag = ev.stmt > 0 ? `stmt_${ev.stmt}: ` : "";
  const a = ev.attempt;
  switch (ev.type) {
    case "state": return ["dim", `state → ${ev.state}`];
    case "split": return ["dim", `split into ${ev.message}`];
    case "formalizing": return ["dim", `${tag}formalizing (${ev.message})`];
    case "statement_error": return ["bad", `${tag}Lean rejected the statement: ${(ev.message || "").split("\n")[0]}`];
    case "formalized": {
      const s = ev.statement || {};
      return ["", `${tag}formalized\n    theorem ${s.name} ${oneLine(s.signature)}\n    means: ${oneLine(s.back_translation)}`];
    }
    case "revising": return ["warn", `${tag}re-formalizing: ${ev.message}`];
    case "confirm_required": return ["warn", `${tag}waiting for your confirmation`];
    case "attempt": {
      if (a.stage === "auto") return ["ok", `${tag}automation \`${a.body}\` ✓ verified`];
      const status = a.verified ? "✓ verified" : "✗ " + (a.error_kinds || []).join(", ");
      const tools = a.tool_calls ? `, ${a.tool_calls} tool calls` : "";
      return [a.verified ? "ok" : "", `${tag}[${a.stage}] attempt ${a.session}.${a.n} ${status}  (llm ${secs(a.llm_time)}s, ${a.reasoning_tokens || 0} thinking tok${tools}, lean ${ms(a.lean_time)}ms, ${money(a.cost_usd)})`];
    }
    case "stage": case "automation": case "thinking": return ["dim", tag + ev.message];
    case "proved": return ["ok", `${tag}PROVED`];
    case "failed": return ["bad", `${tag}FAILED: ${ev.statement?.error || ""}`];
    case "explained": return ["dim", `${tag}explanation ready`];
    case "verifying": return ["dim", `${ev.message}…`];
    case "verified": return ["ok", `verified: ${ev.message}`];
    case "error": return ["bad", `${tag}error: ${ev.message}`];
    case "done": return ["ok", `done: ${ev.message}`];
    default: return ["", `${ev.type} ${ev.message || ""}`];
  }
}

function showEvent(ev, raw) {
  if (settings.raw) { logLine("mono", raw); return; }
  const [cls, text] = describe(ev);
  let extra = null;
  if (settings.feedback && ev.type === "attempt" && !ev.attempt.verified && ev.attempt.feedback) {
    extra = el("pre", { class: "wrap" }, ev.attempt.feedback);
  }
  logLine(cls, text, extra);
}

function badge(status) {
  const cls = status === "proved" ? "ok" : status === "unverified" ? "warn" : "bad";
  return el("span", { class: "badge " + cls }, status);
}

function copyButton(getText, label = "Copy") {
  const b = el("button", { class: "ghost", type: "button" }, label);
  b.addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(getText()); b.textContent = "Copied"; setTimeout(() => (b.textContent = label), 1200); } catch {}
  });
  return b;
}

function renderResult(job, rawText) {
  const box = $("#result");
  box.replaceChildren();
  if (settings.raw) {
    box.append(el("section", { class: "card" }, el("h2", {}, "Result (raw)"), el("pre", { class: "wrap" }, rawText)));
    return;
  }
  const card = el("section", { class: "card" });
  const head = el("div", { class: "row" }, el("h2", {}, "Result"), el("span", { class: "spacer" }),
    el("span", { class: "muted" }, `${job.state}${job.error ? " — " + job.error : ""} · ${money(job.cost_usd)} · ${job.llm_calls} LLM calls`));
  card.append(head);
  for (const s of job.statements || []) {
    const st = el("div", { class: "stmt" },
      el("div", { class: "row" }, el("strong", { class: "mono" }, s.name), badge(s.status)),
      el("div", {}, oneLine(s.text)));
    const kv = el("dl", { class: "kv" });
    if (s.signature) kv.append(el("dt", {}, "Lean"), el("dd", { class: "mono" }, `theorem ${s.name} ${oneLine(s.signature)}`));
    if (s.back_translation) kv.append(el("dt", {}, "Means"), el("dd", { class: "math" }, oneLine(s.back_translation)));
    if (s.formalization_notes) kv.append(el("dt", {}, "Notes"), el("dd", { class: "muted" }, s.formalization_notes));
    if (s.error && s.status !== "proved") kv.append(el("dt", {}, "Error"), el("dd", { class: "bad" }, s.error));
    st.append(kv);
    if (s.proof) {
      st.append(el("div", { class: "row" }, el("span", { class: "label" }, "Proof"), el("span", { class: "spacer" }), copyButton(() => s.proof)),
        el("pre", {}, s.proof));
    }
    if (s.verification?.ok) {
      st.append(el("div", { class: "verified" }, `✓ Independently verified (kernel re-check; axioms: ${(s.verification.axioms || []).join(", ")})`));
    }
    if (s.explanation) {
      st.append(el("span", { class: "label" }, "Explanation"), el("div", { class: "math explanation" }, s.explanation));
    }
    card.append(st);
  }
  if (job.lean_file) {
    const dl = el("button", { class: "ghost", type: "button" }, "Download .lean");
    dl.addEventListener("click", () => {
      const a = el("a", { href: URL.createObjectURL(new Blob([job.lean_file], { type: "text/plain" })), download: `quod-${job.id}.lean` });
      a.click(); setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    });
    card.append(el("div", { class: "row", style: "margin-top:16px" }, el("span", { class: "label" }, "Lean file"),
      el("span", { class: "spacer" }), copyButton(() => job.lean_file), dl), el("pre", {}, job.lean_file));
  }
  box.append(card);
  renderMath(card);
}

// ------------------------------------------------------------------ confirmation

function showConfirm(jobId, p) {
  const box = $("#confirm");
  const correction = el("textarea", { rows: 2, placeholder: "e.g. x is a real number, not a natural number" });
  const send = async (decision) => {
    box.querySelectorAll("button").forEach((b) => (b.disabled = true));
    try {
      await api("POST", `/v1/jobs/${encodeURIComponent(jobId)}/confirm`, { stmt: p.index, ...decision });
      box.hidden = true;
    } catch (e) {
      box.querySelectorAll("button").forEach((b) => (b.disabled = false));
      logLine("bad", e.message);
    }
  };
  box.replaceChildren(
    el("h2", {}, `Confirm ${p.name}`),
    el("dl", { class: "kv" },
      el("dt", {}, "You said"), el("dd", {}, oneLine(p.text)),
      el("dt", {}, "Lean"), el("dd", { class: "mono" }, `theorem ${p.name} ${oneLine(p.signature)}`),
      el("dt", {}, "Means"), el("dd", { class: "math" }, oneLine(p.back_translation)),
      p.notes ? [el("dt", {}, "Notes"), el("dd", { class: "muted" }, p.notes)] : null),
    el("label", { class: "label", style: "margin-top:10px" }, "Correction (optional)"),
    correction,
    el("div", { class: "row" },
      el("button", { class: "ghost danger", type: "button", onclick: () => send({ accept: false }) }, "Skip statement"),
      el("span", { class: "spacer" }),
      el("button", { class: "ghost", type: "button", onclick: () => {
        const c = correction.value.trim();
        if (c) send({ accept: false, correction: c }); else correction.focus();
      } }, "Re-formalize with correction"),
      el("button", { class: "primary", type: "button", onclick: () => send({ accept: true }) }, "Prove this")));
  box.hidden = false;
  renderMath(box);
  box.scrollIntoView({ behavior: "smooth", block: "nearest" });
}

// ------------------------------------------------------------------ jobs

let current = null; // { id, abort }

async function follow(jobId, { fresh } = {}) {
  current?.abort.abort();
  const abort = new AbortController();
  current = { id: jobId, abort };
  $("#progress-card").hidden = false;
  $("#job-id").textContent = "job " + jobId;
  $("#confirm").hidden = true;
  $("#cancel").hidden = false;
  if (!fresh) { $("#log").replaceChildren(); t0.at = Date.now(); }
  try {
    await streamEvents(jobId, async (ev, raw) => {
      showEvent(ev, raw);
      if (ev.type === "confirm_required" && ev.proposal) {
        // Answer only if still pending (we may be replaying history).
        const { json } = await api("GET", `/v1/jobs/${encodeURIComponent(jobId)}`);
        if (json.pending_confirmation?.index === ev.proposal.index) showConfirm(jobId, ev.proposal);
      }
      if (ev.type === "state" && ["done", "failed", "canceled"].includes(ev.state)) $("#cancel").hidden = true;
    }, abort.signal);
  } catch (e) {
    if (!abort.signal.aborted) logLine("bad", e.message);
  }
  if (abort.signal.aborted) return;
  $("#cancel").hidden = true;
  $("#confirm").hidden = true;
  try {
    const { json, text } = await api("GET", `/v1/jobs/${encodeURIComponent(jobId)}${settings.feedback ? "?detail=full" : ""}`);
    renderResult(json, text);
  } catch (e) { logLine("bad", e.message); }
  refreshJobs(); refreshStatus();
}

async function submit() {
  const input = $("#input").value.trim();
  if (!input) { $("#input").focus(); return; }
  if (!base() || !accessKey) { openSettings(); return; }
  const options = {};
  if (settings.confirm) options.confirm = true;
  if (!settings.split) options.no_split = true;
  if (!settings.explain) options.no_explain = true;
  if (settings.attempts > 0) options.attempts = Number(settings.attempts);
  if (settings.parallel > 0) options.parallel = Number(settings.parallel);
  if (settings.budget > 0) options.budget_usd = Number(settings.budget);
  $("#submit").disabled = true;
  $("#result").replaceChildren();
  $("#log").replaceChildren();
  t0.at = Date.now();
  $("#progress-card").hidden = false;
  try {
    const { json, text } = await api("POST", "/v1/jobs", { input, options });
    if (settings.raw) logLine("mono", text.trim());
    else logLine("", `job ${json.id} submitted (budget ${money(json.budget_usd)})`);
    follow(json.id, { fresh: true });
  } catch (e) {
    logLine("bad", e.message);
  } finally {
    $("#submit").disabled = false;
  }
}

async function cancelCurrent() {
  if (!current) return;
  try { await api("POST", `/v1/jobs/${encodeURIComponent(current.id)}/cancel`); logLine("warn", "cancel requested"); }
  catch (e) { logLine("bad", e.message); }
}

async function refreshJobs() {
  const box = $("#jobs");
  if (!base() || !accessKey) { box.textContent = "Configure the server and access key in Settings."; return; }
  try {
    // The list stays a table in raw mode so jobs can still be opened; raw
    // mode applies to a job's progress and result.
    const { json } = await api("GET", "/v1/jobs?limit=20");
    if (!json.jobs.length) { box.textContent = "No jobs yet."; return; }
    const table = el("table", { class: "jobs" },
      el("thead", {}, el("tr", {}, ["State", "Proved", "Cost", "Created", "Input", ""].map((h) => el("th", {}, h)))),
      el("tbody", {}, json.jobs.map((j) => el("tr", {},
        el("td", {}, j.state + (j.awaiting_confirmation ? " *" : "")),
        el("td", {}, `${j.proved}/${j.total}`),
        el("td", {}, money(j.cost_usd)),
        el("td", { class: "mono" }, (j.created || "").slice(0, 16).replace("T", " ")),
        el("td", { class: "input", title: j.input }, oneLine(j.input)),
        el("td", {}, el("button", { class: "ghost", type: "button", onclick: () => follow(j.id) }, "Open"))))));
    box.replaceChildren(el("div", { class: "jobs-wrap" }, table));
  } catch (e) {
    box.textContent = e.message;
  }
}

async function refreshStatus() {
  const s = $("#status");
  s.className = "status";
  if (!base()) { s.textContent = "not configured"; return; }
  try {
    const { json } = await api("GET", "/v1/status");
    const u = json.usage || {};
    const budget = u.daily_budget_usd ? ` of ${money(u.daily_budget_usd)}` : "";
    s.textContent = `${json.principal?.name} · today ${money(u.spent_today_usd)}${budget} · jobs ${u.active_jobs}/${u.max_active_jobs}`;
    s.classList.add("ok");
  } catch (e) {
    s.textContent = e.code === "unauthorized" ? "access key rejected" : e.message;
    s.classList.add("bad");
  }
}

// ------------------------------------------------------------------ settings dialog

const form = $("#settings-form");

function openSettings() {
  for (const [k, v] of Object.entries(settings)) {
    const f = form.elements[k];
    if (!f) continue;
    if (f.type === "checkbox") f.checked = !!v; else f.value = v;
  }
  form.elements.key.value = accessKey;
  form.elements.key.type = "password";
  $("#toggle-key").textContent = "Show";
  $("#settings-error").hidden = true;
  $("#settings").showModal();
}

function readForm() {
  const s = { ...settings };
  for (const k of Object.keys(DEFAULTS)) {
    const f = form.elements[k];
    if (!f) continue;
    s[k] = f.type === "checkbox" ? f.checked : f.type === "number" ? Number(f.value || 0) : f.value.trim();
  }
  s.server = s.server.replace(/\/+$/, "");
  return s;
}

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  const next = readForm();
  const key = form.elements.key.value.trim();
  const err = $("#settings-error");
  err.hidden = true;
  if (!/^https?:\/\/[^/]+$/.test(next.server)) {
    err.textContent = "Server URL must look like https://host[:port] (no path).";
    err.hidden = false; return;
  }
  if (key && !key.startsWith("qk_")) {
    err.textContent = "Access keys start with qk_.";
    err.hidden = false; return;
  }
  settings = next;
  accessKey = key;
  saveSettings(settings);
  storeKey(key, settings.rememberKey);
  $("#save-settings").disabled = true;
  try {
    await api("GET", "/readyz");
    if (key) await api("GET", "/v1/status");
    $("#settings").close();
    refreshStatus(); refreshJobs();
  } catch (e2) {
    err.textContent = e2.message;
    err.hidden = false;
  } finally {
    $("#save-settings").disabled = false;
  }
});

$("#toggle-key").addEventListener("click", () => {
  const f = form.elements.key;
  f.type = f.type === "password" ? "text" : "password";
  $("#toggle-key").textContent = f.type === "password" ? "Show" : "Hide";
});
$("#forget-key").addEventListener("click", () => {
  accessKey = ""; storeKey("", false); form.elements.key.value = "";
  refreshStatus(); refreshJobs();
});
$("#close-settings").addEventListener("click", () => $("#settings").close());
$("#open-settings").addEventListener("click", openSettings);

// ------------------------------------------------------------------ wiring

$("#submit").addEventListener("click", submit);
$("#cancel").addEventListener("click", cancelCurrent);
$("#refresh-jobs").addEventListener("click", refreshJobs);
$("#input").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); submit(); }
});

if (!base() || !accessKey) openSettings();
else { refreshStatus(); refreshJobs(); }
