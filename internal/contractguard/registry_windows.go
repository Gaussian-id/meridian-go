//go:build windows

package contractguard

import (
	"encoding/binary"
	"errors"
	"strings"
	"syscall"
	"unicode/utf16"
)

const internetSettings = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

var errRegistryType = errors.New("contractguard: a proxy registry value has an unexpected type")

// ReadRegistryProxies returns the proxy entries of the Windows registry (Internet Settings), read
// the way Python's getproxies_registry reads them: nothing unless ProxyEnable is on; then
// ProxyServer through ParseProxyServer. An error means the registry cannot be asked, which the
// guard counts as a proxy.
func ReadRegistryProxies() (map[string]string, error) {
	path, err := syscall.UTF16PtrFromString(internetSettings)
	if err != nil {
		return nil, err
	}

	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, syscall.KEY_READ, &key); err != nil {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return map[string]string{}, nil
		}

		return nil, err
	}

	defer func() { _ = syscall.RegCloseKey(key) }()

	// [1] ProxyEnable: a non-zero DWORD, or (as Python reads it) any non-empty string, is on.
	raw, kind, found, err := queryValue(key, "ProxyEnable")
	if err != nil {
		return nil, err
	}

	if !found {
		return map[string]string{}, nil
	}

	var on bool

	switch kind {
	case syscall.REG_DWORD:
		on = len(raw) >= 4 && binary.LittleEndian.Uint32(raw) != 0
	case syscall.REG_SZ, syscall.REG_EXPAND_SZ:
		on = utf16String(raw) != ""
	default:
		return nil, errRegistryType
	}

	if !on {
		return map[string]string{}, nil
	}

	// [2] ProxyServer.
	raw, kind, found, err = queryValue(key, "ProxyServer")
	if err != nil {
		return nil, err
	}

	if !found {
		return map[string]string{}, nil
	}

	if kind != syscall.REG_SZ && kind != syscall.REG_EXPAND_SZ {
		return nil, errRegistryType
	}

	return ParseProxyServer(utf16String(raw)), nil
}

// queryValue reads one value: its bytes, its type and whether it exists.
func queryValue(key syscall.Handle, name string) ([]byte, uint32, bool, error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, 0, false, err
	}

	var kind, size uint32

	if err := syscall.RegQueryValueEx(key, p, nil, &kind, nil, &size); err != nil {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return nil, 0, false, nil
		}

		return nil, 0, false, err
	}

	buf := make([]byte, size)
	if size > 0 {
		if err := syscall.RegQueryValueEx(key, p, nil, &kind, &buf[0], &size); err != nil {
			return nil, 0, false, err
		}
	}

	return buf[:min(int(size), len(buf))], kind, true, nil
}

// utf16String decodes a registry string (UTF-16LE), dropping its terminating NULs.
func utf16String(raw []byte) string {
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}

	return strings.TrimRight(string(utf16.Decode(units)), "\x00")
}
