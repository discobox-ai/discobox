package cli

import "strings"

// sshOptionsWithValue are ssh(1)'s short options that consume a value, either
// attached (`-L8080:localhost:80`) or as the next argument (`-L 8080:...`).
// Everything else ssh accepts is a boolean flag.
//
// The table exists because the user's arguments cannot be passed through as one
// opaque list: ssh's own usage is `ssh [options] host [command]`, and this
// command supplies the host. Options have to land before it and a remote
// command after it, which means telling them apart.
var sshOptionsWithValue = map[rune]bool{
	'B': true, 'b': true, 'c': true, 'D': true, 'E': true, 'e': true,
	'F': true, 'I': true, 'i': true, 'J': true, 'L': true, 'l': true,
	'm': true, 'O': true, 'o': true, 'P': true, 'p': true, 'Q': true,
	'R': true, 'S': true, 'W': true, 'w': true,
}

// splitSSHArgs divides the user's arguments into ssh options and the remote
// command, and reports whether they asked ssh to background itself.
//
// Placing them all after the host happens to work on glibc, whose getopt
// permutes argv, and silently sends every option to the remote as a command
// anywhere else. So options are separated here and passed before the host,
// leaving whatever follows the first non-option argument as the command.
//
// `--` ends the options explicitly and is not forwarded: ssh has no use for it
// once the host is supplied separately.
func splitSSHArgs(args []string) (options, command []string, background bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return options, args[i+1:], background
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			// The first non-option argument is where the remote command
			// starts, exactly as ssh itself would read it.
			return options, args[i:], background
		}
		options = append(options, arg)

		// Walk the bundle: -Nf is two booleans, -NL 8080:… ends in an option
		// whose value is the next argument, and -NL8080:… carries it inline.
		// Anything after a value-taking letter belongs to that value, which is
		// why the scan stops there — the `f` in `-L f:1:2` is not a flag.
		runes := []rune(arg[1:])
		for index, letter := range runes {
			if letter == 'f' {
				background = true
			}
			if sshOptionsWithValue[letter] {
				if index == len(runes)-1 && i+1 < len(args) {
					i++
					options = append(options, args[i])
				}
				break
			}
		}
	}
	return options, nil, background
}
