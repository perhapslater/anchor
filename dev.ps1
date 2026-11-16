[CmdletBinding()]
param(
    [ValidateRange(1024, 65535)]
    [int]$Port = 18080,
    [switch]$Check,
    [switch]$BuildOnly,
    [switch]$Reset,
    [switch]$Tailscale,
    [switch]$SimulateTailscale,
    [ValidateRange(1, 65535)]
    [int]$TailscalePort = 8443
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$localDir = Join-Path $root '.local'
$configDir = Join-Path $localDir 'config'
$binaryDir = Join-Path $localDir 'bin'
$baseName = if ($SimulateTailscale) { 'anchor-dev-identities' } else { 'anchor-dev' }
$binaryName = if ($env:OS -eq 'Windows_NT') { "$baseName.exe" } else { $baseName }
$binary = Join-Path $binaryDir $binaryName
$serveProcess = $null

function Reset-DevelopmentState {
    # Only the launcher's own config directory may be archived. Never follow
    # a redirected .local/config or archive directory into another installation.
    $workspacePath = [System.IO.Path]::GetFullPath($root).TrimEnd([System.IO.Path]::DirectorySeparatorChar)
    $expectedConfig = [System.IO.Path]::GetFullPath((Join-Path $workspacePath '.local/config'))
    $archiveDir = [System.IO.Path]::GetFullPath((Join-Path $workspacePath '.local/config-backups'))
    $sourcePath = [System.IO.Path]::GetFullPath($configDir)
    if ($sourcePath -ne $expectedConfig) { throw 'Reset is restricted to this workspace''s .local/config directory.' }
    foreach ($path in @((Join-Path $workspacePath '.local'), $sourcePath, $archiveDir)) {
        if (Test-Path -LiteralPath $path) {
            $item = Get-Item -LiteralPath $path -Force
            if (-not $item.PSIsContainer -or ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
                throw "Reset refuses redirected or non-directory paths: $path"
            }
        }
    }
    if (-not (Test-Path -LiteralPath $sourcePath)) { return }
    # The database is shared by all launcher ports. Refuse a live database even
    # when this invocation selected a different port from the running server.
    $databasePath = Join-Path $sourcePath 'anchor.db'
    if (Test-Path -LiteralPath $databasePath) {
        $database = $null
        try {
            $database = [System.IO.File]::Open($databasePath, [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
        } catch {
            throw 'Cannot reset an open or inaccessible development database. Stop Anchor with Ctrl+C, then run .\dev.ps1 -Reset again.'
        } finally {
            if ($null -ne $database) { $database.Dispose() }
        }
    }
    $archivePath = [System.IO.Path]::GetFullPath((Join-Path $archiveDir ((Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [guid]::NewGuid().ToString('N'))))
    $workspacePrefix = $workspacePath + [System.IO.Path]::DirectorySeparatorChar
    foreach ($path in @($sourcePath, $archiveDir, $archivePath)) {
        if (-not $path.StartsWith($workspacePrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw 'Reset paths must stay inside this workspace.'
        }
    }
    New-Item -ItemType Directory -Path $archiveDir -Force | Out-Null
    Move-Item -LiteralPath $sourcePath -Destination $archivePath -ErrorAction Stop
    New-Item -ItemType Directory -Path $sourcePath | Out-Null
    Write-Host "Development setup reset. Previous configuration archived at $archivePath"
    Write-Host 'This private archive includes accounts and credentials. Keep it protected.'
}

function Test-ServePort($Config, [string]$ListenPort) {
    if ($null -eq $Config) { return $false }
    if ($null -ne $Config.TCP -and $null -ne $Config.TCP.PSObject.Properties[$ListenPort]) { return $true }
    if ($null -ne $Config.Foreground) {
        foreach ($entry in $Config.Foreground.PSObject.Properties) {
            if (Test-ServePort $entry.Value $ListenPort) { return $true }
        }
    }
    return $false
}
function Find-ServeProxy($Config, [string]$Authority) {
    if ($null -eq $Config) { return $null }
    if ($null -ne $Config.Web -and $null -ne $Config.Web.PSObject.Properties[$Authority]) {
        $site = $Config.Web.PSObject.Properties[$Authority].Value
        if ($null -ne $site.Handlers.PSObject.Properties['/']) { return $site.Handlers.PSObject.Properties['/'].Value.Proxy }
    }
    if ($null -ne $Config.Foreground) {
        foreach ($entry in $Config.Foreground.PSObject.Properties) {
            $proxy = Find-ServeProxy $entry.Value $Authority
            if ($proxy) { return $proxy }
        }
    }
    return $null
}
$previousEnvironment = @{}
Get-ChildItem Env: | Where-Object { $_.Name -like 'ANCHOR_*' -or $_.Name -eq 'GOTOOLCHAIN' } | ForEach-Object {
    $previousEnvironment[$_.Name] = $_.Value
}
Push-Location $root
try {
    if ($Reset -and $BuildOnly) { throw '-Reset starts a fresh setup session; use it without -BuildOnly.' }
    if ($SimulateTailscale -and $Tailscale) { throw 'Choose -SimulateTailscale for local test identities or -Tailscale for real Serve, not both.' }
    if ($SimulateTailscale -and $Reset) { throw '-SimulateTailscale uses existing people. Finish setup with .\dev.ps1 -Reset first.' }
    if ($Reset) {
        $running = Get-Process -Name 'anchor-dev' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $binary }
        if ($running) { throw 'Stop the development server with Ctrl+C before running .\dev.ps1 -Reset.' }
    }
    Get-ChildItem Env: | Where-Object { $_.Name -like 'ANCHOR_*' } | ForEach-Object {
        [Environment]::SetEnvironmentVariable($_.Name, $null, 'Process')
    }
    # Use the installed compiler during local testing, without a toolchain download.
    $env:GOTOOLCHAIN = 'local'
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Install Go before running this script.' }
    New-Item -ItemType Directory -Path $configDir, $binaryDir -Force | Out-Null
    Write-Host 'Building the current Go source and embedded web assets...'
    & go version
    if ($LASTEXITCODE -ne 0) { throw 'Could not read the Go compiler version.' }
    if ($Check) {
        & go vet ./...
        if ($LASTEXITCODE -ne 0) { throw 'Go vet failed.' }
        & go test ./...
        if ($LASTEXITCODE -ne 0) { throw 'Go tests failed.' }
        & go test -tags anchordev ./...
        if ($LASTEXITCODE -ne 0) { throw 'Development identity tests failed.' }
        if (-not (Get-Command node -ErrorAction SilentlyContinue)) { throw 'Install Node.js to run frontend checks.' }
        $testFiles = @(Get-ChildItem (Join-Path $root 'tests') -Filter '*.test.cjs' | ForEach-Object { $_.FullName })
        & node --test --test-timeout=5000 @testFiles
        if ($LASTEXITCODE -ne 0) { throw 'Frontend tests failed.' }
        & (Join-Path $root 'tests/dev-reset.tests.ps1')
    }
    if ($SimulateTailscale) { & go build -tags anchordev -o $binary . }
    else { & go build -o $binary . }
    if ($LASTEXITCODE -ne 0) { throw 'Build failed. The service was not started.' }
    if ($BuildOnly) { Write-Host "Build ready: $binary"; return }
    $probe = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, $Port)
    try { $probe.Start() } catch { throw "Port $Port is in use. Stop the old service or select another port using -Port." } finally { $probe.Stop() }
    $env:ANCHOR_CONFIG_DIR = $configDir
    $env:ANCHOR_LISTEN = "127.0.0.1:$Port"
    $env:ANCHOR_MODE = 'lan'
    $env:ANCHOR_ALLOW_HTTP = '1'
    $env:ANCHOR_PUBLIC_URL = "http://127.0.0.1:$Port"
    if ($SimulateTailscale) {
        $env:ANCHOR_MODE = 'tailscale'
        $env:ANCHOR_DEV_IDENTITIES = '1'
        Remove-Item Env:ANCHOR_ALLOW_HTTP, Env:ANCHOR_PUBLIC_URL -ErrorAction SilentlyContinue
        Write-Host "Open http://127.0.0.1:$Port/__dev/identities to choose an existing Anchor person."
        Write-Host 'Local identity simulation: no Tailscale installation or real Tailscale account is required.'
        Write-Host 'Enabled people with a saved Tailscale sign-in email can sign in. Any invented email can be used for testing.'
        Write-Host 'The browser banner lets you change test user. Changes still save to the local development database.'
    } elseif ($Tailscale) {
        $tailscaleCommand = Get-Command tailscale -ErrorAction SilentlyContinue
        if (-not $tailscaleCommand) { throw 'Install Tailscale and sign in before using -Tailscale.' }
        $statusJSON = & $tailscaleCommand.Source status --json
        if ($LASTEXITCODE -ne 0) { throw 'Cannot read Tailscale status. Check that it is running and that this terminal has access to the Tailscale service.' }
        $status = $statusJSON | ConvertFrom-Json
        if ($status.BackendState -ne 'Running' -or -not $status.Self.DNSName) { throw 'Connect Tailscale and enable MagicDNS before using -Tailscale.' }
        $serveJSON = & $tailscaleCommand.Source serve status --json
        if ($LASTEXITCODE -ne 0) { throw 'Could not read existing Tailscale Serve settings.' }
        if (Test-ServePort ($serveJSON | ConvertFrom-Json) ([string]$TailscalePort)) { throw "Tailscale Serve port $TailscalePort is already configured. Choose a different -TailscalePort; existing settings were not changed." }
        $dnsName = $status.Self.DNSName.TrimEnd('.')
        $authority = "${dnsName}:$TailscalePort"
        $target = "http://127.0.0.1:$Port"
        $runID = [guid]::NewGuid().ToString('N')
        $serveOut = Join-Path $localDir "serve-$runID.stdout.log"
        $serveErr = Join-Path $localDir "serve-$runID.stderr.log"
        # Foreground Serve belongs to this process's session, rather than
        # leaving a persistent --bg mapping behind after development ends.
        $serveProcess = Start-Process -FilePath $tailscaleCommand.Source -ArgumentList @('serve', '--yes', "--https=$TailscalePort", $target) -WindowStyle Hidden -PassThru -RedirectStandardOutput $serveOut -RedirectStandardError $serveErr
        $ready = $false
        for ($attempt = 0; $attempt -lt 40; $attempt++) {
            if ($serveProcess.HasExited) { break }
            $currentJSON = & $tailscaleCommand.Source serve status --json
            if ($LASTEXITCODE -eq 0 -and (Find-ServeProxy ($currentJSON | ConvertFrom-Json) $authority) -eq $target) { $ready = $true; break }
            Start-Sleep -Milliseconds 250
        }
        if (-not $ready) {
            Get-Content -LiteralPath $serveOut, $serveErr -ErrorAction SilentlyContinue | Write-Host
            throw 'Tailscale Serve did not become ready. If Tailscale printed an HTTPS activation link, open it and follow its instructions, then rerun this command.'
        }
        $env:ANCHOR_MODE = 'tailscale'
        Remove-Item Env:ANCHOR_ALLOW_HTTP -ErrorAction SilentlyContinue
        $env:ANCHOR_PUBLIC_URL = "https://$authority"
        Write-Host "Open $($env:ANCHOR_PUBLIC_URL) from a Tailscale-connected device."
        Write-Host 'The linked Tailscale sign-in email selects your Anchor account automatically. localhost does not exercise Tailscale sign-in.'
    } else {
        Write-Host "Open http://127.0.0.1:$Port"
        Write-Host 'Use .\dev.ps1 -SimulateTailscale to switch test identities, or -Tailscale to test real Serve.'
    }
    if ($Reset) { Reset-DevelopmentState }
    if (-not $SimulateTailscale) { Write-Host 'Real setup and local sign-in are enabled. State persists in .local/config/anchor.db.' }
    Write-Host 'The one-time setup code is printed below when first-run setup is required.'
    Write-Host 'To start over later, stop with Ctrl+C and run .\dev.ps1 -Reset.'
    Write-Host 'Press Ctrl+C to stop. Run this script again after edits to rebuild all assets.'
    & $binary
    if ($LASTEXITCODE -ne 0) { throw "Anchor exited with code $LASTEXITCODE." }
} finally {
    if ($null -ne $serveProcess -and -not $serveProcess.HasExited) {
        Stop-Process -Id $serveProcess.Id -ErrorAction SilentlyContinue
        $serveProcess.WaitForExit(5000) | Out-Null
    }
    Get-ChildItem Env: | Where-Object { $_.Name -like 'ANCHOR_*' -or $_.Name -eq 'GOTOOLCHAIN' } | ForEach-Object {
        [Environment]::SetEnvironmentVariable($_.Name, $null, 'Process')
    }
    foreach ($name in $previousEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name], 'Process')
    }
    Pop-Location
}
