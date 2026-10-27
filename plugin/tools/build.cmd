@echo off
rem Build livett (Release) with MSVC + Ninja. Prereqs: plugin/build-deps (see plugin/README.md) and plugin/tools/gen-importlibs.ps1.
setlocal
call "C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\VC\Auxiliary\Build\vcvars64.bat" >nul || exit /b 1
set "ROOT=%~dp0.."
set CC=cl
set CXX=cl
cmake -S "%ROOT%\livett" -B "%ROOT%\build\livett" -G Ninja -DCMAKE_BUILD_TYPE=Release || exit /b 1
cmake --build "%ROOT%\build\livett" || exit /b 1
echo BUILD OK
