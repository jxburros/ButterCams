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
    kill-media     kills the live view process (spec 16.11): recording must not
                   notice, and live view must be back within 10 s
    pin-media      keeps every CPU core busy at the live view process's priority
                   (below normal) for a while: recorders must not slow down
    viewers        opens live streams that connect and vanish at random
                   (needs -User and -Password of a ButterCams account)

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
    [int]$PinSeconds = 120,
    [int]$Viewers = 20,
    [string]$User = "",
    [string]$Password = "",
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

# Get-FrameCounters reads each recording camera's current file and size, so a
# viewer-side action can prove that recording carried on in the same files.
function Get-FrameCounters {
    $out = @{}
    try {
        $root = (Get-ItemProperty "HKLM:\SOFTWARE\ButterCams").DataRoot
        $staging = Get-Staging
        Get-ChildItem -Path $staging -Recurse -Filter "*.part.mp4" -ErrorAction SilentlyContinue |
            ForEach-Object { $out[$_.FullName] = $_.Length }
    } catch { }
    return $out
}

# Compare-Recording fails the round when a file that was being written before
# a viewer-side action is no longer growing in place: the action reached the
# recorder, which it never may (spec 16.9).
function Compare-Recording($before) {
    Start-Sleep -Seconds 6
    $now = [datetime]::Now
    $bad = 0
    foreach ($path in $before.Keys) {
        if ($now.Minute -eq 0 -or $now.Minute -eq 59) { continue }   # the hourly rollover closes files by design
        $f = Get-Item $path -ErrorAction SilentlyContinue
        if (-not $f) { Log "  FAILED: $path was closed during the action"; $bad++ }
        elseif ($f.Length -le $before[$path]) { Log "  FAILED: $path stopped growing during the action"; $bad++ }
    }
    if ($bad -gt 0) { $script:extraFailures++ } else { Log "  recording was not affected ($($before.Count) files kept growing)" }
}

$script:extraFailures = 0

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
        "kill-media" {
            $p = Get-ButterProcs "media" | Select-Object -First 1
            if (-not $p) { Log "  no live view process running (is live view switched on?)"; return }
            $before = Get-FrameCounters
            Log "  killing the live view process pid $($p.ProcessId)"
            Stop-Process -Id $p.ProcessId -Force
            $start = Get-Date
            while (-not (Get-ButterProcs "media") -and ((Get-Date) - $start).TotalSeconds -lt 30) { Start-Sleep -Milliseconds 200 }
            $secs = [math]::Round(((Get-Date) - $start).TotalSeconds, 1)
            if ($secs -gt 10) { Log "  FAILED: the live view process took $secs s to come back (limit 10 s)"; $script:extraFailures++ }
            else { Log "  live view process back after $secs s" }
            Compare-Recording $before
        }
        "pin-media" {
            # The live view process runs at below-normal priority. Load at that
            # priority on every core is what a pinned live view process looks
            # like to the recorders, without needing a hook inside ButterCams.
            $cores = [Environment]::ProcessorCount
            $before = Get-FrameCounters
            Log "  keeping $cores cores busy at below-normal priority for $PinSeconds s"
            $hogs = 1..$cores | ForEach-Object {
                $h = Start-Process powershell -PassThru -WindowStyle Hidden -ArgumentList "-NoProfile", "-Command", "while (`$true) {}"
                $h.PriorityClass = "BelowNormal"
                $h
            }
            try { Start-Sleep -Seconds $PinSeconds } finally { $hogs | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue } }
            Compare-Recording $before
        }
        "viewers" {
            if (-not $User) { Log "  viewers needs -User and -Password"; return }
            $before = Get-FrameCounters
            $jar = Join-Path $env:TEMP "buttercams-chaos-cookies.txt"
            $body = @{ username = $User; password = $Password } | ConvertTo-Json -Compress
            curl.exe -sk -c $jar -H "Content-Type: application/json" -d $body "https://localhost:$Port/api/v1/auth/login" | Out-Null
            $cams = (curl.exe -sk -b $jar "https://localhost:$Port/api/v1/cameras" | ConvertFrom-Json).cameras
            if (-not $cams) { Log "  could not sign in or no cameras"; return }
            Log "  $Viewers viewers connecting and vanishing for $PinSeconds s"
            $end = (Get-Date).AddSeconds($PinSeconds)
            $procs = @()
            while ((Get-Date) -lt $end) {
                $procs = @($procs | Where-Object { -not $_.HasExited })
                while ($procs.Count -lt $Viewers) {
                    $cam = ($cams | Get-Random).id
                    $kind = @("sub", "main") | Get-Random
                    $life = Get-Random -Minimum 1 -Maximum 20
                    # A WebSocket handshake by hand: curl keeps the stream open and discards it.
                    $procs += Start-Process curl.exe -PassThru -WindowStyle Hidden -ArgumentList "-sk", "-b", $jar, "--http1.1", "--max-time", $life,
                        "-H", "Connection: Upgrade", "-H", "Upgrade: websocket", "-H", "Sec-WebSocket-Version: 13",
                        "-H", "Sec-WebSocket-Key: Y2hhb3MtY2hhb3MtY2hhb3M=", "-o", "NUL",
                        "https://localhost:$Port/api/v1/live/$cam/$kind"
                }
                Start-Sleep -Milliseconds 500
            }
            $procs | Where-Object { -not $_.HasExited } | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue }
            Compare-Recording $before
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

$failures += $script:extraFailures
Log "done: $failures failures in $Rounds rounds"
Log "Now check the Recordings page: every gap must line up with a round above and be within the loss targets."
exit $failures
