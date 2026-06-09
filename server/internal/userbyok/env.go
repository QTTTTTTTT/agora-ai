package userbyok

import "os"

// getOSEnv is the indirection target getEnv defaults to. Lives in
// its own file so tests can rebind getEnv to a fake without
// touching the rest of the package.
func getOSEnv(key string) string {
	return os.Getenv(key)
}
