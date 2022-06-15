package tickler

import (
	"container/list"
	"context"
	"log"
	"sync"
)

type (
	JobName            = string
	BackgroundFunction = func() error
)

const (
	defaultRequestLimit = 100
)

type ticklerEvent struct {
	fnOpts *ticklerFunctionOptions
	Job    JobName
	f      BackgroundFunction

	ch chan struct{}
}

type Request struct {
	F   BackgroundFunction
	Job JobName
}

func newTicklerEvent(request Request, opts ...TicklerFunctionOption) *ticklerEvent {
	t := &ticklerEvent{
		fnOpts: &ticklerFunctionOptions{},
		f:      request.F,
		Job:    request.Job,
		ch:     make(chan struct{}),
	}

	for _, opt := range opts {
		opt.apply(t.fnOpts)
	}

	return t
}

type ticklerFunctionOptions struct {
	waitFor []string
}

type TicklerFunctionOption interface {
	apply(*ticklerFunctionOptions)
}

type ticklerFunctionOption struct {
	f func(*ticklerFunctionOptions)
}

func (tfo *ticklerFunctionOption) apply(opts *ticklerFunctionOptions) {
	tfo.f(opts)
}

func newTicklerFunctionOption(f func(*ticklerFunctionOptions)) *ticklerFunctionOption {
	return &ticklerFunctionOption{
		f: f,
	}
}

func WaitForJobs(jobNames ...JobName) TicklerFunctionOption {
	return newTicklerFunctionOption(func(t *ticklerFunctionOptions) {
		t.waitFor = jobNames
	})
}

type TicklerOptions struct {
	sema chan int
}

type Tickler struct {
	mu         sync.Mutex
	queue      *list.List
	options    TicklerOptions
	loopSignal chan struct{}

	currentJobs map[JobName]bool
	jobsWaitFor map[JobName][]chan struct{}
}

func (s *Tickler) EnqueueRequest(request Request, opts ...TicklerFunctionOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.tickleLoop()

	ticklerEvent := newTicklerEvent(request, opts...)

	s.currentJobs[request.Job] = true

	for _, v := range ticklerEvent.fnOpts.waitFor {
		s.jobsWaitFor[v] = append(s.jobsWaitFor[v], ticklerEvent.ch)
	}

	s.queue.PushBack(ticklerEvent)
	log.Printf("Added request to queue with length %d\n", s.queue.Len())
}

func (s *Tickler) loop(ctx context.Context) {
	log.Println("Starting service loop")
	for {
		select {
		case <-s.loopSignal:
			s.tryDequeue()
		case <-ctx.Done():
			log.Printf("Loop context cancelled")
			return
		}
	}
}

func (s *Tickler) tryDequeue() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queue.Len() == 0 {
		return
	}

	select {
	case s.options.sema <- 1:
		request := s.dequeue()
		log.Printf("Dequeued request %v\n", request)
		go s.process(request)
	default:
		log.Printf("Received loop signal, but request limit is reached")
	}
}

func (s *Tickler) dequeue() *ticklerEvent {
	element := s.queue.Front()
	s.queue.Remove(element)
	return element.Value.(*ticklerEvent)
}

func (s *Tickler) removeJob(event *ticklerEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, v := range s.jobsWaitFor[event.Job] {
		v <- struct{}{}
	}

	delete(s.jobsWaitFor, event.Job)
	delete(s.currentJobs, event.Job)
}

func (s *Tickler) process(event *ticklerEvent) {
	defer s.replenish()
	defer s.removeJob(event)

	cnt := len(event.fnOpts.waitFor)

	for {
		if cnt < 1 {
			break
		}

		select {
		case <-event.ch:
			cnt--
		default:
		}
	}

	if err := event.f(); err != nil {
		log.Printf("background task got error: %v", err)
	}
}

func (s *Tickler) replenish() {
	<-s.options.sema
	log.Printf("Replenishing semaphore, now %d/%d slots in use\n", len(s.options.sema), cap(s.options.sema))
	s.tickleLoop()
}

func (s *Tickler) tickleLoop() {
	select {
	case s.loopSignal <- struct{}{}:
	default:
	}
}

func (s *Tickler) Start(ctx context.Context) {
	go s.loop(ctx)
}

// New creates a new Tickler with default settings.
func New() *Tickler {
	service := &Tickler{
		queue: list.New(),
		options: TicklerOptions{
			sema: make(chan int, defaultRequestLimit),
		},
		loopSignal:  make(chan struct{}, defaultRequestLimit),
		currentJobs: make(map[string]bool),
		jobsWaitFor: make(map[string][]chan struct{}),
	}

	return service
}
