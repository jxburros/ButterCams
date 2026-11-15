# Smoke test of buttercams.exe on a real Windows machine. CI and the release
# workflow both run it, so a published build has passed the same checks.
#
#   pwsh installer\smoke.ps1 [-Exe dist\buttercams.exe] [-ExpectVersion 1.0.0]
#
# Needs an elevated prompt and a machine without a ButterCams service: it
# registers the service, checks it, and removes it again.
param(
    [string]$Exe = "dist\buttercams.exe",
    [string]$ExpectVersion = ""
)
$ErrorActionPreference = "Stop"
$exe = (Resolve-Path $Exe).Path
if (-not $env:RUNNER_TEMP) { $env:RUNNER_TEMP = [System.IO.Path]::GetTempPath().TrimEnd('\') }

if ($ExpectVersion) {
    $reported = (& $exe version) -join ""
    if ($reported -ne "ButterCams $ExpectVersion") { throw "the build reports '$reported', expected version $ExpectVersion" }
}

$root = "$env:RUNNER_TEMP\buttercams-data"
& $exe setup init --data-root $root --port 8650 --admin-user admin --admin-password "correct horse" --dest-path "$env:RUNNER_TEMP\recordings" --dest-cap-gb 1
if ($LASTEXITCODE -ne 0) { throw "setup init failed" }
& $exe setup test-destination --dest-path "$env:RUNNER_TEMP\recordings"
if ($LASTEXITCODE -ne 0) { throw "test-destination failed" }
& $exe setup install-service --data-root $root
if ($LASTEXITCODE -ne 0) { throw "install-service failed" }
$ok = $false
foreach ($i in 1..30) {
  Start-Sleep -Seconds 1
  try {
    $h = curl.exe -sk https://localhost:8650/api/v1/health | ConvertFrom-Json
    if ($h.status) { $ok = $true; break }
  } catch {}
}
if (-not $ok) { Get-Content "$root\logs\*.log" -Tail 50; throw "the service did not answer on https://localhost:8650" }
Write-Host "health: $($h.status), version $($h.version)"
if ($ExpectVersion -and $h.version -ne $ExpectVersion) { throw "the service reports version $($h.version), expected $ExpectVersion" }
$svc = Get-CimInstance Win32_Service -Filter "Name='ButterCams'"
if ($svc.StartMode -ne "Auto" -or $svc.State -ne "Running") { throw "service is $($svc.StartMode)/$($svc.State)" }
sc.exe qfailure ButterCams
sc.exe qc ButterCams
$fail = (sc.exe qfailure ButterCams) -join "`n"
if ($fail -notmatch "RESTART -- Delay = 5000") { throw "recovery actions are not set" }
$delayed = (Get-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Services\ButterCams").DelayedAutostart
if ($delayed -ne 1) { throw "the service is not Automatic (Delayed Start)" }
$rule = Get-NetFirewallRule -DisplayName "ButterCams Web"
$profiles = "$($rule.Profile)"
if ($profiles -notmatch "Domain" -or $profiles -notmatch "Private" -or $profiles -match "Public") { throw "firewall rule profiles: $profiles" }
if ((Get-ItemProperty "HKLM:\SOFTWARE\ButterCams").DataRoot -ne $root) { throw "DataRoot is not in the registry" }
# Children must be running inside the service and die with it.
$children = Get-CimInstance Win32_Process -Filter "Name='buttercams.exe'" | Measure-Object
Write-Host "buttercams.exe processes: $($children.Count)"
if ($children.Count -lt 2) { throw "the mover is not running" }
# A clean stop must not be treated as a crash and restarted.
Stop-Service ButterCams
Start-Sleep -Seconds 8
if ((Get-Service ButterCams).Status -ne "Stopped") { throw "the service came back after a clean stop" }
if (Get-Process buttercams -ErrorAction SilentlyContinue) { throw "child processes outlived the service" }
& $exe setup uninstall-service --data-root $root --remove-data
if ($LASTEXITCODE -ne 0) { throw "uninstall-service failed" }
if (Get-Service ButterCams -ErrorAction SilentlyContinue) { throw "the service was not removed" }
if (Test-Path "$root\config.json") { throw "the data root was not removed" }
if (-not (Test-Path "$env:RUNNER_TEMP\recordings")) { throw "the destination folder must never be removed" }
