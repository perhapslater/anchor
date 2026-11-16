$ErrorActionPreference = 'Stop'

# Load only the actual reset function, never launch or reset the developer's
# installation. Every mutation below is confined to a new temporary fixture.
$launcherPath = Join-Path (Split-Path $PSScriptRoot -Parent) 'dev.ps1'
$parseErrors = $null
$tokens = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($launcherPath, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw "Launcher has syntax errors: $parseErrors" }
$resetFunction = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Reset-DevelopmentState' }, $true)
if ($null -eq $resetFunction) { throw 'Reset function not found.' }
Invoke-Expression $resetFunction.Extent.Text

function Assert-True($Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}
function Assert-Throws($Action, [string]$Expected) {
    $caught = $null
    try { & $Action } catch { $caught = $_.Exception.Message }
    Assert-True ($null -ne $caught -and $caught.Contains($Expected)) "Expected failure containing '$Expected', got '$caught'."
}

$fixture = Join-Path ([System.IO.Path]::GetTempPath()) ('anchor-reset-test-' + [guid]::NewGuid().ToString('N'))
$fixture = [System.IO.Path]::GetFullPath($fixture)
$root = Join-Path $fixture 'workspace'
$localDir = Join-Path $root '.local'
$configDir = Join-Path $localDir 'config'
$outside = Join-Path $fixture 'other-installation'
$junction = Join-Path $localDir 'config-backups'
try {
    New-Item -ItemType Directory -Path $configDir, $outside -Force | Out-Null
    Assert-Throws { & $launcherPath -Reset -BuildOnly } 'without -BuildOnly'
    Assert-Throws { & $launcherPath -SimulateTailscale -Tailscale -BuildOnly } 'not both'
    Assert-Throws { & $launcherPath -SimulateTailscale -Reset } 'uses existing people'
    Set-Content -LiteralPath (Join-Path $root 'source.txt') -Value 'source stays unchanged'
    Set-Content -LiteralPath (Join-Path $outside 'sentinel.txt') -Value 'other installation stays unchanged'
    Set-Content -LiteralPath (Join-Path $configDir 'anchor.db') -Value 'synthetic database'
    Set-Content -LiteralPath (Join-Path $configDir 'setup-token') -Value 'synthetic setup token'
    New-Item -ItemType Directory -Path (Join-Path $configDir 'icons') | Out-Null
    Set-Content -LiteralPath (Join-Path $configDir 'icons/custom.svg') -Value 'synthetic custom artwork'

    # A process using this database must prevent a reset, even on another port.
    $openDatabase = [System.IO.File]::Open((Join-Path $configDir 'anchor.db'), [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::ReadWrite)
    try { Assert-Throws { Reset-DevelopmentState } 'Stop Anchor with Ctrl+C' }
    finally { $openDatabase.Dispose() }
    Assert-True (Test-Path -LiteralPath (Join-Path $configDir 'anchor.db')) 'Locked database was moved.'

    $configDir = $outside
    Assert-Throws { Reset-DevelopmentState } 'restricted to this workspace'
    $configDir = Join-Path $localDir 'config'

    # Reject an archive junction instead of moving credentials outside the workspace.
    $junctionCreated = $false
    try {
        New-Item -ItemType Junction -Path $junction -Target $outside | Out-Null
        $junctionCreated = $true
    } catch { Write-Host 'Junction check skipped: junction creation unavailable.' }
    if ($junctionCreated) {
        Assert-Throws { Reset-DevelopmentState } 'redirected or non-directory paths'
        # Remove only the junction itself, using its exact verified fixture path.
        [System.IO.Directory]::Delete($junction)
    }

    Reset-DevelopmentState
    Assert-True ((Get-ChildItem -LiteralPath $configDir -Force).Count -eq 0) 'Reset configuration was not empty.'
    $archives = @(Get-ChildItem -LiteralPath $junction -Directory)
    Assert-True ($archives.Count -eq 1) 'Expected one recoverable archive.'
    Assert-True ((Get-Content -LiteralPath (Join-Path $archives[0].FullName 'anchor.db') -Raw).Trim() -eq 'synthetic database') 'Database archive changed.'
    Assert-True (Test-Path -LiteralPath (Join-Path $archives[0].FullName 'setup-token')) 'Setup token was not archived.'
    Assert-True (Test-Path -LiteralPath (Join-Path $archives[0].FullName 'icons/custom.svg')) 'Custom icons were not archived.'
    Assert-True ((Get-Content -LiteralPath (Join-Path $root 'source.txt') -Raw).Trim() -eq 'source stays unchanged') 'Source was changed.'
    Assert-True ((Get-Content -LiteralPath (Join-Path $outside 'sentinel.txt') -Raw).Trim() -eq 'other installation stays unchanged') 'Other installation was changed.'

    Reset-DevelopmentState
    Assert-True (@(Get-ChildItem -LiteralPath $junction -Directory).Count -eq 2) 'Repeated resets did not keep separate archives.'
    Write-Host 'Development reset checks passed: syntax/options, live database refusal, path boundaries, complete archives, clean configuration and repeat resets.'
    if ($junctionCreated) { Write-Host 'Archive junction refusal also passed.' }
} finally {
    $tempPrefix = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath()).TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
    if (-not $fixture.StartsWith($tempPrefix, [System.StringComparison]::OrdinalIgnoreCase) -or -not ([System.IO.Path]::GetFileName($fixture)).StartsWith('anchor-reset-test-')) {
        throw 'Refusing cleanup outside the owned temporary fixture.'
    }
    if ((Test-Path -LiteralPath $junction) -and ((Get-Item -LiteralPath $junction -Force).Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
        [System.IO.Directory]::Delete($junction)
    }
    Remove-Item -LiteralPath $fixture -Recurse -Force
}
