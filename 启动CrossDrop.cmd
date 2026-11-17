@echo off
setlocal
chcp 65001 >nul
cd /d "%~dp0"
set "CROSSDROP_COMPOSE_FILES=-f compose.yaml"
set "CROSSDROP_LAUNCH_MODE=direct"
powershell.exe -NoProfile -Command ^
  "$ErrorActionPreference = 'Stop'; try {" ^
  "  $mode = 'direct';" ^
  "  if (Test-Path -LiteralPath '.env') {" ^
  "    $modeLine = Get-Content -LiteralPath '.env' | Where-Object { $_ -match '^\s*CROSSDROP_TUNNEL_MODE\s*=' } | Select-Object -Last 1;" ^
  "    if ($modeLine) { $mode = ($modeLine -split '=', 2)[1].Trim().Trim([char[]]@([char]34, [char]39)) };" ^
  "  };" ^
  "  if ($mode -eq 'direct') { exit 0 };" ^
  "  if ($mode -eq 'proxy') { exit 1 };" ^
  "  Write-Host 'CROSSDROP_TUNNEL_MODE must be direct or proxy.'; exit 2;" ^
  "} catch { Write-Host $_.Exception.Message; exit 2 }"
if errorlevel 2 goto config_failed
if errorlevel 1 (
  set "CROSSDROP_COMPOSE_FILES=-f compose.yaml -f compose.proxy.yaml"
  set "CROSSDROP_LAUNCH_MODE=proxy"
)
type nul > PUBLIC_URL.txt

echo [1/3] Checking Docker Desktop...
docker info > STARTUP_LOG.txt 2>&1
if not errorlevel 1 goto docker_ready
echo Starting Docker Desktop. Please wait...
docker desktop start --timeout 90 >> STARTUP_LOG.txt 2>&1
if errorlevel 1 goto docker_unavailable
for /L %%A in (1,1,20) do (
  docker info >> STARTUP_LOG.txt 2>&1
  if not errorlevel 1 goto docker_ready
  timeout /t 3 /nobreak >nul
)
goto docker_unavailable

:docker_ready
echo [2/3] Starting CrossDrop and the public tunnel...
docker compose %CROSSDROP_COMPOSE_FILES% --profile public up -d >> STARTUP_LOG.txt 2>&1
if errorlevel 1 goto startup_failed

echo [3/3] Checking the public address. This can take a few minutes...
powershell.exe -NoProfile -Command ^
  "$ErrorActionPreference = 'Stop'; try {" ^
  "$composeArgs = @('-f', 'compose.yaml');" ^
  "if ($env:CROSSDROP_LAUNCH_MODE -eq 'proxy') { $composeArgs += @('-f', 'compose.proxy.yaml') };" ^
  "$composeArgs += @('--profile', 'public');" ^
  "function Find-ReadyUrl {" ^
  "  $logs = (& docker compose @composeArgs logs --no-color cloudflared 2>$null | Out-String);" ^
  "  $urlMatches = [regex]::Matches($logs, 'https://[a-z0-9-]+[.]trycloudflare[.]com');" ^
  "  if ($urlMatches.Count -eq 0) { return $null };" ^
  "  $address = $urlMatches[$urlMatches.Count - 1].Value;" ^
  "  try {" ^
  "    $health = Invoke-RestMethod -Uri ($address + '/healthz') -TimeoutSec 6;" ^
  "    if ($health.status -eq 'ok') { return $address };" ^
  "  } catch {};" ^
  "  return $null;" ^
  "};" ^
  "$readyUrl = $null;" ^
  "for ($round = 0; $round -lt 2; $round++) {" ^
  "  for ($attempt = 0; $attempt -lt 30; $attempt++) {" ^
  "    $readyUrl = Find-ReadyUrl;" ^
  "    if ($readyUrl) { break };" ^
  "    Write-Host ('Waiting for the public connection (' + ($attempt + 1) + '/30)...');" ^
  "    Start-Sleep -Seconds 5;" ^
  "  };" ^
  "  if ($readyUrl) { break };" ^
  "  if ($round -eq 0) {" ^
  "    $recentLogs = (& docker compose @composeArgs logs --no-color --tail 100 cloudflared 2>$null | Out-String);" ^
  "    $connectionStates = @($recentLogs -split [Environment]::NewLine | Where-Object { $_ -match 'Registered tunnel connection|Unauthorized: Tunnel not found' });" ^
  "    if ($connectionStates.Count -gt 0 -and $connectionStates[-1] -match 'Unauthorized: Tunnel not found') {" ^
  "      Write-Host 'The old tunnel expired. Creating a new address...';" ^
  "      & docker compose @composeArgs restart cloudflared | Out-Host;" ^
  "      if ($LASTEXITCODE -ne 0) { throw 'Tunnel restart failed. Check Docker Desktop.' };" ^
  "    } else { Write-Host 'Still waiting. Checking the existing tunnel again...'; };" ^
  "  };" ^
  "};" ^
  "if (-not $readyUrl) { throw 'Public connection is not ready. Check Clash and the tunnel errors in STARTUP_LOG.txt.' };" ^
  "$urlPath = Join-Path (Get-Location) 'PUBLIC_URL.txt';" ^
  "[System.IO.File]::WriteAllText($urlPath, $readyUrl + [Environment]::NewLine, (New-Object System.Text.UTF8Encoding($false)));" ^
  "Write-Host ('Public address: ' + $readyUrl);" ^
  "Write-Host ('Address saved to: ' + $urlPath);" ^
  "if ($env:CROSSDROP_NO_OPEN -ne '1') { Start-Process $readyUrl };" ^
  "} catch { Write-Host $_.Exception.Message; Add-Content -LiteralPath 'STARTUP_LOG.txt' -Value $_.Exception.Message -Encoding UTF8; exit 1 }"
if errorlevel 1 goto startup_failed

echo.
echo CrossDrop is ready. Other devices can use the address in PUBLIC_URL.txt.
if not "%CROSSDROP_NO_PAUSE%"=="1" pause
exit /b 0

:docker_unavailable
echo Docker is not ready. Open Docker Desktop to check its status, then try again.
echo See STARTUP_LOG.txt in this folder for details.
if not "%CROSSDROP_NO_PAUSE%"=="1" pause
exit /b 1

:startup_failed
echo Startup failed. Check the messages above.
echo See STARTUP_LOG.txt in this folder for details.
if not "%CROSSDROP_NO_PAUSE%"=="1" pause
exit /b 1

:config_failed
echo Check CROSSDROP_TUNNEL_MODE in the project .env file, then try again.
if not "%CROSSDROP_NO_PAUSE%"=="1" pause
exit /b 1
