"use strict";

const $ = (id) => document.getElementById(id);
let token = "";
let library = { games: [], platforms: [], max_rom_bytes: 0 };
let devices = [];
let jobs = [];
let page = 0;
let selectedGameID = "";
let filteredGames = [];
let refreshInFlight = false;
let uploading = false;
let viewVersion = 0;
const pageSize = 50;
const imageURLs = [];
const profileNames = { BATOCERA: "Batocera", MIYOO: "Miyoo Mini Plus", TVOS: "Apple TV" };

function node(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

function size(bytes) {
  if (!bytes) return "0 B";
  const unit = Math.min(3, Math.floor(Math.log(bytes) / Math.log(1024)));
  return `${(bytes / 1024 ** unit).toFixed(unit ? 1 : 0)} ${["B", "KiB", "MiB", "GiB"][unit]}`;
}

function notify(message, error = false) {
  $("notice").textContent = message;
  $("notice").className = error ? "error" : "";
  $("notice").hidden = false;
}

async function api(url, options = {}) {
  const response = await fetch(url, {
    ...options,
    headers: { ...options.headers, Authorization: `Bearer ${token}` },
    cache: "no-store",
  });
  if (response.status === 401) {
    lock();
    throw new Error("Session locked. Enter a valid admin token.");
  }
  if (!response.ok) {
    const body = await response.json();
    throw new Error(body.error || `Request failed (${response.status})`);
  }
  return response;
}

function post(url, data) {
  return api(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) });
}

function releaseImages() {
  viewVersion++;
  imageURLs.splice(0).forEach((url) => URL.revokeObjectURL(url));
}

function lock() {
  token = "";
  library = { games: [], platforms: [], max_rom_bytes: 0 };
  devices = [];
  jobs = [];
  releaseImages();
  $("dashboard").hidden = true;
  $("login").hidden = false;
  $("token").value = "";
  selectedGameID = "";
  $("game-list").replaceChildren();
  $("game-detail").replaceChildren();
  $("device-list").replaceChildren();
  $("job-list").replaceChildren();
  document.querySelectorAll("dialog[open]").forEach((dialog) => dialog.close());
}

function switchTab(tab) {
  for (const name of ["library", "devices", "jobs"]) $(`${name}-panel`).hidden = name !== tab;
  document.querySelectorAll("[data-tab]").forEach((button) => {
    button.classList.toggle("active", button.dataset.tab === tab);
    if (button.dataset.tab === tab) button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
  });
  $("breadcrumb").textContent = { library: "Game library", devices: "Devices", jobs: "Metadata jobs" }[tab];
}

function updateSelect(select, values, all = false) {
  const previous = select.value;
  select.replaceChildren();
  if (all) select.append(new Option("All platforms", ""));
  values.forEach((value) => select.append(new Option(value.toUpperCase(), value)));
  if (values.includes(previous) || (all && previous === "")) select.value = previous;
}

async function refresh() {
  if (!token || refreshInFlight) return;
  refreshInFlight = true;
  const activeToken = token;
  try {
    const results = await Promise.all(["library", "devices", "jobs"].map(async (name) => (await api(`/api/${name}`)).json()));
    if (!token || token !== activeToken) return;
    const changed = library.revision !== results[0].revision;
    [library, devices, jobs] = results;
    $("game-count").textContent = library.games.length.toLocaleString();
    $("platform-count").textContent = library.platforms.length;
    $("device-count").textContent = devices.filter((d) => d.approved).length;
    $("library-size").textContent = size(library.games.reduce((sum, game) => sum + game.size, 0));
    const pending = devices.filter((d) => !d.approved).length;
    $("approval-count").textContent = pending ? `${pending} awaiting approval` : "No pending approvals";
    $("pending-badge").hidden = !pending;
    $("pending-badge").textContent = pending;
    updateSelect($("platform-filter"), library.platforms, true);
    updateSelect($("sync-platform"), library.platforms);
    $("platforms").replaceChildren(...library.platforms.map((p) => new Option(p, p)));
    $("upload-limit").textContent = `Maximum ${size(library.max_rom_bytes)} per file. ZIP/7z files stay archived.`;
    if (changed || !$("game-list").childElementCount) renderGames(true);
    renderDevices();
    renderJobs();
  } finally {
    refreshInFlight = false;
  }
}

function renderGames(preserveSelection = false) {
  const query = $("search").value.toLowerCase();
  const platform = $("platform-filter").value;
  const games = library.games.filter((game) => (!platform || game.platform === platform) && `${game.title} ${game.id}`.toLowerCase().includes(query));
  games.sort((a,b) => a.title.localeCompare(b.title) || a.id.localeCompare(b.id));
  filteredGames = games;
  if (preserveSelection) {
    const selectedIndex = games.findIndex((game) => game.id === selectedGameID);
    if (selectedIndex >= 0) page = Math.floor(selectedIndex / pageSize);
  }
  const pages = Math.max(1, Math.ceil(games.length / pageSize));
  page = Math.min(page, pages - 1);
  $("game-list").replaceChildren();
  $("result-count").textContent = `${games.length} games`;
  $("page-label").textContent = `${page + 1} / ${pages}`;
  $("previous").disabled = page === 0;
  $("next").disabled = page >= pages - 1;
  if (!games.length) $("game-list").append(node("div", "empty", "No games match this view."));
  const visible = games.slice(page * pageSize, (page + 1) * pageSize);
  if (!visible.some((game) => game.id === selectedGameID)) selectedGameID = visible[0]?.id || "";
  for (const [index,game] of visible.entries()) {
    const row = node("div","game-row");
    row.id = `game-option-${page * pageSize + index}`;
    row.dataset.gameId = game.id;
    row.setAttribute("role","option");
    row.setAttribute("aria-selected",String(game.id === selectedGameID));
    const label = node("div","game-row-label");
    label.append(node("strong","",game.title),node("small","",game.id.split("/").slice(1).join("/")));
    row.append(node("span","row-platform",game.platform.toUpperCase()),label,node("small","row-size",size(game.size)));
    row.addEventListener("click",() => { selectGame(game.id); $("game-list").focus({preventScroll:true}); });
    $("game-list").append(row);
  }
  selectGame(selectedGameID);
}

function selectGame(id) {
  selectedGameID = id;
  for (const row of $("game-list").querySelectorAll('[role="option"]')) {
    const active = row.dataset.gameId === id;
    row.setAttribute("aria-selected",String(active));
    if (active) {
      $("game-list").setAttribute("aria-activedescendant",row.id);
      const list = $("game-list");
      const rowBounds = row.getBoundingClientRect();
      const listBounds = list.getBoundingClientRect();
      if (rowBounds.top < listBounds.top) list.scrollTop -= listBounds.top - rowBounds.top;
      else if (rowBounds.bottom > listBounds.bottom) list.scrollTop += rowBounds.bottom - listBounds.bottom;
    }
  }
  releaseImages();
  const version = viewVersion;
  const detail = $("game-detail");
  detail.replaceChildren();
  const game = filteredGames.find((item) => item.id === id);
  if (!game) {
    $("game-list").removeAttribute("aria-activedescendant");
    detail.append(node("div","empty","Select a game to explore its metadata."));
    return;
  }
  const content = node("div","detail-content");
  const art = node("div","detail-art");
  const placeholder = node("span","art-symbol","r/");
  art.append(placeholder,node("span","platform-pill",game.platform.toUpperCase()));
  content.append(art);
  const text = node("div","detail-text");
  text.append(node("p","eyebrow","NOW EXPLORING"),node("h2","",game.title));
  const metadata = game.metadata || {};
  const description = node("p","game-description",metadata.desc || "No description yet. Sync metadata for this platform to discover more about this game.");
  text.append(description);
  const facts = node("dl","game-facts");
  for (const [label,value] of [
    ["Platform",game.platform.toUpperCase()],["Size",size(game.size)],
    ["Developer",metadata.developer],["Publisher",metadata.publisher],["Genre",metadata.genre],
    ["Players",metadata.players],["Released",metadata.releasedate],["Rating",metadata.rating],
  ]) {
    if (!value) continue;
    const pair = node("div");
    pair.append(node("dt","",label),node("dd","",value));
    facts.append(pair);
  }
  text.append(facts,node("p","eyebrow","AVAILABLE ASSETS"));
  const badges = node("div","asset-badges");
  const assets = Object.keys(game.assets || {});
  if (!assets.length) badges.append(node("span","muted","No media synced yet"));
  assets.forEach((name) => badges.append(node("span","status",name.toUpperCase())));
  text.append(badges,node("p","file-path",game.id),node("code","checksum",`SHA-256 ${game.sha256}`));
  content.append(text);
  detail.append(content);
  if (game.image) {
    api(`/api/media?id=${encodeURIComponent(game.image)}`)
        .then((response) => response.blob())
        .then((blob) => {
          if (version !== viewVersion || !token) return;
          const url = URL.createObjectURL(blob);
          imageURLs.push(url);
          const img = node("img");
          img.alt = `${game.title} artwork`;
          img.src = url;
          placeholder.replaceWith(img);
        })
        .catch((error) => { if (version === viewVersion) notify(`Artwork could not load: ${error.message}`, true); });
  }
}

$("game-list").addEventListener("keydown",(event) => {
  if (!["ArrowDown","ArrowUp","Home","End"].includes(event.key) || !filteredGames.length) return;
  event.preventDefault();
  let index = filteredGames.findIndex((game) => game.id === selectedGameID);
  if (event.key === "Home") index = 0;
  else if (event.key === "End") index = filteredGames.length - 1;
  else index = Math.max(0,Math.min(filteredGames.length - 1,index + (event.key === "ArrowDown" ? 1 : -1)));
  const next = filteredGames[index];
  const nextPage = Math.floor(index / pageSize);
  if (nextPage !== page) { page = nextPage; selectedGameID = next.id; renderGames(); }
  else selectGame(next.id);
});

function renderDevices() {
  $("device-list").replaceChildren();
  if (!devices.length) $("device-list").append(node("div", "empty", "No devices registered. Enroll a client through RegisterDevice to see it here."));
  for (const device of devices) {
    const row = node("article", "device-row");
    const info = node("div");
    info.append(node("h3", "", device.name), node("p", "device-meta", `${profileNames[device.profile] || device.profile} / ${device.approved ? "Approved" : "Awaiting approval"}`), node("code", "", device.id));
    const button = node("button", device.approved ? "" : "primary", device.approved ? "Revoke access" : "Approve device");
    button.addEventListener("click", async () => {
      button.disabled = true;
      try {
        await post(`/api/devices/${device.id}`, { approved: !device.approved });
        notify(`${device.name}: ${device.approved ? "access revoked" : "approved"}.`);
        await refresh();
      } catch (error) { notify(error.message, true); button.disabled = false; }
    });
    row.append(info, button);
    $("device-list").append(row);
  }
}

function renderJobs() {
  const expanded = new Set([...$("job-list").querySelectorAll("details[open]")].map((el) => el.dataset.id));
  $("job-list").replaceChildren();
  if (!jobs.length) $("job-list").append(node("div", "empty", "No metadata jobs yet. Queue a sync or upload a ROM to get started."));
  for (const job of jobs) {
    const card = node("article", "job-card");
    const top = node("div", "job-top");
    top.append(node("h3", "", `${job.platform.toUpperCase()} metadata`), node("span", `status ${job.status}`, job.status.toUpperCase()));
    const time = node("time", "", new Date(job.created).toLocaleString());
    time.dateTime = job.created;
    card.append(top, time);
    if (job.error) card.append(node("p", "job-error", job.error));
    const details = node("details");
    details.dataset.id = job.id;
    details.open = expanded.has(job.id);
    details.append(node("summary", "", "View job output"), node("pre", "", job.log || "Waiting for scraper output..."));
    card.append(details);
    $("job-list").append(card);
  }
}

$("login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (refreshInFlight) {
    $("login-error").textContent = "A previous request is finishing. Please try again.";
    return;
  }
  token = $("token").value.trim();
  $("login-error").textContent = "";
  const button = event.submitter;
  button.disabled = true;
  try {
    await refresh();
    if (!token) return;
    $("token").value = "";
    $("login").hidden = true;
    $("dashboard").hidden = false;
    $("notice").hidden = true;
  } catch (error) {
    lock();
    $("login-error").textContent = error.message;
  } finally { button.disabled = false; }
});
$("logout").addEventListener("click", lock);
document.querySelectorAll("[data-tab]").forEach((button) => button.addEventListener("click", () => switchTab(button.dataset.tab)));
document.querySelectorAll("[data-close]").forEach((button) => button.addEventListener("click", () => {
  if (button.dataset.close === "upload-dialog" && uploading) return;
  $(button.dataset.close).close();
}));
$("refresh").addEventListener("click", () => refresh().catch((error) => notify(error.message, true)));
for (const id of ["search", "platform-filter"]) $(id).addEventListener("input", () => { page = 0; renderGames(); });
$("previous").addEventListener("click", () => { page--; renderGames(); });
$("next").addEventListener("click", () => { page++; renderGames(); });
$("open-upload").addEventListener("click", () => $("upload-dialog").showModal());
for (const id of ["open-sync", "jobs-sync"]) $(id).addEventListener("click", () => $("sync-dialog").showModal());
$("sync-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  event.submitter.disabled = true;
  try {
    await post("/api/jobs", { platform: $("sync-platform").value });
    $("sync-dialog").close();
    switchTab("jobs");
    notify("Metadata sync queued.");
    await refresh();
  } catch (error) { $("sync-dialog").close(); notify(error.message, true); }
  finally { event.submitter.disabled = false; }
});

function uploadFile(file, platform, onProgress) {
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    request.open("PUT", `/api/roms?platform=${encodeURIComponent(platform)}&name=${encodeURIComponent(file.name)}`);
    request.setRequestHeader("Authorization", `Bearer ${token}`);
    request.setRequestHeader("Content-Type", "application/octet-stream");
    request.upload.onprogress = (event) => { if (event.lengthComputable) onProgress(100 * event.loaded / event.total); };
    request.onerror = () => reject(new Error("Upload connection lost. Refresh the library before retrying; the file may have been stored."));
    request.onload = () => {
      if (request.status === 401) lock();
      let body;
      try { body = JSON.parse(request.responseText); }
      catch { reject(new Error(`Unexpected server response (${request.status}). Check the library before retrying.`)); return; }
      if (request.status !== 201) reject(new Error(body.error || `Upload failed (${request.status})`));
      else resolve(body);
    };
    request.send(file);
  });
}

$("upload-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (uploading) return;
  const files = [...$("rom-files").files];
  const platform = $("upload-platform").value;
  if (files.some((file) => file.size > library.max_rom_bytes)) {
    $("upload-status").className = "error";
    $("upload-status").textContent = `One or more files exceed ${size(library.max_rom_bytes)}.`;
    return;
  }
  uploading = true;
  $("upload-submit").disabled = true;
  $("upload-progress").hidden = false;
  $("upload-status").className = "";
  const results = [];
  let failures = 0;
  for (const [index, file] of files.entries()) {
    $("upload-progress").value = 0;
    $("upload-status").textContent = `${index + 1} / ${files.length}: ${file.name}`;
    try {
      const result = await uploadFile(file, platform, (percent) => { $("upload-progress").value = percent; });
      results.push(`${file.name}: uploaded${result.warnings?.length ? `; ${result.warnings.join("; ")}` : "; metadata queued"}`);
      if (result.warnings?.length) failures++;
    } catch (error) {
      failures++;
      results.push(`${file.name}: ${error.message}`);
      if (!token) break;
    }
  }
  uploading = false;
  $("upload-submit").disabled = false;
  $("upload-progress").hidden = true;
  $("upload-status").className = failures ? "error" : "";
  $("upload-status").textContent = results.join("\n");
  $("rom-files").value = "";
  notify(failures ? "Uploads completed with errors or warnings. See upload results." : `${files.length} ROM(s) uploaded. Metadata sync queued.`, !!failures);
  await refresh().catch((error) => notify(error.message, true));
});
$("upload-dialog").addEventListener("cancel", (event) => { if (uploading) event.preventDefault(); });
setInterval(() => {
  if (token && !document.hidden) refresh().catch((error) => notify(`Refresh failed: ${error.message}`, true));
}, 5000);
