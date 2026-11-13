//go:build !linux

package proxy

import (
	"context"
	"errors"
	"net"
)

const pinnedDialSupported = false

func dialPinned(context.Context, string, string, string) (net.Conn, error) {
	return nil, errors.ErrUnsupported
}
