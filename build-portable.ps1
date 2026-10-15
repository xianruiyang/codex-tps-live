[CmdletBinding()]
param(
    [string]$GoExe = 'go.exe'
)

$ErrorActionPreference = 'Stop'
$taskRepo = $PSScriptRoot
$taskDist = Join-Path $taskRepo 'dist'
$taskOutput = $taskDist
$taskProgram = Join-Path $taskOutput 'codex-tps-live.exe'
$taskGo = (Get-Command -Name $GoExe -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
New-Item -ItemType Directory -Path $taskOutput -Force | Out-Null

# Build one self-contained executable for baseline Windows x64 machines.
$taskTarget = @{ GOOS = 'windows'; GOARCH = 'amd64'; GOAMD64 = 'v1'; CGO_ENABLED = '0' }
$taskPrevious = @{}
Push-Location -LiteralPath $taskRepo
try {
    foreach ($taskName in $taskTarget.Keys) {
        $taskPrevious[$taskName] = [System.Environment]::GetEnvironmentVariable($taskName, 'Process')
        [System.Environment]::SetEnvironmentVariable($taskName, $taskTarget[$taskName], 'Process')
    }
    & $taskGo build -trimpath -buildvcs=false -o $taskProgram ./cmd/codex-tps-live
    if ($LASTEXITCODE -ne 0) { throw "Go build failed (exit $LASTEXITCODE)." }
}
finally {
    foreach ($taskName in $taskPrevious.Keys) {
        [System.Environment]::SetEnvironmentVariable($taskName, $taskPrevious[$taskName], 'Process')
    }
    Pop-Location
}

& (Join-Path $taskRepo 'scripts/package-portable.ps1')
