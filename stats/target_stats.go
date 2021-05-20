// Package stats provides methods and functionality to register, track, log,
// and StatsD-notify statistics that, for the most part, include "counter" and "latency" kinds.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package stats

import (
	"strconv"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/ios"
)

// Naming Convention:
//  -> "*.n" - counter
//  -> "*.ns" - latency (nanoseconds)
//  -> "*.size" - size (bytes)
//  -> "*.bps" - throughput (in byte/s)
//  -> "*.id" - ID
const (
	// KindCounter - QPS and byte counts (always incremented, never reset)
	GetColdCount   = "get.cold.n"
	GetColdSize    = "get.cold.size"
	LruEvictSize   = "lru.evict.size"
	LruEvictCount  = "lru.evict.n"
	VerChangeCount = "vchange.n"
	VerChangeSize  = "vchange.size"
	// rebalance
	RebTxCount = "reb.tx.n"
	RebTxSize  = "reb.tx.size"
	RebRxCount = "reb.rx.n"
	RebRxSize  = "reb.rx.size"
	// errors
	ErrCksumCount    = "err.cksum.n"
	ErrCksumSize     = "err.cksum.size"
	ErrMetadataCount = "err.md.n"
	ErrIOCount       = "err.io.n"
	// special
	RestartCount = "restart.n"

	// KindLatency
	PutLatency      = "put.ns"
	AppendLatency   = "append.ns"
	GetRedirLatency = "get.redir.ns"
	PutRedirLatency = "put.redir.ns"
	DownloadLatency = "dl.ns"

	// DSort
	DSortCreationReqCount    = "dsort.creation.req.n"
	DSortCreationReqLatency  = "dsort.creation.req.ns"
	DSortCreationRespCount   = "dsort.creation.resp.n"
	DSortCreationRespLatency = "dsort.creation.resp.ns"

	// Downloader
	DownloadSize = "dl.size"

	// KindThroughput
	GetThroughput = "get.bps" // bytes per second
)

//
// public type
//
type (
	Trunner struct {
		statsRunner
		T     cluster.Target `json:"-"`
		MPCap fs.MPCap       `json:"capacity"`
		lines []string
		disk  ios.AllDiskStats
	}
	copyRunner struct {
		Tracker copyTracker `json:"core"`
		MPCap   fs.MPCap    `json:"capacity"`
	}
)

/////////////
// Trunner //
/////////////

// interface guard
var _ cos.Runner = (*Trunner)(nil)

func (r *Trunner) Run() error { return r.runcommon(r) }

func (r *Trunner) Init(t cluster.Target) *atomic.Bool {
	r.Core = &CoreStats{}
	r.Core.init(t.Snode(), 48) // register common (target's own stats are reg()-ed elsewhere)

	r.ctracker = make(copyTracker, 48) // these two are allocated once and only used in serial context
	r.lines = make([]string, 0, 16)
	r.disk = make(ios.AllDiskStats, 16)

	config := cmn.GCO.Get()
	r.Core.statsTime = config.Periodic.StatsTime.D()

	r.statsRunner.name = "targetstats"
	r.statsRunner.daemon = t

	r.statsRunner.stopCh = make(chan struct{}, 4)
	r.statsRunner.workCh = make(chan NamedVal64, 256)

	r.Core.initMetricClient(t.Snode(), &r.statsRunner)
	return &r.statsRunner.startedUp
}

func (r *Trunner) InitCapacity() error {
	availableMountpaths, _ := fs.Get()
	r.MPCap = make(fs.MPCap, len(availableMountpaths))
	cs, err := fs.RefreshCapStatus(nil, r.MPCap)
	if err != nil {
		return err
	}
	if cs.Err != nil {
		glog.Errorf("%s: %v", r.T.Snode(), cs.Err)
	}
	return nil
}

// register target-specific metrics in addition to those that must be
// already added via regCommonMetrics()
func (r *Trunner) reg(name, kind string) { r.Core.Tracker.register(r.T.Snode(), name, kind) }

func nameRbps(disk string) string { return "disk." + disk + ".read.bps" }
func nameWbps(disk string) string { return "disk." + disk + ".read.bps" }
func nameUtil(disk string) string { return "disk." + disk + ".util" }

func (r *Trunner) RegDiskMetrics(disk string) {
	s, n := r.Core.Tracker, nameRbps(disk)
	if _, ok := s[n]; ok { // must be config.TestingEnv()
		return
	}
	r.reg(n, KindComputedThroughput)
	r.reg(nameWbps(disk), KindComputedThroughput)
	r.reg(nameUtil(disk), KindGauge)
}

func (r *Trunner) RegMetrics(node *cluster.Snode) {
	debug.Assert(node == r.T.Snode())

	r.reg(PutLatency, KindLatency)
	r.reg(AppendLatency, KindLatency)
	r.reg(GetColdCount, KindCounter)
	r.reg(GetColdSize, KindCounter)
	r.reg(GetThroughput, KindThroughput)
	r.reg(LruEvictSize, KindCounter)
	r.reg(LruEvictCount, KindCounter)
	r.reg(VerChangeCount, KindCounter)
	r.reg(VerChangeSize, KindCounter)
	r.reg(GetRedirLatency, KindLatency)
	r.reg(PutRedirLatency, KindLatency)

	// errors
	r.reg(ErrCksumCount, KindCounter)
	r.reg(ErrCksumSize, KindCounter)
	r.reg(ErrMetadataCount, KindCounter)

	r.reg(ErrIOCount, KindCounter)

	// rebalance
	r.reg(RebTxCount, KindCounter)
	r.reg(RebTxSize, KindCounter)
	r.reg(RebRxCount, KindCounter)
	r.reg(RebRxSize, KindCounter)

	// special
	r.reg(RestartCount, KindCounter)

	// download
	r.reg(DownloadSize, KindCounter)
	r.reg(DownloadLatency, KindLatency)

	// dsort
	r.reg(DSortCreationReqCount, KindCounter)
	r.reg(DSortCreationReqLatency, KindLatency)
	r.reg(DSortCreationRespCount, KindCounter)
	r.reg(DSortCreationRespLatency, KindLatency)

	// Prometheus
	r.Core.initProm(node)
}

func (r *Trunner) GetWhatStats() interface{} {
	ctracker := make(copyTracker, 48)
	r.Core.copyCumulative(ctracker)
	return &copyRunner{Tracker: ctracker, MPCap: r.MPCap}
}

func (r *Trunner) log(uptime time.Duration) {
	r.lines = r.lines[:0] // TODO: reuse lines as []byte buffers

	// 1 collect disk stats and populate the tracker
	fs.FillDiskStats(r.disk)
	s := r.Core
	for disk, stats := range r.disk {
		v := s.Tracker[nameRbps(disk)]
		v.Value = stats.RBps
		v = s.Tracker[nameWbps(disk)]
		v.Value = stats.WBps
		v = s.Tracker[nameUtil(disk)]
		v.Value = stats.Util
	}

	// 2 copy stats, reset latencies, send via StatsD if configured
	r.Core.updateUptime(uptime)
	if idle := r.Core.copyT(r.ctracker, []string{"kalive", Uptime}); !idle {
		ln, err := cos.MarshalToString(r.ctracker)
		debug.AssertNoErr(err)
		r.lines = append(r.lines, ln)
	}

	// 3. capacity
	cs, updated, _ := fs.CapPeriodic(r.MPCap)
	if updated {
		if cs.Err != nil {
			go r.T.RunLRU("" /*uuid*/, false)
		}
		for mpath, fsCapacity := range r.MPCap {
			ln, err := cos.MarshalToString(fsCapacity)
			debug.AssertNoErr(err)
			debug.SetExpvar(glog.SmoduleStats, mpath+":cap%", int64(fsCapacity.PctUsed))
			r.lines = append(r.lines, mpath+": "+ln)
		}
	}

	// 4. append disk stats to log
	r.logDiskStats()

	// 5. log
	for _, ln := range r.lines {
		glog.Infoln(ln)
	}
}

func (r *Trunner) logDiskStats() {
	for disk, stats := range r.disk {
		if stats.Util == 0 {
			continue
		}
		rbps := cos.B2S(stats.RBps, 0)
		wbps := cos.B2S(stats.WBps, 0)
		l := len(disk) + len(rbps) + len(wbps) + 32
		buf := make([]byte, 0, l)
		buf = append(buf, disk...)
		buf = append(buf, ": "...)
		buf = append(buf, rbps...)
		buf = append(buf, "/s, "...)
		buf = append(buf, wbps...)
		buf = append(buf, "/s, "...)
		buf = append(buf, strconv.FormatInt(stats.Util, 10)...)
		buf = append(buf, "%"...)
		r.lines = append(r.lines, *(*string)(unsafe.Pointer(&buf)))
	}
}

func (r *Trunner) doAdd(nv NamedVal64) {
	var (
		s     = r.Core
		name  = nv.Name
		value = nv.Value
	)
	_, ok := s.Tracker[name]
	debug.Assertf(ok, "invalid stats name: %q", name)
	s.doAdd(name, nv.NameSuffix, value)
}

func (r *Trunner) statsTime(newval time.Duration) {
	r.Core.statsTime = newval
}
