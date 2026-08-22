@echo off
setlocal

:: ----------------------------------------------------
:: 1. กำหนด Extension ID ตรงนี้ (เปลี่ยนค่าหลังเครื่องหมาย =)
:: ----------------------------------------------------
set EXTENSION_ID=<YOUR_EXTENSION_ID_HERE>

echo ==================================================
echo      Quick Download - Installer / Updater
echo ==================================================
echo.

:: ให้ผู้ใช้พิมพ์ ID ใหม่ได้ ถ้าไม่พิมพ์จะใช้ค่าด้านบน
set /p USER_ID="Enter Extension ID (Press ENTER to use default: %EXTENSION_ID%): "
if not "%USER_ID%"=="" set EXTENSION_ID=%USER_ID%

echo.
echo [1/3] Building the Go Engine and Extension...
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command ".\build.ps1"
if %ERRORLEVEL% neq 0 (
    echo [ERROR] Build failed.
    pause
    exit /b %ERRORLEVEL%
)

@REM echo.
@REM echo [2/3] Downloading External Tools (yt-dlp, ffmpeg)...
@REM powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "cd tools; .\get-tools.ps1"
@REM if %ERRORLEVEL% neq 0 (
@REM     echo [ERROR] Failed to download tools.
@REM     pause
@REM     exit /b %ERRORLEVEL%
@REM )

echo.
echo [3/3] Registering Native Messaging Host...
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "cd host; .\install-windows.ps1 -ExtensionId '%EXTENSION_ID%'"
if %ERRORLEVEL% neq 0 (
    echo [ERROR] Failed to register host.
    pause
    exit /b %ERRORLEVEL%
)

echo.
echo ==================================================
echo [SUCCESS] Installation and Build Complete!
echo Extension ID used: %EXTENSION_ID%
echo Please restart Chrome completely for changes to take effect.
echo ==================================================
pause