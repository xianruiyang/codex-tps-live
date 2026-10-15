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
# Offline CLI guides stay in the source tree; link to their published copies.
$taskReadme = $taskReadme.Replace('(README.md)', '(使用说明.md)').Replace('(README.en.md)', '(Usage.en.md)')
$taskReadme = $taskReadme.Replace('(docs/offline-cli.md)', '(https://github.com/xianruiyang/codex-tps-live/blob/main/docs/offline-cli.md)')
[System.IO.File]::WriteAllText((Join-Path $taskOutput '使用说明.md'), $taskReadme, $taskUtf8)
$taskEnglishReadme = [System.IO.File]::ReadAllText((Join-Path $taskRepo 'README.en.md'))
$taskEnglishReadme = $taskEnglishReadme.Replace('(README.md)', '(使用说明.md)').Replace('(README.en.md)', '(Usage.en.md)')
$taskEnglishReadme = $taskEnglishReadme.Replace('(docs/offline-cli.en.md)', '(https://github.com/xianruiyang/codex-tps-live/blob/main/docs/offline-cli.en.md)')
[System.IO.File]::WriteAllText((Join-Path $taskOutput 'Usage.en.md'), $taskEnglishReadme, $taskUtf8)
Copy-Item -LiteralPath (Join-Path $taskRepo 'LICENSE') -Destination (Join-Path $taskOutput 'LICENSE') -Force
Copy-Item -LiteralPath (Join-Path $taskRepo 'LICENSE.zh-CN.md') -Destination (Join-Path $taskOutput 'LICENSE.zh-CN.md') -Force

# Include only the current portable files, never local logs or historical builds.
$taskFiles = @(
    $taskProgram
    (Join-Path $taskOutput '启动监控.cmd')
    (Join-Path $taskOutput 'LICENSE')
    (Join-Path $taskOutput 'LICENSE.zh-CN.md')
    (Join-Path $taskOutput '使用说明.md')
    (Join-Path $taskOutput 'Usage.en.md')
)
Compress-Archive -LiteralPath $taskFiles -DestinationPath $taskArchive -Force
Write-Output "Portable directory: $taskOutput"
Write-Output "Portable ZIP: $taskArchive"
