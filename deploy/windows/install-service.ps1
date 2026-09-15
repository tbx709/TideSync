<#
.SYNOPSIS
    Installs TideSync as a Windows scheduled task (the recommended deployment)
    or as a Windows service.

.DESCRIPTION
    Run this from an elevated PowerShell prompt (Run as Administrator).

    Scheduled task mode (default) runs `tidesync.exe sync -once` every N
    minutes as SYSTEM. It survives reboots, needs no service host, and a long
    synchronisation simply finishes while the next tick waits for the job lock.

    Service mode registers a real Windows service that keeps a daemon running.

.EXAMPLE
    .\install-service.ps1 -Config C:\ProgramData\TideSync\client.json
    .\install-service.ps1 -Config C:\ProgramData\TideSync\client.json -IntervalMinutes 15
    .\install-service.ps1 -Config C:\ProgramData\TideSync\client.json -Mode Service
    .\install-service.ps1 -Config C:\ProgramData\TideSync\server.json -Role server
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Config,
    [string]$Binary = "",
    [string]$Name = "TideSync",
    [ValidateSet("Task", "Service")][string]$Mode = "Task",
    [int]$IntervalMinutes = 5,
    [ValidateSet("client", "server")][string]$Role = "client"
)

$ErrorActionPreference = "Stop"

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw "This script must run from an elevated PowerShell prompt (Run as Administrator)."
    }
}

Assert-Administrator

if (-not (Test-Path -LiteralPath $Config)) {
    throw "Configuration file not found: $Config. Create one with: tidesync init $Role -out `"$Config`""
}
$Config = (Resolve-Path -LiteralPath $Config).Path

if ([string]::IsNullOrWhiteSpace($Binary)) {
    $Binary = Join-Path $PSScriptRoot "tidesync.exe"
    if (-not (Test-Path -LiteralPath $Binary)) {
        $cmd = Get-Command tidesync.exe -ErrorAction SilentlyContinue
        if ($cmd) { $Binary = $cmd.Source } else { throw "tidesync.exe not found. Pass -Binary <path>." }
    }
}
$Binary = (Resolve-Path -LiteralPath $Binary).Path

Write-Host "TideSync installation" -ForegroundColor Cyan
Write-Host "  binary  : $Binary"
Write-Host "  config  : $Config"
Write-Host "  role    : $Role"
Write-Host "  mode    : $Mode"
Write-Host ""

if ($Mode -eq "Task") {
    $action = if ($Role -eq "server") { "serve" } else { "sync" }
    $arguments = "`"$action`" -config `"$Config`""
    if ($Role -eq "client") { $arguments += " -once" }

    Write-Host "Registering scheduled task '$Name' (every $IntervalMinutes minute(s), as SYSTEM)..."
    $taskCommand = "`"$Binary`" $arguments"
    & schtasks.exe /Create /F /TN $Name /SC MINUTE /MO $IntervalMinutes /RU SYSTEM /RL HIGHEST /TR $taskCommand | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "schtasks failed with exit code $LASTEXITCODE" }

    & schtasks.exe /Run /TN $Name | Out-Host
    Write-Host ""
    Write-Host "Installed. Inspect it with:" -ForegroundColor Green
    Write-Host "  schtasks /Query /TN $Name /FO LIST /V"
    Write-Host "  Get-Content C:\ProgramData\TideSync\tidesync.log -Tail 50 -Wait"
    Write-Host "  tidesync.exe sync -config `"$Config`" -once -log-level debug   # run once by hand"
    exit 0
}

Write-Host "Registering Windows service '$Name'..."
$binPath = "`"$Binary`" service run -config `"$Config`""
& sc.exe create $Name binPath= $binPath start= auto DisplayName= "TideSync synchroniser" | Out-Host
& sc.exe description $Name "TideSync - scheduled file synchronisation over the local network" | Out-Host
& sc.exe failure $Name reset= 86400 actions= restart/60000/restart/60000/restart/60000 | Out-Host
& sc.exe start $Name | Out-Host
Write-Host ""
Write-Host "Installed. Inspect it with:  sc.exe query $Name" -ForegroundColor Green
Write-Host "If the service stops immediately, run the same command in a console:" -ForegroundColor Yellow
Write-Host "  `"$Binary`" service run -config `"$Config`""
