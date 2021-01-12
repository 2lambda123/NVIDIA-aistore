// Package runners provides implementation for the AIStore extended actions.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package xrun

import (
	"fmt"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xaction/xreg"
)

func init() {
	xreg.RegisterGlobalXact(&electionProvider{})
	xreg.RegisterGlobalXact(&resilverProvider{})
	xreg.RegisterGlobalXact(&rebalanceProvider{})

	xreg.RegisterBucketXact(&BckRenameProvider{})
	xreg.RegisterBucketXact(&evictDeleteProvider{kind: cmn.ActEvictObjects})
	xreg.RegisterBucketXact(&evictDeleteProvider{kind: cmn.ActDelete})
	xreg.RegisterBucketXact(&PrefetchProvider{})
}

type (
	BckRenameProvider struct {
		xact *bckRename

		t     cluster.Target
		uuid  string
		phase string
		args  *xreg.BckRenameArgs
	}

	bckRename struct {
		xaction.XactBase
		t       cluster.Target
		bckFrom *cluster.Bck
		bckTo   *cluster.Bck
		rebID   xaction.RebID
	}
)

// interface guard
var _ cluster.Xact = (*bckRename)(nil)

func (*BckRenameProvider) New(args xreg.XactArgs) xreg.BucketEntry {
	return &BckRenameProvider{
		t:     args.T,
		uuid:  args.UUID,
		phase: args.Phase,
		args:  args.Custom.(*xreg.BckRenameArgs),
	}
}

func (p *BckRenameProvider) Start(bck cmn.Bck) error {
	p.xact = newBckRename(p.uuid, p.Kind(), bck, p.t, p.args.BckFrom, p.args.BckTo, p.args.RebID)
	return nil
}
func (*BckRenameProvider) Kind() string        { return cmn.ActMoveBck }
func (p *BckRenameProvider) Get() cluster.Xact { return p.xact }
func (p *BckRenameProvider) PreRenewHook(previousEntry xreg.BucketEntry) (keep bool, err error) {
	if p.phase == cmn.ActBegin {
		if !previousEntry.Get().Finished() {
			err = fmt.Errorf("%s: cannot(%s=>%s) older rename still in progress", p.Kind(), p.args.BckFrom, p.args.BckTo)
			return
		}
		// TODO: more checks
	}
	prev := previousEntry.(*BckRenameProvider)
	bckEq := prev.args.BckTo.Equal(p.args.BckTo, false /*sameID*/, false /* same backend */)
	if prev.phase == cmn.ActBegin && p.phase == cmn.ActCommit && bckEq {
		prev.phase = cmn.ActCommit // transition
		keep = true
		return
	}
	err = fmt.Errorf("%s(%s=>%s, phase %s): cannot %s(=>%s)",
		p.Kind(), prev.args.BckFrom, prev.args.BckTo, prev.phase, p.phase, p.args.BckFrom)
	return
}
func (p *BckRenameProvider) PostRenewHook(_ xreg.BucketEntry) {}

func newBckRename(uuid, kind string, bck cmn.Bck, t cluster.Target,
	bckFrom, bckTo *cluster.Bck, rebID xaction.RebID) *bckRename {
	return &bckRename{
		XactBase: *xaction.NewXactBaseBck(uuid, kind, bck),
		t:        t,
		bckFrom:  bckFrom,
		bckTo:    bckTo,
		rebID:    rebID,
	}
}

func (r *bckRename) String() string { return fmt.Sprintf("%s <= %s", r.XactBase.String(), r.bckFrom) }

func (r *bckRename) Run() {
	glog.Infoln(r.String())
	// FIXME: smart wait for resilver. For now assuming that rebalance takes longer than resilver.
	var (
		onlyRunning, finished bool

		flt = xreg.XactFilter{ID: r.rebID.String(), Kind: cmn.ActRebalance, OnlyRunning: &onlyRunning}
	)
	for !finished {
		time.Sleep(10 * time.Second)
		rebStats, err := xreg.GetStats(flt)
		cmn.AssertNoErr(err)
		for _, stat := range rebStats {
			finished = finished || stat.Finished()
		}
	}

	r.t.BMDVersionFixup(nil, r.bckFrom.Bck, false) // piggyback bucket renaming (last step) on getting updated BMD
	r.Finish(nil)
}

//
// evictDelete
//

type (
	listRangeBase struct {
		xaction.XactBase
		t    cluster.Target
		args *xreg.DeletePrefetchArgs
	}
	evictDeleteProvider struct {
		xreg.BaseBckEntry
		xact *evictDelete

		t    cluster.Target
		kind string
		args *xreg.DeletePrefetchArgs
	}
	evictDelete struct {
		listRangeBase
	}
	objCallback = func(args *xreg.DeletePrefetchArgs, objName string) error
)

// interface guard
var _ cluster.Xact = (*evictDelete)(nil)

func (p *evictDeleteProvider) New(args xreg.XactArgs) xreg.BucketEntry {
	return &evictDeleteProvider{
		t:    args.T,
		kind: p.kind,
		args: args.Custom.(*xreg.DeletePrefetchArgs),
	}
}

func (p *evictDeleteProvider) Start(bck cmn.Bck) error {
	p.xact = newEvictDelete(p.args.UUID, p.kind, bck, p.t, p.args)
	return nil
}
func (p *evictDeleteProvider) Kind() string      { return p.kind }
func (p *evictDeleteProvider) Get() cluster.Xact { return p.xact }

func newEvictDelete(uuid, kind string, bck cmn.Bck, t cluster.Target, args *xreg.DeletePrefetchArgs) *evictDelete {
	return &evictDelete{
		listRangeBase: listRangeBase{
			XactBase: *xaction.NewXactBaseBck(uuid, kind, bck),
			t:        t,
			args:     args,
		},
	}
}

func (r *evictDelete) Run() {
	var err error
	if r.args.RangeMsg != nil {
		err = r.iterateBucketRange(r.args)
	} else {
		err = r.listOperation(r.args, r.args.ListMsg)
	}
	r.Finish(err)
}

//
// prefetch
//

type (
	PrefetchProvider struct {
		xreg.BaseBckEntry
		xact *prefetch

		t    cluster.Target
		args *xreg.DeletePrefetchArgs
	}
	prefetch struct {
		listRangeBase
	}
)

// interface guard
var _ cluster.Xact = (*prefetch)(nil)

func (*PrefetchProvider) New(args xreg.XactArgs) xreg.BucketEntry {
	return &PrefetchProvider{
		t:    args.T,
		args: args.Custom.(*xreg.DeletePrefetchArgs),
	}
}

func (p *PrefetchProvider) Start(bck cmn.Bck) error {
	p.xact = newPrefetch(p.args.UUID, p.Kind(), bck, p.t, p.args)
	return nil
}
func (*PrefetchProvider) Kind() string        { return cmn.ActPrefetch }
func (p *PrefetchProvider) Get() cluster.Xact { return p.xact }

func newPrefetch(uuid, kind string, bck cmn.Bck, t cluster.Target, args *xreg.DeletePrefetchArgs) *prefetch {
	return &prefetch{
		listRangeBase: listRangeBase{
			XactBase: *xaction.NewXactBaseBck(uuid, kind, bck),
			t:        t,
			args:     args,
		},
	}
}

func (r *prefetch) Run() {
	var err error
	if r.args.RangeMsg != nil {
		err = r.iterateBucketRange(r.args)
	} else {
		err = r.listOperation(r.args, r.args.ListMsg)
	}
	r.Finish(err)
}
