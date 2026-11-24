package sandboxconfig

import "testing"

func TestRuntimeEnvVar(t *testing.T) {
	for _, name := range []string{"PATH", "HOME", "SHELL", "USER", "LOGNAME", "TERM", "PWD", "TMPDIR", "LANG",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "Path", "ld_preload",
		"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "DISCOBOX_SERVER", "DISCOBOX_CREDENTIALS_URL", "DISCOBOX_ADDRESS", "DISCOBOX_USER_HOME", "SystemRoot", "ComSpec", "USERPROFILE", "TEMP"} {
		if !RuntimeEnvVar(name) {
			t.Errorf("RuntimeEnvVar(%q) = false, want true", name)
		}
	}
	// A credential's usual names, and names that only look like one the
	// discobox runs on, are free.
	for _, name := range []string{"GITHUB_TOKEN", "ANTHROPIC_API_KEY", "MYPATH", "HOMEBREW_GITHUB_API_TOKEN", "OLD_PWD", "LDAP_PASSWORD", "DISCOBOX_TOKEN"} {
		if RuntimeEnvVar(name) {
			t.Errorf("RuntimeEnvVar(%q) = true, want false", name)
		}
	}
}
