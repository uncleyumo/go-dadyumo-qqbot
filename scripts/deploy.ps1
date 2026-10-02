# 日常更新（PowerShell 版）：交叉编译 → 上传二进制 → 重启 systemd。
# 用法： .\scripts\deploy.ps1 -HostAlias your-server
#
# 注意：只替换二进制，绝不覆盖服务器上的 config.json。
param(
    [string]$HostAlias = "your-server",
    [string]$RemoteDir = "/opt/dadyumo-qqbot",
    [string]$Svc = "dadyumo-qqbot"
)

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

Write-Host "==> 编译"
$env:CGO_ENABLED = "0"; $env:GOOS = "linux"; $env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w" -o dist/dadyumo-qqbot ./cmd/dadyumo-qqbot
if ($LASTEXITCODE -ne 0) { throw "编译失败" }

$who = (ssh $HostAlias "whoami").Trim()
$sudo = if ($who -eq "root") { "" } else { "sudo " }

Write-Host "==> 上传"
scp dist/dadyumo-qqbot "$HostAlias`:$RemoteDir/dadyumo-qqbot.new"
if ($LASTEXITCODE -ne 0) { throw "上传失败" }

# 远端命令用 ; 分隔：&& 在 Windows PowerShell 5.1 里不是合法的语句分隔符
Write-Host "==> 切换并重启"
$switchCmd = "$sudo mv -f $RemoteDir/dadyumo-qqbot.new $RemoteDir/dadyumo-qqbot"
$chmodCmd = "$sudo chmod +x $RemoteDir/dadyumo-qqbot"
$restartCmd = "$sudo systemctl restart $Svc"
ssh $HostAlias "$switchCmd ; $chmodCmd ; $restartCmd"
if ($LASTEXITCODE -ne 0) { throw "重启失败" }

Start-Sleep -Seconds 3
$statusCmd = "$sudo systemctl is-active $Svc"
$logsCmd = "$sudo journalctl -u $Svc -n 20 --no-pager"
ssh $HostAlias "$statusCmd ; $logsCmd"
