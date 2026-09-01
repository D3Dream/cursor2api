# Run cursor2api on Windows
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$Binary = Join-Path $Root "cursor2api.exe"
$Config = Join-Path $Root "config.json"
$Example = Join-Path $Root "config.example.json"

if (-not (Test-Path $Config)) {
  Copy-Item $Example $Config
  Write-Host "已创建 $Config"
}

# 总是编译（增量快）：只在缺失时编译会让"代码已更新但跑旧二进制"成为隐蔽故障。
# 注意 $ErrorActionPreference 不约束 native 命令——go build 失败必须手动检查退出码，
# 否则会继续用旧二进制（或报"文件不存在"）启动。
Write-Host "正在编译 cursor2api..."
Push-Location $Root
try {
  go build -o cursor2api.exe ./src
  if ($LASTEXITCODE -ne 0) { throw "go build failed (exit $LASTEXITCODE)" }
} finally {
  Pop-Location
}

# schema 路径是相对于仓库根的（main.go 默认 schema/cursor_fds.json），必须切到 ROOT
Set-Location $Root
& $Binary $Config
