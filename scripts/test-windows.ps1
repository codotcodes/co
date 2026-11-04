# Run credential tests as a fresh standard user on an ephemeral CI runner.
# Elevated Windows tokens can assign Administrators as the default file owner.
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$user = "co-test-$PID"
$root = Join-Path $env:RUNNER_TEMP $user
$password = ConvertTo-SecureString ([guid]::NewGuid().ToString() + 'aA1!') -AsPlainText -Force
$credential = [pscredential]::new("$env:COMPUTERNAME\$user", $password)
$other = "co-other-$PID"
try {
    New-LocalUser -Name $user -Password $password | Out-Null
    New-Item -ItemType Directory -Path $root | Out-Null
    & icacls $root /grant "${user}:(OI)(CI)F" | Out-Null
    if ($LASTEXITCODE) { throw 'Cannot grant test user its scratch directory' }
    & go build -mod=readonly -trimpath -o "$root\co.exe" ./cmd/co
    if ($LASTEXITCODE) { throw 'Native Windows CLI compilation failed' }
    & go test -mod=readonly -c -o "$root\cli.test.exe" ./internal/cli
    if ($LASTEXITCODE) { throw 'Native Windows test compilation failed' }
    $script = @'
param($Root)
$ErrorActionPreference = 'Stop'
$env:TEMP = $Root
$env:TMP = $Root
$env:CO_TEST_GO_BINARY = "$Root\co.exe"
try {
    & "$Root\cli.test.exe" '-test.v' '-test.timeout=5m'
    if ($LASTEXITCODE) { throw 'Native Go tests failed' }
    exit 0
} catch { Write-Error $_; exit 1 }
'@
    $script | Set-Content "$root\run.ps1"
    $process = Start-Process -FilePath (Get-Command powershell.exe).Source -Credential $credential -LoadUserProfile -ArgumentList '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', "$root\run.ps1", '-Root', $root -RedirectStandardOutput "$root\native.stdout.log" -RedirectStandardError "$root\native.stderr.log" -Wait -PassThru
    Get-Content "$root\native.stdout.log"
    Get-Content "$root\native.stderr.log"
    if ($process.ExitCode) { throw 'Standard-user Windows runtime tests failed' }
    # Prove a different Windows identity cannot read persisted credentials.
    $probe = Join-Path $root 'identity-probe'
    $binary = Join-Path $root 'co.exe'
    $env:CO_CONFIG_DIR = $probe
    & $binary logout
    if ($LASTEXITCODE) { throw 'Cannot create ACL probe' }
    # This config is runner-owned, not test-user-owned; even an explicit DACL
    # granting read/write does not authorize co to take over another identity.
    & icacls $probe /grant "${user}:(OI)(CI)F" | Out-Null
    $negative = "`$env:CO_CONFIG_DIR = '$probe'; & '$binary' agent list; if (`$LASTEXITCODE -eq 0) { exit 1 }; exit 0"
    $negative | Set-Content "$root\negative.ps1"
    $process = Start-Process powershell.exe -Credential $credential -LoadUserProfile -ArgumentList '-NoProfile', '-File', "$root\negative.ps1" -RedirectStandardOutput "$root\negative.stdout.log" -RedirectStandardError "$root\negative.stderr.log" -Wait -PassThru
    Get-Content "$root\negative.stderr.log"
    if ($process.ExitCode) { throw 'Config accepted another owner' }
    if ((Get-Content "$root\negative.stderr.log" -Raw) -notmatch 'another Windows identity') { throw 'Owner rejection was not exercised' }
    $otherPassword = ConvertTo-SecureString ([guid]::NewGuid().ToString() + 'aA1!') -AsPlainText -Force
    New-LocalUser -Name $other -Password $otherPassword | Out-Null
    & icacls $root /grant "${other}:(OI)(CI)RX" | Out-Null
    # Reset the probe to owner-only, then use an independent account process.
    & $binary logout
    if ($LASTEXITCODE) { throw 'Cannot reset owner-only probe' }
    $otherCredential = [pscredential]::new("$env:COMPUTERNAME\$other", $otherPassword)
    $process = Start-Process powershell.exe -Credential $otherCredential -LoadUserProfile -ArgumentList '-NoProfile', '-File', "$root\negative.ps1" -RedirectStandardOutput "$root\other.stdout.log" -RedirectStandardError "$root\other.stderr.log" -Wait -PassThru
    if ($process.ExitCode) { throw 'Another account accessed owner-only config' }
} finally {
    Remove-Item Env:CO_CONFIG_DIR -ErrorAction SilentlyContinue
    if (Get-LocalUser -Name $user -ErrorAction SilentlyContinue) { Remove-LocalUser -Name $user }
    if (Get-LocalUser -Name $other -ErrorAction SilentlyContinue) { Remove-LocalUser -Name $other }
}
