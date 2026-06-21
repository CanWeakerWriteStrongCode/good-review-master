@echo off
go build -o good-review-master.exe .
if %errorlevel% equ 0 (
    echo build success: good-review-master.exe
) else (
    echo build failed
)
pause
