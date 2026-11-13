"use strict";

const $ = (id) => document.getElementById(id);
let signedIn = false;
let sessionVersion = 0;
let servicesReady = false;
let accessMode = "";
let restarting = false;
let reconnectRequired = false;
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

async function request(url, options = {}, lockOnUnauthorized = false) {
  const response = await fetch(url, {
    ...options,
    headers: { "X-Romsync-Request": "1", ...options.headers },
    credentials: "same-origin",
    cache: "no-store",
  });
  if (response.status === 401 && lockOnUnauthorized) {
    lock();
    throw new Error("Session expired. Sign in again.");
  }
  if (!response.ok) {
    const body = await response.json();
    throw new Error(body.error || `Request failed (${response.status})`);
  }
  return response;
}

function api(url, options = {}) { return request(url, options, true); }

function post(url, data) {
  return api(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) });
}

function releaseImages() {
  viewVersion++;
  imageURLs.splice(0).forEach((url) => URL.revokeObjectURL(url));
}

function lock() {
  signedIn = false;
  sessionVersion++;
  library = { games: [], platforms: [], max_rom_bytes: 0 };
  devices = [];
  jobs = [];
  releaseImages();
  $("dashboard").hidden = true;
  $("login").hidden = false;
  $("password").value = "";
  selectedGameID = "";
  $("game-list").replaceChildren();
  $("game-detail").replaceChildren();
  $("device-list").replaceChildren();
  $("job-list").replaceChildren();
  document.querySelectorAll("dialog[open]").forEach((dialog) => dialog.close());
}

function switchTab(tab) {
  for (const name of ["library", "devices", "jobs", "settings"]) $(`${name}-panel`).hidden = name !== tab;
  document.querySelectorAll("[data-tab]").forEach((button) => {
    button.classList.toggle("active", button.dataset.tab === tab);
    if (button.dataset.tab === tab) button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
  });
  if (tab === "jobs") $("job-list").querySelectorAll("details[open] pre").forEach(restoreJobOutput);
  $("breadcrumb").textContent = { library: "Game library", devices: "Devices", jobs: "Metadata jobs", settings: "Settings" }[tab];
}

function updateSelect(select, values, all = false) {
  const previous = select.value;
  select.replaceChildren();
  if (all) select.append(new Option("All platforms", ""));
  values.forEach((value) => select.append(new Option(value.toUpperCase(), value)));
  if (values.includes(previous) || (all && previous === "")) select.value = previous;
}

async function refresh() {
  if (!signedIn || refreshInFlight || restarting || reconnectRequired) return;
  refreshInFlight = true;
  const activeSession = sessionVersion;
  try {
    const status = await (await api("/api/status")).json();
    if (!status.authenticated) { lock(); return; }
    const wasReady = servicesReady;
    servicesReady = status.ready;
    if (!servicesReady) {
      notify(status.runtime_error || "Indexing the ROM library: scanning folders and hashing files. Games will appear automatically when this finishes; large libraries can take time.", !!status.runtime_error);
      if (!library.revision) {
        for (const id of ["game-count","platform-count","device-count","library-size"]) $(id).textContent = status.runtime_error ? "Unavailable" : "Indexing";
        $("game-list").replaceChildren(node("div","empty",status.runtime_error || "Your ROM library is being indexed. This is not an empty library; games will appear when the scan finishes."));
      }
      return;
    }
    const results = await Promise.all(["library", "devices", "jobs"].map(async (name) => (await api(`/api/${name}`)).json()));
    if (!signedIn || sessionVersion !== activeSession) return;
    const wasIndexing = !!library.indexing;
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
    if (library.indexing) {
      for (const id of ["game-count","platform-count","library-size"]) $(id).textContent = "Indexing";
      $("game-list").replaceChildren(node("div","empty",library.scan_error || "Your ROM library is being indexed. Systems will appear as their scans finish."));
      notify(library.scan_error || "Scanning the ROM library in the background.", !!library.scan_error);
    } else {
      if (changed || !$("game-list").childElementCount) renderGames(true);
      if (library.scan_error) notify(library.scan_error, true);
    }
    renderDevices();
    renderJobs();
    if ((!wasReady || wasIndexing) && !library.indexing && !library.scan_error) notify("ROM library ready.");
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
  selectGame(selectedGameID, preserveSelection);
}

function selectGame(id, preservePosition = false) {
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
  const previousScroll = preservePosition && detail.dataset.gameId === id ? detail.scrollTop : 0;
  detail.dataset.gameId = id;
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
  text.append(node("p","eyebrow","NOW EXPLORING"),node("h2","",game.title),node("p","detail-filename",game.id.split("/").slice(1).join("/")));
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
  detail.scrollTop = previousScroll;
  if (game.image) {
    api(`/api/media?id=${encodeURIComponent(game.image)}`)
        .then((response) => response.blob())
        .then((blob) => {
          if (version !== viewVersion || !signedIn) return;
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

function restoreJobOutput(output) {
  output.scrollTop = output.dataset.followBottom === "true" ? output.scrollHeight : Number(output.dataset.scrollTop);
}

function renderJobs() {
  const list = $("job-list");
  const existing = new Map([...list.querySelectorAll(".job-card")].map((card) => [card.dataset.id, card]));
  if (!jobs.length) {
    list.replaceChildren(node("div", "empty", "No metadata jobs yet. Queue a sync or upload a ROM to get started."));
    return;
  }
  list.querySelector(".empty")?.remove();
  for (const [index, job] of jobs.entries()) {
    let card = existing.get(job.id);
    if (!card) {
      card = node("article", "job-card");
      card.dataset.id = job.id;
      const top = node("div", "job-top");
      top.append(node("h3"), node("span", "status"));
      const details = node("details");
      details.dataset.id = job.id;
      const output = node("pre");
      output.dataset.followBottom = "true";
      output.dataset.scrollTop = "0";
      output.addEventListener("scroll", () => {
        if (!output.clientHeight) return;
        output.dataset.followBottom = String(output.scrollHeight - output.clientHeight - output.scrollTop <= 8);
        output.dataset.scrollTop = String(output.scrollTop);
      });
      details.addEventListener("toggle", () => {
        if (details.open) restoreJobOutput(output);
      });
      details.append(node("summary", "", "View job output"), output);
      card.append(top, node("time"), node("p", "job-error"), details);
    }
    existing.delete(job.id);
    if (list.children[index] !== card) list.insertBefore(card, list.children[index] || null);
    card.querySelector("h3").textContent = `${job.platform.toUpperCase()} metadata`;
    const status = card.querySelector(".status");
    status.className = `status ${job.status}`;
    status.textContent = job.status.toUpperCase();
    const time = card.querySelector("time");
    time.textContent = new Date(job.created).toLocaleString();
    time.dateTime = job.created;
    const error = card.querySelector(".job-error");
    error.hidden = !job.error;
    error.textContent = job.error || "";
    const output = card.querySelector("pre");
    const text = job.log || "Waiting for scraper output...";
    if (output.textContent !== text) {
      const follow = output.clientHeight ? output.scrollHeight - output.clientHeight - output.scrollTop <= 8 : output.dataset.followBottom === "true";
      const position = output.clientHeight ? output.scrollTop : Number(output.dataset.scrollTop);
      output.textContent = text;
      output.dataset.followBottom = String(follow);
      output.dataset.scrollTop = String(position);
      if (output.clientHeight) output.scrollTop = follow ? output.scrollHeight : position;
    }
  }
  existing.forEach((card) => card.remove());
}

$("login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (refreshInFlight) {
    $("login-error").textContent = "A previous request is finishing. Please try again.";
    return;
  }
  $("login-error").textContent = "";
  const button = event.submitter || $("login-form").querySelector("button[type=submit]");
  button.disabled = true;
  try {
    await request("/api/login", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({username:$("username").value,password:$("password").value})});
    $("password").value = "";
    await initialize();
  } catch (error) {
    lock();
    $("login-error").textContent = error.message;
  } finally { button.disabled = false; }
});
$("logout").addEventListener("click", async () => {
  try { await post("/api/logout", {}); lock(); }
  catch (error) { notify(error.message, true); }
});
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
    request.setRequestHeader("X-Romsync-Request", "1");
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
      if (!signedIn) break;
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
  if (signedIn && !document.hidden) refresh().catch((error) => notify(`Refresh failed: ${error.message}`, true));
}, 5000);

const settingsFields = [
  ["rom_path","ROM directory","text","Existing folder containing platform subdirectories. No files are moved or deleted."],
  ["state_path","Private state directory","text","Device approvals, saves, and backups. Must be outside the ROM directory; changing this does not migrate state."],
  ["web_listen","Dashboard listen address","text","Default 0.0.0.0:18777 accepts connections on all IPv4 interfaces. Use your firewall and reverse proxy to control network access."],
  ["listen","Device gRPC listen address","text","Default 0.0.0.0:7777 accepts connections on all IPv4 interfaces. TLS is optional when a reverse proxy terminates HTTPS."],
  ["tls_cert","TLS certificate path (optional)","text","Server-local PEM certificate path."],
  ["tls_key","TLS private key path (optional)","text","Server-local PEM key path, outside the ROM directory."],
  ["max_rom_bytes","Maximum ROM upload (bytes)","number","Default 2147483648 (2 GiB); maximum 1099511627776 (1 TiB)."],
  ["max_save_bytes","Maximum save upload (bytes)","number","Default 67108864 (64 MiB); maximum 1 TiB."],
  ["skyscraper","Skyscraper executable","text","Executable path or command; use Skyscraper for the default WSL launcher."],
  ["skyscraper_wsl","WSL distribution (Windows only, optional)","text","For example Ubuntu-24.04. Leave empty on Linux."],
  ["source","Metadata source","text","For example screenscraper. Scraper account credentials remain in its private configuration."],
  ["timeout_seconds","Metadata timeout (seconds)","number","Default 1800 (30 minutes); maximum 86400 (24 hours)."],
];

function renderSettings(container, prefix, settings) {
  $(container).replaceChildren();
  for (const [name,label,type,help] of settingsFields) {
    const wrap = node("div","settings-field");
    const title = node("label","",label);
    title.htmlFor = `${prefix}-${name}`;
    const input = node("input");
    input.id = title.htmlFor;
    input.type = type;
    input.value = settings[name] ?? "";
    input.required = !["tls_cert","tls_key","skyscraper_wsl"].includes(name);
    if (type === "number") {
      input.min = "1";
      input.max = name === "timeout_seconds" ? "86400" : "1099511627776";
      input.step = "1";
    }
    wrap.append(title);
    if (name === "rom_path") {
      const picker = node("div","path-picker");
      const browse = node("button","folder-browse");
      browse.type = "button";
      browse.title = "Browse server";
      browse.setAttribute("aria-label","Browse server");
      const icon = document.createElementNS("http://www.w3.org/2000/svg","svg");
      icon.setAttribute("viewBox","0 0 24 24");
      icon.setAttribute("fill","none");
      icon.setAttribute("stroke","currentColor");
      icon.setAttribute("stroke-width","1.8");
      icon.setAttribute("aria-hidden","true");
      const outline = document.createElementNS("http://www.w3.org/2000/svg","path");
      outline.setAttribute("d","M3 7V5a1 1 0 0 1 1-1h5l2 3h9a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V7Z");
      icon.append(outline);
      browse.append(icon);
      browse.addEventListener("click", () => {
        directoryTarget = input;
        $("directory-dialog").showModal();
        browseDirectory(input.value);
      });
      picker.append(input,browse);
      wrap.append(picker);
    } else {
      wrap.append(input);
    }
    wrap.append(node("small","",help));
    $(container).append(wrap);
  }
}

let directoryTarget = null;
let directoryCurrent = "";
let directoryParent = "";
let directoryVersion = 0;

async function browseDirectory(path) {
  const version = ++directoryVersion;
  $("directory-error").textContent = "";
  $("directory-list").replaceChildren();
  $("directory-list").setAttribute("aria-busy","true");
  $("directory-select").disabled = $("directory-up").disabled = true;
  $("directory-path").value = path;
  try {
    const listing = await (await request(`/api/directories?${new URLSearchParams({path})}`, {}, signedIn)).json();
    if (version !== directoryVersion) return;
    directoryCurrent = listing.path;
    directoryParent = listing.parent;
    $("directory-path").value = listing.path;
    for (const folder of listing.directories) {
      const button = node("button","",folder.name);
      button.type = "button";
      button.addEventListener("click", () => browseDirectory(folder.path));
      $("directory-list").append(button);
    }
    if (!listing.directories.length) $("directory-list").append(node("p","muted","No subdirectories."));
    $("directory-select").disabled = false;
    $("directory-up").disabled = !directoryParent;
  } catch (error) {
    if (version === directoryVersion) $("directory-error").textContent = error.message;
  } finally {
    if (version === directoryVersion) $("directory-list").setAttribute("aria-busy","false");
  }
}

$("directory-form").addEventListener("submit", (event) => {
  event.preventDefault();
  browseDirectory($("directory-path").value);
});
$("directory-path").addEventListener("input", () => { $("directory-select").disabled = true; });
$("directory-up").addEventListener("click", () => browseDirectory(directoryParent));
$("directory-select").addEventListener("click", () => {
  if (!directoryTarget) return;
  directoryTarget.value = directoryCurrent;
  directoryTarget.dispatchEvent(new Event("input", {bubbles:true}));
  $("directory-dialog").close();
});
$("directory-dialog").addEventListener("close", () => {
  directoryVersion++;
  directoryTarget = null;
});

function readSettings(prefix) {
  return Object.fromEntries(settingsFields.map(([name,,type]) => [name,type === "number" ? Number($(`${prefix}-${name}`).value) : $(`${prefix}-${name}`).value]));
}

function toggleCredentials(prefix) {
  const login = $(`${prefix}-mode`).value === "login";
  $(`${prefix}-credentials`).hidden = !login;
  $(`${prefix}-anonymous-warning`).hidden = login;
  for (const name of ["username","password","confirm"]) $(`${prefix}-${name}`).required = login;
}
for (const prefix of ["setup","auth"]) $(`${prefix}-mode`).addEventListener("change",() => toggleCredentials(prefix));

async function loadSettings() {
  const view = await (await api("/api/settings")).json();
  $("settings-error").textContent = $("auth-error").textContent = "";
  renderSettings("server-settings","settings",view.settings);
  $("config-path").textContent = `Configuration: ${view.config_path}`;
  $("runtime-status").textContent = view.runtime_error || (!servicesReady ? "Sync services are starting. The ROM library is being indexed." : view.restart_required ? "Saved settings are not active yet. Restart services to apply them." : "Services are using the saved settings.");
  $("auth-mode").value = view.auth.mode;
  $("auth-username").value = view.auth.username || "";
  $("auth-password").value = $("auth-confirm").value = $("auth-current").value = "";
  $("current-password-field").hidden = view.auth.mode !== "login";
  $("auth-current").required = view.auth.mode === "login";
  toggleCredentials("auth");
}

async function initialize() {
  const status = await (await request("/api/status")).json();
  $("setup").hidden = true;
  $("login").hidden = true;
  $("dashboard").hidden = true;
  if (!status.configured) {
    renderSettings("setup-settings","setup",status.defaults);
    $("setup").hidden = false;
    return;
  }
  if (!status.authenticated) { lock(); return; }
  signedIn = true;
  sessionVersion++;
  servicesReady = status.ready;
  accessMode = status.mode;
  $("dashboard").hidden = false;
  $("logout").hidden = accessMode === "anonymous";
  $("access-label").textContent = accessMode === "anonymous" ? "Anonymous administration" : "Signed in";
  await loadSettings();
  if (!servicesReady) switchTab("settings");
  await refresh();
}

$("setup-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = $("setup-submit");
  button.disabled = true;
  $("setup-error").textContent = "";
  try {
    if ($("setup-mode").value === "login" && $("setup-password").value !== $("setup-confirm").value) throw new Error("Passwords do not match.");
    const result = await (await request("/api/setup", {method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({
      settings:readSettings("setup"),mode:$("setup-mode").value,username:$("setup-username").value,password:$("setup-password").value,
    })})).json();
    $("setup-password").value = $("setup-confirm").value = "";
    $("setup-error").textContent = "Configuration saved. Starting services...";
    if (await reconnect(result, $("setup-error"))) await initialize();
  } catch (error) { $("setup-error").textContent = error.message; }
  finally { button.disabled = false; }
});

async function reconnect(result, message) {
    const next = new URL(location.href);
    if (next.protocol === "http:") {
      const address = new URL(`http://${result.web_listen}`);
      if (address.port !== "0") next.port = address.port;
    }
    if (next.origin !== location.origin) {
      reconnectRequired = true;
      message.textContent = `Settings saved. Services are restarting. Open ${next.origin} to reconnect. If that address is unavailable, the server will keep the old dashboard address; refresh here and check Settings.`;
      message.hidden = false;
      const link = node("a","",`Open ${next.origin}`);
      link.href = next.origin;
      message.append(" ",link);
      return false;
    }
    let status;
    for (let attempt=0;attempt<60;attempt++) {
      await new Promise(resolve=>setTimeout(resolve,500));
      try {
        status = await (await request("/api/status")).json();
      } catch (error) {
        if (attempt === 59) throw new Error(`Could not reconnect after restart: ${error.message}. Check the server console.`);
        continue;
      }
      if (status.generation >= result.generation && !status.restarting) break;
    }
    if (!status || status.generation < result.generation) throw new Error("Restart is taking longer than expected. Check the server console and refresh.");
    return true;
}

async function restartServices() {
  if (restarting || !confirm("Restart services? Active transfers will disconnect, metadata jobs will be cancelled, and job history will be cleared.")) return;
  restarting = true;
  try {
    const result = await (await post("/api/restart",{})).json();
    notify("Restarting services...");
    if (!await reconnect(result, $("notice"))) return;
    restarting = false;
    await initialize();
    const status = await (await api("/api/status")).json();
    notify(status.runtime_error || "Services restarted successfully.", !!status.runtime_error);
  } finally { restarting = false; }
}

$("settings-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  $("settings-error").textContent = "";
  const buttons = [...$("settings-form").querySelectorAll("button")];
  buttons.forEach(button=>button.disabled=true);
  try {
    await api("/api/settings",{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify(readSettings("settings"))});
    $("runtime-status").textContent = "Settings saved. Restart services to apply changes.";
    notify("Configuration saved.");
    if (event.submitter?.id === "save-restart") await restartServices();
  } catch (error) { $("settings-error").textContent = error.message; }
  finally { buttons.forEach(button=>button.disabled=false); }
});
$("restart-services").addEventListener("click",() => restartServices().catch(error=>notify(error.message,true)));
$("auth-form").addEventListener("submit",async (event) => {
  event.preventDefault();
  event.submitter.disabled=true;
  $("auth-error").textContent="";
  try {
    if ($("auth-mode").value==="login" && $("auth-password").value!==$("auth-confirm").value) throw new Error("Passwords do not match.");
    await request("/api/auth",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({
      mode:$("auth-mode").value,username:$("auth-username").value,password:$("auth-password").value,current_password:$("auth-current").value,
    })});
    $("auth-password").value = $("auth-confirm").value = $("auth-current").value = "";
    await initialize();
  } catch(error) { $("auth-error").textContent=error.message; }
  finally { event.submitter.disabled=false; }
});

initialize().catch(error=>{
  $("login").hidden=false;
  $("login-error").textContent=`Unable to load server status: ${error.message}. Refresh to retry.`;
});
