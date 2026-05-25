REM Copyright 2026 Alibaba Group Holding Ltd.
REM
REM Licensed under the Apache License, Version 2.0 (the "License");
REM you may not use this file except in compliance with the License.
REM You may obtain a copy of the License at
REM
REM     http://www.apache.org/licenses/LICENSE-2.0
REM
REM Unless required by applicable law or agreed to in writing, software
REM distributed under the License is distributed on an "AS IS" BASIS,
REM WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
REM See the License for the specific language governing permissions and
REM limitations under the License.

@echo off
setlocal enableextensions

set "EXECD_LOG_FILE=%EXECD_LOG_FILE%"
if "%EXECD_LOG_FILE%"=="" set "EXECD_LOG_FILE=C:\OpenSandbox\install.log"

set "EXECD_STDOUT_LOG=%EXECD_STDOUT_LOG%"
if "%EXECD_STDOUT_LOG%"=="" set "EXECD_STDOUT_LOG=C:\OpenSandbox\execd.stdout.log"

set "EXECD_STDERR_LOG=%EXECD_STDERR_LOG%"
if "%EXECD_STDERR_LOG%"=="" set "EXECD_STDERR_LOG=C:\OpenSandbox\execd.stderr.log"

set "EXECD_OEM_BIN=C:\OEM\execd.exe"

if not exist "C:\OpenSandbox" mkdir "C:\OpenSandbox"
if errorlevel 1 (
    echo [install.bat] WARN: failed to create log dir: C:\OpenSandbox
    exit /b 0
)

call :log "startup begin"
call :log "oem bin: %EXECD_OEM_BIN%"
call :log "execd stdout log: %EXECD_STDOUT_LOG%"
call :log "execd stderr log: %EXECD_STDERR_LOG%"

if not exist "%EXECD_OEM_BIN%" (
    call :log "WARN: execd binary not found at %EXECD_OEM_BIN%"
    exit /b 0
)

call :prepare_executable "%EXECD_OEM_BIN%"
call :ensure_firewall_rule

call :log "starting %EXECD_OEM_BIN%"
set "EXECD_BIN_PS=%EXECD_OEM_BIN%"
set "EXECD_STDOUT_LOG_PS=%EXECD_STDOUT_LOG%"
set "EXECD_STDERR_LOG_PS=%EXECD_STDERR_LOG%"
powershell -NoProfile -ExecutionPolicy Bypass -Command "$p=$env:EXECD_BIN_PS; $out=$env:EXECD_STDOUT_LOG_PS; $err=$env:EXECD_STDERR_LOG_PS; try { Start-Process -FilePath $p -WindowStyle Hidden -RedirectStandardOutput $out -RedirectStandardError $err; exit 0 } catch { exit 1 }" >nul 2>&1
if errorlevel 1 (
    call :log "WARN: PowerShell Start-Process failed, fallback to cmd start"
    start "opensandbox-execd" /B cmd /c ""%EXECD_OEM_BIN%" 1>>"%EXECD_STDOUT_LOG%" 2>>"%EXECD_STDERR_LOG%""
)
if errorlevel 1 (
    call :log "WARN: failed to start execd.exe via both powershell and cmd"
    exit /b 0
)

call :log "execd started in background"
exit /b 0

:log
echo [install.bat] %~1
>>"%EXECD_LOG_FILE%" echo [%date% %time%] [install.bat] %~1
exit /b 0

:prepare_executable
set "TARGET_BIN=%~1"
if "%TARGET_BIN%"=="" exit /b 0
set "TARGET_BIN_PS=%TARGET_BIN%"
call :log "preparing executable security metadata for %TARGET_BIN%"
powershell -NoProfile -ExecutionPolicy Bypass -Command "$p=$env:TARGET_BIN_PS; try { if (Test-Path -LiteralPath $p) { Remove-Item -LiteralPath ($p + ':Zone.Identifier') -ErrorAction SilentlyContinue; Unblock-File -LiteralPath $p -ErrorAction SilentlyContinue }; exit 0 } catch { exit 0 }" >nul 2>&1
if errorlevel 1 (
    call :log "WARN: executable prepare returned non-zero"
)
exit /b 0

:ensure_firewall_rule
set "EXECD_FW_RULE=OpenSandbox execd 44772"
call :log "ensuring firewall rule for TCP 44772"
netsh advfirewall firewall delete rule name="%EXECD_FW_RULE%" >nul 2>&1
netsh advfirewall firewall add rule name="%EXECD_FW_RULE%" dir=in action=allow protocol=TCP localport=44772 >nul 2>&1
if errorlevel 1 (
    call :log "WARN: failed to add firewall allow rule for TCP 44772"
    exit /b 0
)
call :log "firewall rule ready for TCP 44772"
exit /b 0
