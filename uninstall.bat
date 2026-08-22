@echo off
setlocal

echo ==================================================
echo      Quick Download - Uninstaller
echo ==================================================
echo.
echo This will remove the Native Messaging Host registration from your Windows Registry.
echo.

powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "cd host; .\uninstall-windows.ps1"

if %ERRORLEVEL% neq 0 (
    echo.
    echo [ERROR] Uninstallation failed.
    pause
    exit /b %ERRORLEVEL%
)

echo.
echo ==================================================
echo [SUCCESS] Uninstallation Complete!
echo The registry keys have been removed. 
echo You can now safely delete the project folder if you want.
echo ==================================================
pause