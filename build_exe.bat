@echo off
cd /d "%~dp0web\frontend"
echo [1/3] Building frontend...
call npm run build:h5
if %errorlevel% neq 0 (
    echo Frontend build failed
    pause
    exit /b 1
)
echo [2/3] Copying to embed directory...
if exist "%~dp0web\server\static\frontend" rmdir /s /q "%~dp0web\server\static\frontend"
xcopy /e /i dist\build\h5 "%~dp0web\server\static\frontend" >nul
cd /d "%~dp0"
echo [3/3] Building Go binary...
go build -o good-review-master.exe .
if %errorlevel% equ 0 (
    echo Build success: good-review-master.exe
) else (
    echo Build failed
)
pause
