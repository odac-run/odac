package auth

import (
	"testing"
)

func TestFirewall_NotBlockedByDefault(t *testing.T) {
	fw := NewFirewall()
	if fw.IsBlocked("192.168.1.1") {
		t.Error("new IP should not be blocked")
	}
}

func TestFirewall_BlockAfterMaxAttempts(t *testing.T) {
	fw := NewFirewall()
	ip := "10.0.0.1"

	// 5 attempts should not block (threshold is >5)
	for i := 0; i < 5; i++ {
		fw.HandleFailedAuth(ip)
	}
	if fw.IsBlocked(ip) {
		t.Error("IP should not be blocked after exactly 5 attempts")
	}

	// 6th attempt triggers block
	fw.HandleFailedAuth(ip)
	if !fw.IsBlocked(ip) {
		t.Error("IP should be blocked after 6 failed attempts")
	}
}

func TestFirewall_ManualBlock(t *testing.T) {
	fw := NewFirewall()
	ip := "172.16.0.1"

	fw.Block(ip, "Suspicious activity")
	if !fw.IsBlocked(ip) {
		t.Error("manually blocked IP should be blocked")
	}
}

func TestFirewall_ClearAttempts(t *testing.T) {
	fw := NewFirewall()
	ip := "10.0.0.2"

	// Accumulate some attempts
	for i := 0; i < 4; i++ {
		fw.HandleFailedAuth(ip)
	}

	// Successful login clears attempts
	fw.ClearAttempts(ip)

	// Should need 6 more attempts to block
	for i := 0; i < 5; i++ {
		fw.HandleFailedAuth(ip)
	}
	if fw.IsBlocked(ip) {
		t.Error("IP should not be blocked after clearing attempts and only 5 new failures")
	}
}

func TestFirewall_DifferentIPsIndependent(t *testing.T) {
	fw := NewFirewall()

	// Block IP1
	for i := 0; i < 6; i++ {
		fw.HandleFailedAuth("1.1.1.1")
	}

	if !fw.IsBlocked("1.1.1.1") {
		t.Error("1.1.1.1 should be blocked")
	}
	if fw.IsBlocked("2.2.2.2") {
		t.Error("2.2.2.2 should not be affected by 1.1.1.1's block")
	}
}
