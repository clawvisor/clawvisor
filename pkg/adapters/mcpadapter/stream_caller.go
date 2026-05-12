package mcpadapter

import (
	"io"

	"github.com/clawvisor/clawvisor/pkg/adapters/mcpclient"
)

// streamCaller bundles the line-delimited JSON-RPC Client with the
// io.Closer that owns the underlying stream lifecycle (subprocess, pipe).
// Implements mcpclient.Caller so stdio and HTTP transports are interchangeable
// from the adapter's perspective.
type streamCaller struct {
	*mcpclient.Client
	closer io.Closer
}

func newStreamCaller(rwc io.ReadWriteCloser) *streamCaller {
	return &streamCaller{Client: mcpclient.New(rwc), closer: rwc}
}

// Close terminates the underlying stream, which ends the Client's read pump
// and unblocks any pending callers.
func (s *streamCaller) Close() error { return s.closer.Close() }

var _ mcpclient.Caller = (*streamCaller)(nil)
