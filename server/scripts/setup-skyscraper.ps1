[CmdletBinding()]
param(
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]*$')]
    [string]$Distribution = 'Ubuntu-24.04'
)

$ErrorActionPreference = 'Stop'
if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) {
    throw 'WSL is required. In an elevated PowerShell run: wsl --install -d Ubuntu-24.04. Restart if requested, launch Ubuntu-24.04 to finish user setup, then rerun this script.'
}

$installer = Join-Path $PSScriptRoot 'setup-skyscraper.sh'
if (-not (Test-Path -LiteralPath $installer)) {
    throw "Linux installer not found: $installer"
}

& wsl.exe --distribution $Distribution --exec true
if ($LASTEXITCODE -ne 0) {
    throw "Cannot start WSL distribution '$Distribution'. Run wsl --install -d $Distribution and complete its first launch, then retry."
}

# A normalized temporary copy supports CRLF checkouts and avoids shell-string quoting.
$temporaryInstaller = [System.IO.Path]::GetTempFileName()
try {
    $script = [System.IO.File]::ReadAllText($installer).Replace("`r`n", "`n")
    [System.IO.File]::WriteAllText($temporaryInstaller, $script, (New-Object System.Text.UTF8Encoding($false)))
    $linuxInstaller = & wsl.exe --distribution $Distribution --exec wslpath -a -u $temporaryInstaller
    if ($LASTEXITCODE -ne 0) { throw 'Failed to translate the setup script path for WSL.' }
    $linuxInstaller = ($linuxInstaller -join "`n").Trim()
    if (-not $linuxInstaller.StartsWith('/')) { throw 'WSL returned an invalid script path.' }
    & wsl.exe --distribution $Distribution --exec bash $linuxInstaller
    if ($LASTEXITCODE -ne 0) { throw 'Skyscraper setup failed. See the WSL output above; fix the reported error and rerun.' }
}
finally {
    Remove-Item -LiteralPath $temporaryInstaller -Force
}

Write-Host ''
Write-Host 'Next: edit the private Linux config (never store credentials in this repository):'
Write-Host "  wsl -d $Distribution --cd ~ -- nano .local/share/romsync/skyscraper/config.ini"
Write-Host 'Uncomment userCreds under [screenscraper] and set your ScreenScraper account credentials.'
Write-Host ''
Write-Host 'Keep romsync native on Windows and add this flag to your usual serve/enrich command:'
Write-Host "  -skyscraper-wsl $Distribution"
Write-Host ''
Write-Host 'Example:'
Write-Host "  go run .\server\cmd\romsync-core serve -roms C:\roms -state .\data -web-listen 127.0.0.1:8080 -skyscraper-wsl $Distribution"
