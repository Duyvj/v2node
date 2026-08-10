package dispatcher

import (
	"sync"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
)

type ManagedWriter struct {
	writer    buf.Writer
	manager   *LinkManager
	closeOnce sync.Once
	closeErr  error
}

func (w *ManagedWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	return w.writer.WriteMultiBuffer(mb)
}

func (w *ManagedWriter) Close() error {
	w.closeOnce.Do(func() {
		w.manager.removeWriter(w)
		w.closeErr = common.Close(w.writer)
	})
	return w.closeErr
}

// LinkManager owns all live links for one exact user generation. Empty
// managers retire themselves from the dispatcher's map so peak connection
// storms do not leave permanent map buckets behind.
type LinkManager struct {
	key   string
	owner *sync.Map

	links  map[*ManagedWriter]buf.Reader
	mu     sync.Mutex
	closed bool
}

func newLinkManager(owner *sync.Map, key string) *LinkManager {
	return &LinkManager{
		key:   key,
		owner: owner,
		links: make(map[*ManagedWriter]buf.Reader),
	}
}

func (m *LinkManager) addLink(writer *ManagedWriter, reader buf.Reader) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.links[writer] = reader
	return true
}

func (m *LinkManager) removeWriter(writer *ManagedWriter) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	delete(m.links, writer)
	retire := len(m.links) == 0
	if retire {
		m.closed = true
		m.links = nil
	}
	m.mu.Unlock()
	if retire {
		m.owner.CompareAndDelete(m.key, m)
	}
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
	m.mu.Unlock()
	m.owner.CompareAndDelete(m.key, m)

	for writer, reader := range links {
		common.Close(writer.writer)
		common.Interrupt(reader)
	}
}

func (m *LinkManager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.links)
}
