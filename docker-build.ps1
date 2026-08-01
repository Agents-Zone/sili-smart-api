<#
.SYNOPSIS
    打包 sili-smart-trace 镜像，并产出离线交付包（自包含，无需 .env）。
.DESCRIPTION
    版本号规则：当日日期（sili-YYYYMMDD）。
      - 临时写入 VERSION，供 Dockerfile 经 ldflags 注入二进制的 common.Version；
      - 用同一值打 image tag，保证「二进制内版本」与「镜像 tag」一致；
      - 把 TAG 同步到 .env，供根目录模板 compose 本地开发引用；
      - 脚本退出时还原 VERSION 为空，保持 git 工作区干净。

    离线交付产物（位于 release/）：
      - sili-smart-trace-YYYYMMDD.tar   自建镜像（load 到服务器）
      - docker-compose-clickhouse.yml   image tag 已固化的交付 compose（load 后直接 up）

    同一天多次构建会覆盖同一 tag；如需区分同日多次构建，把 yyyyMMdd 改成 yyyyMMdd-HHmm。
.NOTES
    运行（PowerShell 7 推荐）：pwsh .\docker-build.ps1
    Windows PowerShell 5.1：.\docker-build.ps1
    若遇执行策略限制：pwsh -ExecutionPolicy Bypass -File .\docker-build.ps1
#>

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$tag = 'sili-' + (Get-Date -Format 'yyyyMMdd')
$tagSuffix = $tag.Substring('sili-'.Length)   # 20260801，用于 tar 命名
$releaseDir = 'release'
$utf8NoBom = New-Object System.Text.UTF8Encoding $false

try {
    Write-Host ">> 写入 VERSION=$tag"
    [System.IO.File]::WriteAllText("$PSScriptRoot\VERSION", $tag, $utf8NoBom)

    Write-Host ">> 构建镜像 sili-smart-trace:$tag"
    docker build -t "sili-smart-trace:$tag" .
    if ($LASTEXITCODE -ne 0) { throw "docker build 失败（退出码 $LASTEXITCODE）" }

    Write-Host ">> 同步 TAG 到 .env（供根目录模板 compose 本地开发引用）"
    [System.IO.File]::WriteAllText("$PSScriptRoot\.env", "TAG=$tag`n", $utf8NoBom)

    Write-Host ">> 产出离线交付包到 $releaseDir/"
    New-Item -ItemType Directory -Force -Path $releaseDir | Out-Null

    $tarName = "sili-smart-trace-$tagSuffix.tar"
    Write-Host "  - 导出镜像 $tarName"
    docker save "sili-smart-trace:$tag" -o (Join-Path $releaseDir $tarName)
    if ($LASTEXITCODE -ne 0) { throw "docker save 失败（退出码 $LASTEXITCODE）" }

    Write-Host "  - 固化 compose（image tag 写死为 $tag）"
    $template = [System.IO.File]::ReadAllText("$PSScriptRoot\docker-compose-clickhouse.yml")
    $fixed = $template.Replace('${TAG:-local}', $tag)
    [System.IO.File]::WriteAllText("$PSScriptRoot\$releaseDir\docker-compose-clickhouse.yml", $fixed, $utf8NoBom)

    Write-Host ''
    Write-Host '✓ 完成。'
    Write-Host "  镜像：sili-smart-trace:$tag（版本号已编进二进制，VERSION 已还原为空）"
    Write-Host ''
    Write-Host "  离线交付（$releaseDir/）："
    Write-Host "    $tarName"
    Write-Host '    docker-compose-clickhouse.yml'
    Write-Host '  服务器加载并启动：'
    Write-Host "    docker load -i $tarName"
    Write-Host '    docker compose -f docker-compose-clickhouse.yml up -d'
}
finally {
    [System.IO.File]::WriteAllText("$PSScriptRoot\VERSION", '', $utf8NoBom)
}
