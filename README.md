# Feathers — Windows-native Pterodactyl Wings

A Windows port of Pterodactyl **Wings** (`v1.12.3`) that runs game servers as
**native Windows processes** instead of Docker containers, while speaking the
Wings HTTP/WebSocket/SFTP contract byte-for-byte. The goal: the **stock
Pterodactyl Panel (and PteroBill/WHMCS) cannot tell a Windows node apart from a
Linux one** — zero Panel changes, Windows support shipped as importable eggs +
this daemon binary.

The daemon source lives in [`feathers/`](feathers/). The execution backend is
`environment/winproc` (Windows Job Objects + `CreateProcessAsUser` + per-server
restricted accounts) in place of the Docker backend.

> ⚠️ **Status: in active development / lab use.** Not production-hardened yet.
> Use on a test VM against a test Panel. See [Status](#status) and
> [Known limitations](#known-limitations).

---

## Status

| Capability | State |
|---|---|
| Boot + register with Panel (node goes green) | ✅ verified |
| Install as a Windows Service | ✅ verified |
| Server **install** (runs egg script natively, no Docker) | ✅ verified |
| Server **power** (start / stop / restart / kill) | ✅ verified |
| **Console** stream + send commands (websocket) | ✅ verified |
| **Resource reporting** (CPU / memory graphs) | ✅ verified (network = 0) |
| **Resource enforcement** (memory + CPU hard caps, live-updatable) | ✅ verified |
| **Per-server isolation** (restricted account + NTFS ACLs + path jail) | ✅ verified (3 servers, no cross-access) |
| **File manager / SFTP** (impersonated as the per-server account) | ✅ verified |
| **Backups** (create → valid tar.gz) | ✅ verified (restore untested) |
| **Daemon-restart persistence** (auto-recovers running servers) | ✅ verified |
| Egg platform tagging (block Win egg on Linux & vice-versa) | ✅ verified |
| Schedules / cron · Win↔Win transfers · full OS-reboot recovery | ⚠️ built, not yet exercised |
| Per-process network stats | ❌ parity gap (reports 0) |
| OOM detection | ❌ parity gap (reports false) |

Validated end-to-end on **Windows Server 2025** against a stock Panel:
- **Minecraft Java 1.21.4** and **The Isle: Legacy** both install (native
  PowerShell/SteamCMD), boot, stream console, and run under their own restricted
  `pt_<uuid8>` accounts with CPU/memory hard-capped.
- 3 servers on one node, each NTFS-locked to its own volume (proven no
  cross-tenant access); backups produce valid archives; SFTP/file writes are
  impersonated; a daemon restart cleanly kills and **auto-restarts** running
  servers.

---

## Prerequisites

**To build (any machine with Go):**
- [Go **1.24+**](https://go.dev/dl/) (developed with 1.26).

**To run (the node):**
- **Windows Server 2022 or 2025** (or Windows 10/11 for testing), with admin access.
- A reachable **Pterodactyl Panel** (`v1.12.x`) you control.
- Network: the node must reach the Panel (80/443), and the Panel/your browser
  must reach the node on **TCP 8080** (daemon) and **2022** (SFTP).
- Per-egg runtime requirements (e.g. a **Java runtime** for the Minecraft test —
  see [Test 2](#test-2-minecraft-java)).

---

## 1. Build `feathers.exe`

The build cross-targets `windows/amd64`, so you can build on any OS with Go and
copy the single `.exe` to the node (the node needs **no** Go).

```powershell
# from the repo, in the feathers/ directory
pwsh ./scripts/build-windows.ps1
# -> produces feathers.exe (version stamped 1.12.3)
```

Optional: `pwsh ./scripts/build-windows.ps1 -Version 1.12.3 -Output C:\path\feathers.exe`

---

## 2. Create the node in the Panel

**Admin → Nodes → Create New:**

| Field | Value |
|---|---|
| **FQDN** | The Windows node's hostname or IP (e.g. `node1.example.com` or `203.0.113.10`) |
| **Communicate over SSL** | **Use HTTP** if your Panel is `http://…`; otherwise SSL (then use `--allow-insecure` for a self-signed cert) |
| **Behind Proxy** | Not behind proxy |
| **Daemon Port** | `8080` |
| **Daemon SFTP Port** | `2022` |
| **Daemon Server File Directory** | Cosmetic on Windows — the daemon always uses `C:\Pterodactyl\volumes`. You can leave the Linux default. |
| Memory / Disk | Whatever the VM offers |

After saving, open the node's **Configuration** tab and copy the `--panel-url`,
`--token` (`ptla_…`), and `--node <id>` values.

---

## 3. Install on the Windows node

Copy **`feathers.exe`** and **[`feathers/scripts/install-windows.ps1`](feathers/scripts/install-windows.ps1)**
to the node (e.g. `C:\feathers-install\`). Then, in an **elevated** PowerShell:

```powershell
cd C:\feathers-install
powershell -ExecutionPolicy Bypass -File .\install-windows.ps1 `
  -ExePath .\feathers.exe `
  -PanelUrl http://YOUR-PANEL `
  -Token ptla_yourNodeConfigToken `
  -NodeId <id> `
  -OpenFirewall `
  -Start
```

This lays out `C:\Pterodactyl\{volumes,backups,archives,tmp,logs}`, copies the
binary, writes `C:\Pterodactyl\config.yml` (via `feathers configure`), opens the
firewall for 8080/2022, and registers + starts the **"Pterodactyl Wings"**
service.

Manage the service afterward:
```powershell
C:\Pterodactyl\feathers.exe service start | stop | status | uninstall
```

### Verify the node is online
```powershell
C:\Pterodactyl\feathers.exe service status        # -> running
Get-Content C:\Pterodactyl\logs\*.log -Tail 30
```
The node dot in **Admin → Nodes** should turn **green** within ~30s.

---

## 4. Import the Windows eggs

Windows eggs live in [`docs/test-eggs/`](docs/test-eggs/). In the Panel:
**Admin → Nests → Import Egg**, upload each `.json`.

- **`feathers-heartbeat-test.json`** — a zero-dependency smoke test (prints a
  heartbeat every 2s). No runtime needed.
- **`minecraft-java-windows.json`** — vanilla Minecraft Java (downloads the
  server jar during install). Needs Java on the node — see below.
- **`the-isle-legacy-windows.json`** / **`the-isle-evrima-windows.json`** —
  The Isle dedicated server (SteamCMD app `412680`, `public`/`evrima` branch).
  Real SteamCMD workload; see the SteamCMD/Unreal notes in
  [Troubleshooting](#troubleshooting) (these eggs already bake in the fixes).

> ### 🔑 Egg platform tagging — important
> Eggs are tagged by their **Docker image**: a Windows egg's image is prefixed
> **`windows`** (e.g. `windows/java`). The daemon **enforces** this:
> - A **Linux** egg on a **Windows** node → rejected ("targets Linux…").
> - A **Windows** egg on a **Linux** node → rejected ("targets Windows…").
>
> So you cannot accidentally cross-install. Author Windows eggs with a
> `windows…` image and a **PowerShell** install script + **Windows-runnable**
> startup command. Existing community (Linux) eggs will **not** work on a Windows
> node.
>
> 📄 **See [`docs/AUTHORING_WINDOWS_EGGS.md`](docs/AUTHORING_WINDOWS_EGGS.md)** for
> the full guide on how Windows eggs differ from normal Ptero eggs and how to port one.

---

## 5. Create and run a test server

Create a server in the Panel as usual (**Admin → Servers → Create New**):
node = your Windows node, egg = one of the imported Windows eggs, give it an
allocation. **You do NOT need to check "Skip Egg Install Script"** — the native
installer handles it. The server installs automatically (this is the same path
PteroBill/WHMCS uses), then **Start** it from the console.

### Test 1: Heartbeat (no dependencies)
Use the **Feathers Heartbeat Test** egg. Start → status flips to **Running** and
you see `heartbeat from feathers …` every 2s. Stop/Kill works. This proves
power + console + install end-to-end.

### Test 2: Minecraft Java
Use the **Minecraft Java (Windows)** egg.

1. **Install Java on the node** (the service runs as LocalSystem, so Java must be
   on the **Machine** PATH), then **restart the service** so it inherits PATH:
   ```powershell
   winget install --id EclipseAdoptium.Temurin.21.JRE -e --scope machine `
     --accept-source-agreements --accept-package-agreements
   $jreBin = (Get-ChildItem 'C:\Program Files\Eclipse Adoptium\' -Directory |
              Where-Object Name -like 'jre*' | Select-Object -First 1).FullName + '\bin'
   $m = [Environment]::GetEnvironmentVariable('Path','Machine')
   if ($m -notlike "*$jreBin*") { [Environment]::SetEnvironmentVariable('Path', "$m;$jreBin", 'Machine') }
   C:\Pterodactyl\feathers.exe service stop; C:\Pterodactyl\feathers.exe service start
   ```
2. In the server's **Startup** tab, set **Minecraft Version** to a version your
   Java supports (Java 21 ⇒ `1.21.4`).
3. Set the server's **Memory** to a real value (e.g. `2048`) — *not* Unlimited,
   or `-Xmx0M` fails.
4. **Settings → Reinstall** (if you changed the version), then **Start**.

You should see the full Minecraft boot and `Done (…)! For help, type "help"`,
and be able to connect a client to `NODE-IP:PORT`.

---

## 6. Verify per-server isolation

While a server runs, on the node (elevated):

```powershell
$u = "<server-uuid>"            # from the Panel / C:\Pterodactyl\volumes
$acct = "pt_" + ($u -replace '-','').Substring(0,8)

# Process runs as the restricted account, NOT SYSTEM:
Get-WmiObject Win32_Process -Filter "Name='java.exe'" |
  ForEach-Object { $o=$_.GetOwner(); "$($_.ProcessId)  $($o.Domain)\$($o.User)" }

# Volume locked to just SYSTEM / Administrators / the account:
icacls "C:\Pterodactyl\volumes\$u"

# The per-server account exists, Users-group only:
net user $acct
```

Each server runs as its own `pt_<uuid8>` account, with its data directory
NTFS-locked so other servers/users can't read it. The account is created on
install/start (random password held only in memory, never written to disk) and
removed when the server is deleted. The file manager and SFTP perform their disk
operations **impersonated as this account**, so daemon-side file access is
ACL-bound too — a new file created via the Files tab or SFTP is owned by
`pt_<uuid8>`, not SYSTEM.

---

## Updating the daemon (deploy loop)

The service locks `feathers.exe`, so stop → replace → start:

```powershell
# build a new feathers.exe (step 1), copy it to the node, then:
C:\Pterodactyl\feathers.exe service stop
Start-Sleep 2
Copy-Item <new>\feathers.exe C:\Pterodactyl\feathers.exe -Force
C:\Pterodactyl\feathers.exe service start
```

No reconfigure needed — `config.yml` persists.

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Node stays **red** | Panel can't reach node on 8080 (firewall), or `/api/system` error — check `C:\Pterodactyl\logs\*.log`. |
| Server **install fails** with a Docker pipe error | The egg is a **Linux** egg (or its image isn't `windows…`). Use a Windows egg. |
| `egg platform mismatch … targets Linux` | Expected — assign a Windows egg to this node. |
| Start fails: `exec: "java": executable file not found` | Java not on the **Machine** PATH, or service not restarted after installing it. See [Test 2](#test-2-minecraft-java). |
| "Unsupported Java Version" modal | Egg's Minecraft version needs a newer Java than installed. Pin a compatible MC version, or install newer Java. (Click **Cancel**, not "Update Docker Image".) |
| Minecraft exits instantly, exit code `1` | `-Xmx0M` — set the server **Memory** to a real value. |
| Server crash-loops with no output | Out of date binary — redeploy the latest `feathers.exe`. |
| Egg won't save: **"docker images format is invalid"** | The display **name** can't contain parentheses. Use `Windows SteamCMD\|windows/steamcmd` (no `( )`), or just the bare image. |
| **SteamCMD install "completes" but downloads nothing** (only its own ~43 MB self-update) | A fresh SteamCMD exits after self-updating without running `+app_update`. Run it **twice** — once `+quit`, then the `+app_update`. (Baked into the Isle eggs.) |
| **SteamCMD creates a `%SystemDrive%` folder / installs to the wrong place** | The child process lacked the base Windows environment, or SteamCMD lived inside `force_install_dir`. Fixed in the daemon (child env now merges `os.Environ()`); keep SteamCMD **outside** the server dir (eggs put it in `%TEMP%`). |
| **Unreal server "runs" but ~8 MB / no console / stuck Starting** | The root `*.exe` is a thin launcher; run the real `…\Binaries\Win64\…-Win64-Shipping.exe` directly, and add **`-stdout`** so UE logs to the console. (Baked into the Isle eggs.) |
| **Game server runs but players can't connect on the allocation port** | Some games (Unreal/The Isle) ignore a bare `?Port` arg. Bind the port on the map URL (`<Map>?listen?Port=…`) and inject other ports with `-ini:Game:<section>:<Key>=<value>` overrides — the Evrima egg does this out of the box. |

---

## Known limitations

- **Per-process network stats** report `0` (no clean per-process Windows source) —
  the Panel network graph stays flat.
- **OOM detection** reports `false` (the Job Object memory cap fails allocations
  rather than emitting a kernel OOM signal).
- **POSIX chmod/chown** are no-ops on Windows (NTFS ACLs replace them).
- **Config-file writes** (the parser, e.g. `server.properties`) run as the daemon,
  not impersonated — cosmetic (owner = Administrators; the account still has access
  via the inherited ACL).
- **Unreal-engine game ports** (e.g. The Isle) are not taken from a bare `?Port`
  launch arg. All ports are defined by the **server's Panel allocations**: the
  daemon exposes every additional allocation as `SERVER_IP_<n>`/`SERVER_PORT_<n>`
  (sorted by IP then port, numbered from 1), and the Evrima egg binds game/query
  to the default allocation (map-URL `?Port=…` + `-QueryPort=…`), queue to
  `SERVER_PORT_1`, and RCON to `SERVER_PORT_2` via UE `-ini:Game:` command-line
  overrides — no `Game.ini` hand-editing, no port numbers typed into variables.
  Give the server 3 allocations (convention: queue = game+1, RCON = game+2).
  (The Legacy egg still needs the same treatment.)
- **Cross-platform transfers** (Win↔Linux) are out of scope; same-platform only.
- **Per-server account logon** uses the built-in *Users* group (interactive logon
  right). Tighter least-privilege via LSA batch-logon-right is a hardening TODO.
- Eggs must be **authored for Windows** (PowerShell install + Windows-runnable
  startup + any runtime installed/bundled). Linux community eggs do not work as-is.
- **Not yet exercised on Windows:** backup *restore*, schedules/cron, Win↔Win
  transfers, full OS-reboot recovery (daemon-restart recovery *is* verified).

---

## Layout

```
feathers/                     daemon source (forked Wings 1.12.3)
  environment/winproc/        Windows execution backend (Job Objects, accounts, ACLs)
  scripts/build-windows.ps1   cross-build feathers.exe
  scripts/install-windows.ps1 node installer (service + dirs + firewall + configure)
docs/phase0-conformance-spec.md   the Wings contract this daemon targets
docs/AUTHORING_WINDOWS_EGGS.md    how Windows eggs differ from normal Ptero eggs
docs/test-eggs/                   importable Windows eggs
```
