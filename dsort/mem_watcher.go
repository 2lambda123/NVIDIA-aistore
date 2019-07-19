// Package dsort provides APIs for distributed archive file shuffling.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package dsort

import (
	"runtime/debug"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/dsort/extract"
	"github.com/NVIDIA/aistore/sys"
)

const (
	memoryReservedInterval    = 50 * time.Millisecond
	memoryExcessInterval      = time.Second
	unreserveMemoryBufferSize = 10000
)

type singleMemoryWatcher struct {
	wg     *sync.WaitGroup
	ticker *time.Ticker
	stopCh cmn.StopCh
}

// memoryWatcher is responsible for monitoring memory changes and decide
// wether specific action should happen or not. It may also decide to return
type memoryWatcher struct {
	m *Manager

	excess, reserved  *singleMemoryWatcher
	maxMemoryToUse    uint64
	reservedMemory    atomic.Uint64
	memoryUsed        atomic.Uint64 // memory used in specifc point in time, it is refreshed once in a while
	unreserveMemoryCh chan uint64
}

func newSingleMemoryWatcher(interval time.Duration) *singleMemoryWatcher {
	return &singleMemoryWatcher{
		wg:     &sync.WaitGroup{},
		ticker: time.NewTicker(interval),
		stopCh: cmn.NewStopCh(),
	}
}

func newMemoryWatcher(m *Manager, maxMemoryUsage uint64) *memoryWatcher {
	return &memoryWatcher{
		m: m,

		excess:            newSingleMemoryWatcher(memoryExcessInterval),
		reserved:          newSingleMemoryWatcher(memoryReservedInterval),
		maxMemoryToUse:    maxMemoryUsage,
		unreserveMemoryCh: make(chan uint64, unreserveMemoryBufferSize),
	}
}

func (mw *memoryWatcher) freeMemory() uint64 {
	curMem, err := sys.Mem()
	if err != nil {
		return 0
	}

	return mw.maxMemoryToUse - curMem.ActualUsed
}

func (mw *memoryWatcher) watch() error {
	mem, err := sys.Mem()
	if err != nil {
		return err
	}
	mw.memoryUsed.Store(mem.ActualUsed)

	mw.reserved.wg.Add(1)
	go mw.watchReserved()
	mw.excess.wg.Add(1)
	go mw.watchExcess(mem)
	return nil
}

func (mw *memoryWatcher) watchReserved() {
	defer mw.reserved.wg.Done()

	// Starting memory updater. Since extraction phase is concurrent and we
	// cannot know how much memory will given compressed shard extract we need
	// to employ mechanism for updating memory. Just before extraction we
	// estimate how much memory shard will contain (by multiplying file size and
	// avg compress ratio). Then we update currently used memory to actual used
	// + reserved. After we finish extraction we put reserved memory for the
	// shard into the unreserve memory channel. Note that we cannot unreserve it
	// right away because actual used memory has not yet been updated (but it
	// surely changed). Once memory updater will fetch and update currently used
	// memory in system we can unreserve memory (it is already calculated in
	// newly fetched memory usage value). This way it is almost impossible to
	// exceed maximum memory which we are able to use (set by user) -
	// unfortunately it can happen when we underestimate the amount of memory
	// which we will use when extracting compressed file.
	for {
		select {
		case <-mw.reserved.ticker.C:
			curMem, err := sys.Mem()
			if err == nil {
				mw.memoryUsed.Store(curMem.ActualUsed)

				unreserve := true
				for unreserve {
					select {
					case size := <-mw.unreserveMemoryCh:
						mw.reservedMemory.Sub(size)
					default:
						unreserve = false
					}
				}
			}
		case <-mw.m.listenAborted():
			return
		case <-mw.reserved.stopCh.Listen():
			return
		}
	}
}

// watchExcess watches memory in order to prevent exceeding provided by user
// limit. If the limit is exceeded watcher tries to spill memory resources
// to the disk.
//
// NOTE: We also watch the memory in `watchReserved` but this may be
// insufficient because there is more factors than just `SGL`s: `Records`,
// `Shards`, `RecordContents`, `ExtractionPaths` etc. All these structures
// require memory, sometimes it can be counted in GBs. That is why we also need
// excess watcher so that it prevents memory overuse.
func (mw *memoryWatcher) watchExcess(memStat sys.MemStat) {
	defer mw.excess.wg.Done()

	buf, slab := mm.AllocDefault()
	defer slab.Free(buf)

	lastMemoryUsage := memStat.ActualUsed
	for {
		select {
		case <-mw.excess.ticker.C:
			curMem, err := sys.Mem()
			if err != nil {
				continue
			}

			// We should look at difference of memory that have accumulated
			// during the interval. It would be incorrect to calculate
			// difference between `maxMemoryToUse - curMem.ActualUsed` and
			// treat as memory excess because Go's runtime does not return the
			// memory immediately to the system and for most of the time the
			// difference would be always `> 0` and never subside to anything
			// lower so we would free SGLs without seeing any improvement in
			// memory usage (we could even free all SGLs, which is absurd!)
			memExcess := int64(curMem.ActualUsed - lastMemoryUsage)
			lastMemoryUsage = curMem.ActualUsed

			if curMem.ActualUsed < mw.maxMemoryToUse {
				continue
			}

			// In case memory is exceeded spill sgls to disk
			mw.m.recManager.RecordContents().Range(func(key, value interface{}) bool {
				var n int64
				if mw.m.extractCreator.SupportsOffset() {
					n = mw.m.recManager.ChangeStoreType(key.(string), extract.OffsetStoreType, value, buf)
				} else {
					n = mw.m.recManager.ChangeStoreType(key.(string), extract.DiskStoreType, value, buf)
				}
				memExcess -= n
				return memExcess > 0 // continue only if we still need to do some memory cleanup
			})

			debug.FreeOSMemory() // try to free the memory
		case <-mw.m.listenAborted():
			return
		case <-mw.excess.stopCh.Listen():
			return
		}
	}
}

func (mw *memoryWatcher) reserveMem(toReserve uint64) (exceeding bool) {
	newReservedMemory := mw.reservedMemory.Add(toReserve)
	// expected total memory after all objects will be extracted is equal
	// to: previously reserved memory + uncompressed size of shard + current memory used
	expectedTotalMemoryUsed := newReservedMemory + mw.memoryUsed.Load()

	exceeding = expectedTotalMemoryUsed >= mw.maxMemoryToUse
	return
}

func (mw *memoryWatcher) unreserveMem(toUnreserve uint64) {
	mw.unreserveMemoryCh <- toUnreserve
}

func (mw *memoryWatcher) stopWatchingExcess() {
	mw.excess.ticker.Stop()
	mw.excess.stopCh.Close()
	mw.excess.wg.Wait()
}

func (mw *memoryWatcher) stopWatchingReserved() {
	mw.reserved.ticker.Stop()
	mw.reserved.stopCh.Close()
	mw.reserved.wg.Wait()
}

func (mw *memoryWatcher) stop() {
	mw.stopWatchingExcess()
	mw.stopWatchingReserved()
	close(mw.unreserveMemoryCh)
}
