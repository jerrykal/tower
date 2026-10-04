//go:build !linux && !darwin

package dirs

// SystemMounts knows no mount table on this platform: every path counts
// as local.
func SystemMounts() (Mounts, error) { return nil, nil }
