package acquisition

import "sync"

const (
	TaskEventSnapshot = "snapshot"
	TaskEventUpdated  = "updated"
	TaskEventRemoved  = "removed"

	taskEventBufferSize = 128
)

// TaskEvent is the small, transport-neutral message sent to task observers.
// Media is intentionally omitted from Task updates: the task dock only needs
// progress and identity fields, and a completed media payload can be large.
type TaskEvent struct {
	Type   string `json:"type"`
	Task   *Task  `json:"task,omitempty"`
	Tasks  []Task `json:"tasks,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

// SubscribeTaskEvents registers a non-blocking task observer and returns a
// consistent snapshot captured while the subscription is installed. A slow
// observer never stalls acquisition workers; its oldest buffered event is
// discarded to make room for the latest state, while the UI's periodic
// refresh remains the recovery path for an overflow or reconnect.
func (m *Manager) SubscribeTaskEvents() (<-chan TaskEvent, []Task, func()) {
	events := make(chan TaskEvent, taskEventBufferSize)
	if m == nil {
		close(events)
		return events, nil, func() {}
	}

	m.mu.Lock()
	if m.subscribers == nil {
		m.subscribers = make(map[chan TaskEvent]struct{})
	}
	m.subscribers[events] = struct{}{}
	snapshot := m.listLocked()
	m.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			m.mu.Lock()
			if _, ok := m.subscribers[events]; ok {
				delete(m.subscribers, events)
				close(events)
			}
			m.mu.Unlock()
		})
	}
	return events, snapshot, unsubscribe
}

func (m *Manager) publishTaskLocked(task Task) {
	if len(m.subscribers) == 0 {
		return
	}
	task.Media = nil
	event := TaskEvent{Type: TaskEventUpdated, Task: &task}
	for subscriber := range m.subscribers {
		nonBlockingTaskEvent(subscriber, event)
	}
}

func (m *Manager) publishTaskRemovedLocked(taskID string) {
	if len(m.subscribers) == 0 {
		return
	}
	event := TaskEvent{Type: TaskEventRemoved, TaskID: taskID}
	for subscriber := range m.subscribers {
		nonBlockingTaskEvent(subscriber, event)
	}
}

func nonBlockingTaskEvent(subscriber chan TaskEvent, event TaskEvent) {
	select {
	case subscriber <- event:
		return
	default:
	}
	// Keep the stream useful when a browser briefly stops reading: progress
	// updates are replaceable, and the next event is more valuable than an old
	// one. The periodic HTTP snapshot repairs any event lost at this boundary.
	select {
	case <-subscriber:
	default:
	}
	select {
	case subscriber <- event:
	default:
	}
}
