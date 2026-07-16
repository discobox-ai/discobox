package cli

// serverAutoLaunch is overridden to "true" by the release build's linker
// flags. Development and other ordinary builds retain the disabled default.
var serverAutoLaunch = "false"
