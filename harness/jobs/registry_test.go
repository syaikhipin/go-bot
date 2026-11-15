package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRegistryAwaitedSettlementDoesNotNotifyTwice(t *testing.T) {
	r, err := New(context.Background(), 1, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	job, err := r.Start("bash", "awaited", func(context.Context, func([]byte)) (int, error) {
		<-release
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.MarkVisible(job.ID); err != nil {
		t.Fatal(err)
	}
	result := make(chan Snapshot, 1)
	go func() {
		snapshot, _ := r.WaitForModel(context.Background(), job.ID, time.Second)
		result <- snapshot
	}()
	deadline := time.After(time.Second)
	for {
		r.mu.Lock()
		waiting := r.jobs[job.ID].waiters == 1
		r.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Job waiter was not registered")
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	if snapshot := <-result; snapshot.Status != Completed {
		t.Fatalf("waited Job = %+v", snapshot)
	}
	if pending := r.PendingSettled(); len(pending) != 0 {
		t.Fatalf("awaited completion generated a duplicate notice: %+v", pending)
	}
}

func TestRegistryCanceledModelWaitStillNotifies(t *testing.T) {
	r, err := New(context.Background(), 1, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	job, err := r.Start("bash", "canceled wait", func(context.Context, func([]byte)) (int, error) {
		<-release
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.MarkVisible(job.ID); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.WaitForModel(waitCtx, job.ID, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled model wait = %v", err)
	}
	close(release)
	if snapshot, err := r.Wait(context.Background(), job.ID, time.Second); err != nil || snapshot.Status != Completed {
		t.Fatalf("completed Job = %+v, %v", snapshot, err)
	}
	if pending := r.PendingSettled(); len(pending) != 1 || pending[0].ID != job.ID {
		t.Fatalf("canceled wait lost completion notice: %+v", pending)
	}
}

func TestRegistryNoticeDeliverySerializesWithSuppression(t *testing.T) {
	r, err := New(context.Background(), 1, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	second, err := r.Start("bash", "notice", func(context.Context, func([]byte)) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.MarkVisible(second.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for len(r.PendingSettled()) == 0 {
		select {
		case <-deadline:
			t.Fatal("settled notice was not queued")
		case <-time.After(time.Millisecond):
		}
	}
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	delivered := make(chan error, 1)
	go func() {
		delivered <- r.DeliverSettled(func(snapshot Snapshot) error {
			if snapshot.ID != second.ID {
				return errors.New("wrong Job notice")
			}
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	suppressed := make(chan struct{})
	go func() { r.SuppressNotice(second.ID); close(suppressed) }()
	select {
	case <-suppressed:
		t.Fatal("terminal read overtook in-flight notice delivery")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-delivered; err != nil {
		t.Fatal(err)
	}
	<-suppressed
	if pending := r.PendingSettled(); len(pending) != 0 {
		t.Fatalf("delivered Job notice remained: %+v", pending)
	}
}

func TestRegistryBoundsAdmissionAndRetainsOutputByCursor(t *testing.T) {
	settled := make(chan Snapshot, 1)
	r, err := New(context.Background(), 1, 8, func(snapshot Snapshot) { settled <- snapshot })
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	started := make(chan struct{})
	job, err := r.Start("bash", "test", func(ctx context.Context, emit func([]byte)) (int, error) {
		emit([]byte("123456"))
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := r.Start("bash", "second", func(context.Context, func([]byte)) (int, error) { return 0, nil }); err == nil {
		t.Fatal("capacity admitted a second running job")
	}
	output, err := r.Read(job.ID, 0)
	if err != nil || output.Text != "123456" || output.Lossy || output.Cursor != 6 {
		t.Fatalf("first output = %#v, %v", output, err)
	}
	snapshot, err := r.Wait(context.Background(), job.ID, time.Millisecond)
	if err != nil || snapshot.Status != Running {
		t.Fatalf("wait timeout killed job: %#v, %v", snapshot, err)
	}
	if _, err := r.Kill(job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case done := <-settled:
		if done.Status != Killed {
			t.Fatalf("kill settlement = %#v", done)
		}
	case <-time.After(time.Second):
		t.Fatal("killed job did not settle")
	}
	second, err := r.Start("bash", "second", func(_ context.Context, emit func([]byte)) (int, error) {
		emit([]byte("abcdefghijk"))
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Wait(context.Background(), second.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	output, err = r.Read(second.ID, 0)
	if err != nil || !output.Lossy || output.Text != "defghijk" || output.Cursor != 11 || output.Job.RetainedFrom != 3 {
		t.Fatalf("bounded ring = %#v, %v", output, err)
	}
}

func TestRegistryCloseCancelsAndDrainsProducer(t *testing.T) {
	r, err := New(context.Background(), 1, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	_, err = r.Start("bash", "test", func(ctx context.Context, _ func([]byte)) (int, error) {
		<-ctx.Done()
		close(finished)
		return 0, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("registry close returned before producer cleanup")
	}
	if len(r.List()) != 0 {
		t.Fatal("closed registry retained owner jobs")
	}
	if _, err := r.Start("bash", "after close", func(context.Context, func([]byte)) (int, error) { return 0, nil }); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed registry admitted job: %v", err)
	}
}

func TestRegistryCompletionNoticeWaitsForVisibilityAndAcknowledgement(t *testing.T) {
	r, err := New(context.Background(), 2, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	fast, err := r.Start("bash", "fast", func(context.Context, func([]byte)) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Wait(context.Background(), fast.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := r.PendingSettled(); len(got) != 0 {
		t.Fatalf("invisible Job generated a notice: %#v", got)
	}
	if _, err := r.MarkVisible(fast.ID); err != nil {
		t.Fatal(err)
	}
	if got := r.PendingSettled(); len(got) != 1 || got[0].ID != fast.ID {
		t.Fatalf("visible Job notice = %#v", got)
	}
	if active, _ := r.Activity(); !active {
		t.Fatal("pending Job completion was reported idle before durable admission")
	}
	if got := r.PendingSettled(); len(got) != 1 {
		t.Fatalf("unacknowledged Job notice was lost: %#v", got)
	}
	r.AckSettled(fast.ID)
	if active, _ := r.Activity(); active {
		t.Fatal("acknowledged Job completion still kept Registry active")
	}
	if got := r.PendingSettled(); len(got) != 0 {
		t.Fatalf("acknowledged Job notice remained: %#v", got)
	}
	if _, err := r.MarkVisible(fast.ID); err != nil {
		t.Fatal(err)
	}
	if got := r.PendingSettled(); len(got) != 0 {
		t.Fatalf("duplicate Job notice generated: %#v", got)
	}

	suppressed, err := r.Start("bash", "suppressed", func(context.Context, func([]byte)) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.MarkVisible(suppressed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Wait(context.Background(), suppressed.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	r.SuppressNotice(suppressed.ID)
	if got := r.PendingSettled(); len(got) != 0 {
		t.Fatalf("suppressed Job generated a notice: %#v", got)
	}
}
