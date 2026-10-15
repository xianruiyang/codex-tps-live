[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$taskRepo = [System.IO.Directory]::GetParent($PSScriptRoot).FullName
$taskOutput = Join-Path $taskRepo 'dist'
$taskProgram = Join-Path $taskOutput 'codex-tps-live.exe'
$taskArchive = Join-Path $taskRepo 'codex-tps-live-windows-amd64.zip'
$taskUtf8 = [System.Text.UTF8Encoding]::new($false)
New-Item -ItemType Directory -Path $taskOutput -Force | Out-Null

$taskLauncher = [string]::Join("`r`n", @(
    '@echo off'
    '"%~dp0codex-tps-live.exe" --open %*'
    'if errorlevel 1 pause'
    ''
))
[System.IO.File]::WriteAllText((Join-Path $taskOutput '启动监控.cmd'), $taskLauncher, $taskUtf8)
$taskReadme = [System.IO.File]::ReadAllText((Join-Path $taskRepo 'README.md'))
# The four-file bundle does not include the source tree's offline CLI document.
$taskReadme = $taskReadme.Replace('(docs/offline-cli.md)', '(https://github.com/adenta/codex-tps/blob/v0.2.0/README.md)')
[System.IO.File]::WriteAllText((Join-Path $taskOutput '使用说明.md'), $taskReadme, $taskUtf8)
Copy-Item -LiteralPath (Join-Path $taskRepo 'LICENSE') -Destination (Join-Path $taskOutput 'LICENSE') -Force

# Include only the current portable files, never local logs or historical builds.
$taskFiles = @(
    $taskProgram
    (Join-Path $taskOutput '启动监控.cmd')
    (Join-Path $taskOutput 'LICENSE')
    (Join-Path $taskOutput '使用说明.md')
)
Compress-Archive -LiteralPath $taskFiles -DestinationPath $taskArchive -Force
Write-Output "Portable directory: $taskOutput"
Write-Output "Portable ZIP: $taskArchive"
