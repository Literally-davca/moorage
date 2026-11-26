[CmdletBinding()]
param(
    [switch]$Ci,
    [switch]$Quick,
    [switch]$AllTests
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path

function Resolve-Tool([string]$name, [string[]]$fallbacks) {
    $cmd = Get-Command $name -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    foreach ($fb in $fallbacks) {
        if ($fb -and (Test-Path $fb)) { return $fb }
    }
    throw "Required tool '$name' was not found on PATH."
}

function Invoke-Native([string]$label, [scriptblock]$action) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & $action
        if ($LASTEXITCODE -ne 0) {
            throw "$label failed (exit code $LASTEXITCODE)"
        }
    }
    finally {
        $ErrorActionPreference = $prev
    }
}

try {
    $go = Resolve-Tool 'go' @(
        (Join-Path $env:LOCALAPPDATA 'Programs\go\bin\go.exe'),
        'C:\Program Files\Go\bin\go.exe'
    )
    $npm = Resolve-Tool 'npm.cmd' @()

    Push-Location $root
    try {
        Write-Output '==> [1/5] Checking for native <select> / <datalist> tags'
        $native = Get-ChildItem -Path (Join-Path $root 'web\src') -Filter '*.svelte' -Recurse |
            Select-String -CaseSensitive -Pattern '<select|<datalist'
        if ($native) {
            $native | ForEach-Object { Write-Output "  $($_.Path):$($_.LineNumber): $($_.Line.Trim())" }
            throw 'Native <select> or <datalist> tags found; use Select.svelte or MultiSelect.svelte.'
        }
        Write-Output '  OK (0 matches)'

        Write-Output '==> [1/5] Checking that {@html} is used only in web/src/lib/Icon.svelte'
        # Text from containers, logs and labels is always rendered as text; Icon.svelte
        # renders its own static SVGs. Any other {@html} could turn container output into markup.
        $iconPath = (Resolve-Path (Join-Path $root 'web\src\lib\Icon.svelte')).Path
        $html = Get-ChildItem -Path (Join-Path $root 'web\src') -Include '*.svelte', '*.ts', '*.js' -Recurse |
            Where-Object { $_.FullName -ne $iconPath } |
            Select-String -SimpleMatch -Pattern '{@html'
        if ($html) {
            $html | ForEach-Object { Write-Output "  $($_.Path):$($_.LineNumber): $($_.Line.Trim())" }
            throw '{@html} found outside web/src/lib/Icon.svelte; render text as text instead.'
        }
        Write-Output '  OK (Icon.svelte only)'

        Write-Output '==> [2/5] Checking and building web UI'
        Push-Location (Join-Path $root 'web')
        try {
            if ($Ci) {
                Invoke-Native 'npm ci' { & $npm ci --ignore-scripts --no-audit --no-fund }
            }
            Invoke-Native 'npm run check' { & $npm run check }
            Invoke-Native 'npm run build' { & $npm run build }
        }
        finally {
            Pop-Location
        }

        Write-Output '==> [3/5] Running go vet ./... (Windows & Linux build tags)'
        Invoke-Native 'go vet (windows)' { & $go vet ./... }
        $savedOs = $env:GOOS
        $savedArch = $env:GOARCH
        try {
            $env:GOOS = 'linux'
            $env:GOARCH = 'amd64'
            Invoke-Native 'go vet (linux/amd64)' { & $go vet ./... }
        }
        finally {
            if ($null -eq $savedOs) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $savedOs }
            if ($null -eq $savedArch) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $savedArch }
        }

        Write-Output '==> [4/5] Compiling all Go tests & running suite'
        Invoke-Native 'go test compile (all packages)' { & $go test '-run=^$' ./... }

        if ($AllTests) {
            Invoke-Native 'go test (all packages)' { & $go test '-count=1' ./... }
        }
        else {
            $linuxOnlyPkgs = @(
                'moorage/internal/api',
                'moorage/internal/engine',
                'moorage/internal/monitor',
                'moorage/internal/selfupdate',
                'moorage/internal/tunnel'
            )
            $allPkgs = & $go list ./...
            if ($LASTEXITCODE -ne 0) { throw "go list ./... failed ($LASTEXITCODE)" }
            $winPkgs = @($allPkgs | Where-Object { $linuxOnlyPkgs -notcontains $_ })
            Write-Output "  Running $($winPkgs.Count) Windows-compatible packages (pass -AllTests to force all; Linux-only POSIX suites run in full via check.sh / GitHub Actions CI)..."
            Invoke-Native 'go test (windows-compatible packages)' { & $go test '-count=1' @winPkgs }
        }

        if ($Quick) {
            Write-Output '==> [5/5] Cross-platform static builds skipped (-Quick)'
            return
        }

        Write-Output '==> [5/5] Building static binaries (linux/amd64, linux/arm64, linux/arm/v7, windows/amd64)'
        $tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("moorage-check-" + [System.Guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null
        $savedCgo = $env:CGO_ENABLED
        $savedOs = $env:GOOS
        $savedArch = $env:GOARCH
        $savedArm = $env:GOARM
        try {
            $targets = @(
                @{ goos = 'linux'; arch = 'amd64'; arm = ''; out = 'moorage-linux-amd64' },
                @{ goos = 'linux'; arch = 'arm64'; arm = ''; out = 'moorage-linux-arm64' },
                @{ goos = 'linux'; arch = 'arm'; arm = '7'; out = 'moorage-linux-armv7' },
                @{ goos = 'windows'; arch = 'amd64'; arm = ''; out = 'moorage-windows-amd64.exe' }
            )
            $env:CGO_ENABLED = '0'
            foreach ($t in $targets) {
                $env:GOOS = $t.goos
                $env:GOARCH = $t.arch
                if ($t.arm) { $env:GOARM = $t.arm } else { Remove-Item Env:GOARM -ErrorAction SilentlyContinue }
                Write-Output "  building $($t.out)..."
                $outFile = Join-Path $tmpDir $t.out
                Invoke-Native "go build $($t.out)" { & $go build -trimpath -ldflags '-s -w -X main.version=dev' -o $outFile ./cmd/moorage }
            }
        }
        finally {
            if ($null -eq $savedCgo) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $savedCgo }
            if ($null -eq $savedOs) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $savedOs }
            if ($null -eq $savedArch) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $savedArch }
            if ($null -eq $savedArm) { Remove-Item Env:GOARM -ErrorAction SilentlyContinue } else { $env:GOARM = $savedArm }
            Remove-Item -Path $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
        }
        Write-Output 'All checks passed.'
    }
    finally {
        Pop-Location
    }
}
catch {
    [Console]::Error.WriteLine($_.Exception.Message)
    exit 1
}
