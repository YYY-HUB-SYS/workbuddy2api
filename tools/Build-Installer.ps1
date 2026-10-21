param(
    [string]$Makensis,
    [string]$Go,
    [switch]$SkipBuild,
    [switch]$TestInstall
)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$cargo = Get-Content -LiteralPath (Join-Path $projectRoot 'rust\Cargo.toml') -Raw
$version = [regex]::Match($cargo, '(?m)^version = "([0-9]+\.[0-9]+\.[0-9]+)"').Groups[1].Value
if (-not $version) { throw 'Cannot read application version.' }
if (-not $Makensis) {
    $command = Get-Command makensis.exe -ErrorAction SilentlyContinue
    if ($command) { $Makensis = $command.Source }
    else {
        $found = Get-ChildItem -LiteralPath (Join-Path $projectRoot '.workbuddy\installer-tools') -Filter makensis.exe -Recurse -ErrorAction SilentlyContinue |
            Where-Object { $_.Directory.Name -match '^nsis-' } | Sort-Object FullName -Descending | Select-Object -First 1
        if ($found) { $Makensis = $found.FullName }
    }
}
if (-not $Makensis -or -not (Test-Path -LiteralPath $Makensis)) { throw 'NSIS 3 Unicode compiler not found. Supply -Makensis with its full path.' }
if (-not $SkipBuild) {
    if (-not $Go) {
        $goCommand = Get-Command go.exe -ErrorAction SilentlyContinue
        if ($goCommand) { $Go = $goCommand.Source }
        else { $Go = Join-Path $projectRoot '.workbuddy\hud-dev\go\bin\go.exe' }
    }
    if (-not (Test-Path -LiteralPath $Go)) { throw 'Go toolchain not found. Supply -Go with its full path.' }
    Push-Location (Join-Path $projectRoot 'gateway')
    try {
        # The portable source archive omits legacy scheduler Python activity scripts.
        # Check every gateway package changed by this release and its upstream path.
        & $Go test ./cmd/server ./internal/freegateway ./internal/server ./internal/upstream
        if ($LASTEXITCODE -ne 0) { throw 'Gateway tests failed.' }
        & $Go build -trimpath -buildvcs=false -ldflags '-s -w' -o (Join-Path $projectRoot 'wb2api.exe') ./cmd/server
        if ($LASTEXITCODE -ne 0) { throw 'Gateway build failed.' }
    } finally { Pop-Location }
    Push-Location (Join-Path $projectRoot 'rust')
    try {
        & cargo test --locked
        if ($LASTEXITCODE -ne 0) { throw 'Rust tests failed.' }
        & cargo build --release --locked
        if ($LASTEXITCODE -ne 0) { throw 'Release build failed.' }
    } finally { Pop-Location }
}
$releaseExe = Join-Path $projectRoot 'rust\target\release\WorkBuddy2API.exe'
if ((Get-Item -LiteralPath $releaseExe).VersionInfo.ProductVersion -ne "$version.0") { throw 'Release executable version does not match Cargo.toml.' }
$outputDir = if ($TestInstall) { Join-Path $projectRoot '.workbuddy\installer-test' } else { Join-Path $projectRoot 'release\windows' }
New-Item -ItemType Directory -Path $outputDir -Force | Out-Null
$suffix = if ($TestInstall) { '-Test' } else { '' }
$output = Join-Path $outputDir "WorkBuddy2API-$version$suffix-Setup.exe"
$compilerArgs = @('/V2', '/WX', '/INPUTCHARSET', 'UTF8', "/DAPP_VERSION=$version", "/DPROJECT_ROOT=$projectRoot", "/DOUTPUT_PATH=$output")
if ($TestInstall) { $compilerArgs += '/DTEST_INSTALL' }
& $Makensis @compilerArgs (Join-Path $projectRoot 'installer\WorkBuddy2API.nsi')
if ($LASTEXITCODE -ne 0) { throw 'NSIS build failed.' }
$hash = (Get-FileHash -LiteralPath $output -Algorithm SHA256).Hash.ToLowerInvariant()
[IO.File]::WriteAllText("$output.sha256", "$hash  $([IO.Path]::GetFileName($output))`r`n", [Text.UTF8Encoding]::new($false))
Get-Item -LiteralPath $output | Select-Object FullName, Length
