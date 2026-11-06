[CmdletBinding()]
param(
    [ValidateRange(1, 480)]
    [int]$Minutes = 15,
    [switch]$StatsOnly
)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$client = Join-Path $projectRoot 'services\vico-bridge\bin\vico-cli.exe'
$cacheDirectory = Join-Path $projectRoot 'local\vicohome-lab-cache'
$resultDirectory = Join-Path $projectRoot 'local\lab-results'
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$summaryPath = Join-Path $resultDirectory "cg6-endurance-$stamp.json"
$videoPath = Join-Path $resultDirectory "cg6-endurance-$stamp.h264"
$plainPassword = $null
$passwordPointer = [IntPtr]::Zero

if (-not (Test-Path -LiteralPath $client)) {
    throw "Lab receiver was not found at $client"
}
New-Item -ItemType Directory -Force -Path $resultDirectory | Out-Null
New-Item -ItemType Directory -Force -Path $cacheDirectory | Out-Null

$email = Read-Host 'VicoHome lab account email'
$securePassword = Read-Host 'VicoHome lab account password' -AsSecureString

try {
    $passwordPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($securePassword)
    $plainPassword = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($passwordPointer)
    $env:VICOHOME_EMAIL = $email
    $env:VICOHOME_PASSWORD = $plainPassword
    $env:VICOHOME_CACHE_DIR = $cacheDirectory
    Remove-Item Env:VICOHOME_DEBUG -ErrorAction SilentlyContinue

    $deviceJSON = & $client devices list --format json
    if ($LASTEXITCODE -ne 0) {
        throw 'Could not authenticate or list lab cameras.'
    }
    $devices = @($deviceJSON | ConvertFrom-Json)
    if ($devices.Count -eq 0) {
        throw 'The lab account has no cameras.'
    }
    if ($devices.Count -eq 1) {
        $selected = $devices[0]
    }
    else {
        for ($i = 0; $i -lt $devices.Count; $i++) {
            Write-Host ("[{0}] {1} ({2})" -f ($i + 1), $devices[$i].deviceName, $devices[$i].modelNo)
        }
        $choice = [int](Read-Host 'Camera number')
        if ($choice -lt 1 -or $choice -gt $devices.Count) {
            throw 'Invalid camera selection.'
        }
        $selected = $devices[$choice - 1]
    }
    Write-Host ("Starting battery: {0}%; charging flag: {1}" -f
        $selected.batteryLevel, $selected.isCharging)

    $observe = '{0}m' -f $Minutes
    $timeout = '{0}m' -f ($Minutes + 2)
    $probeArgs = @('devices', 'media-probe', $selected.serialNumber,
        '--observe-for', $observe, '--timeout', $timeout,
        '--progress-every', '15s')
    if (-not $StatsOnly) {
        $probeArgs += @('--output', $videoPath)
    }
    Write-Host ("Observing {0} for {1} minutes..." -f $selected.deviceName, $Minutes) -ForegroundColor Cyan
    $raw = & $client @probeArgs
    if ($LASTEXITCODE -ne 0) {
        throw 'The live-session probe failed. Check the camera power and Wi-Fi.'
    }
    $result = $raw | ConvertFrom-Json
    $endDeviceJSON = & $client devices list --format json
    $endDevice = if ($LASTEXITCODE -eq 0) {
        @($endDeviceJSON | ConvertFrom-Json) |
            Where-Object { $_.serialNumber -eq $selected.serialNumber } |
            Select-Object -First 1
    }
    $result | Add-Member -NotePropertyName startBatteryPercent -NotePropertyValue $selected.batteryLevel
    $result | Add-Member -NotePropertyName startChargingFlag -NotePropertyValue $selected.isCharging
    if ($endDevice) {
        $result | Add-Member -NotePropertyName endBatteryPercent -NotePropertyValue $endDevice.batteryLevel
        $result | Add-Member -NotePropertyName endChargingFlag -NotePropertyValue $endDevice.isCharging
    }
    $result | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $summaryPath -Encoding utf8

    $windowSeconds = [Math]::Max(1,
        [Math]::Floor($result.lastVideoMillis / 1000) -
        [Math]::Floor($result.firstVideoMillis / 1000) + 1)
    $coverage = [Math]::Round(100 * $result.videoSecondsWithPackets / $windowSeconds, 1)
    Write-Host ("End: {0}; video packets: {1}; longest gap: {2} ms; idle at end: {3} ms" -f
        $result.endReason, $result.videoPackets, $result.largestVideoGapMillis,
        $result.videoIdleAtEndMillis) -ForegroundColor Green
    Write-Host ("Active-second coverage between first and last packet: {0}%" -f $coverage)
    if ($endDevice) {
        Write-Host ("Ending battery: {0}%; charging flag: {1}" -f
            $endDevice.batteryLevel, $endDevice.isCharging)
    }
    Write-Host "Summary: $summaryPath"
    if (-not $StatsOnly) {
        Write-Host "Raw H.264: $videoPath"
    }
}
finally {
    Remove-Item Env:VICOHOME_EMAIL -ErrorAction SilentlyContinue
    Remove-Item Env:VICOHOME_PASSWORD -ErrorAction SilentlyContinue
    Remove-Item Env:VICOHOME_CACHE_DIR -ErrorAction SilentlyContinue
    $plainPassword = $null
    if ($passwordPointer -ne [IntPtr]::Zero) {
        [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($passwordPointer)
    }
    $tokenPath = Join-Path $cacheDirectory 'auth.json'
    if (Test-Path -LiteralPath $tokenPath) {
        Remove-Item -LiteralPath $tokenPath -Force
    }
}
