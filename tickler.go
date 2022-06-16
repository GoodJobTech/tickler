package tickler

import (
	"container/list"
	"context"
	"log"
	"sync"
)

type (
	JobName                 = string
	BackgroundFn            = func() error
	BackgroundFnWithContext = func(context.Context) error
)

type status = int

const (
	statusUndefined status = iota
	statusSuccess
	statusFailure
)

const (
	defaultRequestLimit = 100
)

type Event struct {
	fnOpts *eventOptions
	Job    JobName
	f      BackgroundFn
	ctx    context.Context
	result status

	ch       chan struct{}
	resultCh chan status
}

type Request struct {
	F   BackgroundFn
	FC  BackgroundFnWithContext
	Job JobName
}

func newEvent(ctx context.Context, request Request, opts ...EventOption) *Event {
	t := &Event{
		fnOpts:   &eventOptions{},
		f:        request.F,
		Job:      request.Job,
		ctx:      ctx,
		ch:       make(chan struct{}),
		result:   statusSuccess,
		resultCh: make(chan status),
	}

	for _, opt := range opts {
		opt.apply(t.fnOpts)
	}

	return t
}

type eventOptions struct {
	waitFor   []JobName
	ifSuccess []JobName
	ifFailure []JobName
}

type EventOption interface {
	apply(*eventOptions)
}

type eventOption struct {
	f func(*eventOptions)
}

func (o *eventOption) apply(opts *eventOptions) {
	o.f(opts)
}

func newEventOption(f func(*eventOptions)) *eventOption {
	return &eventOption{
		f: f,
	}
}

func WaitForJobs(jobNames ...JobName) EventOption {
	return newEventOption(func(t *eventOptions) {
		t.waitFor = jobNames
	})
}

func IfSuccess(jobNames ...JobName) EventOption {
	return newEventOption(func(t *eventOptions) {
		t.ifSuccess = jobNames
	})
}

func IfFailure(jobNames ...JobName) EventOption {
	return newEventOption(func(t *eventOptions) {
		t.ifFailure = jobNames
	})
}

type Options struct {
	Limit int64
	Ctx   context.Context

	sema chan int
}

type Tickler struct {
	mu         sync.Mutex
	ctx        context.Context
	queue      *list.List
	options    Options
	loopSignal chan struct{}

	currentJobs map[JobName]bool
	jobsWaitFor map[JobName][]chan struct{}
	resultCh    map[JobName][]chan status
}

func (s *Tickler) EnqueueWithContext(ctx context.Context, request Request, opts ...EventOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.tickleLoop()

	ticklerEvent := newEvent(ctx, request, opts...)

	s.currentJobs[request.Job] = true

	for _, v := range ticklerEvent.fnOpts.waitFor {
		s.jobsWaitFor[v] = append(s.jobsWaitFor[v], ticklerEvent.ch)
	}

	s.queue.PushBack(ticklerEvent)
	log.Printf("Added request to queue with length %d\n", s.queue.Len())
}

func (s *Tickler) Enqueue(request Request, opts ...EventOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.tickleLoop()

	ticklerEvent := newEvent(context.Background(), request, opts...)

	s.currentJobs[request.Job] = true

	for _, v := range ticklerEvent.fnOpts.waitFor {
		s.jobsWaitFor[v] = append(s.jobsWaitFor[v], ticklerEvent.ch)
	}

	for _, v := range ticklerEvent.fnOpts.ifSuccess {
		s.resultCh[v] = append(s.resultCh[v], ticklerEvent.resultCh)
	}

	s.queue.PushBack(ticklerEvent)
	log.Printf("Added request to queue with length %d\n", s.queue.Len())
}

func (s *Tickler) loop() {
	log.Println("Starting service loop")
	for {
		select {
		case <-s.loopSignal:
			s.tryDequeue()
		case <-s.ctx.Done():
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

func (s *Tickler) dequeue() *Event {
	element := s.queue.Front()
	s.queue.Remove(element)
	return element.Value.(*Event)
}

func (s *Tickler) removeJob(event *Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, v := range s.jobsWaitFor[event.Job] {
		v <- struct{}{}
	}

	for _, v := range s.resultCh[event.Job] {
		v <- event.result
	}

	delete(s.jobsWaitFor, event.Job)
	delete(s.currentJobs, event.Job)
}

type eventResults struct {
	SucceededEvents int
	FailedEvents    int
	TotalEvents     int
}

func (s *Tickler) process(event *Event) {
	defer s.replenish()
	defer s.removeJob(event)

	cnt := len(event.fnOpts.waitFor)
	eventRes := eventResults{
		SucceededEvents: len(event.fnOpts.ifSuccess),
		FailedEvents:    len(event.fnOpts.ifFailure),
		TotalEvents:     len(event.fnOpts.ifSuccess) + len(event.fnOpts.ifFailure),
	}

	// Wait for other jobs to be done
	for {
		if cnt < 1 && eventRes.TotalEvents == 0 {
			break
		}

		select {
		case <-event.ch:
			cnt--
		case r := <-event.resultCh:
			eventRes.TotalEvents--
			if r == statusSuccess {
				eventRes.SucceededEvents--
			} else {
				eventRes.FailedEvents--
			}
		}
	}

	// If all jobs are done, then we can proceed
	if eventRes.SucceededEvents != 0 || eventRes.FailedEvents != 0 {
		event.result = statusFailure
		return
	}

	select {
	case <-event.ctx.Done():
		log.Printf("event context cancelled for %v", event.Job)
		return
	default:

		if err := event.f(); err != nil {
			log.Printf("background task got error: %v", err)
			event.result = statusFailure
		}
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

func (s *Tickler) Start() {
	go s.loop()
}

func (s *Tickler) Stop() {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.ctx = ctx
}

// New creates a new Tickler with default settings.
func New() *Tickler {
	service := &Tickler{
		queue: list.New(),
		options: Options{
			sema: make(chan int, defaultRequestLimit),
		},
		ctx:         context.Background(),
		loopSignal:  make(chan struct{}, defaultRequestLimit),
		currentJobs: make(map[string]bool),
		jobsWaitFor: make(map[string][]chan struct{}),
		resultCh:    make(map[string][]chan status),
	}

	return service
}
