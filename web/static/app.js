// The page is deliberately plain: poll the JSON API, rebuild the lists, no
// framework and no build step, so it can be edited on the server if it ever needs
// to be.

const POLL_MS = 3000;

const state = { paused: false, series: [], log: null };

// api calls the JSON API and turns an error response into an Error whose message
// the page can show as-is.
async function api(path, options = {}) {
  const res = await fetch(path, {
    headers: options.body ? { "Content-Type": "application/json" } : undefined,
    ...options,
  });
  if (res.status === 204) return null;
  const text = await res.text();
  let body = null;
  if (text) {
    try { body = JSON.parse(text); } catch { body = null; }
  }
  if (!res.ok) throw new Error((body && body.error) || `HTTP ${res.status}`);
  return body;
}

// el builds an element with attributes and children, which keeps the render code
// free of HTML strings and therefore free of escaping bugs.
function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") node.className = value;
    else if (key === "text") node.textContent = value;
    else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value === true ? "" : value);
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

const fmtDate = (iso) => {
  if (!iso) return "—";
  const d = new Date(iso);
  if (isNaN(d)) return "—";
  return d.toLocaleString(undefined, { dateStyle: "short", timeStyle: "short" });
};

const STATUS_LABEL = {
  queued: "en cola",
  running: "descargando",
  done: "completado",
  failed: "fallido",
  cancelled: "cancelado",
  interrupted: "interrumpido",
  none: "sin empezar",
};

// ---- series --------------------------------------------------------------

function jobProgress(job) {
  if (!job || !job.total) return null;
  return Math.max(0, Math.min(100, Math.round((job.done / job.total) * 100)));
}

// renderSeries draws one entry per series, with its most recent run inside it.
// The run history is there for the log, not for the list: a series downloaded
// ten times is still one thing the operator wants.
function renderSeries(entry) {
  const job = entry.job;
  const status = job ? job.status : "none";
  const percent = jobProgress(job);
  const running = job && (job.status === "running" || job.status === "queued");

  const bar = el(
    "div",
    { class: percent === null && running ? "bar indeterminate" : "bar", title: job && job.total ? `${job.done}/${job.total} episodios` : "" },
    el("div", { style: `width:${percent === null ? 0 : percent}%` })
  );

  const tracks = Object.entries((job && job.tracks) || {}).map(([name, pct]) =>
    el("span", { text: `${name} ${Math.round(pct)}%` })
  );

  const lines = [];
  if (job && job.current) lines.push(el("div", { class: "job-line", text: job.current }));
  if (job && job.waiting) lines.push(el("div", { class: "job-line warn", text: job.waiting }));
  if (job && job.uploading) lines.push(el("div", { class: "job-line muted", text: "subiendo al servidor…" }));
  if (job && job.total) lines.push(el("div", { class: "job-line muted", text: `${job.done} de ${job.total} episodios` }));
  if (job && job.failures && job.failures.length) {
    lines.push(el("div", { class: "job-line bad", text: `${job.failures.length} episodio(s) con error:` }));
    for (const failure of job.failures.slice(0, 5)) {
      lines.push(el("div", { class: "job-line bad", text: "· " + failure }));
    }
  }
  if (job && job.error) lines.push(el("div", { class: "job-line bad", text: job.error }));

  const actions = [];
  if (running) {
    actions.push(el("button", { class: "secondary tiny", type: "button", text: "Cancelar", onclick: () => cancelJob(job.id) }));
  } else {
    actions.push(el("button", { class: "secondary tiny", type: "button", text: entry.inWishlist ? "Buscar nuevos" : "Reintentar", onclick: () => queueSeries(entry) }));
  }
  if (job) actions.push(el("button", { class: "secondary tiny", type: "button", text: "Registro", onclick: () => openLog(entry) }));
  if (entry.inWishlist) {
    actions.push(el("button", { class: "danger tiny", type: "button", text: "Quitar", onclick: () => removeSeries(entry) }));
  } else if (job) {
    actions.push(el("button", { class: "danger tiny", type: "button", text: "Borrar", onclick: () => deleteSeries(entry) }));
  }

  const meta = [entry.url];
  if (job) meta.push(`última ejecución ${fmtDate(job.createdAt)}`);
  if (entry.runs > 1) meta.push(`${entry.runs} ejecuciones`);

  return el(
    "div",
    { class: "job" },
    el(
      "div",
      { class: "job-head" },
      el(
        "div",
        { class: "job-title" },
        entry.title || "Serie",
        el("small", { text: entry.season ? `temporada ${entry.season}` : "todas las temporadas" })
      ),
      el(
        "div",
        { style: "display:flex;align-items:center;gap:.6rem;flex-wrap:wrap" },
        el("span", { class: `status ${status}`, text: STATUS_LABEL[status] || status }),
        el("div", { class: "job-actions" }, actions)
      )
    ),
    job ? bar : null,
    tracks.length ? el("div", { class: "tracks" }, tracks) : null,
    ...lines,
    el("div", { class: "job-line muted", text: meta.join(" · ") })
  );
}

function renderSeriesList() {
  const container = document.getElementById("series");
  container.replaceChildren(...state.series.map(renderSeries));
  document.getElementById("series-empty").hidden = state.series.length > 0;
}

async function cancelJob(id) {
  if (!confirm("¿Cancelar esta descarga?")) return;
  await guarded(() => api(`/api/jobs/${id}/cancel`, { method: "POST" }));
  refresh();
}

// queueSeries re-queues a series: an entry in the list through the wishlist (so
// new episodes land in the same place), an ad-hoc download through the same URL.
async function queueSeries(entry) {
  const response = await guarded(() =>
    entry.inWishlist
      ? api(`/api/wishlist/${entry.id}/download`, { method: "POST" })
      : api("/api/jobs", { method: "POST", body: JSON.stringify({ url: entry.url, season: entry.season }) })
  );
  // The API returns the run it queued, and a series that is already active comes
  // back as that same run, so the message always confirms the click landed.
  if (response) {
    const name = entry.title || "La serie";
    say(response.status === "running" ? `${name} ya se está descargando` : `${name}: en cola`);
  }
  refresh();
}

async function removeSeries(entry) {
  if (!confirm(`¿Quitar "${entry.title || entry.url}" de la lista? Lo descargado se queda donde está.`)) return;
  await guarded(() => api(`/api/wishlist/${entry.id}`, { method: "DELETE" }));
  refresh();
}

async function deleteSeries(entry) {
  if (!confirm("¿Borrar esta descarga del historial?")) return;
  await guarded(() => api(`/api/jobs/${entry.job.id}`, { method: "DELETE" }));
  refresh();
}


// ---- shell ---------------------------------------------------------------

function renderQueueState() {
  const pill = document.getElementById("queue-state");
  const jobs = state.series.map((entry) => entry.job).filter(Boolean);
  const running = jobs.find((job) => job.status === "running");
  const queued = jobs.filter((job) => job.status === "queued").length;

  if (state.paused) {
    pill.className = "pill paused";
    pill.textContent = "en pausa";
  } else if (running) {
    pill.className = "pill running";
    pill.textContent = queued ? `descargando · ${queued} en cola` : "descargando";
  } else {
    pill.className = "pill";
    pill.textContent = queued ? `${queued} en cola` : "inactivo";
  }
  document.getElementById("pause-btn").textContent = state.paused ? "Reanudar" : "Pausar";
}

// guarded runs an action and reports a failure without breaking the page.
async function guarded(fn) {
  try {
    return await fn();
  } catch (err) {
    alert(err.message || String(err));
    return null;
  }
}

async function refresh() {
  try {
    const data = await api("/api/jobs");
    state.series = data.series || [];
    state.paused = !!data.paused;
    renderSeriesList();
    renderQueueState();
  } catch (err) {
    const pill = document.getElementById("queue-state");
    pill.className = "pill paused";
    pill.textContent = "sin conexión";
  }
}

// ---- config --------------------------------------------------------------

const configForm = document.getElementById("config-form");

async function loadConfig() {
  const cfg = await api("/api/config");
  for (const [key, value] of Object.entries(cfg)) {
    if (key === "sftp" || key === "hasEtpRt") continue;
    const field = configForm.elements[key];
    if (!field) continue;
    if (field.type === "checkbox") field.checked = !!value;
    else field.value = value ?? "";
  }
  for (const [key, value] of Object.entries(cfg.sftp || {})) {
    const field = configForm.elements[`sftp.${key}`];
    if (!field) continue;
    if (field.type === "checkbox") field.checked = !!value;
    else field.value = value ?? "";
  }
  document.getElementById("etp-rt").value = "";
  document.getElementById("etp-rt").placeholder = cfg.hasEtpRt ? "(guardada)" : "(sin cookie)";
  document.getElementById("etp-rt-state").textContent = cfg.hasEtpRt ? "Hay una cookie guardada." : "Falta la cookie: las descargas fallarán.";
  document.getElementById("sftp-password").value = "";
  document.getElementById("sftp-password").placeholder = cfg.sftp.hasSecret ? "(guardada)" : "(sin contraseña)";
  document.getElementById("sftp-password-state").textContent = cfg.sftp.hasSecret ? "Hay una contraseña guardada." : "";
  toggleSFTP(cfg.destination);
}

function toggleSFTP(destination) {
  const fields = document.getElementById("sftp-fields");
  const isSFTP = destination === "sftp";
  fields.disabled = !isSFTP;
  fields.style.opacity = isSFTP ? "1" : ".55";
}

configForm.elements.destination.addEventListener("change", (event) => toggleSFTP(event.target.value));

configForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = new FormData(configForm);
  const body = {
    downloadDir: form.get("downloadDir") || "",
    destination: form.get("destination") || "local",
    videoQuality: form.get("videoQuality") || "",
    audioQuality: form.get("audioQuality") || "",
    audioLang: form.get("audioLang") || "",
    subsLang: form.get("subsLang") || "",
    ccLang: form.get("ccLang") || "",
    downloadDelay: form.get("downloadDelay") || "",
    renumberSeasons: form.get("renumberSeasons") === "on",
    absoluteNumbering: form.get("absoluteNumbering") === "on",
    sftp: {
      host: form.get("sftp.host") || "",
      port: form.get("sftp.port") || "",
      user: form.get("sftp.user") || "",
      keyFile: form.get("sftp.keyFile") || "",
      remoteDir: form.get("sftp.remoteDir") || "",
      knownHosts: form.get("sftp.knownHosts") || "",
      keepLocal: form.get("sftp.keepLocal") === "on",
    },
  };

  // Secrets are only sent when the operator typed a new one; leaving the field
  // empty keeps whatever is stored, which is why the page never shows it.
  const etpRt = document.getElementById("etp-rt").value;
  if (etpRt) body.etpRt = etpRt;
  const sftpPassword = document.getElementById("sftp-password").value;
  if (sftpPassword) body.sftpPassword = sftpPassword;

  const status = document.getElementById("config-status");
  try {
    await api("/api/config", { method: "PUT", body: JSON.stringify(body) });
    status.textContent = "Guardado.";
    setTimeout(() => (status.textContent = ""), 2500);
    loadConfig();
  } catch (err) {
    status.textContent = err.message;
  }
});

// ---- wishlist form -------------------------------------------------------

document.getElementById("add-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const url = document.getElementById("add-url").value.trim();
  const title = document.getElementById("add-title").value.trim();
  const season = Number(document.getElementById("add-season").value || 0);
  const response = await guarded(() =>
    api("/api/wishlist", { method: "POST", body: JSON.stringify({ url, title, season }) })
  );
  if (response) {
    event.target.reset();
    document.getElementById("add-season").value = "0";
    refresh();
  }
});

// say writes a short message next to the Series heading, since the list is rebuilt
// on every refresh and anything appended to it would vanish.
let sayTimer = null;
function say(message) {
  const status = document.getElementById("series-status");
  status.textContent = message;
  clearTimeout(sayTimer);
  sayTimer = setTimeout(() => (status.textContent = ""), 8000);
}

document.getElementById("download-all-btn").addEventListener("click", async () => {
  const response = await guarded(() => api("/api/wishlist/download", { method: "POST" }));
  if (!response) return;
  const queued = response.queued || 0;
  const running = response.alreadyRunning || 0;
  const parts = [];
  if (queued) parts.push(`${queued} en cola`);
  if (running) parts.push(`${running} ya estaba en marcha`);
  say(parts.length ? parts.join(" · ") : "Nada que encolar");
  refresh();
});

document.getElementById("pause-btn").addEventListener("click", async () => {
  await guarded(() => api("/api/pause", { method: "POST", body: JSON.stringify({ paused: !state.paused }) }));
  refresh();
});

// ---- log viewer ----------------------------------------------------------

const dialog = document.getElementById("log-dialog");
const logBody = document.getElementById("log-body");

async function openLog(entry) {
  const job = entry.job;
  state.log = { id: job.id, offset: 0, done: false };
  logBody.textContent = "";
  document.getElementById("log-title").textContent = `Registro · ${entry.title || entry.url}`;
  document.getElementById("log-status").textContent = "";
  dialog.showModal();
  await pollLog();
}

async function pollLog() {
  const current = state.log;
  if (!current) return;
  try {
    const data = await api(`/api/jobs/${current.id}/log?offset=${current.offset}`);
    if (state.log !== current) return; // another log was opened meanwhile
    if (data.text) {
      logBody.append(document.createTextNode(data.text));
      if (document.getElementById("log-follow").checked) logBody.scrollTop = logBody.scrollHeight;
    }
    current.offset = data.offset;
    current.done = data.done;
    document.getElementById("log-status").textContent = data.done ? "El trabajo ha terminado." : "Actualizándose…";
  } catch (err) {
    document.getElementById("log-status").textContent = err.message;
    current.done = true;
  }
  if (!current.done) setTimeout(pollLog, 1500);
}

document.getElementById("log-close").addEventListener("click", () => {
  state.log = null;
  dialog.close();
});

// ---- start ---------------------------------------------------------------

refresh();
loadConfig();
setInterval(refresh, POLL_MS);
setInterval(() => {
  if (state.log && !state.log.done) pollLog();
}, 1500);
