<#
.SYNOPSIS
    Removes the TideSync scheduled task and/or Windows service.

.EXAMPLE
    .\uninstall-service.ps1
    .\uninstall-service.ps1 -Name MySync
#>
[CmdletBinding()]
param([string]$Name = "TideSync")

$ErrorActionPreference = "Continue"

if (Test-Path -LiteralPath (Join-Path $env:SystemRoot "System32\schtasks.exe")) {
    & schtasks.exe /Delete /F /TN $Name | Out-Host
}
& sc.exe stop $Name | Out-Host
& sc.exe delete $Name | Out-Host

Write-Host "Removed '$Name' (if it was installed)." -ForegroundColor Green
Write-Host "Log files and the sync journal are kept; delete them by hand if you want a clean slate:"
Write-Host "  $env:ProgramData\TideSync"
