<#
.SYNOPSIS
    打包 sili/sili-smart-trace 镜像，并固化交付用 compose。
.DESCRIPTION
    版本号规则：当日日期（sili-YYYYMMDD）。
      - 临时写入 VERSION，供 Dockerfile 经 ldflags 注入二进制的 common.Version；
      - 用同一值打 image tag，保证「二进制内版本」与「镜像 tag」一致；
      - 把 TAG 同步到 .env，供本地开发引用；
      - 脚本退出时还原 VERSION 为空，保持 git 工作区干净。

    交付产物：
      - docker-compose-clickhouse.yml        从模板固化 image tag 的交付 compose（根目录）
      - release/sili-smart-trace-YYYYMMDD.tar   自建镜像

    模板 docker-compose-clickhouse.template.yml（含 ${TAG:-local} 占位符）入库；
    固化后的 docker-compose-clickhouse.yml 被 .gitignore 忽略，每次构建重新生成。

    同一天多次构建会覆盖同一 tag；如需区分同日多次构建，把 yyyyMMdd 改成 yyyyMMdd-HHmm。
.NOTES
    运行（PowerShell 7）：pwsh .\docker-build.ps1
    Windows PowerShell 5.1（默认 Restricted 禁止脚本，直接运行会被拦）：
      powershell -ExecutionPolicy Bypass -File .\docker-build.ps1
#>

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$tag = 'sili-' + (Get-Date -Format 'yyyyMMdd')
$tagSuffix = $tag.Substring('sili-'.Length)   # 20260801，用于 tar 命名
$releaseDir = 'release'
$template = 'docker-compose-clickhouse.template.yml'
$compose = 'docker-compose-clickhouse.yml'
$utf8NoBom = New-Object System.Text.UTF8Encoding $false

try {
    Write-Host ">> 写入 VERSION=$tag"
    [System.IO.File]::WriteAllText("$PSScriptRoot\VERSION", $tag, $utf8NoBom)

    # 本地若已有同 tag 镜像，先删除，避免 build 后旧镜像变 dangling
    docker image inspect "sili/sili-smart-trace:$tag" *> $null
    if ($LASTEXITCODE -eq 0) {
        Write-Host ">> 本地已有 sili/sili-smart-trace:$tag，先删除"
        docker rmi "sili/sili-smart-trace:$tag" *> $null
        if ($LASTEXITCODE -ne 0) {
            Write-Host "  （删除失败，可能有容器占用；build 后旧镜像将变 dangling）"
        }
    }

    Write-Host ">> 构建镜像 sili/sili-smart-trace:$tag"
    docker build -t "sili/sili-smart-trace:$tag" .
    if ($LASTEXITCODE -ne 0) { throw "docker build 失败（退出码 $LASTEXITCODE）" }

    Write-Host ">> 同步 TAG 到 .env（供本地开发引用）"
    [System.IO.File]::WriteAllText("$PSScriptRoot\.env", "TAG=$tag`n", $utf8NoBom)

    Write-Host ">> 固化交付 compose（$compose，image tag 写死为 $tag）"
    $tpl = [System.IO.File]::ReadAllText("$PSScriptRoot\$template")
    $fixed = $tpl.Replace('${TAG:-local}', $tag)
    [System.IO.File]::WriteAllText("$PSScriptRoot\$compose", $fixed, $utf8NoBom)

    Write-Host ">> 导出镜像到 $releaseDir/"
    New-Item -ItemType Directory -Force -Path $releaseDir | Out-Null
    $tarName = "sili-smart-trace-$tagSuffix.tar"
    docker save "sili/sili-smart-trace:$tag" -o (Join-Path $releaseDir $tarName)
    if ($LASTEXITCODE -ne 0) { throw "docker save 失败（退出码 $LASTEXITCODE）" }

    Write-Host ''
    Write-Host '✓ 完成。'
    Write-Host "  镜像：sili/sili-smart-trace:$tag（版本号已编进二进制，VERSION 已还原为空）"
    Write-Host ''
    Write-Host '  交付物：'
    Write-Host "    $compose                  （image 已固化）"
    Write-Host "    $releaseDir/$tarName      （镜像）"
    Write-Host '  服务器加载并启动：'
    Write-Host "    docker load -i $tarName"
    Write-Host "    docker compose -f $compose up -d"
}
finally {
    [System.IO.File]::WriteAllText("$PSScriptRoot\VERSION", '', $utf8NoBom)
}
