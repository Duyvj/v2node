package splithttp

// upload_queue is a specialized priority queue + channel to reorder generic
// packets by a sequence number.

import (
	"container/heap"
	"io"
	"sync"
	"sync/atomic"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/signal/done"
)

const maxUploadQueuePackets = 256

var (
	errPacketQueueClosed = errors.New("packet queue closed")
	errPacketQueueFull   = errors.New("packet queue is full")
	errDuplicatePacket   = errors.New("duplicate or already consumed packet sequence")
)

type Packet struct {
	Reader  *httpServerConn
	Payload []byte
	Seq     uint64

	reservation *packetReservation
}

func (p *Packet) release() {
	if p.reservation != nil {
		p.reservation.release()
		p.reservation = nil
	}
	p.Payload = nil
	p.Reader = nil
}

type packetReservation struct {
	queue *uploadQueue
	seq   uint64

	bytesMu sync.Mutex
	bytes   *byteReservation

	released atomic.Bool
	enqueued atomic.Bool
}

func (r *packetReservation) release() {
	if r == nil || !r.released.CompareAndSwap(false, true) {
		return
	}
	r.queue.mu.Lock()
	delete(r.queue.reservations, r)
	if r.queue.sequences[r.seq] == r {
		delete(r.queue.sequences, r.seq)
	}
	r.queue.mu.Unlock()
	r.bytesMu.Lock()
	bytes := r.bytes
	r.bytes = nil
	r.bytesMu.Unlock()
	if bytes != nil {
		bytes.release()
	}
}

func (r *packetReservation) attachBytes(bytes *byteReservation) bool {
	r.bytesMu.Lock()
	if r.released.Load() || r.bytes != nil {
		r.bytesMu.Unlock()
		return false
	}
	r.bytes = bytes
	r.bytesMu.Unlock()
	return true
}

type uploadQueue struct {
	reader      atomic.Pointer[httpServerConn]
	readerReady chan struct{}

	// Packet count is reserved before a handler reads a body. reservations is
	// therefore the combined channel + heap + in-progress-body budget.
	mu           sync.Mutex
	reservations map[*packetReservation]struct{}
	sequences    map[uint64]*packetReservation
	nextSeq      uint64
	closedFlag   bool

	readMu        sync.Mutex
	pushedPackets chan Packet
	heap          uploadHeap
	maxPackets    int
	closed        *done.Instance
	closeOnce     sync.Once
	closeErr      error
}

func NewUploadQueue(maxPackets int) *uploadQueue {
	if maxPackets <= 0 {
		maxPackets = 1
	}
	if maxPackets > maxUploadQueuePackets {
		maxPackets = maxUploadQueuePackets
	}
	return &uploadQueue{
		readerReady:   make(chan struct{}),
		pushedPackets: make(chan Packet, maxPackets),
		heap:          uploadHeap{},
		nextSeq:       0,
		closed:        done.New(),
		maxPackets:    maxPackets,
		reservations:  make(map[*packetReservation]struct{}),
		sequences:     make(map[uint64]*packetReservation),
	}
}

func (h *uploadQueue) reserve(seq uint64, bytes *byteReservation) (*packetReservation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closedFlag {
		return nil, errPacketQueueClosed
	}
	if seq < h.nextSeq || h.sequences[seq] != nil {
		return nil, errDuplicatePacket
	}
	if len(h.reservations) >= h.maxPackets {
		return nil, errPacketQueueFull
	}
	reservation := &packetReservation{queue: h, seq: seq, bytes: bytes}
	h.reservations[reservation] = struct{}{}
	h.sequences[seq] = reservation
	return reservation, nil
}

func (h *uploadQueue) Push(p Packet) error {
	if p.Reader != nil {
		h.mu.Lock()
		if h.closedFlag {
			h.mu.Unlock()
			_ = p.Reader.Close()
			return errPacketQueueClosed
		}
		if !h.reader.CompareAndSwap(nil, p.Reader) {
			h.mu.Unlock()
			_ = p.Reader.Close()
			return errors.New("h.reader already exists")
		}
		close(h.readerReady)
		h.mu.Unlock()
		return nil
	}

	reservation := p.reservation
	if reservation == nil {
		var err error
		reservation, err = h.reserve(p.Seq, nil)
		if err != nil {
			p.release()
			return err
		}
		p.reservation = reservation
	}
	if reservation.queue != h || reservation.seq != p.Seq || reservation.released.Load() || !reservation.enqueued.CompareAndSwap(false, true) {
		p.release()
		return errDuplicatePacket
	}

	h.mu.Lock()
	if h.closedFlag || reservation.released.Load() {
		h.mu.Unlock()
		p.release()
		return errPacketQueueClosed
	}
	select {
	case h.pushedPackets <- p:
		h.mu.Unlock()
		return nil
	default:
		// This should be unreachable because the reservation budget includes
		// both this channel and the heap. Keep it defensive and release all
		// ownership if an invariant is ever violated.
		h.mu.Unlock()
		p.release()
		return errPacketQueueFull
	}
}

func (h *uploadQueue) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closedFlag = true
		_ = h.closed.Close()
		reader := h.reader.Load()
		h.mu.Unlock()

		if reader != nil {
			h.closeErr = reader.Close()
		}

		// Serialize with Read before clearing backing storage. This guarantees
		// no packet is still being copied when its reservation is released.
		h.readMu.Lock()
		for {
			select {
			case packet := <-h.pushedPackets:
				packet.release()
			default:
				goto drained
			}
		}
	drained:
		for i := range h.heap {
			h.heap[i].release()
			h.heap[i] = Packet{}
		}
		h.heap = nil
		h.pushedPackets = nil
		h.readMu.Unlock()
		h.reader.Store(nil)

		// Include reservations held by handlers that had not reached Push.
		h.mu.Lock()
		remaining := make([]*packetReservation, 0, len(h.reservations))
		for reservation := range h.reservations {
			remaining = append(remaining, reservation)
		}
		h.mu.Unlock()
		for _, reservation := range remaining {
			reservation.release()
		}
		h.mu.Lock()
		h.reservations = nil
		h.sequences = nil
		h.mu.Unlock()
	})
	return h.closeErr
}

func (h *uploadQueue) Read(b []byte) (int, error) {
	h.readMu.Lock()
	defer h.readMu.Unlock()

	if reader := h.reader.Load(); reader != nil {
		return reader.Read(b)
	}

	for {
		if len(h.heap) == 0 {
			select {
			case <-h.readerReady:
				reader := h.reader.Load()
				if reader == nil {
					return 0, io.EOF
				}
				return reader.Read(b)
			case packet := <-h.pushedPackets:
				heap.Push(&h.heap, packet)
			case <-h.closed.Wait():
				return 0, io.EOF
			}
		}

		packet := heap.Pop(&h.heap).(Packet)
		if packet.Seq == h.currentNextSeq() {
			n := copy(b, packet.Payload)
			if n < len(packet.Payload) {
				packet.Payload = packet.Payload[n:]
				heap.Push(&h.heap, packet)
				return n, nil
			}

			h.advance(packet.Seq + 1)
			packet.release()
			if n > 0 {
				return n, nil
			}
			// Empty packets advance the sequence but carry no readable data.
			continue
		}

		// The smallest packet is ahead of nextSeq. Put it back and wait for
		// another reserved packet. The global reservation cap prevents heap
		// growth while the missing packet is slow or absent.
		heap.Push(&h.heap, packet)
		select {
		case packet = <-h.pushedPackets:
			heap.Push(&h.heap, packet)
		case <-h.closed.Wait():
			return 0, io.EOF
		}
	}
}

func (h *uploadQueue) currentNextSeq() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nextSeq
}

func (h *uploadQueue) advance(next uint64) {
	h.mu.Lock()
	if next > h.nextSeq {
		h.nextSeq = next
	}
	h.mu.Unlock()
}

func (h *uploadQueue) reservationCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.reservations)
}

// heap code directly taken from https://pkg.go.dev/container/heap
type uploadHeap []Packet

func (h uploadHeap) Len() int           { return len(h) }
func (h uploadHeap) Less(i, j int) bool { return h[i].Seq < h[j].Seq }
func (h uploadHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *uploadHeap) Push(x any) {
	*h = append(*h, x.(Packet))
}

func (h *uploadHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = Packet{}
	if n == 1 {
		*h = nil
	} else {
		*h = old[:n-1]
	}
	return x
}
