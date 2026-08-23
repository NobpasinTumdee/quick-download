@echo off
title Building Quick Download Release...
echo =======================================
echo     Creating Release Package for Users
echo =======================================

echo.
echo [1/4] Compiling Extension (TypeScript)...
:: เปลี่ยนโฟลเดอร์ตรงนี้ให้ตรงกับที่เก็บ package.json จริงๆ ของโปรเจกต์คุณ
cd extension
call npm install >nul 2>&1
call npm run build
cd ..

echo.
echo [2/4] Compiling Go Backend...
cd backend
go build -trimpath -ldflags "-s -w" -o "..\bin\quick-download.exe" .
cd ..

echo.
echo [3/4] Assembling Release Folder...
if exist ReleaseTemp rmdir /s /q ReleaseTemp
mkdir ReleaseTemp
mkdir ReleaseTemp\extension
mkdir ReleaseTemp\bin

:: ก๊อปปี้เฉพาะไฟล์ของ Extension ที่ใช้งานจริง (ข้าม node_modules และ src)
xcopy /E /I /Y extension\dist ReleaseTemp\extension\dist >nul
xcopy /E /I /Y extension\icons ReleaseTemp\extension\icons >nul
copy extension\manifest.json ReleaseTemp\extension\ >nul
copy extension\popup.html ReleaseTemp\extension\ >nul
copy extension\popup.css ReleaseTemp\extension\ >nul
:: ถ้ามีไฟล์ .html หรือ .css อื่นๆ ที่อยู่หน้าแรกของ extension ให้ใส่ copy เพิ่มบรรทัดนี้ได้เลย

:: ก๊อปปี้โฟลเดอร์ bin (ที่มี quick-download.exe)
xcopy /E /I /Y bin ReleaseTemp\bin >nul

:: สร้างไฟล์ install.bat ง่ายๆ สำหรับให้ User ทั่วไปกดติดตั้ง
echo @echo off > ReleaseTemp\install_host.bat
echo title Quick Download - Host Setup >> ReleaseTemp\install_host.bat
echo set /p EXT_ID="Paste your Chrome Extension ID here and press Enter: " >> ReleaseTemp\install_host.bat
echo powershell.exe -ExecutionPolicy Bypass -File "host\install-windows.ps1" -ExtensionId %%EXT_ID%% -SkipBuild >> ReleaseTemp\install_host.bat
echo echo Setup Complete! You can close this window. >> ReleaseTemp\install_host.bat
echo pause >> ReleaseTemp\install_host.bat

:: ดึงไฟล์ .ps1 ที่จำเป็นไปใส่ให้ User ด้วย
mkdir ReleaseTemp\host
copy host\install-windows.ps1 ReleaseTemp\host\ >nul

echo.
echo [4/4] Zipping into QuickDownload-Release.zip...
if exist QuickDownload-Release.zip del QuickDownload-Release.zip
powershell -NoProfile -Command "Compress-Archive -Path ReleaseTemp\* -DestinationPath QuickDownload-Release.zip -Force"
rmdir /s /q ReleaseTemp

echo.
echo =======================================
echo SUCCESS! 'QuickDownload-Release.zip' is ready.
echo Upload this file to your GitHub Releases page.
echo =======================================
pause