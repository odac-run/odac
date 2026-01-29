package proxy

import (
	"testing"
)

func TestBufferPool(t *testing.T) {
	bp := bufferPool{}

	// Test Get
	buf := bp.Get()
	if len(buf) != proxyBufferSize {
		t.Errorf("Expected buffer size %d, got %d", proxyBufferSize, len(buf))
	}
	if cap(buf) < proxyBufferSize {
		t.Errorf("Expected buffer cap >= %d, got %d", proxyBufferSize, cap(buf))
	}

	// Test Put and Reuse
	// We want to ensure that putting it back and getting it doesn't panic
	// and returns a valid slice.
	buf[0] = 0xAA
	bp.Put(buf)

	buf2 := bp.Get()
	if len(buf2) != proxyBufferSize {
		t.Errorf("Expected buffer size %d, got %d", proxyBufferSize, len(buf2))
	}

	// Clean up
	bp.Put(buf2)
}
