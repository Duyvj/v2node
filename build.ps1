$ErrorActionPreference = "Stop"

Write-Host "======================================" -ForegroundColor Cyan
Write-Host " Building Resource-Optimized v2node" -ForegroundColor Cyan
Write-Host "======================================" -ForegroundColor Cyan

$env:CGO_ENABLED = "0"
$env:GOEXPERIMENT = "jsonv2"

$version = git describe --tags --always 2>$null
if (-not $version) {
    $version = "custom"
}

$ldflags = "-s -w -X 'github.com/wyx2685/v2node/cmd.Version=$version'"

& go build -ldflags "$ldflags" -trimpath -v -o v2node_optimized.exe .

if ($LASTEXITCODE -eq 0 -or (Test-Path "v2node_optimized.exe")) {
    $item = Get-Item "v2node_optimized.exe"
    $sizeMB = [math]::Round($item.Length / 1MB, 2)
    Write-Host "Build successful: v2node_optimized.exe ($sizeMB MB)" -ForegroundColor Green
} else {
    Write-Host "Build failed!" -ForegroundColor Red
}
