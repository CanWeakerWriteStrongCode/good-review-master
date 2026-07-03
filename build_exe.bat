@echo off
cd /d "%~dp0web\frontend"
echo [1/2] Building frontend...
call npm run build:h5
if %errorlevel% neq 0 (
    echo Frontend build failed
    pause
    exit /b 1
)
cd /d "%~dp0"
echo [2/2] Building Go binary...
go build -o good-review-master.exe .
if %errorlevel% equ 0 (
    echo Build success: good-review-master.exe
) else (
    echo Build failed
)
pause
