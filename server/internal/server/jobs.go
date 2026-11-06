package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var errQueueFull = errors.New("metadata queue is full; retry after a job finishes")

type MetadataJob struct {
	ID       string     `json:"id"`
	Platform string     `json:"platform"`
	Status   string     `json:"status"`
	Created  time.Time  `json:"created"`
	Finished *time.Time `json:"finished,omitempty"`
	Log      string     `json:"log"`
	Error    string     `json:"error,omitempty"`
}

type jobQueue struct {
	mu     sync.Mutex
	jobs   []*MetadataJob
	wake   chan struct{}
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	run    func(context.Context, string, io.Writer) error
}

func newJobQueue(ctx context.Context, run func(context.Context, string, io.Writer) error) *jobQueue {
	ctx, cancel := context.WithCancel(ctx)
	q := &jobQueue{ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}), run: run}
	go q.worker()
	return q
}

func (q *jobQueue) close() { q.cancel(); <-q.done }

func (q *jobQueue) enqueue(platform string) (MetadataJob, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.ctx.Err(); err != nil {
		return MetadataJob{}, err
	}
	pending := 0
	for _, job := range q.jobs {
		if job.Status == "queued" {
			if job.Platform == platform {
				return *job, nil
			}
			pending++
		}
	}
	if pending >= 32 {
		return MetadataJob{}, errQueueFull
	}
	id, err := randomID()
	if err != nil {
		return MetadataJob{}, err
	}
	job := &MetadataJob{ID: id, Platform: platform, Status: "queued", Created: time.Now().UTC()}
	// Retain a bounded recent history without dropping active jobs.
	if len(q.jobs) >= 64 {
		for i, old := range q.jobs {
			if old.Status != "queued" && old.Status != "running" {
				q.jobs = append(q.jobs[:i], q.jobs[i+1:]...)
				break
			}
		}
	}
	q.jobs = append(q.jobs, job)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return *job, nil
}

func (q *jobQueue) snapshot() []MetadataJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]MetadataJob, 0, len(q.jobs))
	for i := len(q.jobs) - 1; i >= 0; i-- {
		out = append(out, *q.jobs[i])
	}
	return out
}

type jobLog struct {
	q   *jobQueue
	job *MetadataJob
}

func (w jobLog) Write(data []byte) (int, error) {
	w.q.mu.Lock()
	defer w.q.mu.Unlock()
	const limit = 32 << 10
	text := w.job.Log + string(data)
	if len(text) > limit {
		text = text[len(text)-limit:]
	}
	w.job.Log = text
	return len(data), nil
}

func (q *jobQueue) worker() {
	defer close(q.done)
	defer func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		for _, job := range q.jobs {
			if job.Status == "queued" {
				now := time.Now().UTC()
				job.Status, job.Error, job.Finished = "cancelled", "server shutting down", &now
			}
		}
	}()
	for {
		select {
		case <-q.ctx.Done():
			return
		case <-q.wake:
		}
		for {
			if q.ctx.Err() != nil {
				return
			}
			q.mu.Lock()
			var current *MetadataJob
			for _, job := range q.jobs {
				if job.Status == "queued" {
					current = job
					current.Status = "running"
					break
				}
			}
			q.mu.Unlock()
			if current == nil {
				break
			}
			err := q.run(q.ctx, current.Platform, jobLog{q: q, job: current})
			q.mu.Lock()
			now := time.Now().UTC()
			current.Finished, current.Status = &now, "succeeded"
			if err != nil {
				current.Status, current.Error = "failed", fmt.Sprint(err)
				if q.ctx.Err() != nil {
					current.Status = "cancelled"
				}
			}
			q.mu.Unlock()
		}
	}
}
