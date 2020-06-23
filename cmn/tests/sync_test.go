// Package test provides tests for common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package tests

import (
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/cmn"
)

func TestTimeoutGroupSmoke(t *testing.T) {
	wg := cmn.NewTimeoutGroup()
	wg.Add(1)
	wg.Done()
	if wg.WaitTimeout(time.Second) {
		t.Error("wait timed out")
	}
}

func TestTimeoutGroupWait(t *testing.T) {
	wg := cmn.NewTimeoutGroup()
	wg.Add(2)
	wg.Done()
	wg.Done()
	wg.Wait()
}

func TestTimeoutGroupGoroutines(t *testing.T) {
	wg := cmn.NewTimeoutGroup()

	for i := 0; i < 100000; i++ {
		wg.Add(1)
		go func() {
			time.Sleep(time.Second * 2)
			wg.Done()
		}()
	}

	if wg.WaitTimeout(time.Second * 5) {
		t.Error("wait timed out")
	}
}

func TestTimeoutGroupTimeout(t *testing.T) {
	wg := cmn.NewTimeoutGroup()
	wg.Add(1)

	go func() {
		time.Sleep(time.Second * 3)
		wg.Done()
	}()

	if !wg.WaitTimeout(time.Second) {
		t.Error("group did not time out")
	}

	if wg.WaitTimeout(time.Second * 3) { // now wait for actual end of the job
		t.Error("group timed out")
	}
}

func TestTimeoutGroupStop(t *testing.T) {
	wg := cmn.NewTimeoutGroup()
	wg.Add(1)

	go func() {
		time.Sleep(time.Second * 3)
		wg.Done()
	}()

	if !wg.WaitTimeout(time.Second) {
		t.Error("group did not time out")
	}

	stopCh := make(chan struct{}, 1)
	stopCh <- struct{}{}

	timed, stopped := wg.WaitTimeoutWithStop(time.Second, stopCh)
	if timed {
		t.Error("group should not time out")
	}

	if !stopped {
		t.Error("group should be stopped")
	}

	if timed, stopped = wg.WaitTimeoutWithStop(time.Second*3, stopCh); timed || stopped {
		t.Error("group timed out or was stopped on finish")
	}
}

func TestTimeoutGroupStopAndTimeout(t *testing.T) {
	wg := cmn.NewTimeoutGroup()
	wg.Add(1)

	go func() {
		time.Sleep(time.Second * 3)
		wg.Done()
	}()

	stopCh := make(chan struct{}, 1)
	timed, stopped := wg.WaitTimeoutWithStop(time.Second, stopCh)
	if !timed {
		t.Error("group should time out")
	}

	if stopped {
		t.Error("group should not be stopped")
	}

	if timed, stopped = wg.WaitTimeoutWithStop(time.Second*3, stopCh); timed || stopped {
		t.Error("group timed out or was stopped on finish")
	}
}

func TestDynSemaphore(t *testing.T) {
	limit := 10

	sema := cmn.NewDynSemaphore(limit)

	var i atomic.Int32
	wg := &sync.WaitGroup{}
	ch := make(chan int32, 10*limit)

	for j := 0; j < 10*limit; j++ {
		sema.Acquire()
		wg.Add(1)
		go func() {
			ch <- i.Inc()
			time.Sleep(time.Millisecond)
			i.Dec()
			sema.Release()
			wg.Done()
		}()
	}

	wg.Wait()
	close(ch)

	res := int32(0)
	for c := range ch {
		res = cmn.MaxI32(res, c)
	}

	if int(res) != limit {
		t.Fatalf("acutal limit %d was different than expected %d", res, limit)
	}
}
