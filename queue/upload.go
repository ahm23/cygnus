package queue

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"cygnus/types"

	"go.uber.org/zap"
)

type UploadQueue struct {
	mu          sync.RWMutex
	queue       *list.List
	jobs        map[string]*types.UploadJob
	concurrency int // max concurrent jobs
	addChan     chan *types.UploadJob
	getChan     chan *types.UploadJob
	logger      *zap.Logger
	stopChan    chan struct{}
	wg          sync.WaitGroup
}

func NewUploadQueue(workers int, logger *zap.Logger) *UploadQueue {
	return &UploadQueue{
		queue:       list.New(),
		jobs:        make(map[string]*types.UploadJob),
		concurrency: workers,
		addChan:     make(chan *types.UploadJob, 1000),
		getChan:     make(chan *types.UploadJob),
		logger:      logger,
		stopChan:    make(chan struct{}),
	}
}

func (uq *UploadQueue) Start(ctx context.Context) {
	uq.logger.Info("Creating upload queue",
		zap.Int("workers", uq.concurrency),
		zap.Int("buffer_size", 1000))

	// start queue manager
	uq.wg.Add(1)
	go uq.runQueueManager(ctx)

	// start worker pool
	for i := 0; i < uq.concurrency; i++ {
		uq.wg.Add(1)
		go uq.worker(ctx, i)
	}
}

func (uq *UploadQueue) Stop() {
	close(uq.stopChan)
	uq.wg.Wait()
	close(uq.addChan)
	close(uq.getChan)
	uq.logger.Info("Upload queue closed")
}

func (uq *UploadQueue) runQueueManager(ctx context.Context) {
	defer uq.wg.Done()

	uq.logger.Debug("Upload queue manager started")

	for {
		select {
		case <-uq.stopChan:
			return
		case <-ctx.Done():
			return
		case job := <-uq.addChan:
			uq.enqueueJob(job)
			// [TODO]: fuck the addChan
		default:
			uq.dispatchJobs()
			time.Sleep(100 * time.Millisecond) // Prevent busy-waiting
		}
	}
}

func (uq *UploadQueue) enqueueJob(job *types.UploadJob) {
	job.Status = "pending"
	// job.ID = uuid.New().String()
	// job.CreatedAt = time.Now()

	uq.mu.Lock()
	defer uq.mu.Unlock()

	// store in pending jobs
	uq.pendingJobs[job.ID] = job

	// add to processing queue
	uq.queue.PushBack(job)

	uq.logger.Debug("Job enqueued",
		zap.String("job_id", job.ID),
		zap.String("owner", job.Owner),
		zap.Int64("size", job.Size))
}

// ------------
// dispatches new jobs if workers are available
func (uq *UploadQueue) dispatchJobs() {
	uq.mu.Lock()
	defer uq.mu.Unlock()

	// fill available workers
	for len(uq.activeJobs) < uq.concurrency && uq.queue.Len() > 0 {
		e := uq.queue.Front()
		job := e.Value.(*types.UploadJob)
		uq.queue.Remove(e)

		// Move from pending to active
		delete(uq.pendingJobs, job.ID)
		uq.activeJobs[job.ID] = job
		job.Status = "processing"

		// Send to worker
		select {
		case uq.getChan <- job:
			uq.logger.Debug("Job dispatched to worker",
				zap.String("job_id", job.ID),
				zap.Int("active_jobs", len(uq.activeJobs)))
		default:
			// Channel full, put back in queue
			uq.queue.PushFront(job)
			delete(uq.activeJobs, job.ID)
			uq.pendingJobs[job.ID] = job
			job.Status = "pending"
			return
		}
	}
}

func (uq *UploadQueue) worker(ctx context.Context, id int) {
	defer uq.wg.Done()

	uq.logger.Debug("Worker started", zap.Int("id", id))

	for {
		select {
		case <-uq.stopChan:
			return
		case <-ctx.Done():
			return
		case job := <-uq.getChan:
			uq.processJob(ctx, id, job)
		}
	}
}

// ------------
// upload job processor
func (uq *UploadQueue) processJob(ctx context.Context, workerID int, job *types.UploadJob) {
	uq.logger.Info("Processing job",
		zap.Int("worker", workerID),
		zap.String("job_id", job.ID),
		zap.String("owner", job.Owner))

	// [TODO]: process upload
	time.Sleep(2 * time.Second)

	// job completed
	uq.mu.Lock()
	job.Status = "completed"
	job.CompletedAt = time.Now()
	uq.mu.Unlock()

	uq.logger.Info("Job completed",
		zap.Int("worker", workerID),
		zap.String("job_id", job.ID))
}

// Public API methods
func (uq *UploadQueue) EnqueueJob(job *types.UploadJob) (string, error) {
	select {
	case uq.addChan <- job:
		return job.ID, nil
	default:
		return "", fmt.Errorf("queue is full")
	}
}

func (uq *UploadQueue) GetJobStatus(jobID string) (*types.UploadJob, error) {
	uq.mu.RLock()
	defer uq.mu.RUnlock()

	// Check in each state
	if job, exists := uq.pendingJobs[jobID]; exists {
		return job, nil
	}
	if job, exists := uq.activeJobs[jobID]; exists {
		return job, nil
	}
	if job, exists := uq.completedJobs[jobID]; exists {
		return job, nil
	}

	return nil, fmt.Errorf("job not found")
}

func (uq *UploadQueue) ListJobs(owner string, status string, limit int) ([]*types.UploadJob, error) {
	uq.mu.RLock()
	defer uq.mu.RUnlock()

	var jobs []*types.UploadJob

	// Helper to check and add jobs
	addJobs := func(jobMap map[string]*types.UploadJob) {
		for _, job := range jobMap {
			// Filter by owner if specified
			if owner != "" && job.Owner != owner {
				continue
			}
			// Filter by status if specified
			if status != "" && job.Status != status {
				continue
			}
			jobs = append(jobs, job)
			if limit > 0 && len(jobs) >= limit {
				return
			}
		}
	}

	// Check in order: active -> pending -> completed
	addJobs(uq.activeJobs)
	if limit > 0 && len(jobs) >= limit {
		return jobs, nil
	}

	addJobs(uq.pendingJobs)
	if limit > 0 && len(jobs) >= limit {
		return jobs, nil
	}

	addJobs(uq.completedJobs)

	return jobs, nil
}

func (uq *UploadQueue) QueueStats() map[string]interface{} {
	uq.mu.RLock()
	defer uq.mu.RUnlock()

	return map[string]interface{}{
		"pending":    len(uq.pendingJobs),
		"active":     len(uq.activeJobs),
		"completed":  len(uq.completedJobs),
		"queue_size": uq.queue.Len(),
		"capacity":   uq.capacity,
	}
}

func (uq *UploadQueue) RetryJob(jobID string) error {
	uq.mu.Lock()
	defer uq.mu.Unlock()

	// Check if job exists in completed/failed state
	job, exists := uq.completedJobs[jobID]
	if !exists {
		return fmt.Errorf("job not found or not retryable")
	}

	// Only retry failed jobs
	if job.Status != "failed" {
		return fmt.Errorf("job cannot be retried (status: %s)", job.Status)
	}

	// Reset job for retry
	job.Status = "pending"
	job.RetryCount++
	job.LastError = ""

	// Move from completed to pending
	delete(uq.completedJobs, jobID)
	uq.pendingJobs[jobID] = job
	uq.queue.PushBack(job)

	uq.logger.Info("Job scheduled for retry",
		zap.String("job_id", jobID),
		zap.Int("retry_count", job.RetryCount))

	return nil
}
