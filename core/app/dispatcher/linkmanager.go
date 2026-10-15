package dispatcher

import (
	"sync"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
)

const minLinkMapShrinkSize = 64

type ManagedWriter struct {
	writer  buf.Writer
	manager *LinkManager
}

func (w *ManagedWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	return w.writer.WriteMultiBuffer(mb)
}

func (w *ManagedWriter) Close() error {
	w.manager.RemoveWriter(w)
	return common.Close(w.writer)
}

type LinkManager struct {
	links     map[*ManagedWriter]buf.Reader
	mu        sync.Mutex
	highWater int
	closed    bool
}

func newLinkManager() *LinkManager {
	return &LinkManager{}
}

func (m *LinkManager) AddLink(writer *ManagedWriter, reader buf.Reader) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	if m.links == nil {
		m.links = make(map[*ManagedWriter]buf.Reader)
	}
	m.links[writer] = reader
	if count := len(m.links); count > m.highWater {
		m.highWater = count
	}
	return true
}

func (m *LinkManager) RemoveWriter(writer *ManagedWriter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	delete(m.links, writer)
	count := len(m.links)
	if count == 0 {
		m.links = nil
		m.highWater = 0
		return
	}
	if m.highWater < minLinkMapShrinkSize || count*4 > m.highWater {
		return
	}

	shrunk := make(map[*ManagedWriter]buf.Reader, count)
	for activeWriter, reader := range m.links {
		shrunk[activeWriter] = reader
	}
	m.links = shrunk
	m.highWater = count
}

func (m *LinkManager) CloseAll() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true

	links := m.links
	m.links = nil
	m.highWater = 0
	m.mu.Unlock()

	for w, r := range links {
		common.Close(w.writer)
		common.Interrupt(r)
	}
}
