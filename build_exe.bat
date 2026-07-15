@echo off
cd /d "%~dp0web\frontend"
echo [1/3] Building frontend...
where pnpm >nul 2>&1 || npm install -g pnpm
call pnpm install --frozen-lockfile
if %errorlevel% neq 0 (
    echo pnpm install failed
    pause
    exit /b 1
)
call pnpm run build:h5
if %errorlevel% neq 0 (
    echo Frontend build failed
    pause
    exit /b 1
)
echo [2/3] Copying to embed directory...
if exist "%~dp0web\server\static\frontend" rmdir /s /q "%~dp0web\server\static\frontend"
xcopy /e /i dist\build\h5 "%~dp0web\server\static\frontend" >nul
cd /d "%~dp0"
echo [3/3] Building Go binaries (cross-compile)...
echo Downloading Go dependencies (first run may take a while)...
go mod download
if %errorlevel% neq 0 (
    echo Go module download failed
    pause
    exit /b 1
)
if not exist dist mkdir dist

for /f "tokens=*" %%i in ('git describe --tags --always --dirty 2^>nul') do set VERSION=%%i
if "%VERSION%"=="" set VERSION=dev

for /f "tokens=*" %%i in ('git rev-parse --short HEAD 2^>nul') do set COMMIT=%%i
if "%COMMIT%"=="" set COMMIT=unknown

for /f "tokens=*" %%i in ('powershell -NoProfile -Command "Get-Date -Format ''yyyy-MM-ddTHH:mm:ssZ'' -AsUTC"') do set BUILD_TIME=%%i

set LDFLAGS=-s -w -X good-review-master/version.Version=%VERSION% -X good-review-master/version.Commit=%COMMIT% -X good-review-master/version.BuildTime=%BUILD_TIME%

set GOOS=windows
set GOARCH=amd64
go build -ldflags "%LDFLAGS%" -o dist\good-review-master-windows-amd64-%VERSION%.exe .
if %errorlevel% neq 0 goto :build_fail

set GOOS=linux
set GOARCH=amd64
go build -ldflags "%LDFLAGS%" -o dist\good-review-master-linux-amd64-%VERSION% .
if %errorlevel% neq 0 goto :build_fail

set GOOS=darwin
set GOARCH=amd64
go build -ldflags "%LDFLAGS%" -o dist\good-review-master-darwin-amd64-%VERSION% .
if %errorlevel% neq 0 goto :build_fail

set GOOS=darwin
set GOARCH=arm64
go build -ldflags "%LDFLAGS%" -o dist\good-review-master-darwin-arm64-%VERSION% .
if %errorlevel% neq 0 goto :build_fail

echo Build success:
dir dist /b
pause
exit /b 0

:build_fail
echo Build failed
pause
exit /b 1
