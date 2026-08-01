//go:build !linux

package swap

// Non-Linux builds never manage swap: the snapshot is always !ok, so decide()
// holds and no actuator runs. Swap is a Linux host feature (see SWAP_PLAN.md).

func readSnapshot(dir string) snapshot { return snapshot{} }

// newController returns a no-op actuator on non-Linux hosts.
func newController() controller { return noopController{} }
