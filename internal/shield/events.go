package shield

import (
	"sync"
	"sync/atomic"
	"time"
)

// Event is a notable shield decision, shown in the panel's security log.
// Challenges aren't recorded: under attack mode every visitor gets one.
type Event struct {
	Time    time.Time `json:"time"`
	Site    string    `json:"site"`
	IP      string    `json:"ip"`
	Verdict string    `json:"verdict"`
	Reason  string    `json:"reason"`
	Path    string    `json:"path"`
}

// eventLog keeps the most recent events in a fixed ring: bounded memory no
// matter how hard the server is attacked.
type eventLog struct {
	mu   sync.Mutex
	buf  []Event
	next int
	full bool
	// tee, if set, also gets every event (log shipping). It runs on the
	// request path: it must not block.
	tee atomic.Pointer[func(Event)]
}

func newEventLog(n int) *eventLog { return &eventLog{buf: make([]Event, n)} }

func (l *eventLog) add(e Event) {
	if len(e.Path) > 200 {
		e.Path = e.Path[:200] + "…"
	}
	l.mu.Lock()
	l.buf[l.next] = e
	l.next = (l.next + 1) % len(l.buf)
	l.full = l.full || l.next == 0
	l.mu.Unlock()
	if f := l.tee.Load(); f != nil {
		(*f)(e)
	}
}

// recent returns events newest first, optionally only one site's.
func (l *eventLog) recent(site string, limit int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.next
	if l.full {
		n = len(l.buf)
	}
	out := []Event{}
	for i := 0; i < n && len(out) < limit; i++ {
		e := l.buf[(l.next-1-i+len(l.buf))%len(l.buf)]
		if site == "" || e.Site == site {
			out = append(out, e)
		}
	}
	return out
}
