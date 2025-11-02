package sse

import (
	"sync"

	"ella.to/pipe/signal"
)

type Mapper struct {
	mtx    sync.Mutex
	values map[string]chan *signal.Msg
}

func (m *Mapper) Get(inbox string) chan *signal.Msg {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	ch, ok := m.values[inbox]
	if !ok {
		ch = make(chan *signal.Msg, 16)
		m.values[inbox] = ch
	}

	return ch
}

func (m *Mapper) Delete(inbox string) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	ch, ok := m.values[inbox]
	if !ok {
		return
	}

	close(ch)
	delete(m.values, inbox)
}

func NewMapper() *Mapper {
	return &Mapper{
		values: make(map[string]chan *signal.Msg),
	}
}
