//go:build !windows

package contractguard

// ReadRegistryProxies returns no entries: there is no Windows registry here.
func ReadRegistryProxies() (map[string]string, error) {
	return map[string]string{}, nil
}
