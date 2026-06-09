# Phase 0 — Wings Conformance Spec (Windows-native daemon)

**Pinned upstream:** Pterodactyl Panel `v1.12.4` + Wings `v1.12.3`
**Goal:** a Windows-native daemon (`feathers`) that speaks the Wings contract byte-for-byte, so the **stock Panel cannot tell it apart from a Linux node**. Zero Panel code changes. Windows support ships as importable eggs + this daemon binary.

This document is the conformance target for all later phases. Source references point at `vendor/wings` (v1.12.3) and `vendor/panel` (v1.12.4), which are git-ignored reference clones.

---

## 0. Strategy: fork Wings, swap one interface

Wings already isolates execution behind **`environment.ProcessEnvironment`** (`vendor/wings/environment/environment.go:27-115`) — a 26-method interface. The Docker backend lives in `environment/docker/`. **Everything above the interface is OS-portable and stays unchanged:** HTTP router, WebSocket console, SFTP server, remote (Panel) client, backups, transfers, config, activity log, cron.

The plan:
1. Fork Wings at `v1.12.3`.
2. Build-tag the Linux/Docker-only code out (`//go:build linux`).
3. Add `environment/winproc/` (`//go:build windows`) implementing `ProcessEnvironment` with **Windows Job Objects + `CreateProcessAsUser` + redirected pipes** instead of Docker.
4. Stub/skip the handful of Unix-only boot steps (pterodactyl user, passwd/machine-id mounts, timezone detection).
5. Ship as a Windows Service.

If the daemon implements the interface and the boot/remote handshake correctly, the Panel side is satisfied with no changes.

---

## 1. ⚠️ The single most important design discovery — startup command substitution

**On Linux, Wings does NOT substitute `{{SERVER_MEMORY}}`-style placeholders.** It passes the raw `Invocation` string to the container as the `STARTUP` env var (`server/server.go:151-174`), and the **container's entrypoint script** (baked into the Pterodactyl "yolk" Docker images) does the `{{...}}` → value substitution and then runs the command via a shell.

There is no container and no entrypoint script on Windows. **`feathers` must replicate that entrypoint logic itself:**

1. Take `Invocation` (e.g. `java -Xmx{{SERVER_MEMORY}}M -jar server.jar nogui`).
2. Substitute every `{{VAR}}` from the assembled environment map (`SERVER_MEMORY`, `SERVER_IP`, `SERVER_PORT`, `SERVER_MEMORY`, and all custom egg vars — see `GetEnvironmentVariables` at `server/server.go:151-174`).
3. **Tokenize into an argv array** (`exe`, `args...`) and launch via `CreateProcessAsUser` with the argv form — **never** hand the interpolated string to `cmd.exe /c`, or egg variables become a command-injection vector.

This substitution+tokenize step is the heart of the `winproc.Start()` implementation. It is new code with no Linux equivalent.

> Placeholder note: `parser/` only handles `{{config.*}}` substitution for **config files** (server.properties, etc.) — that logic is reusable as-is. The `{{SERVER_*}}` startup substitution is a separate concern done by the container entrypoint on Linux, and is what we must reimplement.

---

## 2. The `ProcessEnvironment` interface — Docker → Windows mapping

26 methods (`environment/environment.go:27-115`). Implement each in `winproc`:

| Method | Docker does | Windows replacement |
|---|---|---|
| `Type()` | returns `"docker"` | return `"winproc"` (or `"windows"`) |
| `Config()` | RWMutex read of `*Configuration` | identical — same struct, no Docker fields needed |
| `Events()` | returns `*events.Bus` | identical bus; drop the 3 `DockerImagePull*` events |
| `Exists()` | `ContainerInspect` | check Job Object handle exists |
| `IsRunning(ctx)` | `c.State.Running` | Job Object process list non-empty / `GetExitCodeProcess == STILL_ACTIVE` |
| `InSituUpdate()` | `ContainerUpdate` limits | `SetInformationJobObject` (mem/CPU); CPU affinity can't be un-set |
| `OnBeforeStart(ctx)` | destroy + recreate container w/ fresh config | recreate Job Object with current limits |
| `Start(ctx)` | attach → `ContainerStart` | **§1 substitution+argv** → `CreateProcessAsUser` → `AssignProcessToJobObject`; attach pipes BEFORE start |
| `Stop(ctx)` | stop command via stdin, or signal, else `ContainerStop` | stop command to stdin; fallback `TerminateJobObject` (no graceful timeout — must poll) |
| `WaitForStop(ctx,dur,term)` | `ContainerWait`; terminate on timeout | poll `IsRunning` until dur; `Terminate` if `term` |
| `Terminate(ctx,sig)` | `SignalContainer` | `TerminateJobObject`; set state offline |
| `Destroy()` | `ContainerRemove` (force, volumes) | `CloseHandle(job)` (kills all) — keep server files |
| `ExitState()` | `(ExitCode, OOMKilled)` | `GetExitCodeProcess`; infer OOM from job mem-limit stats |
| `Create()` | pull image + `ContainerCreate` | no pull; `CreateJobObject` + set limits (name = server UUID) |
| `Attach(ctx)` | hijack container stream; goroutine scans → `logCallback`; polls stats | `CreatePipe` stdout/stderr/stdin; goroutine scans → `logCallback`; polls Job Object stats |
| `SendCommand(s)` | write `s\n` to stream | write to stdin pipe (**watch `\n` vs `\r\n`** per game) |
| `Readlog(n)` | `ContainerLogs` tail | maintain in-memory ring buffer of console output; return last n |
| `State()`/`SetState()` | `system.AtomicString`, emits `StateChangeEvent` | identical |
| `Uptime(ctx)` | parse `State.StartedAt` | `GetProcessTimes` creation time |
| `SetLogCallback()` | mutex-guarded store | identical |

### Resource stats (`environment/stats.go`)
Reported struct fields (JSON): `memory_bytes`, `memory_limit_bytes`, `cpu_absolute`, `network` (`rx_bytes`/`tx_bytes`), `uptime`.
- **Memory/CPU:** `QueryInformationJobObject(JobObjectExtendedLimitInformation)` + `GetProcessMemoryInfo`; CPU% from `TotalUserTime + TotalKernelTime` deltas.
- **Network:** ⚠️ no clean per-process Job Object source on Windows. Needs ETW/WMI/PDH, or report `0` initially. **Known parity gap — document, don't block.**
- Emit `ResourceEvent` on the same poll cadence as Docker (`Attach` spawns `pollResources`).

### Server-layer integration (unchanged, just must be satisfied)
- `server/power.go:56-167` `HandlePowerAction` drives Start / `WaitForStop(10m,true)` / Terminate(SIGKILL).
- `server/crash.go:47-79` reads `ExitState()` on offline; non-zero exit or OOM → crash handler. So `ExitState` accuracy matters for crash/restart behavior.
- `server/listeners.go:84-142` subscribes to the events bus (resource + state-change). Skip `DockerImagePull*`.

---

## 3. Inbound HTTP API — every route the Panel calls (must implement all)

Auth: `Authorization: Bearer <config.Token.Token>`, constant-time compared (`router/middleware/middleware.go:166-187`). Signed-URL routes use scoped one-time JWTs (`?token=`). All of this is **above the interface and reused unchanged** — listed here as the conformance checklist.

**System / lifecycle**
- `GET /api/system` (`router_system.go:21`) — returns `architecture, cpu_count, kernel_version, os, version`. `os` will be `"windows"` — Panel just displays it; harmless.
- `GET /api/servers` (`:50`) · `POST /api/servers` create (`:61`) · `POST /api/update` config (`:122`) · `POST /api/deauthorize-user` (`:160`)
- `GET/DELETE /api/servers/:server` · `GET .../logs` · `POST .../sync` · `POST .../install` · `POST .../reinstall`

**Power & commands**
- `POST /api/servers/:server/power` — `{action: start|stop|restart|kill, wait_seconds}` → 202
- `POST /api/servers/:server/commands` — `{commands: [...]}` → 204 (502 if not running)

**File manager** (`router_server_files.go`) — contents, list-directory, write (raw body + `Content-Length`), rename (PUT, batch), copy, delete (batch), create-directory, compress (tar.gz), decompress, chmod (octal string), pull (remote download, max 3), upload (multipart, JWT).
- ⚠️ `chmod` + the post-write `Chown` (`sftp`/fs layer) are POSIX. On Windows translate to **NTFS ACLs** or no-op gracefully; `compress`/`decompress` use Go-native tar/gzip and port cleanly.

**Backups** (`router_server_backup.go`) — create (`{adapter: wings|s3, uuid, ignore}`), restore (`truncate_directory`, S3 `download_url`), delete; `GET /download/backup?token=`. Go-native (`pkg/sftp`-style) tar.gz + S3 — format-identical so Panel restore works. Ports cleanly.

**Transfers** (`router_server_transfer.go`, `router_transfer.go`) — outgoing/incoming server transfer + cancel. **Scope decision: same-platform only.** Win↔Win works; Win↔Linux explicitly out of scope.

**WebSocket** — `GET /api/servers/:server/ws` (see §4).

---

## 4. WebSocket console protocol (`router/websocket/`) — reused unchanged

First message must be `auth` with a scoped JWT (`websocket.go:304-358`). Then:

| Event | Dir | Payload |
|---|---|---|
| `auth` / `auth success` / `jwt error` / `token expiring` / `token expired` | ↔ | JWT / — |
| `set state` | panel→daemon | start/stop/restart/kill |
| `send command` | panel→daemon | command string → stdin |
| `send logs` / `send stats` | panel→daemon | — |
| `stats` | daemon→panel | `{memory_bytes, cpu_absolute, network_rx_bytes, network_tx_bytes}` |
| `status` | daemon→panel | running/starting/stopping/offline |
| `console output` / `install output` | daemon→panel | text line (install gated by perm) |
| `install started`/`completed`, `backup completed`, `transfer logs/status`, `daemon error/message`, `throttled` | daemon→panel | varies |

This all rides on the events bus + log sinks that `winproc.Attach` feeds. Implement the bus + console ring buffer correctly and the console "just works."

---

## 5. SFTP (`sftp/`) — reused, with Windows file-permission attention

- Pure-Go SSH/SFTP server; username `{user}.{uuid8}`, auth delegated to Panel (`sftp/server.go:212-250`, `remote` `POST /sftp/auth`).
- Filesystem is **jailed to the server root** by the server's `Filesystem()` (path traversal blocked) — reused as-is; just map root to `C:\pterodactyl\volumes\<uuid>`.
- Permission model `file.read / read-content / create / update / delete` (`sftp/handler.go:20-26`) — reused.
- ⚠️ `Setstat`/chmod (forces 0755 dirs / 0644 files) and post-op `Chown` assume POSIX. Translate to **NTFS ACLs** (per-server user gets the tree) or no-op. This is the same POSIX-permission seam as the file-manager `chmod`.

---

## 6. Boot sequence & Panel handshake (`cmd/root.go`, `remote/`) — reused, Unix steps skipped

Boot (`cmd/root.go:93-391`): load `config.yml` → init logging → **timezone (skip/replace)** → create dirs → **`EnsurePterodactylUser` (skip on Windows)** → **`ConfigurePasswd` (skip)** → create `remote.Client` (`Bearer {token_id}.{token}`) → `server.NewManager` → **`GetServers` (paginated) from Panel** → **`ConfigureDocker` (replace with winproc init)** → read persisted states → boot servers (workerpool) → `ResetServersState` → start SFTP + HTTP + cron.

Outbound Panel calls (`remote/servers.go`, all reused unchanged): `GET /servers` (boot), `POST /servers/reset`, `GET /servers/{uuid}`, `GET/POST /servers/{uuid}/install`, `POST .../archive`, `POST .../transfer/{success|failure}`, `POST /sftp/auth`, `GET/POST /backups/{id}`, `POST /backups/{id}/restore`, `POST /activity`.

Node setup (`cmd/configure.go`): `GET /api/application/nodes/{id}/configuration` with setup token → writes `config.yml`. Reused — admin adds the Windows node through the **normal Create Node flow** in the Panel.

### `config.yml` Windows deltas
- **`docker:` section** — entirely Docker-specific. Stub/ignore (or repurpose minimal fields). The egg's Docker image string is received but **ignored** by feathers (it can serve as a label/version hint only).
- `system.username`, `system.user.uid/gid`, `passwd`, `machine_id` — Linux-only; disable/ignore.
- Paths (`root_directory`, `data`, `archive_directory`, `backup_directory`, `tmp_directory`) → Windows paths, e.g. `C:\pterodactyl\...`.
- `api`, `remote`, `remote_query`, `sftp`, `crash_detection`, `backups`, `transfers`, `throttles`, `token`/`token_id` — all reused.

---

## 7. Isolation model (decision: per-server local user + Job Objects)

Per server UUID:
1. Restricted local Windows account `pt_<uuid8>` (or assigned from a pre-provisioned pool to avoid account churn — **leaning pool**).
2. NTFS ACLs: that account gets full control of **only** `volumes\<uuid>`; daemon service account retains management; deny elsewhere. (This is also where SFTP/file `chmod` semantics land — §5.)
3. `CreateProcessAsUser` under that token, assigned to a per-server Job Object: `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, memory cap, CPU rate control, `ActiveProcessLimit` (anti-forkbomb).
4. Daemon runs as a dedicated service account (never SYSTEM); game servers never run as Administrator.
5. argv-array launch only (§1) — no `cmd.exe` string interpolation.

---

## 8. Known parity gaps (document, don't block)

1. **Per-process network stats** — no clean Windows source; report 0 or wire ETW/PDH later.
2. **Per-server network bandwidth throttling** — Docker uses cgroups; no clean Windows equivalent.
3. **Cross-platform (Win↔Linux) transfers** — out of scope by decision; same-platform only.
4. **POSIX file permissions** (chmod/chown) — translated to NTFS ACLs or no-op'd.
5. **OOM detection** — inferred from Job Object memory-limit stats rather than a kernel OOM flag.

---

## 9. Phase roadmap (unchanged, now grounded)

- **Phase 1** — fork, build-tag Docker out, boot + Panel handshake green on Windows (node shows online, no servers).
- **Phase 2** — `winproc` power + console (Job Objects + §1 substitution) — SteamCMD target first.
- **Phase 3** — files + SFTP with per-server user + NTFS ACLs.
- **Phase 4** — resource limits + install-script execution (SteamCMD/FiveM install flow).
- **Phase 5** — backups, then Win↔Win transfers.
- **Phase 6** — egg library (SteamCMD/Minecraft/FiveM), hardening, code-signed Windows Service installer.
