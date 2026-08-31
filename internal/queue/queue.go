// Package queue is a miniature Redis-backed distributed job queue in the
// spirit of Zerodha's Tasqueue (github.com/kalbhor/tasqueue).
//
// Jobs are JSON payloads pushed onto a Redis list per stage; workers
// BRPOP from the list, do the work, and enqueue a job for the next
// stage. Redis doubles as the job-state store, just like in the real
// pipeline. Because the broker is Redis, workers could run on any
// number of machines — here we run worker pools as goroutines in one
// process, standing in for Zerodha's ~40 nightly EC2 instances.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Job is the unit of work handed between stages. One job == one
// client's contract note, matching Zerodha's "independent job units".
type Job struct {
	ID       string `json:"id"`        // client ID
	Stage    string `json:"stage"`     // generate | sign | email
	ObjKey   string `json:"obj_key"`   // object-store key of this stage's input
	DataJSON string `json:"data_json"` // trade data payload (generate stage)
}

type Queue struct {
	rdb *redis.Client
}

func New(addr string) (*Queue, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("redis at %s: %w", addr, err)
	}
	return &Queue{rdb: rdb}, nil
}

func listKey(stage string) string { return "jobs:" + stage }

// Enqueue pushes a job onto a stage's queue and records its state.
func (q *Queue) Enqueue(ctx context.Context, stage string, j Job) error {
	j.Stage = stage
	b, _ := json.Marshal(j)
	if err := q.rdb.LPush(ctx, listKey(stage), b).Err(); err != nil {
		return err
	}
	return q.rdb.HSet(ctx, "jobstate", j.ID+":"+stage, "queued").Err()
}

// Dequeue blocks up to timeout waiting for a job on a stage's queue.
// Returns nil when the queue stays empty (workers use this to drain).
func (q *Queue) Dequeue(ctx context.Context, stage string, timeout time.Duration) (*Job, error) {
	res, err := q.rdb.BRPop(ctx, timeout, listKey(stage)).Result()
	if err == redis.Nil {
		return nil, nil // queue drained
	}
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal([]byte(res[1]), &j); err != nil {
		return nil, err
	}
	q.rdb.HSet(ctx, "jobstate", j.ID+":"+stage, "processing")
	return &j, nil
}

// Done marks a job's stage complete in the shared state hash.
func (q *Queue) Done(ctx context.Context, j *Job) error {
	return q.rdb.HSet(ctx, "jobstate", j.ID+":"+j.Stage, "done").Err()
}

// Fail records a job failure.
func (q *Queue) Fail(ctx context.Context, j *Job, cause error) error {
	return q.rdb.HSet(ctx, "jobstate", j.ID+":"+j.Stage, "failed: "+cause.Error()).Err()
}

// Reset clears all queues and job state before a fresh run.
func (q *Queue) Reset(ctx context.Context) error {
	return q.rdb.Del(ctx, "jobstate", listKey("generate"), listKey("sign"), listKey("email")).Err()
}
