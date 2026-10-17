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
	if w.manager != nil {
		w.manager.RemoveWriter(w)
	}
	return common.Close(w.writer)
}

type LinkManager struct {
	links     map[*ManagedWriter]buf.Reader
	mu        sync.Mutex
	highWater int
	closed    bool
	onEmpty   func()
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
	if m.removeWriter(writer) && m.onEmpty != nil {
		m.onEmpty()
	}
}

func (m *LinkManager) removeWriter(writer *ManagedWriter) (empty bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	if _, ok := m.links[writer]; !ok {
		return false
	}
	delete(m.links, writer)
	count := len(m.links)
	if count == 0 {
		m.links = nil
		m.highWater = 0
		return true
	}
	if m.highWater < minLinkMapShrinkSize || count*4 > m.highWater {
		return false
	}

	shrunk := make(map[*ManagedWriter]buf.Reader, count)
	for activeWriter, reader := range m.links {
		shrunk[activeWriter] = reader
	}
	m.links = shrunk
	m.highWater = count
	return false
}

func (m *LinkManager) isEmpty() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.links) == 0
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
