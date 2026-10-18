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
	"time"
)

// budgetRetry is how soon a held connection waiting for the shared buffer
// budget looks again. Nothing tells it when another connection frees some.
const budgetRetry = 100 * time.Millisecond

// clientStream reads a caller in the background while its connection is
// held, so a caller who sends a request body and leaves is noticed at once,
// rather than when the workload is Ready or maxWait runs out. Reading to the
// caller's EOF means buffering what it sent: up to the limits' per-connection
// cap, and within their budget shared by every held connection. A caller
// past either stops being read until its connection is passed on, or the
// budget frees, as it would without the doorman, and its leaving goes
// unnoticed until then.
//
// What's buffered is kept in chunks of clientBufferSize, and the budget
// counts whole chunks, the memory really held. A held connection's first
// chunk isn't counted, since maxHeld already bounds it, so a caller who
// sent less than a chunk and left is noticed even once the budget is spent.
//
// A caller's EOF means it left, whatever its protocol: over TCP, closing a
// connection and shutting down its sending side are the same FIN, and
// nearly every caller that sends one has given up, such as a database
// driver whose connect timed out.
//
// Once the connection is passed on, the stream is the proxy's reader, and
// buffers no more than one read at a time.
type clientStream struct {
	conn      net.Conn
	admission *admission

	mu      sync.Mutex
	changed *sync.Cond
	// chunks hold what the caller sent and hasn't been passed on, from
	// chunks[0][off:]. The caller is read into the last one's spare
	// capacity; reading is set while that's under way.
	chunks  [][]byte
	off     int
	reading bool
	err     error
	held    bool
	counted int
	closed  bool
	retry   *time.Timer

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
	for {
		into, ok := s.awaitRoom()
		if !ok {
			return
		}
		n, err := s.conn.Read(into)

		s.mu.Lock()
		s.reading = false
		if last := len(s.chunks) - 1; !s.closed {
			s.chunks[last] = s.chunks[last][:len(s.chunks[last])+n]
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

// awaitRoom waits until another read fits, and returns the spare capacity
// to read it into.
func (s *clientStream) awaitRoom() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed {
		if into, ok := s.room(); ok {
			s.reading = true
			return into, true
		}
		s.changed.Wait()
	}
	return nil, false
}

// room is the spare capacity of the last chunk, or of a new one when the
// limits allow it. s.mu must be held.
func (s *clientStream) room() ([]byte, bool) {
	if !s.held && s.unread() > 0 {
		return nil, false
	}
	s.compact()
	if n := len(s.chunks); n > 0 {
		if last := s.chunks[n-1]; len(last) < cap(last) {
			return last[len(last):cap(last)], true
		}
	}
	if s.held && len(s.chunks) > 0 {
		if (len(s.chunks)+1)*clientBufferSize > s.admission.limits.maxBufferedPerConn {
			return nil, false
		}
		if !s.admission.reserveBuffer(clientBufferSize) {
			s.retryLater()
			return nil, false
		}
		s.counted += clientBufferSize
	}
	chunk := make([]byte, 0, clientBufferSize)
	s.chunks = append(s.chunks, chunk)
	return chunk[:clientBufferSize], true
}

// retryLater wakes the stream after budgetRetry, to look for budget again.
// s.mu must be held.
func (s *clientStream) retryLater() {
	if s.retry != nil {
		s.retry.Reset(budgetRetry)
		return
	}
	s.retry = time.AfterFunc(budgetRetry, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.changed.Broadcast()
	})
}

// unread is how much has been read from the caller and not passed on.
// s.mu must be held.
func (s *clientStream) unread() int {
	n := -s.off
	for _, c := range s.chunks {
		n += len(c)
	}
	return n
}

// compact drops the chunks passed on in full, and empties the last one for
// reuse once it has been and nothing is being read into it. s.mu must be
// held.
func (s *clientStream) compact() {
	for len(s.chunks) > 1 && s.off == len(s.chunks[0]) {
		s.chunks[0] = nil
		s.chunks = s.chunks[1:]
		s.off = 0
	}
	if len(s.chunks) == 1 && s.off == len(s.chunks[0]) && !s.reading {
		s.chunks[0] = s.chunks[0][:0]
		s.off = 0
	}
}

// Read returns what the caller sent, in order, and its EOF once it's all
// been read.
func (s *clientStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.passedOn()
	for s.unread() == 0 && s.err == nil && !s.closed {
		s.changed.Wait()
	}
	if s.unread() == 0 {
		if s.closed {
			return 0, io.ErrClosedPipe
		}
		return 0, s.err
	}
	s.compact()
	n := copy(p, s.chunks[0][s.off:])
	s.off += n
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
	s.chunks, s.off = nil, 0
	if s.retry != nil {
		s.retry.Stop()
	}
	s.changed.Broadcast()
}
