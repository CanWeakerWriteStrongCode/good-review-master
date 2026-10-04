@echo off
REM Regenerate app\wire_gen.go only when the dependency graph actually changed.
REM
REM Why a gate at all: `go tool wire ./app` costs ~2.3s while `go build ./app` costs
REM ~0.55s, so running it on every start is a real tax. Using wire's own `diff` as the
REM gate would be worse than not gating at all -- diff costs the same ~2.3s as gen,
REM so you'd pay check + generate.
REM
REM What the gate watches: the two files that DEFINE the graph (wire.go's provider
REM list, providers.go's signatures) plus go.mod (a dependency upgrade can change a
REM constructor's shape). wire_gen.go's own mtime is the reference point.
REM
REM Known gap, and why it is acceptable: if a constructor used by the graph changes
REM signature in ANOTHER package (e.g. mcpclient.New gains a parameter), none of the
REM watched files change and the gate stays shut -- but wire_gen.go then holds a call
REM that no longer compiles, so `go build` fails loudly. The gate trades completeness
REM for speed, but it never trades correctness for it: nothing here can silently keep
REM running an out-of-date graph.
REM
REM The one thing neither the gate nor `go tool wire` can catch: writing a new
REM provideXxx and forgetting to list it in wire.Build. That produces no diff at all.
REM The only detector is looking at `git status` after generating.
setlocal
cd /d "%~dp0"

REM PowerShell exits 0 when something is newer than wire_gen.go, 1 when nothing is.
REM (build_exe.bat already depends on PowerShell for the build timestamp, so this
REM adds no new requirement.)
powershell -NoProfile -Command "$generated = Get-Item -LiteralPath 'app\wire_gen.go' -ErrorAction SilentlyContinue; if (-not $generated) { exit 0 }; $stale = @('app\wire.go','app\providers.go','go.mod') | Where-Object { (Get-Item -LiteralPath $_).LastWriteTime -gt $generated.LastWriteTime }; if ($stale) { exit 0 } else { exit 1 }"
if errorlevel 1 (
    echo [wire] graph unchanged, skipping generation
    exit /b 0
)

echo [wire] graph changed, regenerating app\wire_gen.go...
go tool wire ./app
if %errorlevel% neq 0 (
    echo [wire] generation failed -- fix the provider graph, then re-run
    exit /b 1
)
echo [wire] app\wire_gen.go updated
exit /b 0
