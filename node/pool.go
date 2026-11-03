package node

import "sync"

// pool runs the file system work of the promise and callback operations
// on at most max goroutines at once. The jobs beyond wait in a queue, not
// on goroutines of their own; a worker that finds the queue empty ends.
type pool struct {
	mu      sync.Mutex
	queue   []job
	head    int
	running int
	max     int
	closed  bool
}

// job is a unit of work: run does it, on a worker; drop ends it without
// doing it, once the pool is closed.
type job struct {
	run, drop func()
}

func newPool(n int) *pool {
	if n <= 0 {
		n = 4
	}
	return &pool{max: n}
}

// submit runs j on a worker, at once or once one is free. Once the pool
// is closed it drops j.
func (p *pool) submit(j job) {
	p.mu.Lock()
	switch {
	case p.closed:
		p.mu.Unlock()
		j.drop()
		return
	case p.running < p.max:
		p.running++
		p.mu.Unlock()
		go p.work(j)
		return
	}
	p.queue = append(p.queue, j)
	p.mu.Unlock()
}

func (p *pool) work(j job) {
	for {
		j.run()
		p.mu.Lock()
		if p.closed || p.head == len(p.queue) {
			p.running--
			p.mu.Unlock()
			return
		}
		j = p.queue[p.head]
		p.queue[p.head] = job{}
		if p.head++; p.head == len(p.queue) {
			p.queue, p.head = p.queue[:0], 0
		}
		p.mu.Unlock()
	}
}

// close drops the jobs that wait; the running ones finish.
func (p *pool) close() {
	p.mu.Lock()
	p.closed = true
	queued := p.queue[p.head:]
	p.queue, p.head = nil, 0
	p.mu.Unlock()
	for _, j := range queued {
		j.drop()
	}
}
