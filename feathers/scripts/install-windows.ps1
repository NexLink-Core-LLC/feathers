# Installs the Wings (feathers) daemon as a Windows Service on a node host
# (e.g. Windows Server 2022). Run from an elevated (Administrator) PowerShell.
#
# Example:
#   pwsh ./scripts/install-windows.ps1 -ExePath .\feathers.exe `
#        -PanelUrl https://panel.example.com -Token <node-config-token> -NodeId 1
#
# If -PanelUrl/-Token/-NodeId are omitted, the service is installed but you must
# run `feathers configure` (or place a config.yml) before starting it.

param(
    [Parameter(Mandatory = $true)][string]$ExePath,                # path to the built feathers.exe
    [string]$InstallDir = "C:\Pterodactyl",                        # base dir (config + server data)
    [string]$PanelUrl,                                             # optional: panel URL for auto-configure
    [string]$Token,                                                # optional: node configuration token
    [string]$NodeId,                                               # optional: node id
    [switch]$OpenFirewall,                                         # open daemon (8080) + SFTP (2022) ports
    [switch]$Start                                                 # start the service after install
)

$ErrorActionPreference = "Stop"

# Require elevation — service + firewall changes need Administrator.
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)) {
    throw "This script must be run from an elevated (Administrator) PowerShell session."
}

if (-not (Test-Path $ExePath)) { throw "feathers.exe not found at: $ExePath" }

# 1. Lay out the directory structure mirroring the Linux /var/lib/pterodactyl layout.
$dirs = @(
    $InstallDir,
    (Join-Path $InstallDir "volumes"),
    (Join-Path $InstallDir "backups"),
    (Join-Path $InstallDir "archives"),
    (Join-Path $InstallDir "tmp"),
    (Join-Path $InstallDir "logs")
)
foreach ($d in $dirs) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Path $d | Out-Null }
}
Write-Host "Directory structure ready under $InstallDir"

# 2. Copy the binary into place.
$dest = Join-Path $InstallDir "feathers.exe"
Copy-Item -Path $ExePath -Destination $dest -Force
Write-Host "Installed binary: $dest"

$configPath = Join-Path $InstallDir "config.yml"

# 3. Optionally auto-configure against the Panel (writes config.yml).
if ($PanelUrl -and $Token -and $NodeId) {
    Write-Host "Configuring against Panel $PanelUrl (node $NodeId) ..."
    & $dest configure --panel-url $PanelUrl --token $Token --node $NodeId --config-path $configPath
    if ($LASTEXITCODE -ne 0) { throw "configure failed" }
} elseif (-not (Test-Path $configPath)) {
    Write-Warning "No config.yml found at $configPath. Run '$dest configure ...' before starting the service."
}

# 4. Optionally open the firewall for the daemon API and SFTP.
if ($OpenFirewall) {
    foreach ($p in 8080, 2022) {
        $name = "Wings ($p)"
        if (-not (Get-NetFirewallRule -DisplayName $name -ErrorAction SilentlyContinue)) {
            New-NetFirewallRule -DisplayName $name -Direction Inbound -Action Allow `
                -Protocol TCP -LocalPort $p | Out-Null
            Write-Host "Opened firewall TCP/$p"
        }
    }
    Write-Warning "Game-server allocation ports must be opened separately per server/egg."
}

# 5. Register the service (idempotent: uninstall first if present).
# `service status` writes to stderr and exits non-zero when the service is not
# installed. Under $ErrorActionPreference = "Stop" that stderr is promoted to a
# terminating error, which would abort the whole install on a fresh host. Relax
# the preference just around this probe so we can branch on $LASTEXITCODE.
$prevEAP = $ErrorActionPreference
$ErrorActionPreference = "Continue"
& $dest service status *> $null
$serviceExists = ($LASTEXITCODE -eq 0)
$ErrorActionPreference = $prevEAP
if ($serviceExists) {
    Write-Host "Existing service found; reinstalling ..."
    & $dest service stop *> $null
    & $dest service uninstall
}
& $dest service install --config $configPath
if ($LASTEXITCODE -ne 0) { throw "service install failed" }

# 6. Optionally start it.
if ($Start) {
    & $dest service start
    if ($LASTEXITCODE -ne 0) { throw "service start failed" }
}

Write-Host ""
Write-Host "Done. Manage the service with:"
Write-Host "  $dest service start | stop | status | uninstall"
Write-Host "  (or services.msc -> 'Pterodactyl Wings')"
