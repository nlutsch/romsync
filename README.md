# romsync

A highly autonomous, lightweight, local-first ROM and game save orchestration engine written in Go. This ecosystem replaces clunky network shares and battery-draining background daemons with a unified, session-aware sync loop running cleanly over public Nginx reverse proxies via gRPC streaming.

## System Architecture

```text
                     ┌───────────────────────────────┐
                     │         Central Server        │
                     │      romsync-core Server      │
                     └───────────────┬───────────────┘
                                     │
           ┌─────────────────────────┼─────────────────────────┐
           ▼                         ▼                         ▼
 ┌───────────────────┐     ┌───────────────────┐     ┌───────────────────┐
 │  Miyoo Mini Plus  │     │ Batocera clients  │     │ Apple TV clients  │
 │  clients (OnionOS)│     │  (Batocera Linux) │     │    (tvOS App)     │
 └───────────────────┘     └───────────────────┘     └───────────────────┘
```

## Key Architectural Principles

- **Local-First Performance:** ROMs and save states are stored locally on target devices for minimal latency and zero-friction offline play. 
- **gRPC over HTTP/2:** Connects clients to the server over standard HTTPS (port 443) using Nginx's native `grpc_pass`. No heavy VPN setups required.
- **Session-Aware Save Management:** Replaces generic file-watchers (like Syncthing) with transactional sync boundaries triggered exactly when game sessions open or close.
- **Server-Side Enrichment:** Metadata, scraping, and image aspect-ratio optimization are performed once on the server and pushed seamlessly down to edge nodes.

## Target Profiles & Execution Mechanics

### 1. Central Server
- Runs `romsync-core` server engine.
- Implements manual device approval for authorization.
- Monitors the master ROM layout via `fsnotify` and triggers server-pushed gRPC catalog streams.
- Loads a persisted catalog immediately, scans for changes in the background, and automatically queues Skyscraper enrichment after each changed system scan. Manual enrichment is also available.
- **Save Backup Integrity:** Never overwrites save states in-place. Every accepted upload is retained as a timestamped snapshot before the current-save pointer is updated. Conflicting uploads are accepted with an explicit conflict flag; the latest committed upload becomes current.

### 2. Batocera Clients
- Runs a persistent Go background daemon initiated by `/userdata/system/custom.sh`.
- Listens to the persistent `WatchCatalog` channel. 
- Automatically drops ROMs and matching media assets natively into the read-write filesystem, then executes `batocera-es-reload` to hot-refresh the user interface.

### 3. Miyoo Mini Plus Clients (OnionOS)
- Compiles via `GOOS=linux GOARCH=arm GOARM=7` into an ephemeral CLI utility.
- Operates transactionally to save battery. Injected directly into the wrapper scripts (`launch.sh`) of emulator cores.
- Executes a transactional state query (`CheckSaveState`) at game launch (PULL) and game exit (PUSH), then completely terminates the network interface loop.

### 4. Apple TV Clients (tvOS)
- Embedded inside a custom compiled and signed sideloaded emulator (Provenance/RetroArch).
- Saves assets securely inside the application sandbox's `Documents` container with `isExcludedFromBackup` explicitly flagged to prevent aggressive tvOS cache purging.

---

## Getting Started

### Repository Layout

One Go module covers the server and future Go clients, sharing the same protobuf contract and generated bindings.

```text
server/
  cmd/romsync-core/   Server executable
  internal/server/   Server implementation and tests
    web/             Embedded admin dashboard (HTML/CSS/JavaScript)
  deploy/            Nginx and systemd examples
  scripts/           Linux and Windows Skyscraper setup
proto/v1/            Shared protobuf API
gen/romsync/v1/      Shared generated Go bindings
```

Future device applications will live under `clients/`, alongside `server/`. Server-only implementation details stay in `server/internal/` so client packages cannot import them. Run the commands below from the repository root.

### Prerequisites
- Go 1.25+ (older Go installations with automatic toolchain downloads enabled can fetch it).
- Skyscraper, configured separately with scraper credentials, for optional enrichment.
- Protocol Buffers compiler (`protoc`) and Go plugins only when changing the API. Generated bindings are checked in.

### Build and Run the Server

The server, shared API, tests, and deployment examples live in this repository. Device applications are not implemented yet.

```bash
go build -ldflags="-s -w" -o bin/romsync-core ./server/cmd/romsync-core
mkdir -p roms/snes
# Place legally obtained ROMs in roms/<platform>/ before starting.
./bin/romsync-core
```

Open **http://127.0.0.1:18777** and complete first-run setup: choose your ROM directory, state directory, and dashboard login or anonymous access. No ROM path or environment token is needed to launch the executable. Device sync starts after setup.

The application has two listeners: web HTTP on `0.0.0.0:18777` and device gRPC on `0.0.0.0:7777`. Both accept connections on all IPv4 interfaces without IP or hostname allowlists. The dashboard starts immediately; device sync starts after setup. Optional gRPC TLS certificate/key settings support direct encrypted connections; Nginx can instead terminate TLS. Keep private state, configuration, and TLS keys outside the ROM tree.

#### Network Access

Browse to `http://<server-hostname-or-IP>:18777` from the LAN. No hostname registration or listener flag is needed. To change the ports or restrict the bind addresses, use Settings or CLI overrides:

```bash
go run ./server/cmd/romsync-core -web-listen 0.0.0.0:18778 -listen 0.0.0.0:7778
```

Use your firewall, LAN configuration, and reverse proxy to control network access. Plain HTTP/gRPC do not encrypt credentials, cookies, device tokens, or transfers. Use HTTPS for untrusted networks and complete first-run setup before exposing the service publicly. Login and device approval remain enforced; anonymous administration grants full access to anyone who can reach the dashboard.

Previously saved bind addresses remain unchanged; update them in Settings to use the new all-interface defaults. Legacy `dashboard_url` and `dashboard_lan_url` fields are accepted for configuration compatibility but are ignored and no longer shown in Settings.

Configuration is saved atomically to **`romsync.json` in the current working directory** by default, with restrictive file permissions where supported. The default private state directory is `data` beside that configuration file. Use `-config <path>` to select a different file:

```bash
go run ./server/cmd/romsync-core
```

For an alternate configuration file:

```powershell
.\bin\romsync-core.exe -config C:\romsync\romsync.json
```

Explicit CLI flags override saved settings for that invocation. Web saves persist those effective values, and service restarts apply the saved file without reapplying original flags. Relative paths in the configuration/UI resolve against the configuration file's directory; explicit CLI filesystem paths resolve against the invocation directory. WSL executable paths remain Linux paths. ROM folders must already exist; the server creates private state directories, but does not move your files.

If existing ROM storage becomes unavailable, the dashboard remains available to correct settings. Invalid JSON, unsupported config versions, or an invalid/unavailable initial dashboard address produce explicit startup errors; fix the file or use the appropriate CLI override. If a changed dashboard port cannot be bound during a web-triggered restart, the old address remains available and Settings reports the failure.

gRPC reflection (v1 and v1alpha) is enabled on the same listener. In Postman, create a gRPC request to `127.0.0.1:7777`, disable TLS for this local connection, and load the API using server reflection instead of importing the proto. Reflection exposes service schemas without authentication; sync RPCs still require an approved device token. The Nginx example forwards reflection as well; remove its reflection locations if public schema discovery is unwanted.

ROM layout:

```text
roms/
  snes/
    game.sfc
  psx/
    game.chd
```

Non-hidden regular files beneath each platform are cataloged. Symlinks and hidden trees are excluded. Do not put credentials, saves, backups, unrelated files, or state under the ROM root. Multi-file games are transferred as individual catalog files; grouping and client launch integration are future work.

System folder names are case-insensitive and accept common aliases: `snes`, `sfc`, `SNES`, `SFC`, and `Super Nintendo` all publish as `snes`; `nes`, `fc`, and `Famicom` publish as `nes`. Other examples include `Game Boy`/`gb`, `Game Boy Advance`/`gba`, `PlayStation`/`psx`, and `Genesis`/`megadrive`. Standard lowercase platform identifiers remain supported. The actual server folders are never renamed. Catalog IDs, client paths, generated-data folders, and Skyscraper platform arguments use canonical identifiers. Keep **one folder per canonical system**: having both `SNES` and `SFC` produces an explicit duplicate-folder error rather than merging or overwriting files.

### Persistent Catalog

The index is saved atomically as **`catalog.json` inside the configured private state directory** (by default `data/catalog.json` beside the configuration). It contains file paths, sizes, modification times, SHA-256 checksums, catalog entries, and successful automatic metadata revisions; it does not copy ROMs or store saves.

Startup reads that snapshot before scanning, so an existing library is available immediately. A background scan reconciles additions, changes, and deletions, with filesystem notifications and a 30-second periodic reconciliation. Unchanged files reuse checksums based on size and modification time instead of rereading the whole library. Downloads still verify the transferred bytes against the advertised checksum. On the first scan, completed systems are published and persisted individually, without waiting for every system to finish.

The cache is bound to its ROM root. Changing roots triggers a fresh index; invalid caches are reported in the logs and rebuilt. It is safe to rebuild the cache without deleting device records or saves. Indexing/storage failures are reported in server logs and the dashboard rather than replacing a usable cached library with an empty one.

### Admin Dashboard

The dashboard is embedded in the Go binary and starts by default; no Node.js runtime or frontend build is required. Device sync and the dashboard can use the cached catalog while background scanning runs. Without a cache, the library displays **Indexing** until the first system is available. Console logs show a plain timestamp and message followed by relevant details. Terminal messages are blue for info, yellow for warnings, and red for errors; redirected output or `NO_COLOR` disables ANSI colors. The dashboard is announced before device initialization. The server runs in the foreground; use Ctrl+C to stop it. Scans check cancellation between file reads.

Launch the compiled executable, then visit its logged dashboard URL:

```powershell
.\bin\romsync-core.exe
```

First-run setup works through the LAN or an HTTPS reverse proxy. Any hostname may access the dashboard; browser mutations still require the application request header and matching origin. No CORS access is enabled. Restrict network access until setup is complete.

Choose either:

- **Login (recommended):** one administrator username/password, with a minimum 12-character password. The file contains a random salt and PBKDF2-HMAC-SHA256 hash (600,000 iterations), never the plaintext password. Login creates a random HttpOnly, SameSite=Strict cookie valid for 12 hours; HTTPS sessions also use Secure cookies. Refreshing the page retains the session. Sign-out invalidates that session. Password/access changes and process restarts invalidate all sessions; web-triggered service restarts preserve them. Login attempts are rate-limited.
- **Anonymous administration:** anyone who can reach the dashboard has full access, including uploads, device approval, configuration, and restarts. Do not expose this mode to untrusted networks. It does **not** disable device approval or device tokens.

Settings lets you change access mode or replace the login later. Changing an existing login or disabling it requires the current password. The old `ROMSYNC_ADMIN_TOKEN` environment variable and dashboard Bearer-token authentication are no longer used.

#### Forgotten Password

With CLI access, stop the running server (or stop its systemd service), run:

```powershell
.\bin\romsync-core.exe reset-password
# If using a custom configuration:
.\bin\romsync-core.exe reset-password -config C:\romsync\romsync.json
# To replace anonymous access with a login:
.\bin\romsync-core.exe reset-password -username admin
```

Enter and confirm a new password at the prompts; interactive terminal input is hidden. No old password is required, and no password is supplied in command-line arguments. The saved username is reused unless `-username` is provided. Start the server again after resetting. Do not delete the configuration or state directory to recover access; device approvals and saves are unaffected. Restrict filesystem access to the config file, since anyone who can modify it controls dashboard access.

The dashboard provides:

- **Game library:** searchable, platform-filtered, paginated game list with an animated metadata detail pane. Scraped game names are preferred, with filename titles as fallback; displayed titles omit dump flags and common region/version tags. Original filenames remain visible in smaller text and are never renamed. Click a game or focus the list and use Up/Down (Home/End jump to the beginning/end). Composite artwork, descriptions, game facts, available media types, sizes, and checksums appear when available. The layout stacks on smaller screens and respects reduced-motion preferences.
- **Devices:** approve or revoke registered clients without restarting; device credentials are never exposed in dashboard responses.
- **Add ROMs:** choose an existing platform or enter a platform identifier/common alias. Uploads use the existing server folder for that system, regardless of its casing or alias; new systems create a canonical folder. Uploads stream to hidden staging files and are atomically published without replacing existing filenames. Individual files are limited to 2 GiB by default (`-max-rom-bytes`); archives are not extracted. Common ROM formats are accepted (`nes`, `sfc`, `smc`, `gb`, `gbc`, `gba`, `n64`, `z64`, `v64`, `zip`, `7z`, `chd`, `cue`, `bin`, `iso`, `rom`, `md`, `gen`, `sms`, `gg`, `pce`, `cso`, `pbp`, `gdi`). Filesystems must support hard links for atomic, no-replace upload publication.
- **Metadata jobs:** automatically queued when a system's ROM scan finishes for the first time, its ROM inventory changes (including uploads), or the configured source changes. Unchanged scans and generated-only changes do not retrigger enrichment. Successful completion is persisted across restarts. One job runs at a time; queued jobs for the same platform are combined, and ROM changes during an active job queue another pass. Manual jobs accept the same aliases. The UI polls every five seconds and shows status, errors, and the most recent 32 KiB of scraper output. Expanded output follows the bottom until you scroll away; scrolling back down resumes following. Refreshes preserve the output's open state and reading position.

The selected game's dashboard metadata updates during enrichment without switching your selection: text appears after the first metadata export, then artwork appears as each game's Batocera assets are generated. Skyscraper's initial gathering/export remains batch-based; this does not launch additional per-game scraper processes or force per-game catalog scans. Dashboard previews are transient and expose only completed generated artwork, never raw scraper paths. Final manifests and device synchronization keep their existing completed-export/checksum contracts.
- **Settings:** configure ROM/state directories, gRPC and dashboard addresses, TLS paths, upload limits, Skyscraper executable/WSL distribution/source/timeout, and dashboard access. **Save settings** persists without changing running sync services. **Save and restart services** applies changes without exiting the executable; **Restart services** applies already-saved settings. Restart confirmation warns about disconnecting transfers, cancelling metadata jobs, and clearing job history. Existing ROMs, device records, and saves are not migrated when paths change. A different dashboard port requires opening its new URL and updating proxy configuration if applicable. Changing dashboard access applies immediately without a service restart.

Install/configure Skyscraper for the operating-system user running the server. The dashboard uses the same `-skyscraper`, `-source`, and `-timeout` options as CLI enrichment. If Skyscraper is missing or fails, ROMs remain available and the metadata job visibly fails. Failed automatic jobs are not retried on every unchanged scan; retry manually after fixing configuration, or restart services for another attempt. Avoid running separate CLI enrichment against the same platform while a dashboard job is active.

The metadata queue holds at most 32 pending jobs and 64 recent history entries, in memory only. Shutdown cancels active/queued jobs; restarting clears history and automatically queues unfinished system revisions after reconciliation. If queuing or catalog refresh fails after a ROM was stored, the upload response explicitly reports a warning instead of suggesting a retry that could duplicate the file.

The ROM directory field includes a folder-icon **Browse server** button in first-run setup and Settings. It lists directories on the host running romsync, not on the browser's computer. Navigate into folders, go up, or enter an absolute server path (including a drive path on Windows), then choose **Use this directory**. Blank and relative paths start beside the configuration file. Selection only fills the field; save settings separately to apply it. The browser does not create, delete, or move folders and lists directories rather than files or symlink entries. The server process's filesystem permissions still apply.

### Admin HTTP API

Except for status, first-run setup, and login, `/api/` routes require the dashboard session cookie in login mode; anonymous mode explicitly permits full access. No CORS access is enabled. Every mutating request requires **`X-Romsync-Request: 1`**, including login/setup and anonymous requests. Cross-site requests and mismatched browser origins are rejected. The static login page exposes no library data. This API is for administrators, not device clients:

| Method | Route | Purpose |
|--------|-------|---------|
| GET | `/api/status` | Setup/login state and service readiness; setup defaults available before setup |
| GET | `/api/directories?path=...` | Host directory listing (`path`, `parent`, `directories` with `name`/`path`); requires `X-Romsync-Request: 1`; available before setup, then follows dashboard authentication |
| POST | `/api/setup` | One-time local setup with `settings`, `mode`, and optional `username`/`password` |
| POST | `/api/login` | `username`/`password`; returns a dashboard session cookie |
| POST | `/api/logout` | Invalidate the current session |
| GET / PUT | `/api/settings` | Read configuration/status or save a complete settings object (no password hashes exposed) |
| POST | `/api/auth` | Change `mode`, `username`, `password`; include `current_password` when login is enabled |
| POST | `/api/restart` | Restart services using saved settings; returns HTTP 202 and target generation/address |
| GET | `/api/library` | Games with metadata and asset IDs, canonical platforms, upload limit, initial `indexing` state, and optional `scan_error` |
| GET | `/api/devices` | Registered devices without token hashes |
| POST | `/api/devices/{id}` | Approve/revoke with `{"approved":true}` |
| GET | `/api/jobs` | Recent metadata jobs and output |
| POST | `/api/jobs` | Queue with `{"platform":"snes"}` |
| PUT | `/api/roms?platform=snes&name=game.sfc` | Raw ROM body, duplicate filenames return HTTP 409 |
| GET | `/api/media?id=...` | Cataloged PNG artwork |

### Device Approval

Clients call `RegisterDevice` with a name and a profile (`BATOCERA`, `MIYOO`, or `TVOS`) and securely retain the returned token. Enrollment does not grant access. Administrators approve devices locally:

```bash
./bin/romsync-core devices -state ./data
./bin/romsync-core approve -state ./data -device <device-id>
./bin/romsync-core revoke -state ./data -device <device-id>
```

All sync RPCs require `authorization: Bearer <token>` metadata. Tokens are stored only as SHA-256 hashes on the server. Approval changes apply without restarting; idle catalog streams recheck approval every five seconds. Lost tokens require a new registration. Enrollment is capped at 1,024 registrations; remove unused device JSON files locally to reclaim slots. Rate-limit public enrollment at the reverse proxy if exposed to untrusted networks.

### Sync Contract

The API is defined in [proto/v1/romsync.proto](proto/v1/romsync.proto).

- `WatchCatalog` sends an initial full manifest and subsequent changed manifests (up to 32 MiB; clients must configure their receive limit accordingly). Recursive filesystem events are debounced; a 30-second reconciliation scan catches missed events. Scans hash files using bounded buffers. Large libraries may need future incremental indexing.
- `DownloadFile` requires a catalog ID and its advertised SHA-256. Data chunks are at most 64 KiB. Clients must verify the final RPC status and checksum before atomically installing any download; a changing source aborts the transfer.
- Optional `assets` on `RegisterDevice` saves a device's media preferences; approved clients can change them with `SetAssetPreferences`. `WatchCatalog` and `DownloadFile` accept per-request overrides. Omitted selections use saved preferences, or **composite artwork only** for devices without preferences. An explicit `{"assets": {}}` selects no media, while retaining ROMs and textual metadata. Types must be unique and cannot include `ASSET_UNSPECIFIED`.
- Available types are `COMPOSITE`, `BOXART`, `SCREENSHOT`, `WHEEL` (logo), `MARQUEE`, `FANART`, `VIDEO`, and `MANUAL` (guide/PDF). Only media actually available from the scraper is delivered. Profiles still control image dimensions and platform compatibility. The web dashboard always uses composites independently of device preferences.
- For example, send `{"assets":{"types":["BOXART","SCREENSHOT","VIDEO","MANUAL"]}}` to `SetAssetPreferences` in Postman. Subsequent catalog/download requests can omit `assets`. If overriding a watch request, use the **same override on downloads** so filtered game-list checksums agree. Changing saved preferences notifies existing catalog streams.
- Media catalog entries include `asset_type`, associated ROM `game_id`, and `target_path` relative to the client's platform directory. Server-side generated files live in `romsync-data`, but logical IDs remain `<platform>/...`; clients must not copy that server storage prefix. Downloaded game lists reference client-relative media paths and include only requested, available media; their advertised checksums/sizes describe those exact filtered bytes. Screenshots use `<image>` when composites are not requested or available, otherwise `<screenshot>` alongside the composite `<image>`. Clients can use typed media entries to adapt placement to their frontend.
- `CheckSaveState` takes a catalog ROM ID, local SHA-256 (empty for no local save), and the last synchronized revision (empty on first sync). Revisions are content hashes. Results are `UP_TO_DATE`, `SERVER_IS_NEWER`, `CLIENT_IS_NEWER`, or `CONFLICT`.
- `UploadSave` starts with a header-only `FileChunk` containing `game_id`, `sha256`, and optional `base_revision`. Subsequent chunks contain only `data`, at most 64 KiB each. The default total limit is 64 MiB (`-max-save-bytes`). The server validates the checksum before committing. Uploads with a stale base are accepted, backed up, and marked `replaced_conflict` when both sides changed. Concurrent uploads are committed in completion order, not device-clock order.
- `DownloadSave` requires the revision returned by `CheckSaveState`. Clients must stage and verify data before replacing local saves.

Saves are shared across approved devices by exact ROM catalog ID. This initial API supports one save blob per ROM, not multiple save slots or accounts. Moving/renaming a ROM changes its save key. The server does not delete backup history automatically.

### Metadata and Image Enrichment

#### Set Up Skyscraper

The setup scripts install a pinned official Skyscraper **3.21.1** AppImage for x86_64 or aarch64, check its SHA-256 against the upstream release digest, and extract the bundled runtime so FUSE is not required. They do not download ROMs, request credentials, upgrade the operating system, or replace existing configuration. The binary requires a glibc-based Linux distribution with glibc 2.35+ (for example Ubuntu 22.04/24.04 or current Arch Linux). Alpine/musl and older distributions are not supported by this binary installer.

**Windows (native romsync, Skyscraper in WSL):**

```powershell
# Only if a suitable distribution is not already installed:
wsl --install -d Ubuntu-24.04
# Launch it once and complete its Linux username/password setup.
wsl -d Ubuntu-24.04

# From the repository root in PowerShell:
.\server\scripts\setup-skyscraper.ps1
```

The script defaults to `Ubuntu-24.04`; select another compatible distribution with `-Distribution <name>`. It requires WSL already installed and initialized. The installer checks the CLI and reports missing libraries; on a minimal Ubuntu installation, run `sudo apt-get update && sudo apt-get install libopengl0` inside that distribution, then rerun the script.

Edit the private Linux config without copying credentials into this repository:

```powershell
wsl -d Ubuntu-24.04 --cd ~ -- nano .local/share/romsync/skyscraper/config.ini
```

Under `[screenscraper]`, uncomment `userCreds` and replace its placeholder with your [ScreenScraper](https://www.screenscraper.fr/) username and password. Keep the value double-quoted and escape literal backslashes/double quotes as `\\`/`\"`. The config is mode `0600`; the server never receives these credentials as command-line arguments. Do not share that file or scraper logs that may contain account details.

In dashboard Settings, choose `Skyscraper` as the executable and `Ubuntu-24.04` as the WSL distribution, then save and restart services. Alternatively, use CLI overrides with a stable configuration file:

```powershell
go run .\server\cmd\romsync-core -roms C:\roms -state .\data -skyscraper-wsl Ubuntu-24.04

# Or perform one enrichment directly:
go run .\server\cmd\romsync-core enrich -roms C:\roms -platform snes -skyscraper-wsl Ubuntu-24.04
```

romsync translates Windows paths using `wslpath`, runs Skyscraper's gathering/export stages in the selected distribution, and converts exported Linux paths back into client-relative asset paths. `C:\roms` stays on Windows. The default executable resolves to the WSL user's `~/.local/bin/Skyscraper`; if overriding `-skyscraper` in WSL mode, supply the absolute **Linux** launcher path, not a Windows path. Run setup and romsync under the same Windows account and WSL default user. WSL is not used by native Linux servers.

**Linux:**

```bash
bash server/scripts/setup-skyscraper.sh
# Edit the private configuration and set [screenscraper] userCreds:
nano "$HOME/.local/share/romsync/skyscraper/config.ini"

./bin/romsync-core serve -roms ./roms -state ./data \
  -skyscraper "$HOME/.local/bin/Skyscraper"
```

Prerequisites are Bash, curl, CA certificates, GNU core utilities, and the OpenGL runtime (`libopengl0` on Debian/Ubuntu, `libglvnd` on Arch). Install missing prerequisites with your distribution's package manager. No root privileges are needed for the script itself. On minimal/headless systems, additional runtime libraries may be required; the final CLI verification surfaces loader errors explicitly.

Defaults are `~/.local/share/romsync/skyscraper` for the release, private config, and resource cache, and `~/.local/bin/Skyscraper` for the launcher. Use `--prefix /absolute/path --bin-dir /absolute/path` to customize them. The script refuses to overwrite an unrelated launcher. Re-running verifies the existing installation and preserves credentials. Keep this directory outside the ROM root and outside the repository.

For the systemd example, run setup as the service user with its configured home (for example `sudo -u romsync env HOME=/var/lib/romsync bash server/scripts/setup-skyscraper.sh`) after creating that user's home/state directory. Set `-skyscraper /var/lib/romsync/.local/bin/Skyscraper` in `ExecStart`, and configure ScreenScraper under `/var/lib/romsync/.local/share/romsync/skyscraper/config.ini`.

Upstream documentation: [installation](https://github.com/Gemba/skyscraper#-how-to-install-skyscraper) and [configuration](https://gemba.github.io/skyscraper/CONFIGINI/).

#### Run Enrichment

Run enrichment from the dashboard, or as a separate command with write access to the ROM tree:

```bash
./bin/romsync-core enrich -roms ./roms -platform snes -source screenscraper
```

This gathers metadata into Skyscraper's external cache, exports independent media and composite artwork, then generates profile assets under **`<ROM root>/romsync-data/<platform>/`**. ROM platform directories are not modified. A separate logo export avoids Skyscraper's EmulationStation convention of substituting wheel logos for marquees.

For example:

```text
C:\roms\
  snes\                         ROM files only
  nes\                          ROM files only
  romsync-data\                 Reserved generated-data folder, not a platform
    snes\
      gamelist.xml
      miyoogamelist.xml
      tvosgamelist.xml
      media\
        batocera\
          composite\            Combined artwork for the dashboard
          boxart\               Individual images for client frontends
          screenshot\
          wheel\
          marquee\
          fanart\
          video\
          manual\
        miyoo\                  Same asset-type layout, smaller images
        tvos\                   Same asset-type layout, larger images
      .enrichment\              Raw scraper export and artwork rules
      .logos\                   Separate wheel/marquee export
      .composite\               Composite scraper export
```

Only available media folders/files are created. Filenames are derived from ROM-relative paths to avoid collisions. The three generated game lists and PNG image bounds are:

| Profile | Game list | Maximum PNG dimensions |
|---------|-----------|------------------------|
| Batocera | `gamelist.xml` | 1280 x 960 |
| Miyoo Mini Plus | `miyoogamelist.xml` | 640 x 480 |
| tvOS | `tvosgamelist.xml` | 1920 x 1080 |

Each image is resized independently, preserving aspect ratio without upscaling. PNG/JPEG inputs are supported, with a 40-megapixel input limit. Textual metadata is retained. Videos and manuals are copied unchanged using bounded buffers rather than loaded into server memory. Skyscraper's private configuration/resource cache remains outside the ROM tree at the installer location described above. The running server publishes changed files automatically. Schedule this command with your preferred system timer if desired; scraping failures exit nonzero. Use `-skyscraper` to select the binary and `-timeout` to override the 30-minute limit.

#### Existing Metadata Files

There is **no automatic migration or deletion**. Finish or stop jobs running with the old server, restart with the updated application, and sync each platform again to populate `romsync-data`. Existing platform-local game lists and `media`/`.enrichment` folders are ignored by the new catalog and left untouched. After verifying the new exports, you may manually remove only the old generated `.enrichment`, `media`, `gamelist.xml`, `miyoogamelist.xml`, and `tvosgamelist.xml` inside each platform folder. Preserve any manually maintained files and do not remove ROMs or the new `romsync-data` folder.

tvOS catalogs allow only `nes`, `snes`, `gb`, `gbc`, `gba`, `megadrive`, `genesis`, `mastersystem`, `gamegear`, `pcengine`, `psx`, `n64`, and `dreamcast`. Each profile receives only its own media and game list. Actual emulator support remains the client's responsibility.

### Persistence and Deployment

The private state directory contains `devices/` (hashed tokens and approval), `saves/` (atomic current-save pointers), and `backups/` (immutable, timestamped save uploads). Files are synced before publication; directory changes are also synced on Linux (Windows does not support this through Go's directory handles). Incomplete or invalid uploads do not change the current pointer. A failure during final publication can have an uncertain outcome: reconcile using `CheckSaveState` before retrying. A crash between snapshot creation and pointer replacement can leave an unreferenced snapshot, which is deliberately retained. Back up the entire state directory; do not prune files referenced by current-save pointers. Run only one server process per state directory.

See [server/deploy/nginx.conf](server/deploy/nginx.conf) and [server/deploy/romsync.service](server/deploy/romsync.service). Create a dedicated `romsync` service account, grant it read/write access to `/srv/romsync/roms` for uploads and enrichment, install the binary at `/usr/local/bin/romsync-core`, and adjust the example hostnames/certificates. The systemd unit creates `/var/lib/romsync`, stores configuration at `/var/lib/romsync/romsync.json`, and sets that directory as the service's `HOME` so Skyscraper can keep its configuration/cache there despite `ProtectHome=true`. Complete first-run setup from your trusted network before public exposure, choosing `/srv/romsync/roms` and your intended private state path. No admin-token environment file is required. Configure Skyscraper using that home directory as the service user, not root. If changing paths in Settings, also update the service's writable-path restrictions.

The Nginx example uses separate HTTPS virtual hosts: `romsync.example.com` proxies web requests to `http://127.0.0.1:18777`, and `grpc.romsync.example.com` proxies gRPC requests to `grpc://127.0.0.1:7777` with HTTP/2. Replace both hostnames and certificate paths with your own; no application hostname setting is required. For Nginx on another machine, replace loopback upstreams with the application server's reachable address. The dashboard location allows 2 GiB uploads with request buffering disabled; keep that limit aligned with the configured maximum ROM upload size. Only Nginx's HTTPS port should be publicly reachable. For device-only remote access, omit the dashboard virtual host and restrict the web listener with your firewall.

No client retry loop is needed inside the server: clients should reconnect streams with bounded exponential backoff and jitter after network failures. Save uploads require an explicit success response before the client updates its synchronized revision; an uncertain result should be reconciled with `CheckSaveState`.

### Development

```bash
go test ./...
go vet ./...

# Optional offline Windows-to-WSL integration test (PowerShell):
# $env:ROMSYNC_TEST_WSL = "Ubuntu-24.04"
# go test -count=1 -run TestWSLEnrichment -v ./server/internal/server

# Regenerate the shared API after editing the proto.
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
protoc --go_out=. --go_opt=module=github.com/nlutsch/romsync \
  --go-grpc_out=. --go-grpc_opt=module=github.com/nlutsch/romsync \
  proto/v1/romsync.proto

# Linux server binary
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/romsync-core ./server/cmd/romsync-core
```