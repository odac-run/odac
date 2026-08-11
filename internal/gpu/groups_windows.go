package gpu

// deviceGID has no meaning on Windows, which has neither the DRM nodes nor
// POSIX group ownership.
func deviceGID(string) (int, bool) { return 0, false }
