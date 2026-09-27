//go:build windows

package vault

// Windows protects the key with the account's own DPAPI and inherits the
// directory's ACL for the fallback key file; there is no mode to tighten.
func restrictKeyFile(string) {}
