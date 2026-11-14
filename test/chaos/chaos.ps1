<#
.SYNOPSIS
  ButterCams chaos test for Windows (spec 14.1): breaks things at random and
  checks that the service recovers on its own.

.DESCRIPTION
  Run in an elevated PowerShell on a TEST machine where ButterCams is
  installed and recording (the camera simulator is fine). Every round picks
  one action:

    kill-worker    kills one recorder worker process
    kill-mover     kills the copy process
    kill-guardian  kills the service process (the service manager restarts it)
    block-share    blocks SMB to the share host with a firewall rule for a while
    fill-staging   fills the staging drive down to a few GB free, then frees it
    clock-jump     moves the system clock forward or back, then restores it

  After each action it waits, then polls https://localhost:<port>/api/v1/health
  until the status is "healthy" again and records how long that took. Any
  round that does not recover within -RecoverySeconds is a failure.

.EXAMPLE
  .\chaos.ps1 -Rounds 50 -Actions kill-worker,kill-mover,kill-guardian
.EXAMPLE
  .\chaos.ps1 -Rounds 20 -Actions block-share -ShareHost nas01 -BlockSeconds 600
#>
[CmdletBinding()]
param(
    [int]$Rounds = 20,
    [string[]]$Actions = @("kill-worker", "kill-mover", "kill-guardian"),
    [int]$Port = 8650,
    [int]$PauseSeconds = 30,
    [int]$RecoverySeconds = 180,
    [string]$ShareHost = "",
    [int]$BlockSeconds = 120,
    [int]$LeaveFreeGB = 2,
    [int]$ClockJumpMinutes = 90,
    [string]$LogFile = "chaos-$(Get-Date -Format yyyyMMdd-HHmmss).log"
)

$ErrorActionPreference = "Stop"
if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Run this script in an elevated PowerShell."
}

function Log($msg) {
    $line = "$(Get-Date -Format o) $msg"
    Write-Host $line
    Add-Content -Path $LogFile -Value $line
}

function Get-Health {
    try {
        return (curl.exe -sk --max-time 5 "https://localhost:$Port/api/v1/health" | ConvertFrom-Json)
    } catch {
        return $null
    }
}

function Wait-Healthy($what) {
    $start = Get-Date
    while (((Get-Date) - $start).TotalSeconds -lt $RecoverySeconds) {
        $h = Get-Health
        if ($h -and $h.status -eq "healthy") {
            $secs = [int]((Get-Date) - $start).TotalSeconds
            Log "  recovered from $what in $secs s"
            return $true
        }
        Start-Sleep -Seconds 2
    }
    $h = Get-Health
    $reasons = if ($h) { ($h.reasons | ForEach-Object { $_.message }) -join " | " } else { "no answer from the service" }
    Log "  FAILED to recover from $what within $RecoverySeconds s: $reasons"
    return $false
}

function Get-ButterProcs($role) {
    Get-CimInstance Win32_Process -Filter "Name='buttercams.exe'" |
        Where-Object { $_.CommandLine -match "\b$role\b" }
}

function Get-Staging {
    $root = (Get-ItemProperty "HKLM:\SOFTWARE\ButterCams").DataRoot
    $cfg = Get-Content (Join-Path $root "config.json") -Raw | ConvertFrom-Json
    return $cfg.staging.path
}

function Invoke-Action($action) {
    switch ($action) {
        "kill-worker" {
            $p = Get-ButterProcs "worker" | Get-Random
            if (-not $p) { Log "  no worker running"; return }
            Log "  killing worker pid $($p.ProcessId)"
            Stop-Process -Id $p.ProcessId -Force
        }
        "kill-mover" {
            $p = Get-ButterProcs "mover" | Select-Object -First 1
            if (-not $p) { Log "  no mover running"; return }
            Log "  killing mover pid $($p.ProcessId)"
            Stop-Process -Id $p.ProcessId -Force
        }
        "kill-guardian" {
            $p = Get-ButterProcs "service" | Select-Object -First 1
            if (-not $p) { Log "  service process not found"; return }
            Log "  killing guardian pid $($p.ProcessId); the service manager restarts it after 5 s"
            Stop-Process -Id $p.ProcessId -Force
            Start-Sleep -Seconds 2
            if (Get-ButterProcs "worker") { Log "  WARNING: workers outlived the guardian" }
        }
        "block-share" {
            if (-not $ShareHost) { Log "  block-share needs -ShareHost"; return }
            $ip = (Resolve-DnsName $ShareHost -Type A | Select-Object -First 1).IPAddress
            Log "  blocking SMB to $ShareHost ($ip) for $BlockSeconds s"
            New-NetFirewallRule -DisplayName "ButterCams chaos" -Direction Outbound -Action Block `
                -RemoteAddress $ip -Protocol TCP -RemotePort 445 | Out-Null
            try {
                Start-Sleep -Seconds $BlockSeconds
                $h = Get-Health
                Log "  while blocked: status $($h.status)"
            } finally {
                Remove-NetFirewallRule -DisplayName "ButterCams chaos"
                Log "  share unblocked"
            }
        }
        "fill-staging" {
            $staging = Get-Staging
            $drive = (Get-Item $staging).PSDrive
            $bytes = $drive.Free - ($LeaveFreeGB * 1GB)
            if ($bytes -le 0) { Log "  staging drive already below $LeaveFreeGB GB free"; return }
            $filler = Join-Path $drive.Root "buttercams-chaos-filler.bin"
            Log "  filling $($drive.Root) with a $([int]($bytes / 1GB)) GB file"
            fsutil file createnew $filler $bytes | Out-Null
            try {
                Start-Sleep -Seconds 90
                $h = Get-Health
                Log "  while full: status $($h.status) ($(($h.reasons | ForEach-Object { $_.code }) -join ','))"
            } finally {
                Remove-Item $filler -Force
                Log "  filler removed"
            }
        }
        "clock-jump" {
            $delta = if ((Get-Random -Maximum 2) -eq 0) { $ClockJumpMinutes } else { -$ClockJumpMinutes }
            Log "  moving the clock by $delta minutes"
            Stop-Service w32time -ErrorAction SilentlyContinue
            Set-Date (Get-Date).AddMinutes($delta) | Out-Null
            try {
                Start-Sleep -Seconds 60
            } finally {
                Set-Date (Get-Date).AddMinutes(-$delta) | Out-Null
                Start-Service w32time -ErrorAction SilentlyContinue
                Log "  clock restored"
            }
        }
        default { throw "unknown action $action" }
    }
}

Log "ButterCams chaos test: $Rounds rounds of [$($Actions -join ', ')]"
if (-not (Wait-Healthy "start")) { throw "ButterCams is not healthy before the test; fix that first." }

$failures = 0
for ($i = 1; $i -le $Rounds; $i++) {
    $action = $Actions | Get-Random
    Log "round $i/${Rounds}: $action"
    Invoke-Action $action
    Start-Sleep -Seconds 5
    if (-not (Wait-Healthy $action)) { $failures++ }
    Start-Sleep -Seconds $PauseSeconds
}

Log "done: $failures of $Rounds rounds failed to recover"
Log "Now check the Recordings page: every gap must line up with a round above and be within the loss targets."
exit $failures
