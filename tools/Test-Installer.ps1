param([string]$Setup = (Join-Path $PSScriptRoot '..\.workbuddy\installer-test\WorkBuddy2API-0.1.6-Test-Setup.exe'))
$ErrorActionPreference = 'Stop'
$setupPath = (Resolve-Path -LiteralPath $Setup).Path
if (Get-Process -Name WorkBuddy2API -ErrorAction SilentlyContinue) { throw 'Close existing GUI before isolated installer acceptance.' }
$installDir = Join-Path $env:LOCALAPPDATA 'Programs\WorkBuddy2API-InstallerTest'
$profileDir = Join-Path $env:LOCALAPPDATA 'WorkBuddy2API-InstallerTest'
$shortcutPath = Join-Path ([Environment]::GetFolderPath('Desktop')) 'WorkBuddy2API Installer Test.lnk'
$registryPath = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\WorkBuddy2API-InstallerTest'
if ((Test-Path -LiteralPath $installDir) -or (Test-Path -LiteralPath $profileDir)) { throw 'Isolated test directories already exist; preserve and inspect before rerunning.' }
$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$listener.Start(); $port = $listener.LocalEndpoint.Port; $listener.Stop()
foreach ($name in @('auths','data','logs')) { New-Item -ItemType Directory -Path (Join-Path $profileDir $name) -Force | Out-Null }
$utf8 = [Text.UTF8Encoding]::new($false)
$config = @{
    listen = "127.0.0.1:$port"; api_key = ''; auth_dir = './auths'; state_file = './data/state.json'
    schedule = @{ checkin_enabled=$false; travel_enabled=$false; activity_enabled=$false; keepalive_enabled=$false; school_enabled=$false; cat_enabled=$false }
    global = @{enabled=$false}
    free_gateways = @{ opencode = @{ enabled = $false }; kilo = @{ enabled = $false } }
}
[IO.File]::WriteAllText((Join-Path $profileDir 'config.json'), ($config | ConvertTo-Json -Depth 5), $utf8)
[IO.File]::WriteAllText((Join-Path $profileDir 'auths\preserve.txt'), 'synthetic-account-directory-sentinel', $utf8)
[IO.File]::WriteAllText((Join-Path $profileDir 'logs\preserve.txt'), 'synthetic-log-sentinel', $utf8)
$expected = @{}
foreach ($file in Get-ChildItem -LiteralPath $profileDir -File -Recurse) { $expected[$file.FullName] = (Get-FileHash -LiteralPath $file.FullName).Hash }
$uiProcess = $null; $gatewayProcess = $null
function Invoke-Setup {
    $process = Start-Process -FilePath $setupPath -ArgumentList '/S' -WindowStyle Hidden -PassThru
    if (-not $process.WaitForExit(30000) -or $process.ExitCode -ne 0) { throw 'Silent test installation failed.' }
}
function Start-Fixture {
    $script:gatewayProcess = Start-Process -FilePath (Join-Path $installDir 'wb2api.exe') -ArgumentList @('-config', ('"'+(Join-Path $profileDir 'config.json')+'"')) -WorkingDirectory $profileDir -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $profileDir 'logs\gateway.out.log') -RedirectStandardError (Join-Path $profileDir 'logs\gateway.err.log')
    [IO.File]::WriteAllText((Join-Path $profileDir 'data\panel_state.json'), (@{pid=$script:gatewayProcess.Id;port=$port} | ConvertTo-Json), $utf8)
    $script:uiProcess = Start-Process -FilePath (Join-Path $installDir 'WorkBuddy2API.exe') -ArgumentList @('--minimized','--data-dir',('"'+$profileDir+'"')) -WorkingDirectory $installDir -WindowStyle Hidden -PassThru
    Start-Sleep -Seconds 2
    if ($script:uiProcess.HasExited -or $script:gatewayProcess.HasExited) { throw 'Installed fixture failed to start.' }
}
try {
    Invoke-Setup
    if (-not (Test-Path -LiteralPath $shortcutPath) -or -not (Test-Path -LiteralPath $registryPath)) { throw 'Installer registration or desktop shortcut missing.' }
    $payload = @(Get-ChildItem -LiteralPath $installDir -Name | Sort-Object)
    $publicFiles = @('LICENSE','NOTICE.md','Uninstall.exe','wb2api.exe','WorkBuddy2API.exe','WorkBuddy2API.ico') | Sort-Object
    if (Compare-Object $payload $publicFiles) { throw 'Unexpected installer payload, possibly runtime/private files.' }
    [IO.File]::WriteAllText((Join-Path $installDir 'user-created.txt'), 'must-survive-uninstall', $utf8)
    Start-Fixture
    Invoke-Setup
    if (-not $uiProcess.WaitForExit(5000) -or -not $gatewayProcess.WaitForExit(5000)) { throw 'Upgrade left old processes running.' }
    foreach ($path in $expected.Keys) { if ((Get-FileHash -LiteralPath $path).Hash -ne $expected[$path]) { throw 'Upgrade changed retained data.' } }
    Start-Fixture
    $uninstall = Start-Process -FilePath (Join-Path $installDir 'Uninstall.exe') -ArgumentList '/S' -WindowStyle Hidden -PassThru
    $null = $uninstall.WaitForExit(30000)
    for ($attempt=0; $attempt -lt 60 -and (Test-Path -LiteralPath (Join-Path $installDir 'WorkBuddy2API.exe')); $attempt++) { Start-Sleep -Milliseconds 500 }
    if ((Test-Path -LiteralPath (Join-Path $installDir 'WorkBuddy2API.exe')) -or (Test-Path -LiteralPath $registryPath) -or (Test-Path -LiteralPath $shortcutPath)) { throw 'Uninstaller did not remove binaries/registration/shortcut.' }
    if (-not $uiProcess.WaitForExit(5000) -or -not $gatewayProcess.WaitForExit(5000)) { throw 'Uninstall left app processes running.' }
    foreach ($path in $expected.Keys) { if ((Get-FileHash -LiteralPath $path).Hash -ne $expected[$path]) { throw 'Uninstall changed retained data.' } }
    if (-not (Test-Path -LiteralPath (Join-Path $installDir 'user-created.txt'))) { throw 'Uninstaller removed an unrelated user-created file.' }
    $evidenceDir = Join-Path (Split-Path -Parent $setupPath) ('acceptance-'+(Get-Date -Format 'yyyyMMdd-HHmmss'))
    New-Item -ItemType Directory -Path $evidenceDir | Out-Null
    # Only the two fixed test directories are eligible for moving/removing.
    if ([IO.Path]::GetFullPath($profileDir) -ne (Join-Path $env:LOCALAPPDATA 'WorkBuddy2API-InstallerTest')) { throw 'Unexpected test data path.' }
    if ([IO.Path]::GetFullPath($installDir) -ne (Join-Path $env:LOCALAPPDATA 'Programs\WorkBuddy2API-InstallerTest')) { throw 'Unexpected test install path.' }
    Move-Item -LiteralPath $profileDir -Destination (Join-Path $evidenceDir 'retained-profile')
    Move-Item -LiteralPath (Join-Path $installDir 'user-created.txt') -Destination (Join-Path $evidenceDir 'retained-user-file.txt')
    Remove-Item -LiteralPath $installDir # Empty only; never recursive.
    $result = [ordered]@{install=$true;upgrade=$true;uninstall=$true;privateDataPreserved=$true;unrelatedFilePreserved=$true;processesStopped=$true;payload=$payload;evidenceDirectory=$evidenceDir}
    $result | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $evidenceDir 'result.json') -Encoding utf8
    $result | ConvertTo-Json
} finally {
    foreach ($process in @($uiProcess,$gatewayProcess)) { if ($process -and -not $process.HasExited) { Stop-Process -Id $process.Id -ErrorAction SilentlyContinue } }
}
