@echo off
setlocal EnableExtensions
pushd "%~dp0"
title Quick Download - first-time setup

:: ---------------------------------------------------------------------------
:: Quick Download - first-time setup
::
:: Everything a fresh clone needs: dependencies, both builds, the helper
:: binaries, and the native messaging registration Chrome needs before the
:: extension can talk to the engine.
::
:: Usage:  init_project.bat [extension-id]
::
:: The extension id is the awkward part, and there is no way around it: Chrome
:: only assigns one once the folder is loaded, and the native host manifest has
:: to name it. So the script builds everything first, then asks. Run it again
:: with the id once you have it - re-running is safe and skips nothing.
:: ---------------------------------------------------------------------------

set "EXT_ID=%~1"
set "ENGINE=bin\quick-download.exe"

echo ===============================================================
echo   Quick Download - first-time setup
echo ===============================================================
echo.

:: --- 1. prerequisites ------------------------------------------------------
echo [1/6] Checking prerequisites
where node >nul 2>&1
if errorlevel 1 goto :no_node
where npm >nul 2>&1
if errorlevel 1 goto :no_node
where go >nul 2>&1
if errorlevel 1 goto :no_go

for /f "tokens=*" %%v in ('node --version 2^>nul') do set "NODE_V=%%v"
for /f "tokens=3" %%v in ('go version 2^>nul') do set "GO_V=%%v"
echo       node %NODE_V%
echo       go   %GO_V%
echo.

:: --- 2. extension dependencies ---------------------------------------------
echo [2/6] Installing extension dependencies
pushd extension
call npm install
if errorlevel 1 goto :npm_failed

:: --- 3. compile the extension ----------------------------------------------
echo.
echo [3/6] Compiling the extension TypeScript
call npm run build
if errorlevel 1 goto :tsc_failed
popd
echo       extension\dist\
echo.

:: --- 4. build the engine ---------------------------------------------------
:: The main package lives in backend\, not backend\cmd\... - one module, one
:: binary. -trimpath keeps local paths out of the executable.
echo [4/6] Building the Go engine
if not exist bin mkdir bin
pushd backend
go build -trimpath -ldflags "-s -w" -o "..\%ENGINE%" .
if errorlevel 1 goto :go_failed
popd
echo       %ENGINE%
echo.

:: --- 5. helper binaries ----------------------------------------------------
:: yt-dlp and ffmpeg are what make streaming sites work at all. They are
:: third-party downloads, so this only runs when they are actually missing and
:: get-tools.ps1 asks before fetching anything.
echo [5/6] Checking yt-dlp and ffmpeg
if exist "bin\yt-dlp.exe" if exist "bin\ffmpeg.exe" goto :tools_ready
echo       one or both are missing - fetching them
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "tools\get-tools.ps1"
if errorlevel 1 echo       [WARN] could not fetch them. Direct downloads still work; see tools\README.md
goto :tools_done
:tools_ready
echo       both present
:tools_done
echo.

:: --- 6. native messaging registration --------------------------------------
echo [6/6] Registering the native messaging host
if defined EXT_ID goto :have_id

:: Re-running? The id we registered last time is in the host manifest.
if not exist "bin\com.downloader.app.json" goto :ask_id
:: The manifest line reads "chrome-extension://<id>/" - and for /f collapses
:: the doubled delimiter, so the id is the second token. Done with findstr
:: rather than PowerShell because an unescaped ) inside a for /f backtick
:: command closes the loop's own parenthesis and silently yields nothing.
for /f "usebackq tokens=2 delims=/" %%i in (`findstr /c:"chrome-extension" "bin\com.downloader.app.json"`) do set "EXT_ID=%%i"
if defined EXT_ID echo       found a previous registration for %EXT_ID%

:ask_id
echo.
echo       Chrome assigns the id when you load the folder, so if you have not
echo       done that yet:
echo         1. open chrome://extensions
echo         2. turn on Developer mode
echo         3. Load unpacked  -^>  %CD%\extension
echo         4. copy the id it shows under the extension name
echo.
set /p "EXT_ID=      Extension ID [ENTER to skip for now]: "

:have_id
if not defined EXT_ID goto :skipped_registration

:: Exactly 32 characters, a-p only. Character 33 must not exist and
:: character 32 must, which pins the length; findstr checks the alphabet.
if not "%EXT_ID:~32,1%"=="" goto :bad_id
if "%EXT_ID:~31,1%"=="" goto :bad_id
echo %EXT_ID%| findstr /R /C:"^[a-p]*$" >nul
if errorlevel 1 goto :bad_id

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "host\install-windows.ps1" -ExtensionId "%EXT_ID%" -SkipBuild
if errorlevel 1 goto :register_failed

echo.
echo ===============================================================
echo   [SUCCESS] Quick Download is set up
echo ===============================================================
echo.
echo   Engine     : %CD%\%ENGINE%
echo   Extension  : %CD%\extension
echo   Registered : %EXT_ID%
echo.
echo   Last step: quit Chrome completely - every window, not just the tab -
echo   and start it again. A native host is only picked up on a cold start.
echo.
echo   Then open the extension popup: the chip should read "engine v...".
echo   Dashboard: http://127.0.0.1:9090
echo.
goto :done

:skipped_registration
echo.
echo ===============================================================
echo   [ALMOST] Everything is built, but the host is NOT registered
echo ===============================================================
echo.
echo   The extension will load and sniff, but every download will fail with
echo   "native host unreachable" until you finish this step:
echo.
echo     1. chrome://extensions  -^>  Developer mode  -^>  Load unpacked
echo     2. choose  %CD%\extension
echo     3. run this script again, pasting the id it shows:
echo          init_project.bat ^<extension-id^>
echo.
goto :done

:: --- failure paths ----------------------------------------------------------
:no_node
echo.
echo   [ERROR] Node.js is not on PATH.
echo   Install the LTS build from https://nodejs.org, reopen this window,
echo   and run the script again.
goto :failed

:no_go
echo.
echo   [ERROR] Go is not on PATH.
echo   Install Go 1.22 or newer from https://go.dev/dl, reopen this window,
echo   and run the script again.
goto :failed

:npm_failed
popd
echo.
echo   [ERROR] npm install failed. The message above says why - usually no
echo   network, or a proxy that needs configuring.
goto :failed

:tsc_failed
popd
echo.
echo   [ERROR] The TypeScript build failed. Nothing was installed; fix the
echo   errors above and run the script again.
goto :failed

:go_failed
popd
echo.
echo   [ERROR] go build failed. If it says the file is in use, close Chrome
echo   so the running engine exits, then try again.
goto :failed

:bad_id
echo.
echo   [ERROR] "%EXT_ID%" is not a Chrome extension id.
echo   Ids are exactly 32 characters, letters a to p only. Copy it from the
echo   extension's card at chrome://extensions.
goto :failed

:register_failed
echo.
echo   [ERROR] Registering the native messaging host failed. The PowerShell
echo   error above has the detail; no admin rights are needed for this step.
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
