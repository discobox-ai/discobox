# Install the discobox command line client.
#
#   irm https://discobox.ai/install.ps1 | iex
#   irm https://edge.discobox.ai/install.ps1 | iex
#   $env:DISCOBOX_CHANNEL = 'latest'; irm https://discobox.ai/install.ps1 | iex
#   & ([scriptblock]::Create((irm https://discobox.ai/install.ps1))) -Version v0.7.1
#
# The copy a release uploads is stamped with that release and the SHA-256 of
# every binary it uploaded, and run with no arguments installs exactly that
# release. Asked for any other version or channel, it downloads the installer
# that release uploaded and hands over to it, so the code installing a release
# is always the code that release shipped (ADR 0110). install.sh is the same
# installer for sh: the two take the same options and follow the same rules.
#
#   -Channel CHANNEL   stable  the newest release marked stable
#                      latest  the newest vX.Y.Z release, stable or not
#                      edge    the newest release of any kind, -alpha and -rc too
#   -Version VERSION   one release, such as v0.7.1
#   -InstallDir DIR    where to put the discobox command
#                      (default: %LOCALAPPDATA%\Programs\Discobox on Windows)
#   -Stage             download the server too, rather than on first use
#   -NoModifyPath      leave the user's PATH alone
#
# Each can also be set in the environment as DISCOBOX_CHANNEL, DISCOBOX_VERSION,
# DISCOBOX_INSTALL_DIR, DISCOBOX_INSTALL_STAGE, or DISCOBOX_NO_MODIFY_PATH,
# which is the only way to pass one through `iex`. A parameter beats the
# environment, and a version beats a channel.
#
# This runs under Windows PowerShell 5.1 as well as PowerShell 7, and stays
# ASCII because 5.1 reads a file with no byte order mark in the ANSI code page.
# Everything happens inside Install-Discobox, so `iex` leaves no preference
# behind in the caller's session, and nothing here calls exit, which would
# close that session.

param(
    [string]$Channel,
    [string]$Version,
    [string]$InstallDir,
    [switch]$Stage,
    [switch]$NoModifyPath
)

function Install-Discobox {
    param(
        [string]$Channel,
        [string]$Version,
        [string]$InstallDir,
        [switch]$Stage,
        [switch]$NoModifyPath
    )

    Set-StrictMode -Version 3.0
    $ErrorActionPreference = 'Stop'
    # Invoke-WebRequest in 5.1 downloads many times slower while it draws
    # progress.
    $ProgressPreference = 'SilentlyContinue'

    # Stamped by internal/cmd/discobox-installers into the copy a release
    # uploads. Empty here in the source tree, where this script can only hand
    # over to a release's own installer.
    $release = ''
    $checksums = @{}

    # Where release assets come from, tried in order as <base>/<tag>/<asset>:
    # the mirror first and the release itself last, every one checked against
    # the same digest (ADR 0106). Overridable for a test or a private mirror.
    $sources = @('https://assets.discobox.ai/discobox', 'https://github.com/discobox-ai/discobox/releases/download')
    if ($env:DISCOBOX_INSTALL_SOURCES) {
        $sources = @($env:DISCOBOX_INSTALL_SOURCES -split '\s+' | Where-Object { $_ })
    }
    # Where a channel is looked up.
    $api = 'https://api.github.com/repos/discobox-ai/discobox'
    if ($env:DISCOBOX_INSTALL_API) { $api = $env:DISCOBOX_INSTALL_API }
    $releasesPage = 'https://github.com/discobox-ai/discobox/releases'

    # The mark, in 24-bit color and in the nearest xterm-256 indices, as base64
    # of the escape sequences. Written by `go generate ./installer` from the
    # TUI's own cell data; see internal/cmd/discobox-installer-logo. Base64
    # because this file stays ASCII, which is what 5.1 needs to read it.
    # BEGIN generated logo
    $logo24bit = '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'
    $logo256 = 'ICAgICAbWzM4OzU7OTJt4paX4paWG1swbQogICAgIBtbNzszODs1OzIwN23ilo0bWzBtG1szODs1OzIwN23ilosbWzBtCiAgICAgG1s3OzM4OzU7MjA3beKWjBtbMG0bWzM4OzU7MjA3beKWixtbMG0gIBtbMzg7NTsyMDdt4paB4paBG1swbQogICAgICAbWzc7Mzg7NTsyMDdt4paEG1swbRtbMzg7NTsyMDdt4paG4paHG1swbRtbNzszODs1OzIwN23iloTiloTiloPiloIbWzBtG1szODs1OzIwN23ilobiloXiloPiloLiloEbWzBtCiAgICAgICAbWzc7Mzg7NTsyMDdt4paOG1swbRtbMzg7NTsyMDdt4paMG1swbSAbWzM4OzU7MjA3beKWl+KWhOKWlhtbMG0gIBtbNzszODs1OzIwN23ilobiloXiloTiloPiloIbWzBtG1szODs1OzIwN23ilobiloQbWzBtCiAgICAgICAbWzc7Mzg7NTsyMDdt4paNG1swbRtbMzg7NTsyMDdt4paMG1swbSAbWzM4OzU7MjA3beKWnRtbMG0bWzc7Mzg7NTsyMDdt4paDG1swbRtbMzg7NTsyMDdt4paYG1swbRtbNzszODs1OzIwN23ilpgbWzBtG1szODs1OzIwN23ilofiloYbWzBtIBtbMzg7NTsyMDdt4paF4paFG1swbSAgG1s3OzM4OzU7MjA3beKWjhtbMG0bWzM4OzU7MjA3beKWjRtbMG0KICAgICAgICAbWzc7Mzg7NTsyMDdt4paWG1swbRtbMzg7NTsyMDdt4paEG1swbSAgIBtbNzszODs1OzIwN23ilofiloYbWzBtICAbWzc7Mzg7NTsyMDdt4paE4paEG1swbSAbWzM4OzU7MjA3beKWlxtbMG0bWzc7Mzg7NTsyMDdtIBtbMG0bWzM4OzU7OTJt4paOG1swbQogICAgICAgG1szODs1OzkybeKWgxtbMG0bWzM4OzU7MjA3beKWhBtbMG0bWzc7Mzg7NTsyMDdtIBtbMG0bWzM4OzU7MjA3beKWh+KWhOKWg+KWghtbMG0bWzM4OzU7OTJt4paBG1swbSAgG1szODs1OzIwN23iloHiloPiloYbWzBtG1s3OzM4OzU7MjA3beKWg+KWhuKWlhtbMG0bWzM4OzU7MjA3beKWlhtbMG0KICAgICAgG1s3OzM4OzU7MjA3beKWiyAgICAg4paB4paC4paDICAgICDilp0bWzBtG1szODs1OzkybeKWlhtbMG0bWzc7Mzg7NTsyMDdt4paDG1swbRtbMzg7NTsyMDdt4paYG1swbQogICAgICAbWzc7Mzg7NTsyMDdt4paMICAgICAbWzBtG1szODs1OzIwN23ilpYbWzBtICAbWzM4OzU7MjA3beKWnRtbMG0bWzc7Mzg7NTsyMDdtICAgICAgG1swbRtbMzg7NTsyMDdt4paEG1swbQogICAgICAbWzM4OzU7OTJt4padG1swbRtbNzszODs1OzIwN23iloXiloXiloXiloXiloUbWzBtG1s3OzM4OzU7OTJt4paFG1swbSAgIBtbMzg7NTs5Mm3ilp0bWzBtG1s3OzM4OzU7MjA3beKWheKWheKWheKWheKWheKWhRtbMG0K'
    # END generated logo

    if ($PSVersionTable.PSVersion.Major -lt 5) { throw 'this installer needs PowerShell 5.1 or later' }
    if ($PSVersionTable.PSEdition -ne 'Core') {
        # Windows PowerShell 5.1 can still default to TLS 1.0, which GitHub
        # refuses.
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    }

    # How this looks: the TUI's palette (cli/internal/tui/theme.go), and only
    # where the terminal will show it. A redirected stream, a console without
    # virtual terminal sequences, and NO_COLOR all get plain text; CLICOLOR_FORCE
    # or FORCE_COLOR turns it back on.
    function Get-DiscoboxStyle {
        $esc = [char]27
        $plain = @{ Depth = 0; Reset = ''; Dim = ''; Mark = ''; Ok = ''; Warn = ''; Err = ''; Step = '>'; Tick = '+'; Bang = '!'; Cross = 'x'; Unicode = $false }
        if ($env:NO_COLOR) { return $plain }
        if (-not ($env:CLICOLOR_FORCE -or $env:FORCE_COLOR)) {
            if ([Console]::IsOutputRedirected) { return $plain }
            $vt = $true
            try { $vt = $Host.UI.SupportsVirtualTerminal } catch { }
            if (-not $vt) { return $plain }
        }

        # Windows Terminal and any VT-capable console do 24-bit; the older
        # conhost that only just does sequences at all gets the 256 indices.
        $depth = 256
        if ($env:WT_SESSION -or $env:COLORTERM -eq 'truecolor' -or $env:COLORTERM -eq '24bit') { $depth = 16777216 }

        # Block characters come out as boxes in a console still on a legacy
        # code page, and the mark is nothing but block characters.
        $unicode = $true
        try { $unicode = [Console]::OutputEncoding.CodePage -eq 65001 } catch { }
        if ($env:WT_SESSION) { $unicode = $true }

        $style = @{
            Depth   = $depth
            Reset   = "$esc[0m"
            Dim     = "$esc[2m"
            Mark    = if ($depth -ge 16777216) { "$esc[38;2;244;92;255m" } else { "$esc[38;5;207m" }
            Ok      = "$esc[38;5;83m"
            Warn    = "$esc[38;5;214m"
            Err     = "$esc[38;5;196m"
            Unicode = $unicode
        }
        if ($unicode) {
            $style.Step = [char]0x2192
            $style.Tick = [char]0x2713
            $style.Bang = [char]0x26a0
            $style.Cross = [char]0x2717
        } else {
            $style.Step = '>'; $style.Tick = '+'; $style.Bang = '!'; $style.Cross = 'x'
        }
        return $style
    }

    function Write-DiscoboxStep([string]$Message) {
        if ($style.Depth -gt 0) { Write-Host "$($style.Dim)$($style.Step) $Message$($style.Reset)" } else { Write-Host $Message }
    }

    function Write-DiscoboxOk([string]$Message) {
        if ($style.Depth -gt 0) { Write-Host "$($style.Ok)$($style.Tick)$($style.Reset) $Message" } else { Write-Host $Message }
    }

    function Write-DiscoboxWarn([string]$Message) {
        if ($style.Depth -gt 0) { Write-Host "$($style.Warn)$($style.Bang)$($style.Reset) $Message" } else { Write-Host $Message }
    }

    # The mark, on the terms the TUI draws it on: it is shading rather than line
    # art, so a terminal that cannot color it gets none of it. It also needs the
    # room and the encoding.
    function Show-DiscoboxLogo {
        if ($style.Depth -lt 256 -or -not $style.Unicode) { return }
        $width = 80
        try { $width = [Console]::WindowWidth } catch { }
        if ($width -lt 29) { return }
        $art = if ($style.Depth -ge 16777216) { $logo24bit } else { $logo256 }
        if (-not $art) { return }
        Write-Host ''
        Write-Host ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($art)))
    }

    function Get-HttpStatus($ErrorRecord) {
        # 5.1 and 7 put different response types here, but both have a
        # StatusCode; strict mode forbids asking an exception that has neither.
        $response = $ErrorRecord.Exception.PSObject.Properties['Response']
        if ($response -and $response.Value) { return [int]$response.Value.StatusCode }
        return 0
    }

    # The newer of two versions by MAJOR.MINOR.PATCH.
    function Test-DiscoboxNewer([string]$A, [string]$B) {
        $core = { param($v) [version](($v -replace '^v', '') -replace '[-+].*$', '') }
        return (& $core $A) -gt (& $core $B)
    }

    # The release a channel is at, with the same rules the Homebrew tap uses
    # for its two formulae, plus edge (ADR 0110 section 3).
    function Resolve-DiscoboxChannel([string]$Name) {
        try {
            $list = Invoke-RestMethod -Uri "$api/releases?per_page=100" -Headers @{ Accept = 'application/vnd.github+json' } -UseBasicParsing
        } catch {
            $status = Get-HttpStatus $_
            if ($status -eq 403 -or $status -eq 429) {
                throw "GitHub is rate limiting this address, so the $Name channel cannot be looked up right now; pin a release with -Version instead (see $releasesPage)"
            }
            throw "could not look up the $Name channel at ${api}: $($_.Exception.Message)"
        }
        # A release that states neither its tag nor whether it is a prerelease
        # is an answer this cannot read. install.sh refuses it because pairing
        # the two keys by hand would otherwise misreport every release after it;
        # this half parses structurally and could skip it, but the two scripts
        # answer the same way on the same input, so it refuses too.
        foreach ($entry in $list) {
            if (-not $entry.PSObject.Properties['tag_name'] -or -not $entry.PSObject.Properties['prerelease']) {
                throw "could not read the release list from ${api}: a release states no tag, or no prerelease flag. Pin a release with -Version instead (see $releasesPage)"
            }
        }
        # Only CLI tags count, newest first, as GitHub returns them.
        $releases = @($list | Where-Object { $_.tag_name -match '^v[0-9]' })
        $stable = $releases | Where-Object { -not $_.prerelease } | Select-Object -First 1
        $found = $null
        switch ($Name) {
            'stable' { $found = $stable }
            'latest' { $found = $releases | Where-Object { $_.tag_name -match '^v[0-9]+\.[0-9]+\.[0-9]+$' } | Select-Object -First 1 }
            'edge' {
                # The newer of the newest stable release and the newest
                # prerelease, which is what edge.discobox.ai works out from the
                # mirror's two aliases. A stable release is always a dot release,
                # so comparing version cores is enough, and stable wins a tie.
                $found = $stable
                $pre = $releases | Where-Object { $_.prerelease } | Select-Object -First 1
                if ($pre -and (-not $stable -or (Test-DiscoboxNewer $pre.tag_name $stable.tag_name))) { $found = $pre }
            }
        }
        if (-not $found) { throw "there is no release on the $Name channel yet" }
        Write-DiscoboxStep "the $Name channel is at $($found.tag_name)"
        return $found.tag_name
    }

    # Downloads <tag>/<asset> from the first source that has it and, given a
    # digest, whose bytes match it.
    function Get-DiscoboxFile([string]$Tag, [string]$Name, [string]$OutFile, [string]$Sha256) {
        foreach ($base in $sources) {
            $url = "$base/$Tag/$Name"
            try {
                Invoke-WebRequest -Uri $url -OutFile $OutFile -UseBasicParsing
            } catch {
                continue
            }
            if (-not $Sha256) { return $true }
            if ((Get-FileHash -Algorithm SHA256 -LiteralPath $OutFile).Hash.ToLowerInvariant() -eq $Sha256) { return $true }
            Write-DiscoboxWarn "$url is not the file $release was released with; trying the next source"
        }
        return $false
    }

    # The release's names for this machine, refusing one no release can run on.
    function Get-DiscoboxPlatform {
        $onWindows = ($PSVersionTable.PSEdition -ne 'Core') -or $IsWindows
        $arch = $null
        try { $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() } catch { }
        if (-not $arch -and $onWindows) {
            $arch = $env:PROCESSOR_ARCHITECTURE
            if ($env:PROCESSOR_ARCHITEW6432) { $arch = $env:PROCESSOR_ARCHITEW6432 }
        }
        switch -regex ($arch) {
            '^(x64|amd64)$' { $arch = 'amd64' }
            '^arm64$' { $arch = 'arm64' }
            default { throw "discobox is not released for $arch" }
        }
        if ($onWindows) { return @{ OS = 'windows'; Arch = $arch; Exe = '.exe' } }
        if ($IsMacOS) {
            if ($arch -eq 'amd64') {
                # A shell running under Rosetta reports x64 on Apple Silicon.
                $translated = ''
                try { $translated = "$(& sysctl -n sysctl.proc_translated 2>$null)" } catch { }
                if ($translated.Trim() -ne '1') { throw 'discobox needs an Apple Silicon Mac; it is not released for Intel Macs' }
                $arch = 'arm64'
            }
            return @{ OS = 'darwin'; Arch = $arch; Exe = '' }
        }
        if ($IsLinux) {
            # The loader release:binary names, since a release binary is
            # dynamic whatever CGO_ENABLED says and a musl system has none.
            $loader = '/lib/ld-linux-aarch64.so.1'
            if ($arch -eq 'amd64') { $loader = '/lib64/ld-linux-x86-64.so.2' }
            if (-not (Test-Path -LiteralPath $loader)) {
                throw "discobox needs glibc, and $loader is missing (a musl system such as Alpine cannot run it)"
            }
            return @{ OS = 'linux'; Arch = $arch; Exe = '' }
        }
        throw 'discobox is not released for this operating system'
    }

    # Puts the install directory on the user's PATH, for new terminals and for
    # this one.
    function Add-DiscoboxPath([string]$Dir) {
        if ((@($env:Path -split ';') -notcontains $Dir)) { $env:Path = "$env:Path;$Dir" }
        $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
        try {
            # Read unexpanded and written back as REG_EXPAND_SZ, so the entries
            # that use %VARIABLES% keep working. [Environment] would flatten
            # them into whatever they expand to today.
            $current = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
            $entries = @($current -split ';' | Where-Object { $_ })
            if ($entries -contains $Dir) { return }
            $key.SetValue('Path', (($entries + $Dir) -join ';'), [Microsoft.Win32.RegistryValueKind]::ExpandString)
        } finally {
            $key.Close()
        }
        # Writing the registry tells nobody. Setting a user variable through
        # [Environment] broadcasts WM_SETTINGCHANGE, which is what makes
        # Explorer, and every terminal it starts from now on, reread PATH;
        # clearing one that does not exist changes nothing else.
        [Environment]::SetEnvironmentVariable('DISCOBOX_INSTALL_REFRESH', $null, 'User')
        Write-DiscoboxOk "added $Dir to your PATH; open a new terminal to use discobox there"
    }

    function Install-DiscoboxRelease {
        $platform = Get-DiscoboxPlatform
        $asset = "discobox-$($platform.OS)-$($platform.Arch)$($platform.Exe)"
        if (-not $checksums.ContainsKey($asset)) { throw "$release has no build for $($platform.OS) on $($platform.Arch)" }

        Show-DiscoboxLogo
        # The name of the thing being installed, under its own mark. No symbol:
        # a tick here would claim something finished before anything has.
        if ($style.Depth -gt 0) { Write-Host "$($style.Mark)$([char]27)[1mdiscobox $release$($style.Reset)`n" }
        Write-DiscoboxStep "downloading discobox $release for $($platform.OS)/$($platform.Arch)"
        $downloaded = Join-Path $tmp $asset
        if (-not (Get-DiscoboxFile $release $asset $downloaded $checksums[$asset])) {
            throw "could not download $asset for $release from any source with the SHA-256 it was released with"
        }

        $dir = $InstallDir
        if (-not $dir) {
            if ($platform.OS -eq 'windows') {
                $dir = Join-Path $env:LOCALAPPDATA 'Programs\Discobox'
            } elseif ("$(& id -u)" -eq '0') {
                $dir = '/usr/local/bin'
            } else {
                $dir = Join-Path $HOME '.local/bin'
            }
        }
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $dir = (Resolve-Path -LiteralPath $dir).ProviderPath
        $name = "discobox$($platform.Exe)"
        $dest = Join-Path $dir $name
        # Copied in beside the destination and renamed over it, so a copy that
        # fails part way never leaves half a binary where discobox should be.
        $staged = Join-Path $dir ".$name.new"
        Copy-Item -LiteralPath $downloaded -Destination $staged -Force
        if ($platform.OS -ne 'windows') { & chmod 755 $staged }
        if ($platform.OS -eq 'windows' -and (Test-Path -LiteralPath $dest)) {
            # Windows will not overwrite an executable that is running, but it
            # will rename one.
            $old = "$dest.old"
            if (Test-Path -LiteralPath $old) {
                try { Remove-Item -LiteralPath $old -Force } catch { $old = "$dest.$([guid]::NewGuid().ToString('N')).old" }
            }
            Move-Item -LiteralPath $dest -Destination $old -Force
        }
        Move-Item -LiteralPath $staged -Destination $dest -Force

        try {
            $out = (& $dest --version | Out-String).Trim()
        } catch {
            throw "installed $dest, but it does not run: $($_.Exception.Message)"
        }
        if ($LASTEXITCODE -ne 0) { throw "installed $dest, but it does not run: $out" }
        if (-not $out.Contains($release)) { throw "installed $dest, but it says it is '$out' rather than $release" }
        Write-DiscoboxOk "installed discobox $release to $dest"

        if ($Stage) {
            # The install itself has already succeeded, so a failure here says
            # so rather than reading as a broken install, but it is still a
            # failure, because it is what was asked for.
            Write-DiscoboxStep "staging the server discobox $release runs"
            & $dest admin server stage
            if ($LASTEXITCODE -ne 0) {
                throw "discobox $release is installed at $dest, but staging its server failed. Run '$dest admin server stage' to try again."
            }
        } else {
            # Where the other half comes from, as the Homebrew formula's caveats
            # say it. The server is not in this download and never was: the CLI
            # fetches the one it was cut against, checked against digests it
            # carries (ADR 0099).
            Write-Host ''
            Write-Host 'The Discobox server is a separate program. The CLI downloads the one it'
            Write-Host 'was cut against the first time something needs a server on this machine,'
            Write-Host 'keeps it under your state directory by version, and checks it against the'
            Write-Host 'SHA-256 it carries for it. To do that now rather than then:'
            Write-Host ''
            Write-Host "  $dest admin server stage      download and verify it now"
            Write-Host "  $dest admin server manifest   show exactly what that fetches"
        }

        $separator = [IO.Path]::PathSeparator
        if (@($env:PATH -split $separator) -notcontains $dir) {
            if ($platform.OS -eq 'windows' -and -not $NoModifyPath) {
                Add-DiscoboxPath $dir
            } else {
                Write-DiscoboxWarn "$dir is not on your PATH; add it to use discobox by name"
            }
        }
        $first = Get-Command discobox -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($first -and $first.Source -ne $dest) {
            Write-DiscoboxWarn "$($first.Source) comes before it on your PATH, so that is the one 'discobox' runs"
        }
    }

    if ($Version) {
        $Channel = ''
    } elseif (-not $Channel) {
        if ($env:DISCOBOX_VERSION) { $Version = $env:DISCOBOX_VERSION }
        elseif ($env:DISCOBOX_CHANNEL) { $Channel = $env:DISCOBOX_CHANNEL }
    }
    if (-not $InstallDir -and $env:DISCOBOX_INSTALL_DIR) { $InstallDir = $env:DISCOBOX_INSTALL_DIR }
    if ($env:DISCOBOX_NO_MODIFY_PATH) { $NoModifyPath = $true }
    if ($env:DISCOBOX_INSTALL_STAGE) { $Stage = $true }

    if ($Version) {
        if ($Version -notmatch '^v') { $Version = "v$Version" }
        if ($Version -notmatch '^v[0-9][0-9A-Za-z.+-]*$') { throw "$Version is not a release version, such as v0.7.1" }
    }
    if ($Channel -and @('stable', 'latest', 'edge') -notcontains $Channel) {
        throw "there is no channel called ${Channel}: choose stable, latest, or edge"
    }

    $style = Get-DiscoboxStyle

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('discobox-install-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        if ($Version) { $target = $Version }
        elseif ($Channel) { $target = Resolve-DiscoboxChannel $Channel }
        elseif ($release) { $target = $release }
        else { $target = Resolve-DiscoboxChannel 'stable' }

        if ($target -eq $release) {
            Install-DiscoboxRelease
            return
        }
        # A release's installer is always asked for its own release, so a
        # second hand-over means that release uploaded the wrong script. Stop
        # rather than follow it anywhere.
        if ($env:DISCOBOX_INSTALL_DELEGATED) {
            throw "the installer $($env:DISCOBOX_INSTALL_DELEGATED) uploaded is stamped for '$release'"
        }

        Write-DiscoboxStep "installing $target with the installer it was released with"
        $script = Join-Path $tmp 'install.ps1'
        if (-not (Get-DiscoboxFile $target 'install.ps1' $script '')) {
            throw "$target has no installer: there is no such release, or it came before install.ps1 did (see $releasesPage/tag/$target)"
        }
        $params = @{ Version = $target }
        if ($InstallDir) { $params['InstallDir'] = $InstallDir }
        if ($NoModifyPath) { $params['NoModifyPath'] = $true }
        if ($Stage) { $params['Stage'] = $true }
        # A script block rather than the file, because an execution policy may
        # refuse to run a downloaded .ps1 and never refuses this, which is also
        # how `iex` ran the first one.
        $env:DISCOBOX_INSTALL_DELEGATED = $target
        try {
            & ([scriptblock]::Create((Get-Content -Raw -LiteralPath $script))) @params
        } finally {
            Remove-Item Env:\DISCOBOX_INSTALL_DELEGATED -ErrorAction SilentlyContinue
        }
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Install-Discobox -Channel $Channel -Version $Version -InstallDir $InstallDir -Stage:$Stage -NoModifyPath:$NoModifyPath
