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
- Automates metadata scraping with Skyscraper and splits assets into target profiles.
- **Save Backup Integrity:** Never overwrites save states in-place. Maintains historical snapshots (`/backups/[game].srm.[timestamp]`) for bulletproof disaster recovery.

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

### Prerequisites
- Go 1.21+
- Protocol Buffers compiler (`protoc`)
- Go plugins for gRPC (`protoc-gen-go` and `protoc-gen-go-grpc`)

### Compiling Protobuf Layouts
```bash
protoc --go_out=. --go-grpc_out=. proto/v1/romsync.proto
```

### Cross-Compiling Target Binaries

```bash
# Build Server & Batocera Clients
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/romsync-amd64 cmd/client/main.go

# Build Miyoo Mini Plus Client
GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o bin/romsync-miyoo cmd/miyoo/main.go
```