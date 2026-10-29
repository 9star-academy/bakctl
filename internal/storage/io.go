package storage

import (
	"context"
	"io"
)

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type contextReadCloser struct {
	contextReader
	closer io.Closer
}

func (r *contextReadCloser) Close() error { return r.closer.Close() }
