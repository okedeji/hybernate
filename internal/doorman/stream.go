/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package doorman

import (
	"io"
	"net"
	"sync"
)

// clientStream reads a caller in the background while its connection is
// held, so a caller who sends a request body and leaves is noticed at once,
// rather than when the workload is Ready or maxWait runs out. Reading to the
// caller's EOF means buffering what it sent: up to the limits' per-connection
// cap, and within their budget shared by every held connection. A caller
// past either stops being read until its connection is passed on, as it
// would without the doorman, and its leaving goes unnoticed until then.
//
// Once the connection is passed on, the stream is the proxy's reader, and
// buffers no more than one read at a time.
type clientStream struct {
	conn      net.Conn
	admission *admission

	mu      sync.Mutex
	changed *sync.Cond
	buf     []byte
	err     error
	held    bool
	counted int
	closed  bool

	// gone is closed when reading the caller ends: it left, or its
	// connection failed.
	gone chan struct{}
}

func newClientStream(conn net.Conn, a *admission) *clientStream {
	s := &clientStream{conn: conn, admission: a, held: true, gone: make(chan struct{})}
	s.changed = sync.NewCond(&s.mu)
	return s
}

// run reads the caller until it ends or the stream is closed.
func (s *clientStream) run() {
	defer close(s.gone)
	chunk := make([]byte, clientBufferSize)
	for {
		reserved, ok := s.awaitRoom()
		if !ok {
			return
		}
		n, err := s.conn.Read(chunk)

		s.mu.Lock()
		if !s.closed {
			s.buf = append(s.buf, chunk[:n]...)
		}
		if s.held {
			s.counted += n
			s.admission.releaseBuffer(reserved - n)
		} else {
			s.admission.releaseBuffer(reserved)
		}
		if err != nil {
			s.err = err
		}
		s.changed.Broadcast()
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// awaitRoom waits until another read fits, and returns how much of the
// held connections' budget it reserved for it.
func (s *clientStream) awaitRoom() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed {
		limit := clientBufferSize
		if s.held {
			limit = s.admission.limits.maxBufferedPerConn
		}
		if len(s.buf)+clientBufferSize <= limit {
			if !s.held {
				return 0, true
			}
			if s.admission.reserveBuffer(clientBufferSize) {
				return clientBufferSize, true
			}
		}
		s.changed.Wait()
	}
	return 0, false
}

// Read returns what the caller sent, in order, and its EOF once it's all
// been read.
func (s *clientStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.passedOn()
	for len(s.buf) == 0 && s.err == nil && !s.closed {
		s.changed.Wait()
	}
	if len(s.buf) == 0 {
		if s.closed {
			return 0, io.ErrClosedPipe
		}
		return 0, s.err
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	if len(s.buf) == 0 {
		s.buf = nil
	}
	s.changed.Broadcast()
	return n, nil
}

// passedOn stops counting the stream against the held connections' budget,
// once it's being read by the proxy. s.mu must be held.
func (s *clientStream) passedOn() {
	if !s.held {
		return
	}
	s.held = false
	s.admission.releaseBuffer(s.counted)
	s.counted = 0
	s.changed.Broadcast()
}

// close ends the stream. Reading the caller stops at the next read, which
// closing the connection ends at once.
func (s *clientStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.passedOn()
	s.closed = true
	s.buf = nil
	s.changed.Broadcast()
}
