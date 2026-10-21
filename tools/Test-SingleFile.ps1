param([string]$Exe = (Join-Path $PSScriptRoot '..\WorkBuddy2API.exe'))
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Net.Http
$sourceExe = (Resolve-Path -LiteralPath $Exe).Path
if (Get-Process -Name WorkBuddy2API -ErrorAction SilentlyContinue) {
    throw 'Close the existing WorkBuddy2API workbench before the isolated single-file test (single-instance mode).'
}
$projectRoot = Split-Path -Parent $sourceExe
$testDir = Join-Path $projectRoot ('.workbuddy\single-file-smoke-' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
$null = New-Item -ItemType Directory -Path $testDir
$testExe = Join-Path $testDir 'WorkBuddy2API.exe'
$profileDir = Join-Path $testDir 'profile'
Copy-Item -LiteralPath $sourceExe -Destination $testExe
$uiProcess = $null
$gatewayProcess = $null
$http = [System.Net.Http.HttpClient]::new()
$http.Timeout = [TimeSpan]::FromSeconds(2)
$result = [ordered]@{ singleFile = $false; extractedGatewayMatches = $false; healthStatus = $null; modelsStatus = $null; liveStatus = $null; idleActiveRequests = $null; testDirectory = $testDir }
try {
    # Initially only the single delivered exe is present; no credentials are copied.
    $uiProcess = Start-Process -FilePath $testExe -ArgumentList @('--minimized', '--data-dir', ('"' + $profileDir + '"')) -WorkingDirectory $testDir -WindowStyle Hidden -PassThru
    $gatewayExe = Join-Path $profileDir 'bin\wb2api.exe'
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        Start-Sleep -Milliseconds 500
        if ($uiProcess.HasExited) { throw 'Single-file GUI exited during startup.' }
        if (Test-Path -LiteralPath $gatewayExe) { break }
    }
    if (-not (Test-Path -LiteralPath $gatewayExe)) { throw 'Embedded gateway was not extracted.' }
    $result.singleFile = -not $uiProcess.HasExited
    $result.extractedGatewayMatches = (Get-FileHash -LiteralPath $gatewayExe).Hash -eq (Get-FileHash -LiteralPath (Join-Path $projectRoot 'wb2api.exe')).Hash
    if (-not $result.extractedGatewayMatches) { throw 'Extracted gateway differs from the embedded source.' }
    if (Test-Path -LiteralPath (Join-Path $testDir 'wb2api.exe')) { throw 'Standalone app must not extract files beside the executable.' }
    $shutdown = Start-Process -FilePath $testExe -ArgumentList @('--shutdown', '--data-dir', ('"' + $profileDir + '"')) -WindowStyle Hidden -PassThru
    if (-not $shutdown.WaitForExit(20000) -or $shutdown.ExitCode -ne 0) { throw 'Graceful hidden GUI shutdown failed.' }
    if (-not $uiProcess.WaitForExit(5000)) { throw 'Hidden GUI did not exit.' }
    $uiProcess = $null

    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $listener.Start()
    $port = $listener.LocalEndpoint.Port
    $listener.Stop()
    foreach ($name in @('auths', 'data', 'logs')) { $null = New-Item -ItemType Directory -Path (Join-Path $profileDir $name) -Force }
    $config = @{
        listen = "127.0.0.1:$port"; api_key = ''; auth_dir = './auths'; state_file = './data/state.json'
        schedule = @{ checkin_enabled = $false; travel_enabled = $false; activity_enabled = $false; keepalive_enabled = $false; school_enabled = $false; cat_enabled = $false }
        global = @{ enabled = $false }
        free_gateways = @{ opencode = @{ enabled = $false }; kilo = @{ enabled = $false } }
    }
    $configPath = Join-Path $profileDir 'config.json'
    # Windows PowerShell 5's -Encoding utf8 adds a BOM, which the Go gateway rejects.
    [System.IO.File]::WriteAllText($configPath, ($config | ConvertTo-Json -Depth 5), [System.Text.UTF8Encoding]::new($false))
    $gatewayProcess = Start-Process -FilePath $gatewayExe -ArgumentList @('-config', ('"' + $configPath + '"')) -WorkingDirectory $profileDir -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $profileDir 'logs\gateway.out.log') -RedirectStandardError (Join-Path $profileDir 'logs\gateway.err.log')
    for ($attempt = 0; $attempt -lt 20; $attempt++) {
        Start-Sleep -Milliseconds 500
        if ($gatewayProcess.HasExited) { throw 'Isolated gateway exited during startup.' }
        try {
            $response = $http.GetAsync("http://127.0.0.1:$port/healthz").GetAwaiter().GetResult()
            $result.healthStatus = [int]$response.StatusCode
            $response.Dispose()
            break
        } catch { if ($attempt -eq 19) { throw } }
    }
    $response = $http.GetAsync("http://127.0.0.1:$port/v1/models").GetAwaiter().GetResult()
    $result.modelsStatus = [int]$response.StatusCode
    $response.Dispose()
    if ($result.healthStatus -ne 503) { throw "Expected account-free health status 503, received $($result.healthStatus)." }
    if ($result.modelsStatus -ne 200) { throw "Model endpoint returned $($result.modelsStatus)." }
    $response = $http.GetAsync("http://127.0.0.1:$port/v1/usage/live").GetAwaiter().GetResult()
    $result.liveStatus = [int]$response.StatusCode
    $liveText = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
    $response.Dispose()
    if ($result.liveStatus -ne 200) { throw "Live usage endpoint returned $($result.liveStatus)." }
    $liveStats = $liveText | ConvertFrom-Json
    $result.idleActiveRequests = $liveStats.active_requests
    if ($liveStats.active_requests -ne 0 -or $liveStats.completed_tokens -ne 0) { throw 'Account-free isolated gateway must report zero live usage.' }
    $shutdown = Start-Process -FilePath $testExe -ArgumentList @('--shutdown', '--data-dir', ('"' + $profileDir + '"')) -WindowStyle Hidden -PassThru
    if (-not $shutdown.WaitForExit(15000) -or $shutdown.ExitCode -ne 0 -or -not $gatewayProcess.WaitForExit(5000)) { throw 'Installer shutdown did not stop its owned gateway.' }
    $result | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $testDir 'result.json') -Encoding utf8
    $result | ConvertTo-Json
} finally {
    $http.Dispose()
    foreach ($process in @($gatewayProcess, $uiProcess)) {
        if ($null -ne $process -and -not $process.HasExited) { Stop-Process -Id $process.Id -ErrorAction SilentlyContinue }
    }
}
