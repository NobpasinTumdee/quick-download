@echo off
setlocal EnableExtensions
pushd "%~dp0"
title Quick Download - rebuild

:: ---------------------------------------------------------------------------
:: Quick Download - rebuild after a code change.
::
:: Rebuilds both halves. It does NOT touch the native messaging registration:
:: that points at bin\quick-download.exe, which keeps its path, so it survives
:: every rebuild. Run init_project.bat instead if you reloaded the extension
:: and Chrome gave it a new id.
:: ---------------------------------------------------------------------------

set "ENGINE=bin\quick-download.exe"

echo ===============================================================
echo   Quick Download - rebuild
echo ===============================================================
echo.

:: --- 1. stop the running engine --------------------------------------------
:: Chrome starts the engine on demand and it stays resident, holding the
:: executable open. Windows will not let go build overwrite a running binary.
echo [1/4] Stopping the running engine
taskkill /F /IM quick-download.exe >nul 2>&1
if errorlevel 1 goto :not_running
echo       stopped
goto :stopped
:not_running
echo       not running
:stopped

:: Go renames a locked binary out of the way rather than failing; clear the
:: leftover so bin\ does not collect them.
if exist "%ENGINE%~" del /f /q "%ENGINE%~" >nul 2>&1
echo.

:: --- 2. extension ----------------------------------------------------------
echo [2/4] Compiling the extension TypeScript
pushd extension
call npm run build
if errorlevel 1 goto :tsc_failed
popd
echo       extension\dist\
echo.

:: --- 3. engine -------------------------------------------------------------
echo [3/4] Rebuilding the Go engine
if not exist bin mkdir bin
pushd backend
go build -trimpath -ldflags "-s -w" -o "..\%ENGINE%" .
if errorlevel 1 goto :go_failed
popd
echo       %ENGINE%
echo.

:: --- 4. what the user has to do ---------------------------------------------
echo [4/4] Done
echo.
echo ===============================================================
echo   [SUCCESS] Both halves rebuilt
echo ===============================================================
echo.
echo   The engine restarts by itself the next time the extension talks to it.
echo   The browser side does not - Chrome is still running the old code:
echo.
echo     1. chrome://extensions  -^>  the Reload icon on the Quick Download card
echo     2. refresh any tab you want the floating button on. A content script
echo        is injected at page load, so open tabs keep the previous build
echo        until they are reloaded.
echo.
goto :done

:tsc_failed
popd
echo.
echo   [ERROR] The TypeScript build failed - the old dist\ is untouched, so
echo   the extension still runs the previous build. Fix the errors above.
goto :failed

:go_failed
popd
echo.
echo   [ERROR] go build failed. If it says the file is in use, something
echo   restarted the engine mid-build: close Chrome and run this again.
goto :failed

:failed
echo.
popd
pause
exit /b 1

:done
popd
pause
exit /b 0
