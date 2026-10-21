# Installs the latest morcore release on Windows.
#
#   irm https://raw.githubusercontent.com/rcondron/Morpheus-Core-Node/main/scripts/install.ps1 | iex
#
# Environment:
#   MORCORE_VERSION       release tag to install (default: latest)
#   MORCORE_INSTALL_DIR   target directory (default: %LOCALAPPDATA%\morcore\bin)
#   MORCORE_DOWNLOAD_BASE alternate download location (mirrors, testing)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'rcondron/Morpheus-Core-Node'
$version = if ($env:MORCORE_VERSION) { $env:MORCORE_VERSION } else { 'latest' }

if (-not [Environment]::Is64BitOperatingSystem) { throw '32-bit Windows is not supported.' }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { 'arm64' } else { 'amd64' }
$asset = "morcore_windows_$arch.zip"
$base = if ($env:MORCORE_DOWNLOAD_BASE) { $env:MORCORE_DOWNLOAD_BASE } elseif ($version -eq 'latest') { "https://github.com/$repo/releases/latest/download" } else { "https://github.com/$repo/releases/download/$version" }

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("morcore-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "==> Downloading $asset ($version)" -ForegroundColor Green
    Invoke-WebRequest "$base/$asset" -OutFile (Join-Path $tmp $asset) -UseBasicParsing
    Invoke-WebRequest "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt') -UseBasicParsing

    $line = Select-String -Path (Join-Path $tmp 'checksums.txt') -Pattern " $([Regex]::Escape($asset))$" | Select-Object -First 1
    if (-not $line) { throw "No checksum published for $asset" }
    $expected = ($line.Line -split '\s+')[0].ToLower()
    $actual = (Get-FileHash (Join-Path $tmp $asset) -Algorithm SHA256).Hash.ToLower()
    if ($expected -ne $actual) { throw "Checksum mismatch for $asset" }
    Write-Host '==> Checksum verified' -ForegroundColor Green

    Expand-Archive -Path (Join-Path $tmp $asset) -DestinationPath $tmp -Force
    $dir = if ($env:MORCORE_INSTALL_DIR) { $env:MORCORE_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'morcore\bin' }
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    Copy-Item (Join-Path $tmp "morcore_windows_$arch\morcore.exe") (Join-Path $dir 'morcore.exe') -Force

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not ($userPath -split ';' | Where-Object { $_ -eq $dir })) {
        [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ";$dir"), 'User')
        Write-Host "==> Added $dir to your PATH" -ForegroundColor Green
    }
    & (Join-Path $dir 'morcore.exe') version
    Write-Host "`nOpen a new terminal and run:  morcore`n"
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
