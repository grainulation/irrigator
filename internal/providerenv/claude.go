package providerenv

import "os"

// Claude preserves the operating-system context required by the CLI and its
// existing home/keychain login. Credentials for unrelated services and runtime
// control tokens are not inherited. The returned slice is non-nil even when no
// baseline variables are set: a nil exec.Cmd.Env would inherit the entire parent.
func Claude() []string {
	env := make([]string, 0, 6)
	for _, name := range []string{"PATH", "HOME", "USER", "TMPDIR", "TEMP", "SystemRoot"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}
