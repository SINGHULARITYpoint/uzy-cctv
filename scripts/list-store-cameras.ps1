[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$client = Join-Path $projectRoot 'services\vico-bridge\bin\vico-cli.exe'
$cacheDirectory = Join-Path $projectRoot 'local\vicohome-store-cache'
$resultDirectory = Join-Path $projectRoot 'local\private'
$resultPath = Join-Path $resultDirectory 'store-device-list.json'
$plainPassword = $null
$passwordPointer = [IntPtr]::Zero

if (-not (Test-Path -LiteralPath $client)) {
    throw "Bridge binary was not found at $client. Run scripts/build-bridge.ps1 first."
}

New-Item -ItemType Directory -Force -Path $cacheDirectory | Out-Null
New-Item -ItemType Directory -Force -Path $resultDirectory | Out-Null

$email = Read-Host 'Dedicated VicoHome bridge account email'
$securePassword = Read-Host 'Dedicated VicoHome bridge account password' -AsSecureString

try {
    $passwordPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($securePassword)
    $plainPassword = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($passwordPointer)
    $env:VICOHOME_EMAIL = $email
    $env:VICOHOME_PASSWORD = $plainPassword
    $env:VICOHOME_CACHE_DIR = $cacheDirectory
    Remove-Item Env:VICOHOME_DEBUG -ErrorAction SilentlyContinue

    $raw = & $client devices list --format json
    if ($LASTEXITCODE -ne 0) {
        throw 'The bridge command failed while listing cameras.'
    }
    $devices = @($raw | ConvertFrom-Json)
    if ($devices.Count -eq 0) {
        throw 'The dedicated bridge account can authenticate but has no visible cameras.'
    }

    $devices | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $resultPath -Encoding utf8
    Write-Host ("Dedicated bridge account can see {0} camera(s):" -f $devices.Count) -ForegroundColor Green
    foreach ($device in $devices) {
        Write-Host ("- {0} ({1}), battery {2}%, charging flag {3}" -f
            $device.deviceName, $device.modelNo, $device.batteryLevel, $device.isCharging)
    }
    Write-Host "Private inventory: $resultPath"
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
