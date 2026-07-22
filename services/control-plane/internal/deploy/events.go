package deploy

import (
	"sync"
	"time"
)

// Event is one progress record in a run's stream.
type Event struct {
	Seq     int       `json:"seq"`
	Time    time.Time `json:"time"`
	Type    string    `json:"type"` // step-start | log | step-done | step-failed | deploy-done | deploy-failed
	Service string    `json:"service,omitempty"`
	Line    string    `json:"line,omitempty"`
}

// Run is one deploy/remove execution. Events are buffered for replay so an
// SSE subscriber that connects late still sees the full log.
type Run struct {
	ID       int      `json:"id"`
	Services []string `json:"services"`
	Skipped  []string `json:"skipped,omitempty"` // deps left out because already deployed
	Remove   bool     `json:"remove"`

	mu     sync.Mutex
	events []Event
	subs   []chan Event
	done   bool
	result string
}

func newRun(id int, services []string, remove bool) *Run {
	return &Run{ID: id, Services: services, Remove: remove}
}

func (r *Run) emit(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev.Seq = len(r.events)
	ev.Time = time.Now()
	r.events = append(r.events, ev)
	for _, ch := range r.subs {
		select {
		case ch <- ev:
		default: // a stalled subscriber must not block the deploy
		}
	}
}

func (r *Run) finish(result string) {
	r.emit(Event{Type: result})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	r.result = result
	for _, ch := range r.subs {
		close(ch)
	}
	r.subs = nil
}

// Done reports whether the run has finished.
func (r *Run) Done() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

// Result returns "" while running, else deploy-done or deploy-failed.
func (r *Run) Result() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result
}

// Subscribe returns a replay of past events and a channel of future ones.
// The channel is nil when the run has already finished.
func (r *Run) Subscribe() ([]Event, <-chan Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	replay := make([]Event, len(r.events))
	copy(replay, r.events)
	if r.done {
		return replay, nil
	}
	ch := make(chan Event, 256)
	r.subs = append(r.subs, ch)
	return replay, ch
}
