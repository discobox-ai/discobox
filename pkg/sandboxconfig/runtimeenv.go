package sandboxconfig

import "strings"

// runtimeEnvNames are the variables a discobox's own processes run on, upper
// case: where a program is found and loaded from, who runs it and where home
// is, the terminal and locale, the egress proxy every request leaves through
// and the CA bundle that trusts it (what the pool sets in every discobox, and
// sudo carries to root), the discobox's own address and its API and
// credential endpoints, and their
// Windows counterparts. A credential's sentinel in one of them breaks every
// process the discobox starts (#1545).
//
// A variable the pool or the sandbox agent starts setting in every discobox
// joins this list in the same change; tests in both hold them to it.
var runtimeEnvNames = map[string]struct{}{
	"PATH": {}, "HOME": {}, "SHELL": {}, "USER": {}, "LOGNAME": {},
	"TERM": {}, "PWD": {}, "TMPDIR": {}, "LANG": {},
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "ALL_PROXY": {}, "NO_PROXY": {},
	"SSL_CERT_FILE": {}, "SSL_CERT_DIR": {}, "NODE_EXTRA_CA_CERTS": {}, "REQUESTS_CA_BUNDLE": {},
	"PIP_CERT": {}, "CURL_CA_BUNDLE": {}, "GIT_SSL_CAINFO": {}, "NODE_USE_ENV_PROXY": {},
	"DISCOBOX_API_URL": {}, "DISCOBOX_SERVER": {}, "DISCOBOX_CREDENTIALS_URL": {}, "DISCOBOX_ADDRESS": {},
	"SYSTEMROOT": {}, "WINDIR": {}, "COMSPEC": {}, "PATHEXT": {}, "USERPROFILE": {},
	"TEMP": {}, "TMP": {}, "APPDATA": {}, "LOCALAPPDATA": {},
}

// runtimeEnvPrefixes are the families of variables the dynamic loader reads,
// on Linux and on macOS, and the discobox user's identity the sandbox agent
// hands every process.
var runtimeEnvPrefixes = []string{"LD_", "DYLD_", "DISCOBOX_USER_"}

// RuntimeEnvVar reports whether name is a variable the discobox runs on, so
// that no credential may be delivered in it: a binding, a discobox's ask and a
// harness's declared secret are refused one, and a binding stored before the
// refusal is not delivered. Case does not matter: the proxy variables are read
// in either, and a Windows discobox's variables are one name in any case.
func RuntimeEnvVar(name string) bool {
	name = strings.ToUpper(name)
	if _, ok := runtimeEnvNames[name]; ok {
		return true
	}
	for _, prefix := range runtimeEnvPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
