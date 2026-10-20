$ErrorActionPreference = 'Stop'

Write-Output "Source commit: $env:BUILDKITE_COMMIT"
Write-Output "Windows: $([Environment]::OSVersion.VersionString)"
buildkite-agent --version
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

buildkite-agent artifact download runtime.test.exe . --step windows-process-build
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

# Test descendants must be gone before this process returns, while the agent's
# enclosing Job Object and the hosted VM are both still alive.
& .\runtime.test.exe -test.run '^TestWindowsProcessTree$' -test.v -test.count=3 -test.timeout=3m
exit $LASTEXITCODE
