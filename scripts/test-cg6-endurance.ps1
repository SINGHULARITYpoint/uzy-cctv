[CmdletBinding()]
param(
    [ValidateRange(1, 480)]
    [int]$Minutes = 15,
    [switch]$StatsOnly,
    [switch]$RecoveryProbe,
    [switch]$UseSavedCredential
)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$client = Join-Path $projectRoot 'services\vico-bridge\bin\vico-cli.exe'
$cacheDirectory = Join-Path $projectRoot 'local\vicohome-lab-cache'
$resultDirectory = Join-Path $projectRoot 'local\lab-results'
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$probeName = if ($RecoveryProbe) { 'cg6-recovery' } else { 'cg6-endurance' }
$summaryPath = Join-Path $resultDirectory "$probeName-$stamp.json"
$videoPath = Join-Path $resultDirectory "$probeName-$stamp.h264"
$decodePath = Join-Path $resultDirectory "$probeName-$stamp.decode.log"
$plainPassword = $null
$passwordPointer = [IntPtr]::Zero

if (-not (Test-Path -LiteralPath $client)) {
    throw "Lab receiver was not found at $client"
}
if ($RecoveryProbe -and $StatsOnly) {
    throw 'The recovery probe requires file output for full decode.'
}
if ($RecoveryProbe) {
    $decoder = Get-Command ffmpeg -CommandType Application
}
New-Item -ItemType Directory -Force -Path $resultDirectory | Out-Null
New-Item -ItemType Directory -Force -Path $cacheDirectory | Out-Null

if ($UseSavedCredential) {
    $credentialPath = Join-Path $projectRoot 'local\private\vicohome-lab.credential.xml'
    $credential = Import-Clixml -LiteralPath $credentialPath
    if ($credential -isnot [pscredential]) {
        throw 'Expected a Windows-protected lab credential.'
    }
    $email = $credential.UserName
    $securePassword = $credential.Password
}
else {
    $email = Read-Host 'VicoHome lab account email'
    $securePassword = Read-Host 'VicoHome lab account password' -AsSecureString
}

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
    if ($RecoveryProbe) {
        $observe = '25s'
        $timeout = '45s'
    }
    $probeArgs = @('devices', 'media-probe', $selected.serialNumber,
        '--observe-for', $observe, '--timeout', $timeout,
        '--progress-every', '15s')
    if (-not $StatsOnly) {
        $probeArgs += @('--output', $videoPath)
    }
    if ($RecoveryProbe) {
        $probeArgs += @('--lab-drop-after', '4s')
    }
    Write-Host ("Observing {0} for {1}..." -f $selected.deviceName, $observe) -ForegroundColor Cyan
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
    if ($RecoveryProbe) {
        & $decoder.Source -nostdin -v error -xerror -err_detect explode -f h264 -i $videoPath -map 0:v:0 -f null - 2>$decodePath
        $result | Add-Member -NotePropertyName decodeExit -NotePropertyValue $LASTEXITCODE
        $result | Add-Member -NotePropertyName decodeErrorLines -NotePropertyValue @(Get-Content -LiteralPath $decodePath).Count
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
    if ($RecoveryProbe) {
        Write-Host ('PLI negotiated: {0}; injected drop, sent PLI, recovered IDR in ms: {1}, {2}, {3}' -f
            [bool]$result.pictureLossNegotiated, $result.labDropMillis, $result.labPliMillis, $result.labIdrMillis)
        Write-Host ('Last IDR before injection: {0} ms; prior IDR interval: {1} ms' -f
            $result.labPreviousIdrMillis, $result.labIdrIntervalMillis)
        Write-Host ('Full decode exit: {0}; error lines: {1}' -f $result.decodeExit, $result.decodeErrorLines)
    }
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
