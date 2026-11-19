package typhon

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

const cleanupTimeout = 2 * time.Second

type resourceCloser struct {
	name string
	run  func(context.Context) error
}

// The owner drains resources after its VM and tasks stop using them.
// Removing an explicitly closed handle also drops its registration immediately.
type resourceScope struct {
	mu      sync.Mutex
	closed  bool
	order   list.List
	entries map[any]*list.Element
	once    sync.Once
	err     error
}

func (s *resourceScope) add(ctx context.Context, key any, name string, close func(context.Context) error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.Join(errors.New("native resource scope is closed"), close(ctx))
	}
	if s.entries == nil {
		s.entries = make(map[any]*list.Element)
	}
	s.entries[key] = s.order.PushBack(resourceCloser{name: name, run: close})
	s.mu.Unlock()
	return nil
}

func (s *resourceScope) remove(key any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.entries[key]; entry != nil {
		s.order.Remove(entry)
		delete(s.entries, key)
	}
}

func (s *resourceScope) close(ctx context.Context) error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		var closers []resourceCloser
		for entry := s.order.Back(); entry != nil; entry = entry.Prev() {
			closers = append(closers, entry.Value.(resourceCloser))
		}
		s.order.Init()
		s.entries = nil
		s.mu.Unlock()
		var failures []error
		for _, closer := range closers {
			if err := closer.run(ctx); err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", closer.name, err))
			}
		}
		s.err = errors.Join(failures...)
	})
	return s.err
}

type requestScope struct {
	resources resourceScope
	tasks     []*Task // guarded by the Engine's execMu
}

func (s *requestScope) finish(e *Engine) error {
	e.execMu.Lock()
	tasks := s.tasks
	s.tasks = nil
	e.execMu.Unlock()
	for _, task := range tasks {
		<-task.done
	}
	e.execMu.Lock()
	defer e.execMu.Unlock()
	e.requestTasks -= len(tasks)
	var failures []error
	for _, task := range tasks {
		if err := task.result.err; err != nil && !task.observed && !errors.Is(err, context.Canceled) {
			failures = append(failures, fmt.Errorf("request task: %w", err))
		}
	}
	ctx, stop := context.WithTimeout(context.Background(), cleanupTimeout)
	defer stop()
	return errors.Join(errors.Join(failures...), s.resources.close(ctx))
}

func (vm *VM) resourceScope() *resourceScope {
	if vm.request != nil {
		return &vm.request.resources
	}
	return &vm.engine.resources
}

func (vm *VM) taskList() *[]*Task {
	if vm.request != nil {
		return &vm.request.tasks
	}
	return &vm.engine.tasks
}

func finalizeRun(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	return errors.Join(primary, fmt.Errorf("native cleanup: %w", cleanup))
}

func reportCleanupError(operation string, err error) {
	if err != nil {
		log.Printf("Typhon cleanup (%s): %v", operation, err)
	}
}
