[CmdletBinding()]
param([switch]$Stop)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$runtime = Join-Path $root 'local/home-dashboard'
$web = Join-Path $runtime 'www'
$restreamer = Join-Path $runtime 'go2rtc.exe'
$state = Join-Path $runtime 'process.json'
$recorder = Join-Path $runtime 'mediamtx/mediamtx.exe'
$recorderState = Join-Path $runtime 'recorder.json'
$bridge = Join-Path $root 'services/vico-bridge/bin/vico-cli.exe'
$url = 'http://127.0.0.1:1984/'

function Get-DashboardProcess($stateFile, $executable) {
    if (Test-Path -LiteralPath $stateFile) {
        $saved = Get-Content -Raw -LiteralPath $stateFile | ConvertFrom-Json
        $process = Get-Process -Id $saved.id -ErrorAction SilentlyContinue
        if ($process -and $process.Path -eq $executable -and $process.StartTime.ToUniversalTime().Ticks -eq $saved.startTicks) {
            return $process
        }
    }
}

function Stop-DashboardProcess($stateFile, $executable) {
    $process = Get-DashboardProcess $stateFile $executable
    if ($process) {
        & taskkill.exe /PID $process.Id /T /F | Out-Null
        if ($LASTEXITCODE -ne 0 -and (Get-DashboardProcess $stateFile $executable)) { throw 'CCTV process could not be stopped.' }
    }
    Remove-Item -LiteralPath $stateFile -ErrorAction SilentlyContinue
}

$running = Get-DashboardProcess $state $restreamer
$recording = Get-DashboardProcess $recorderState $recorder
if ($Stop) {
    Stop-DashboardProcess $state $restreamer
    Stop-DashboardProcess $recorderState $recorder
    Write-Host 'Home CCTV dashboard and recording stopped.'
    return
}
if ($running -and $recording) {
    Write-Host "Home CCTV dashboard is running at $url"
    return
}
if ($running -or $recording) {
    throw 'Only part of the CCTV system is running. Use -Stop and then start again.'
}
if (Get-NetTCPConnection -LocalPort 1984,18554,9996 -State Listen -ErrorAction SilentlyContinue) {
    throw 'One of the local CCTV ports (1984, 18554, 9996) is already in use.'
}

New-Item -ItemType Directory -Path $web,(Join-Path $runtime 'recordings/home_camera') -Force | Out-Null

function Get-VerifiedAsset($uri, $destination, $sha256) {
    if (!(Test-Path -LiteralPath $destination)) {
        $partial = "$destination.download"
        try {
            Invoke-WebRequest -Uri $uri -OutFile $partial -TimeoutSec 90
            if ((Get-FileHash -LiteralPath $partial).Hash -ne $sha256) { throw "Checksum mismatch for $uri" }
            Move-Item -LiteralPath $partial -Destination $destination
        }
        finally {
            Remove-Item -LiteralPath $partial -ErrorAction SilentlyContinue
        }
    }
    elseif ((Get-FileHash -LiteralPath $destination).Hash -ne $sha256) {
        throw "Checksum mismatch for cached file $destination"
    }
}

if (!(Test-Path -LiteralPath $restreamer)) {
    if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') {
        throw 'The dashboard launcher currently requires x64 Windows.'
    }
    $archive = Join-Path $runtime 'go2rtc_win64.zip'
    Get-VerifiedAsset 'https://github.com/AlexxIT/go2rtc/releases/download/v1.9.15/go2rtc_win64.zip' $archive '5b448fc90f15c925406d8da0984301985e385050222c1c2bbdf0a76fe4b1e9cb'
    Expand-Archive -LiteralPath $archive -DestinationPath $runtime -Force
}
if (!(Test-Path -LiteralPath $recorder)) {
    $archive = Join-Path $runtime 'mediamtx_v1.21.3_windows_amd64.zip'
    Get-VerifiedAsset 'https://github.com/bluenviron/mediamtx/releases/download/v1.21.3/mediamtx_v1.21.3_windows_amd64.zip' $archive 'b2f5246b07cb72961df03b8d5e8cad43dc657bcf3f49a87bb031bdf22721af74'
    Expand-Archive -LiteralPath $archive -DestinationPath (Split-Path -Parent $recorder) -Force
}
Get-VerifiedAsset 'https://raw.githubusercontent.com/AlexxIT/go2rtc/v1.9.15/www/video-rtc.js' (Join-Path $web 'video-rtc.js') 'd48ce627baf7c341a92c0f5844a3c546431f9db873ff21489671aba2ecfe64fb'
foreach ($file in 'dashboard.css','profile.js','viewer.js','archive.js','index.html') {
    Copy-Item -LiteralPath (Join-Path $root "services/home-dashboard/$file") -Destination (Join-Path $web $file) -Force
}

if (!(Test-Path -LiteralPath $bridge)) {
    & (Join-Path $PSScriptRoot 'build-bridge.ps1')
}
$credential = Import-Clixml -LiteralPath (Join-Path $root 'local/private/vicohome-lab.credential.xml')
if ($credential -isnot [pscredential]) { throw 'The saved home-lab credential must be a Windows PSCredential.' }
$cache = Join-Path $runtime 'auth'
$environment = @{
    VICOHOME_EMAIL = $credential.UserName
    VICOHOME_PASSWORD = $credential.GetNetworkCredential().Password
    VICOHOME_CACHE_DIR = $cache
    VICOHOME_DEBUG = $null
}
try {
    $previous = @{}
    foreach ($name in $environment.Keys) {
        $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        [Environment]::SetEnvironmentVariable($name, $environment[$name], 'Process')
    }
    $inventory = & $bridge devices list --format json 2> (Join-Path $runtime 'inventory.error.log')
    if ($LASTEXITCODE -ne 0) { throw 'Could not list the home-lab camera. See the private inventory error log.' }
    $devices = @($inventory | ConvertFrom-Json)
    if ($devices.Count -ne 1 -or $devices[0].deviceName -ne 'test Camera' -or $devices[0].serialNumber -notmatch '^[A-Za-z0-9_-]+$') {
        throw 'Expected the single previously verified home test camera on the lab account.'
    }
    $config = Get-Content -Raw -LiteralPath (Join-Path $root 'infra/go2rtc.home.template.yaml')
    $config.Replace('__HOME_CAMERA__', $devices[0].serialNumber) | Set-Content -LiteralPath (Join-Path $runtime 'go2rtc.yaml') -Encoding utf8
    $process = Start-Process -FilePath $restreamer -ArgumentList '-config', 'local/home-dashboard/go2rtc.yaml' -WorkingDirectory $root `
        -WindowStyle Hidden -RedirectStandardOutput (Join-Path $runtime 'go2rtc.log') -RedirectStandardError (Join-Path $runtime 'go2rtc.error.log') -PassThru
}
finally {
    foreach ($name in $previous.Keys) {
        [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process')
    }
    $environment.Clear()
}

function Save-DashboardProcess($process, $stateFile) {
    @{ id = $process.Id; startTicks = $process.StartTime.ToUniversalTime().Ticks } | ConvertTo-Json | Set-Content -LiteralPath $stateFile -Encoding utf8
}

Save-DashboardProcess $process $state
try {
    $archiveProcess = Start-Process -FilePath $recorder -ArgumentList 'infra/mediamtx.home.yaml' -WorkingDirectory $root `
        -WindowStyle Hidden -RedirectStandardOutput (Join-Path $runtime 'recorder.log') -RedirectStandardError (Join-Path $runtime 'recorder.error.log') -PassThru
    Save-DashboardProcess $archiveProcess $recorderState
    for ($attempt = 0; $attempt -lt 20; $attempt++) {
        if ($process.HasExited -or $archiveProcess.HasExited) { break }
        try {
            $page = Invoke-WebRequest -Uri $url -TimeoutSec 1
            $history = Invoke-WebRequest -Uri 'http://127.0.0.1:9996/list?path=home_camera' -TimeoutSec 1 -SkipHttpErrorCheck
            if ($page.Content -match '<title>UZY CCTV</title>' -and $history.StatusCode -in 200,404) {
                Write-Host "Home CCTV dashboard is running at $url. Recording starts when the camera connects."
                return
            }
        }
        catch { Start-Sleep -Milliseconds 250 }
    }
    throw 'The CCTV system did not start. See the private go2rtc and recorder logs under local/home-dashboard.'
}
catch {
    & $PSCommandPath -Stop
    throw
}
