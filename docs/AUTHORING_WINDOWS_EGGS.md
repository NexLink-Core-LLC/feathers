# Authoring Windows (feathers) Eggs

How eggs for a **feathers / native-Windows node** differ from normal Pterodactyl
(Linux/Docker) eggs — and the gotchas that bite when porting one.

> **TL;DR** — a Windows egg is a *normal Ptero egg JSON* with Windows-appropriate
> content: the Docker image field becomes a **platform tag** (`windows/...`), the
> install script is **PowerShell** (not bash), the startup command launches a
> **native Windows process** (no shell), and any runtime (Java, .NET, SteamCMD)
> must be **on the host or installed by the egg** — there is no container image
> bringing it. You import it exactly like any other egg.

---

## Why they differ at all

Normal Wings runs each server **inside a Docker container** built from the egg's
image. The image provides the OS userland + runtime, namespaces provide isolation,
and cgroups enforce limits.

feathers has **no containers**. Each server is a **native Windows process tree**:
- isolation = a per-server restricted local account (`pt_<uuid8>`) + NTFS ACLs
- limits = a Windows **Job Object** (hard memory/CPU caps, tree-kill)
- the console = the process's stdout/stderr piped back, same as the Docker stream

Everything an egg "got for free" from its container image is now **your
responsibility to provide on the node or in the install script.** That single
fact explains most of the differences below.

---

## The field-by-field differences

### 1. `docker_images` → **platform tag** (not a real image)
There is no container, so the image string is repurposed as a **platform marker**.
A Windows egg's image **must be prefixed `windows`** (e.g. `windows/steamcmd`,
`windows/java`). feathers **enforces** this:

- A **Linux** egg (non-`windows` image) on a Windows node → **rejected** ("targets Linux…")
- A **Windows** egg on a **Linux** node → **rejected** ("targets Windows…")

So you can't cross-install by accident. The string is otherwise inert — nothing
is pulled. Pick a descriptive `windows/<thing>` label.

> ⚠️ The egg's display **name** can't contain parentheses, or the panel rejects
> the egg with "docker images format is invalid." Use `Windows SteamCMD|windows/steamcmd`, not `... (windows/steamcmd)`.

### 2. Install script: **bash → PowerShell**
Normal eggs run their install script in a throwaway Linux container with a
`bash`/`sh` entrypoint. feathers writes the script to disk and runs it natively:

```
powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File install.ps1
```

Consequences:
- Write **PowerShell**, not bash. No `apt`, `curl`, `tar`, `chmod`, `&&`-chains.
  Use `Invoke-WebRequest`, `Expand-Archive`, `New-Item`, etc.
- Line endings are normalized to **CRLF**.
- The script runs in the **server's data directory** with a per-server writable
  `TEMP`/`TMP` already provided.
- Panel egg variables arrive as **environment variables** (`$env:SERVER_NAME`,
  `$env:P_SERVER_UUID`, etc.), same as Linux.
- A non-zero exit is **logged, not fatal** (matches Docker-install semantics); the
  full install log is retained even on failure.

> ⚠️ **Today the install runs as the daemon (LocalSystem)**, not the restricted
> per-server account. Treat egg install scripts as trusted until that's hardened
> (tracked as a Bar-B item). Don't paste untrusted community install scripts.

### 3. Startup command: **native Windows process, no shell**
Normal startup runs inside the container (`./srcds_run`, `java -jar …`). feathers
launches a **native process** with an **argv-form** command (no `cmd.exe /c`, no
shell) — this is what makes it injection-safe.

- `argv[0]` is resolved **against the server directory first** (e.g.
  `TheIsle/Binaries/Win64/TheIsleServer-Win64-Shipping.exe`), then PATH (e.g.
  `java`).
- Use the panel's `{{VARIABLE}}` placeholders for substitution. **Do not** rely on
  shell features: no `$VAR` expansion, no `&&`, no pipes, no globbing.
- The command must be runnable on Windows (a Windows `.exe`, or `java`/`python`
  that exists on the host — see runtimes below).

### 4. Runtimes are **not provided by an image** — host or egg must supply them
This is the biggest authoring shift. On Linux the yolks image *is* the runtime
(`ghcr.io/parkervcp/yolks:java_21` ships Java). On Windows there is no image, so:

- **Java / .NET / Node / Python must be installed on the node** (on the **Machine**
  PATH, since the service runs as LocalSystem) **or** downloaded/bundled by the egg.
- After installing a runtime on the node, **restart the wings service** so it
  inherits the updated Machine PATH.
- **SteamCMD** is downloaded by the egg into `%TEMP%` (see gotchas).

### 5. Ports come from **allocations** (`SERVER_PORT_<n>`) — a feathers extra
Stock Wings only exposes the primary allocation as `{{SERVER_PORT}}`. **feathers
also exposes every *additional* allocation** as `{{SERVER_IP_<n>}}` /
`{{SERVER_PORT_<n>}}`, sorted by IP then port, numbered from 1. This lets
multi-port games bind straight to the server's Panel allocations instead of
hand-typed port variables that drift.

Example (the Evrima egg): game/query ride `{{SERVER_PORT}}`, queue rides
`{{SERVER_PORT_1}}`, RCON rides `{{SERVER_PORT_2}}`. Give the server 3 allocations
(convention: queue = game+1, RCON = game+2) and they flow through automatically.

> This works on Linux/Docker nodes too (the env var is set in the shared server
> layer), so it's a safe pattern to standardize on.

### 6. Config-file management (`config.files`) — mostly the same, one caveat
The egg's `config.files` parser (rewriting `server.properties`, `Game.ini`, etc.
at boot) is **OS-neutral** and works on feathers. Caveat: config writes run as the
**daemon**, not impersonated (cosmetic — the per-server account still has access
via the inherited ACL).

Some games (Unreal) **ignore launch args** for ports and read their own config.
Two ways to handle it: bake the value into `Game.ini` via the parser, **or** use
UE `-ini:` command-line overrides (what the Evrima egg does — see below).

### 7. Stop configuration
- A **command stop** is written to the process's **stdin** (default line ending
  `\n`; some Windows servers need `\r\n` — a known rough edge).
- A **signal stop** (or no stop config) falls back to **terminating the Job
  Object** (Windows has no POSIX signals). This kills the whole process tree.

---

## Game-specific gotchas (learned the hard way)

**SteamCMD**
- A fresh SteamCMD **exits after self-updating** without running `+app_update`.
  Run it **twice**: once `+quit` to bootstrap, then the real `+app_update`.
- Keep SteamCMD **outside** the server dir (the eggs put it in `%TEMP%`), and know
  that the child env merges the host's base Windows environment so `%SystemDrive%`
  etc. resolve (otherwise SteamCMD makes junk like a `%SystemDrive%` folder).

**Unreal Engine servers (The Isle, etc.)**
- The root `*.exe` is a **thin launcher** (~0.3 MB). Launch the **real** binary
  directly: `…\Binaries\Win64\…-Win64-Shipping.exe`, and add **`-stdout`** so UE
  logs reach the console (otherwise you get a stuck "Starting" with no output).
- Ports are **not** taken from a bare `?Port` arg. Bind the game port on the **map
  URL** (`<Map>?listen?Port=…`) and inject the rest with **`-ini:Game:<section>:<Key>=<value>`**
  command-line overrides. The Evrima egg binds all four ports this way from
  allocations — no `Game.ini` hand-editing.
- Prereqs: run `UE4PrereqSetup_x64.exe /quiet /norestart` in the install (VC++
  redists) if the game needs them.

**Minecraft / Java**
- Java must be on the **Machine** PATH and the service restarted after installing
  it. Set the server **Memory** to a real value (not Unlimited) or `-Xmx0M` fails.

---

## What is the SAME as a normal egg
- The **egg JSON schema** and the **import flow** (Admin → Nests → Import Egg).
- Egg **variables** (name/description/env_variable/default/rules/field_type).
- The `config.startup` "done" string that flips the server to *running*.
- `config.files` / `config.logs` parsers.
- Backups, schedules, the file manager, SFTP, the console — all work the same
  from the panel's side.

So: **port an egg by rewriting its install (PowerShell), its image (`windows/…`),
and its startup (native + host runtime), keeping the variables/config/done-string
structure intact.**

---

## Reference eggs
- [`the-isle-evrima-windows.json`](test-eggs/the-isle-evrima-windows.json) — the
  canonical example: SteamCMD (run twice), Shipping-exe launch, `-stdout`, EOS
  creds to `Engine.ini`, and all ports bound from allocations via `-ini:` overrides.
- [`the-isle-legacy-windows.json`](test-eggs/the-isle-legacy-windows.json) — Legacy branch.
- [`minecraft-java-windows.json`](test-eggs/minecraft-java-windows.json) — Java-on-host example.
- [`feathers-heartbeat-test.json`](test-eggs/feathers-heartbeat-test.json) —
  zero-dependency smoke test (no runtime needed).

See the top-level [`README.md`](../README.md) Troubleshooting table for the full
symptom→fix list.
