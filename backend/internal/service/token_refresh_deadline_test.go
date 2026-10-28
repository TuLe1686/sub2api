package service

import (
	"context"
	"testing"
	"time"
)

type refreshDeadlinePendingContext struct {
	context.Context
	deadline time.Time
}

func (c refreshDeadlinePendingContext) Deadline() (time.Time, bool) {
	return c.deadline, true
}

func TestRefreshAttemptDeadlineExceededBeforeTimerCallback(t *testing.T) {
	attempt := refreshDeadlinePendingContext{
		Context:  context.Background(),
		deadline: time.Now().Add(-time.Millisecond),
	}
	if attempt.Err() != nil {
		t.Fatal("timer callback must still be pending")
	}
	if !refreshAttemptDeadlineExceeded(context.Background(), attempt) {
		t.Fatal("elapsed deadline must be detected before the timer callback")
	}
}

func TestRefreshAttemptDeadlineExceededPreservesParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	attempt := refreshDeadlinePendingContext{
		Context:  context.Background(),
		deadline: time.Now().Add(-time.Millisecond),
	}
	if refreshAttemptDeadlineExceeded(parent, attempt) {
		t.Fatal("parent cancellation must not become an attempt timeout")
	}
}

func TestRefreshAttemptDeadlineExceededKeepsLiveAttempt(t *testing.T) {
	attempt := refreshDeadlinePendingContext{
		Context:  context.Background(),
		deadline: time.Now().Add(time.Hour),
	}
	if refreshAttemptDeadlineExceeded(context.Background(), attempt) {
		t.Fatal("an attempt before its deadline must stay live")
	}
}
