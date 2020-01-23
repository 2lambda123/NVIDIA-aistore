// Package reb provides resilvering and rebalancing functionality for the AIStore object storage.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package reb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/ec"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/transport"
	jsoniter "github.com/json-iterator/go"
	"github.com/klauspost/reedsolomon"
)

// TODO: At this moment the module contains duplicated code borrowed from EC
// package. Extracting common stuff and moving them to EC package is
// a goal for the next MRs.

// High level overview of how EC rebalance works.
// Note: really it is not only rebalance, it also repairs damaged objects
// 01. When rebalance starts, it checks if EC is enabled. If not, it starts
//     regular rebalance.
// 02. First stage is to build global namespace of existing data (rebStageTraverse)
// 03. Each target traverses local directories that contain EC metadata and
//     collects those that have corresponding slice/object
// 04. When the list is complete, a target sends the list to other targets
// 05. Each target waits for all other targets to receive the data and then
//     rebalance moves to next stage (rebStageECDetect)
// 06. Each target processes the list of CTs and groups by isAIS/Bucket/Objname/ObjHash
// 07. All other steps use the newest CT list of an object. Though it has
//     and issue. TODO: rare corner case: current object is EC'ed, after putting a
//     new version, the object gets replicated. Replicated object requires less
//     CTs, so the algorithm chooses the list that belongs to older hash version
// 08. If a local CT is on incorrect mpath, it is added to local repair list.
// 09. If one or few object parts are missing, and it is possible to restore
//     them from existing ones, the object is added to 'broken' object list
// 10. If 'broken' and 'local repair' lists are empty, the rebalance finishes.
// 11. 'broken' list is sorted by isAIS/Bucket/Objname to have on all targets
//     determined order
// 12. First, 'local repair' list is processed and all CTs are moved to correct mpath
// 13. Next the rebalance proceeds with the next stage (rebStageECGlobRepair)
//     and wait for all other nodes
// 14. To minimize memory/GC load, the 'broken' list is processed in batches.
//     At this moment a batch size is 8 objects
// 15. If object was replicated:
//     - this node does not have a replica. Calculate HrwTargetList, and if
//       the node is in it, start waiting for replica from another node
//     - this node has a replica. Calculate HrwTarget and if the node is the
//       first one in the list that has replica, start sending replica to other nodes
// 16. If object was EC'ed:
//   Step #1 - transferring existing CTs:
//     - if full object does not exist, and this targets has a slice, the node
//       sends slice to the "default" node to rebuild
//     - if full object does not exists, and this node should have a slice by HRW,
//       it starts waiting for a slice from "default" target
//     - if this node is "default" and object does not exist, the node starts
//       waiting all existing slices and add the object to 'rebuild' list
//     - if full object exists but misplaced, the "default" node starts waiting for it
//     - if full object exists, and this nodes should have a slices, wait for it
//   Step #2 - now "default" node has ether full object or all existing slices
//     - if 'rebuild' list is not empty, rebuild missing slices of all objects
//       from the list and send to other nodes
// 17. Notify other nodes that batch is done and wait for other nodes
// 18. After the batch is processed, cleanup all allocated memory and open the next batch
// 19. If anything goes wrong, rebalance may abort and in deferred procedure
//     it cleans up all allocated resources
// 20. All batches processed, rebalance moves to the next stage (rebStageECCleanup).
//     Targets finalize rebalance.

const (
	// transport name for CT list exchange
	DataECRebStreamName = "reb-ec-data"
	// the number of objects processed a time
	ecRebBatchSize = 8
	// a target wait for the first slice(not a full replica) to come
	anySliceID = math.MaxInt16
)

// A "default" target wait state when it does not have full object
const (
	// target wait for a single slice that would be just saved to local drives
	waitForSingleSlice = iota
	// full object exists but on another target, wait until that target sends it
	waitForReplica
	// full object does exist, wait other targets send their slices and then rebuild
	waitForAllSlices
)

// status of an object which CT the target awaits
const (
	// not enough local slices to rebuild the object
	objWaiting = iota
	// all slices have been received, can start rebuilding
	objReceived
	// object has been rebuilt and all new slices were sent to other targets
	objDone
)

type (
	// a full info about a CT found on a target.
	// It is information sent to other targets that must be sufficient
	// to check and restore everything
	rebCT struct {
		realFQN string       // mpath CT real
		hrwFQN  string       // mpath CT by HRW
		meta    *ec.Metadata // metadata loaded from a local file

		Bucket       string `json:"bck"`
		Provider     string `json:"prov,omitempty"`
		Objname      string `json:"obj"`
		DaemonID     string `json:"sid"` // a target that has the CT
		ObjHash      string `json:"cksum"`
		ObjSize      int64  `json:"size"`
		SliceID      int16  `json:"sliceid,omitempty"`
		DataSlices   int16  `json:"data"`
		ParitySlices int16  `json:"parity"`
	}

	// A single object description which CT the targets waits for
	waitObject struct {
		// list of CTs to wait
		cts []*waitCT
		// wait type: see waitFor* enum
		wt int
		// object wait status: objWaiting, objReceived, objDone
		status int
	}

	ctList     = map[string][]*rebCT    // EC CTs grouped by a rule
	ctWaitList = map[string]*waitObject // object uid <-> list of CTs
	// a full info about an object that resides on the local target.
	// Contains both global and calculated local info
	rebObject struct {
		cts          ctList            // obj hash <-> list of CTs with the same hash
		hrwTargets   []*cluster.Snode  // the list of targets that should have CT
		rebuildSGLs  []*memsys.SGL     // temporary slices for [re]building EC
		fh           *cmn.FileHandle   // main fh when building slices from existing main
		sender       *cluster.Snode    // target responsible to send replicas over the cluster (first by HRW)
		locCT        map[string]*rebCT // CT locations: maps daemonID to CT for faster check what nodes have the CT
		ctExist      []bool            // marks existing CT: SliceID <=> Exists
		mainDaemon   string            // hrw target for an object
		uid          string            // unique identifier for the object (Bucket#Object#IsAIS)
		bucket       string            // bucket name for faster acceess
		provider     string            // cloud provider of the bucket
		objName      string            // object name for faster access
		objSize      int64             // object size
		sliceSize    int64             // a size of an object slice
		dataSlices   int16             // the number of data slices
		paritySlices int16             // the number of parity slices
		mainSliceID  int16             // sliceID on the main target
		isECCopy     bool              // replicated or erasure coded
		hasCT        bool              // local target has any obj's CT
		mainHasAny   bool              // is default target has any part of the object
		isMain       bool              // is local target a default one
		inHrwList    bool              // is local target should have any CT according to HRW
		fullObjFound bool              // some target has the full object, no need to rebuild, just copy
		hasAllSlices bool              // true: all slices existed before rebalance
	}
	rebBck struct {
		objs map[string]*rebObject // maps ObjectName <-> object info
	}
	// final result of scanning the existing objects
	globalCTList struct {
		ais   map[string]*rebBck // maps BucketName <-> map of objects
		cloud map[string]*rebBck // maps BucketName <-> map of objects
	}

	// CT destination (to use later for retransmitting lost CTs)
	retransmitCT struct {
		daemonID string
		header   transport.Header
	}

	// a list of CTs waiting for receive acknowledge from remote targets
	ackCT struct {
		mtx sync.Mutex
		ct  map[string]*retransmitCT
	}

	// Callback is called if a target did not report that it is in `stage` or
	// its notification was lost. Callback either request the current state
	// directly or makes the target to resend.
	// The callback returns `true` only if the target is in "stage" stage or
	// reached any next stage already
	StageCallback = func(si *cluster.Snode) bool

	ecRebalancer struct {
		t      cluster.Target
		statsT stats.Tracker
		cts    ctList // maps daemonID <-> CT List
		ra     *globArgs
		data   *transport.StreamBundle // to send CT and namespaces
		mtx    sync.Mutex
		mgr    *Manager
		waiter *ctWaiter // helper to manage a list of CT for current batch to wait by local target
		broken []*rebObject
		ackCTs ackCT
		onAir  atomic.Int64 // the number of CTs passed to transport but not yet sent to a remote target

		// list of CTs that should be moved between local mpaths
		localActions []*rebCT
	}

	// a description of a CT that local target awaits from another target
	waitCT struct {
		sgl     *memsys.SGL // SGL to save the received CT
		meta    []byte      // CT's EC metadata
		sliceID int16       // slice ID to wait (special value `anySliceID` - wait for the first slice for the object)
		recv    atomic.Bool // if this CT has been received already (for proper clean, rebuild, and check)
	}
	// helper object that manages slices the local target waits for from remote targets
	ctWaiter struct {
		mx        sync.Mutex
		waitFor   atomic.Int32 // the current number of slices local target awaits
		toRebuild atomic.Int32 // the current number of objects the target has to rebuild
		objs      ctWaitList   // the CT for the current batch
		mem       *memsys.Mem2
	}
)

var (
	ecPadding = make([]byte, 256) // more than enough for max number of slices
)

// Generate unique ID for an object
func uniqueWaitID(bucket, provider, objName string) string {
	return fmt.Sprintf("%s#%s#%s", bucket, provider, objName)
}

// Generate unique ID for a CT (id is a CT ordinal number).
// The combination of id and daemonID is unique as a target can contain
// only one item (either replica or slice)
func ctUID(id int, daemonID string) string {
	return fmt.Sprintf("@%d/%s", id, daemonID)
}

// Generate unique ID for a CT acknowledge.
func ackID(bucket, provider, objName, ctUID string) string {
	return fmt.Sprintf("%s#%s#%s#%s", bucket, provider, objName, ctUID)
}

//
// Rebalance object methods
// All methods should be called only after it is clear that the object exists
// or we have one or few slices. That is why `Assert` is used.
// And since a new slice is added to the list only if it matches previously
// added one, it is OK to read all info from the very first slice always
//

// Returns the list of slices with the same object hash that is the newest one.
// In majority of cases the object will contain only one list.
// TODO: implement better detection of the newest object version. Now the newest
// is determined only by the number of slices: the newest has the biggest number
func (so *rebObject) newest() []*rebCT {
	var l []*rebCT
	max := 0
	for _, ctList := range so.cts {
		if max < len(ctList) {
			max = len(ctList)
			l = ctList
		}
	}
	return l
}

// Returns how many CTs(including the original object) must exists
func (so *rebObject) requiredCT() int {
	if so.isECCopy {
		return int(so.paritySlices + 1)
	}
	return int(so.dataSlices + so.paritySlices + 1)
}

// Returns how many CTs found across all targets
func (so *rebObject) foundCT() int {
	return len(so.locCT)
}

// Returns a random CT which has metadata.
func (so *rebObject) ctWithMD() *rebCT {
	for _, ct := range so.locCT {
		if ct.meta != nil {
			return ct
		}
	}
	return nil
}

// Returns the list of targets that does not have any CT but
// they should have according to HRW
func (so *rebObject) emptyTargets(skip *cluster.Snode) cluster.Nodes {
	freeTargets := make(cluster.Nodes, 0)
	for _, tgt := range so.hrwTargets {
		if skip != nil && skip.DaemonID == tgt.DaemonID {
			continue
		}
		if _, ok := so.locCT[tgt.DaemonID]; ok {
			continue
		}
		freeTargets = append(freeTargets, tgt)
	}
	return freeTargets
}

//
//  Rebalance result methods
//

// Merge given CT with already existing CTs of an object.
// It checks if the CT is unique(in case of the object is erasure coded),
// and the CT's info about object matches previously found CTs.
func (rr *globalCTList) addCT(ct *rebCT, tgt cluster.Target) error {
	bckList := rr.ais
	if cmn.IsProviderCloud(ct.Provider, false /*acceptAnon*/) {
		bckList = rr.cloud
	}
	bck, ok := bckList[ct.Bucket]
	if !ok {
		bck = &rebBck{objs: make(map[string]*rebObject)}
		bckList[ct.Bucket] = bck
	}

	obj, ok := bck.objs[ct.Objname]
	if !ok {
		// first CT of the object
		b := &cluster.Bck{Name: ct.Bucket, Provider: ct.Provider}
		if err := b.Init(tgt.GetBowner()); err != nil {
			return err
		}
		si, err := cluster.HrwTarget(b.MakeUname(ct.Objname), tgt.GetSowner().Get())
		if err != nil {
			return err
		}
		obj = &rebObject{
			cts:        make(ctList),
			mainDaemon: si.DaemonID,
			provider:   ct.Provider,
		}
		obj.cts[ct.ObjHash] = []*rebCT{ct}
		bck.objs[ct.Objname] = obj
		return nil
	}

	// sanity check: sliceID must be unique (unless it is 0)
	if ct.SliceID != 0 {
		list := obj.cts[ct.ObjHash]
		for _, found := range list {
			if found.SliceID == ct.SliceID {
				err := fmt.Errorf("Duplicated %s/%s SliceID %d from %s (discarded)",
					ct.Bucket, ct.Objname, ct.SliceID, ct.DaemonID)
				return err
			}
		}
	}
	obj.cts[ct.ObjHash] = append(obj.cts[ct.ObjHash], ct)
	return nil
}

//
//  EC Rebalancer methods and utilities
//

func newECRebalancer(t cluster.Target, mgr *Manager, statsT stats.Tracker) *ecRebalancer {
	return &ecRebalancer{
		t:            t,
		mgr:          mgr,
		waiter:       newWaiter(t.GetMem2()),
		cts:          make(ctList),
		localActions: make([]*rebCT, 0),
		ackCTs:       ackCT{ct: make(map[string]*retransmitCT)},
		statsT:       statsT,
	}
}

func (s *ecRebalancer) init(ra *globArgs, netd string) {
	s.ra = ra
	client := transport.NewIntraDataClient()
	dataArgs := transport.SBArgs{
		Network:    netd,
		Trname:     DataECRebStreamName,
		Multiplier: int(ra.config.Rebalance.Multiplier),
	}
	s.data = transport.NewStreamBundle(s.t.GetSowner(), s.t.Snode(), client, dataArgs)
}

// Returns a CT list collected by `si` target
func (s *ecRebalancer) nodeData(daemonID string) ([]*rebCT, bool) {
	s.mtx.Lock()
	cts, ok := s.cts[daemonID]
	s.mtx.Unlock()
	return cts, ok
}

// Store a CT list received from `daemonID` target
func (s *ecRebalancer) setNodeData(daemonID string, cts []*rebCT) {
	s.mtx.Lock()
	s.cts[daemonID] = cts
	s.mtx.Unlock()
}

// Add a CT to list of CTs of a given target
func (s *ecRebalancer) appendNodeData(daemonID string, ct *rebCT) {
	s.mtx.Lock()
	s.cts[daemonID] = append(s.cts[daemonID], ct)
	s.mtx.Unlock()
}

// Sends local CT along with EC metadata to remote targets.
// The CT is on a local drive and not loaded into SGL. Just read and send.
func (s *ecRebalancer) sendFromDisk(ct *rebCT, targets ...*cluster.Snode) error {
	cmn.Assert(ct.meta != nil)
	req := pushReq{
		DaemonID: s.t.Snode().DaemonID,
		Stage:    rebStageECGlobRepair,
		RebID:    s.mgr.globRebID.Load(),
		Extra:    cmn.MustMarshal(ct.meta),
	}

	fqn := ct.realFQN
	if ct.hrwFQN != "" {
		fqn = ct.hrwFQN
	}
	resolved, _, err := cluster.ResolveFQN(fqn)
	if err != nil {
		return err
	}
	hdr := transport.Header{
		Bucket:   ct.Bucket,
		Provider: ct.Provider,
		ObjName:  ct.Objname,
		ObjAttrs: transport.ObjectAttrs{
			Size: ct.ObjSize,
		},
		Opaque: cmn.MustMarshal(req),
	}

	if resolved.ContentType == fs.ObjectType {
		lom := cluster.LOM{T: s.t, FQN: fqn}
		if err := lom.Init(ct.Bucket, ct.Provider); err != nil {
			return err
		}
		if err := lom.Load(false); err != nil {
			return err
		}

		hdr.ObjAttrs.Atime = lom.AtimeUnix()
		hdr.ObjAttrs.Version = lom.Version()
		if cksum := lom.Cksum(); cksum != nil {
			hdr.ObjAttrs.CksumType, hdr.ObjAttrs.CksumValue = cksum.Get()
		}
	}
	if ct.SliceID != 0 {
		hdr.ObjAttrs.Size = ec.SliceSize(ct.ObjSize, int(ct.DataSlices))
	}

	fh, err := cmn.NewFileHandle(ct.hrwFQN)
	if err != nil {
		return err
	}

	// temporary list of UIDs for proper cleanup if Send fails
	rebUIDs := make([]string, 0, len(targets))
	for _, tgt := range targets {
		dest := &retransmitCT{daemonID: tgt.ID(), header: hdr}
		ctUID := ctUID(int(ct.SliceID), tgt.ID())
		uid := ackID(ct.Bucket, ct.Provider, ct.Objname, ctUID)
		s.ackCTs.mtx.Lock()
		s.ackCTs.ct[uid] = dest
		s.ackCTs.mtx.Unlock()
		rebUIDs = append(rebUIDs, uid)
	}
	s.onAir.Inc()
	if err := s.data.Send(transport.Obj{Hdr: hdr, Callback: s.transportCB}, fh, targets...); err != nil {
		s.onAir.Dec()
		fh.Close()
		s.ackCTs.mtx.Lock()
		for _, uid := range rebUIDs {
			delete(s.ackCTs.ct, uid)
		}
		s.ackCTs.mtx.Unlock()
		return fmt.Errorf("Failed to send slices to nodes [%s..]: %v", targets[0].DaemonID, err)
	}
	s.statsT.AddMany(
		stats.NamedVal64{Name: stats.TxRebCount, Value: 1},
		stats.NamedVal64{Name: stats.TxRebSize, Value: hdr.ObjAttrs.Size},
	)
	return nil
}

// Track the number of sent CTs
func (s *ecRebalancer) transportCB(_ transport.Header, reader io.ReadCloser, _ unsafe.Pointer, _ error) {
	s.onAir.Dec()
}

// Sends reconstructed slice along with EC metadata to remote target.
// EC metadata is of main object, so its internal field SliceID must be
// fixed prior to sending.
// Use the function to send only slices (not for replicas/full object)
func (s *ecRebalancer) sendFromReader(reader cmn.ReadOpenCloser,
	ct *rebCT, sliceID int, xxhash string, target *cluster.Snode) error {
	cmn.AssertMsg(ct.meta != nil, ct.Objname)
	newMeta := *ct.meta // copy meta (it does not contain pointers)
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("Sending slice %d[%s] of %s to %s", sliceID, xxhash, ct.Objname, target.Name())
	}
	newMeta.SliceID = sliceID
	req := pushReq{
		DaemonID: s.t.Snode().DaemonID,
		Stage:    rebStageECGlobRepair,
		RebID:    s.mgr.globRebID.Load(),
		Extra:    cmn.MustMarshal(&newMeta),
	}
	cmn.AssertMsg(ct.ObjSize != 0, ct.Objname)
	size := ec.SliceSize(ct.ObjSize, int(ct.DataSlices))
	hdr := transport.Header{
		Bucket:   ct.Bucket,
		Provider: ct.Provider,
		ObjName:  ct.Objname,
		ObjAttrs: transport.ObjectAttrs{
			Size: size,
		},
		Opaque: cmn.MustMarshal(req),
	}
	if xxhash != "" {
		hdr.ObjAttrs.CksumValue = xxhash
		hdr.ObjAttrs.CksumType = cmn.ChecksumXXHash
	}

	sliceUID := ctUID(sliceID, target.ID())
	uid := ackID(ct.Bucket, ct.Provider, ct.Objname, sliceUID)
	dest := &retransmitCT{daemonID: target.ID(), header: hdr}
	s.ackCTs.mtx.Lock()
	s.ackCTs.ct[uid] = dest
	s.ackCTs.mtx.Unlock()

	s.onAir.Inc()
	if err := s.data.Send(transport.Obj{Hdr: hdr, Callback: s.transportCB}, reader, target); err != nil {
		s.onAir.Dec()
		s.ackCTs.mtx.Lock()
		delete(s.ackCTs.ct, uid)
		s.ackCTs.mtx.Unlock()
		return fmt.Errorf("Failed to send slices to node %s: %v", target.Name(), err)
	}

	s.statsT.AddMany(
		stats.NamedVal64{Name: stats.TxRebCount, Value: 1},
		stats.NamedVal64{Name: stats.TxRebSize, Value: size},
	)
	return nil
}

// Saves received CT to a local drive if needed:
//   1. Full object/replica is received
//   2. A CT is received and this target is not the default target (it
//      means that the CTs came from default target after EC had been rebuilt)
func (s *ecRebalancer) saveCTToDisk(data *memsys.SGL, req *pushReq, md *ec.Metadata, hdr transport.Header) error {
	cmn.Assert(req.Extra != nil)
	var (
		ctFQN    string
		lom      *cluster.LOM
		bck      = &cluster.Bck{Name: hdr.Bucket, Provider: hdr.Provider}
		needSave = md.SliceID == 0 // full object always saved
	)
	if err := bck.Init(s.t.GetBowner()); err != nil {
		return err
	}
	uname := bck.MakeUname(hdr.ObjName)
	if !needSave {
		// slice is saved only if this target is not "main" one.
		// Main one receives slices as well but it uses them only to rebuild "full"
		tgt, err := cluster.HrwTarget(uname, s.t.GetSowner().Get())
		if err != nil {
			return err
		}
		needSave = tgt.DaemonID != s.t.Snode().DaemonID
	}
	if !needSave {
		return nil
	}
	mpath, _, err := cluster.HrwMpath(uname)
	if err != nil {
		return err
	}
	if md.SliceID != 0 {
		ctFQN = mpath.MakePathBucketObject(ec.SliceType, hdr.Bucket, hdr.Provider, hdr.ObjName)
	} else {
		lom = &cluster.LOM{T: s.t, Objname: hdr.ObjName}
		if err := lom.Init(hdr.Bucket, hdr.Provider); err != nil {
			return err
		}
		ctFQN = lom.FQN
		lom.SetSize(hdr.ObjAttrs.Size)
		if hdr.ObjAttrs.Version != "" {
			lom.SetVersion(hdr.ObjAttrs.Version)
		}
		if hdr.ObjAttrs.CksumType != "" {
			lom.SetCksum(cmn.NewCksum(hdr.ObjAttrs.CksumType, hdr.ObjAttrs.CksumValue))
		}
		if hdr.ObjAttrs.Atime != 0 {
			lom.SetAtimeUnix(hdr.ObjAttrs.Atime)
		}
		lom.Lock(true)
		defer lom.Unlock(true)
		lom.Uncache()
	}

	buffer, slab := s.t.GetMem2().AllocDefault()
	metaFQN := mpath.MakePathBucketObject(ec.MetaType, hdr.Bucket, hdr.Provider, hdr.ObjName)
	_, err = cmn.SaveReader(metaFQN, bytes.NewReader(req.Extra), buffer, false)
	if err != nil {
		slab.Free(buffer)
		return err
	}
	tmpFQN := mpath.MakePathBucketObject(fs.WorkfileType, hdr.Bucket, hdr.Provider, hdr.ObjName)
	cksum, err := cmn.SaveReaderSafe(tmpFQN, ctFQN, memsys.NewReader(data), buffer, true)
	if md.SliceID == 0 && hdr.ObjAttrs.CksumType == cmn.ChecksumXXHash && hdr.ObjAttrs.CksumValue != cksum.Value() {
		err = fmt.Errorf("Mismatched hash for %s/%s, version %s, hash calculated %s/header %s/md %s",
			hdr.Bucket, hdr.ObjName, hdr.ObjAttrs.Version, cksum.Value(), hdr.ObjAttrs.CksumValue, md.ObjCksum)
	}
	slab.Free(buffer)
	if err != nil {
		// Persist may call FSHC, too. To avoid double FSHC call, do extra check now.
		s.t.FSHC(err, ctFQN)
	} else if md.SliceID == 0 {
		err = lom.Persist()
	}

	if err != nil {
		os.Remove(tmpFQN)
		if rmErr := os.Remove(metaFQN); rmErr != nil && !os.IsNotExist(rmErr) {
			glog.Errorf("Nested error: save replica -> remove metadata file: %v", rmErr)
		}
		if rmErr := os.Remove(ctFQN); rmErr != nil && !os.IsNotExist(rmErr) {
			glog.Errorf("Nested error: save replica -> remove replica: %v", rmErr)
		}
	}

	return err
}

// Receives a CT from another target, saves to local drive because it is a missing one
func (s *ecRebalancer) receiveCT(req *pushReq, hdr transport.Header, reader io.Reader) error {
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("%s GOT CT for %s/%s/%s from %s", s.t.Snode().Name(), hdr.Provider, hdr.Bucket, hdr.ObjName, req.DaemonID)
	}
	var md ec.Metadata
	cmn.Assert(req.Extra != nil)
	if err := jsoniter.Unmarshal(req.Extra, &md); err != nil {
		cmn.DrainReader(reader)
		return err
	}

	s.mgr.laterx.Store(true)
	sliceID := md.SliceID
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof(">>> %s got CT %d for [%s]%s/%s", s.t.Snode().Name(), sliceID, hdr.Provider, hdr.Bucket, hdr.ObjName)
	}
	uid := uniqueWaitID(hdr.Bucket, hdr.Provider, hdr.ObjName)
	waitFor := s.waiter.lookupCreate(uid, int16(sliceID), waitForSingleSlice)
	cmn.Assert(waitFor != nil)
	cmn.Assert(waitFor.sgl != nil)
	cmn.Assert(!waitFor.recv.Load())

	waitFor.meta = req.Extra
	n, err := io.Copy(waitFor.sgl, reader)
	if err != nil {
		return fmt.Errorf("failed to read slice %d for %s/%s/%s: %v", sliceID, hdr.Provider, hdr.Bucket, hdr.ObjName, err)
	}
	s.statsT.AddMany(
		stats.NamedVal64{Name: stats.RxRebCount, Value: 1},
		stats.NamedVal64{Name: stats.RxRebSize, Value: n},
	)
	ckval, _ := cksumForSlice(memsys.NewReader(waitFor.sgl), waitFor.sgl.Size(), s.t.GetMem2())
	if hdr.ObjAttrs.CksumValue != "" && hdr.ObjAttrs.CksumValue != ckval {
		return fmt.Errorf("received checksum mismatches checksum in header %s vs %s",
			hdr.ObjAttrs.CksumValue, ckval)
	}
	waitFor.recv.Store(true)
	waitRebuild := s.waiter.updateRebuildInfo(uid)

	if !waitRebuild || sliceID == 0 {
		if err := s.saveCTToDisk(waitFor.sgl, req, &md, hdr); err != nil {
			glog.Errorf("Failed to save CT %d of %s: %v", sliceID, hdr.ObjName, err)
			s.mgr.abortGlobal()
		}
	}

	// notify that another slice is received successfully
	remains := s.waiter.waitFor.Dec()
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("CTs to get remains: %d", remains)
	}

	// send acknowledge back to the caller
	smap := (*cluster.Smap)(s.mgr.smap.Load())
	tsi := smap.GetTarget(req.DaemonID)
	hdr.Opaque = []byte(ctUID(sliceID, s.mgr.t.Snode().ID()))
	hdr.ObjAttrs.Size = 0
	if err := s.mgr.acks.Send(transport.Obj{Hdr: hdr}, nil, tsi); err != nil {
		glog.Error(err)
	}

	return nil
}

// On receiving a list of collected CT from another target.
func (s *ecRebalancer) OnData(w http.ResponseWriter, hdr transport.Header, reader io.Reader, err error) {
	if err != nil {
		glog.Errorf("Failed to get ack for %s/%s/%s: %v", hdr.Provider, hdr.Bucket, hdr.ObjName, err)
		return
	}

	var req pushReq
	if err := jsoniter.Unmarshal(hdr.Opaque, &req); err != nil {
		glog.Errorf("Invalid push notification: %v", err)
		return
	}

	// a target was too late in sending(rebID is obsolete) its data or too early (ra == nil)
	if s.ra == nil || req.RebID != s.mgr.globRebID.Load() {
		glog.Warningf("Local node has not started or already has finished rebalancing")
		cmn.DrainReader(reader)
		return
	}

	// a remote target sent CT
	if req.Stage == rebStageECGlobRepair {
		if err := s.receiveCT(&req, hdr, reader); err != nil {
			glog.Errorf("Failed to receive CT for %s/%s/%s: %v", hdr.Provider, hdr.Bucket, hdr.ObjName, err)
			return
		}
		return
	}

	// otherwise a remote target sent collected list of its CTs:
	// - receive the CT list
	// - update the remote target stage
	if req.Stage != rebStageECNamespace {
		glog.Errorf("Invalid stage %s : %s (must be %s)", hdr.ObjName,
			stages[req.Stage], stages[rebStageECNamespace])
		cmn.DrainReader(reader)
		return
	}

	b, err := ioutil.ReadAll(reader)
	if err != nil {
		glog.Errorf("Failed to read data from %s: %v", req.DaemonID, err)
		return
	}

	cts := make([]*rebCT, 0)
	if err = jsoniter.Unmarshal(b, &cts); err != nil {
		glog.Errorf("Failed to unmarshal data from %s: %v", req.DaemonID, err)
		return
	}

	s.setNodeData(req.DaemonID, cts)
	s.mgr.stages.setStage(req.DaemonID, req.Stage, 0)
}

// Build a list buckets with their objects from a flat list of all CTs
func (s *ecRebalancer) mergeCTs() *globalCTList {
	res := &globalCTList{
		ais:   make(map[string]*rebBck),
		cloud: make(map[string]*rebBck),
	}

	// process all received CTs
	localDaemon := s.t.Snode().DaemonID
	smap := s.t.GetSowner().Get()
	for sid := range smap.Tmap {
		local := sid == localDaemon
		ctList, ok := s.nodeData(sid)
		if !ok {
			continue
		}
		for _, ct := range ctList {
			if ct.SliceID != 0 && local {
				b := &cluster.Bck{Name: ct.Bucket, Provider: ct.Provider}
				if err := b.Init(s.t.GetBowner()); err != nil {
					s.mgr.abortGlobal()
					return nil
				}
				t, err := cluster.HrwTarget(b.MakeUname(ct.Objname), smap)
				cmn.Assert(err == nil)
				if t.DaemonID == localDaemon {
					glog.Infof("Skipping CT %d of %s (it must have main object)", ct.SliceID, ct.Objname)
					continue
				}
			}
			if err := res.addCT(ct, s.t); err != nil {
				glog.Warning(err)
				continue
			}
			if local && ct.hrwFQN != ct.realFQN {
				s.localActions = append(s.localActions, ct)
				if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
					glog.Infof("%s %s -> %s", s.t.Snode().Name(), ct.hrwFQN, ct.realFQN)
				}
			}
		}
	}
	return res
}

// Find objects that have either missing or misplaced parts. If a part is a
// slice or replica(not the "default" object) and mpath is correct the object
// is not considered as broken one even if its target is not in HRW list
func (s *ecRebalancer) detectBroken(res *globalCTList) {
	s.broken = make([]*rebObject, 0)
	bowner := s.t.GetBowner()
	bmd := bowner.Get()
	smap := s.t.GetSowner().Get()

	providers := map[string]map[string]*rebBck{
		cmn.ProviderAIS:              res.ais,
		cmn.GCO.Get().Cloud.Provider: res.cloud,
	}
	for provider, tp := range providers {
		for bckName, objs := range tp {
			if provider == "" {
				// Cloud provider can be empty so we do not need to do anything.
				continue
			}

			bck := &cluster.Bck{Name: bckName, Provider: provider}
			if err := bck.Init(bowner); err != nil {
				// bucket might be deleted while rebalancing - skip it
				glog.Errorf("Invalid bucket %s: %v", bckName, err)
				delete(tp, bckName)
				continue
			}
			bprops, ok := bmd.Get(bck)
			if !ok {
				// bucket might be deleted while rebalancing - skip it
				glog.Errorf("Bucket %s does not exist", bckName)
				delete(tp, bckName)
				continue
			}
			for objName, obj := range objs.objs {
				if err := s.calcLocalProps(bck, obj, smap, &bprops.EC); err != nil {
					glog.Warningf("Detect %s failed, skipping: %v", obj.objName, err)
					continue
				}

				mainHasObject := (obj.mainSliceID == 0 || obj.isECCopy) && obj.mainHasAny
				allCTFound := obj.foundCT() >= obj.requiredCT()
				// the object is good, nothing to restore:
				// 1. Either objects is replicated, default target has replica
				//    and total number of replicas is sufficient for EC
				// 2. Or object is EC'ed, default target has the object and
				//    the number of slices equals Data+Parity number
				if allCTFound && mainHasObject {
					continue
				}

				if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
					glog.Infof("[%s] BROKEN: %s [Main %d on %s], CTs %d of %d",
						s.t.Snode().Name(), objName, obj.mainSliceID, obj.mainDaemon, obj.foundCT(), obj.requiredCT())
				}
				s.broken = append(s.broken, obj)
			}
		}
	}

	// sort the list of broken object to have deterministic order on all targets
	// sort order: IsAIS/Bucket name/Object name
	ctLess := func(i, j int) bool {
		if s.broken[i].provider != s.broken[j].provider {
			return cmn.IsProviderAIS(s.broken[j].provider)
		}
		bi := s.broken[i].bucket
		bj := s.broken[j].bucket
		if bi != bj {
			return bi < bj
		}
		return s.broken[i].objName < s.broken[j].objName
	}
	sort.Slice(s.broken, ctLess)
}

// merge, sort, and detect what to fix and how
func (s *ecRebalancer) checkCTs() {
	cts := s.mergeCTs()
	if cts == nil {
		return
	}
	s.detectBroken(cts)
}

// mountpath walker - walks through files in /meta/ directory
func (s *ecRebalancer) jog(path string, wg *sync.WaitGroup) {
	defer wg.Done()
	opts := &fs.Options{
		Callback: s.walk,
		Sorted:   false,
	}
	if err := fs.Walk(path, opts); err != nil {
		if s.mgr.xreb.Aborted() || s.mgr.xreb.Finished() {
			glog.Infof("Aborting %s traversal", path)
		} else {
			glog.Warningf("failed to traverse %q, err: %v", path, err)
		}
	}
}

// a file walker:
// - loads EC metadata from file
// - checks if the corresponding CT exists
// - calculates where "main" object for the CT is
// - store all the info above to memory
func (s *ecRebalancer) walk(fqn string, de fs.DirEntry) (err error) {
	if s.mgr.xreb.Aborted() {
		// notify `dir.Walk` to stop iterations
		return errors.New("Interrupt walk")
	}

	if de.IsDir() {
		return nil
	}

	md, err := ec.LoadMetadata(fqn)
	if err != nil {
		glog.Warningf("Damaged file? Failed to load metadata from %q: %v", fqn, err)
		return nil
	}

	ct, err := cluster.NewCTFromFQN(fqn, s.t.GetBowner())
	if err != nil {
		return nil
	}
	// do not touch directories for buckets with EC disabled (for now)
	// TODO: what to do if we found metafile on a bucket with EC disabled?
	if !ct.Bprops().EC.Enabled {
		return filepath.SkipDir
	}

	// generate CT path in the same mpath that metadata is, and detect CT
	isReplica := true
	fileFQN := ct.Make(fs.ObjectType)
	if _, err := os.Stat(fileFQN); err != nil {
		isReplica = false
		fileFQN = ct.Make(ec.SliceType)
		_, err = os.Stat(fileFQN)
		// found metadata without a corresponding CT
		if err != nil {
			glog.Warningf("%s no CT for metadata: %s", s.t.Snode().Name(), fileFQN)
			return nil
		}
	}

	// calculate correct FQN
	var hrwFQN string
	if isReplica {
		hrwFQN, _, err = cluster.HrwFQN(fs.ObjectType, ct.Bck(), ct.ObjName())
	} else {
		hrwFQN, _, err = cluster.HrwFQN(ec.SliceType, ct.Bck(), ct.ObjName())
	}
	if err != nil {
		return err
	}

	ct, err = cluster.NewCTFromFQN(fileFQN, s.t.GetBowner())
	if err != nil {
		return nil
	}

	id := s.t.Snode().DaemonID
	rec := &rebCT{
		Bucket:       ct.Bucket(),
		Provider:     ct.Provider(),
		Objname:      ct.ObjName(),
		DaemonID:     id,
		ObjHash:      md.ObjCksum,
		ObjSize:      md.Size,
		SliceID:      int16(md.SliceID),
		DataSlices:   int16(md.Data),
		ParitySlices: int16(md.Parity),
		realFQN:      fileFQN,
		hrwFQN:       hrwFQN,
		meta:         md,
	}
	s.appendNodeData(id, rec)

	return nil
}

// Empties internal temporary data to be ready for the next rebalance.
func (s *ecRebalancer) cleanup() {
	s.mtx.Lock()
	s.cts = make(ctList)
	s.localActions = make([]*rebCT, 0)
	s.broken = nil
	s.mtx.Unlock()
	s.waiter.cleanup()
}

func (s *ecRebalancer) endStreams() {
	if s.data != nil {
		s.data.Close(true)
		s.data = nil
	}
}

// Main method - starts all mountpaths walkers, waits for them to finish, and
// changes internal stage after that to 'traverse done', so the caller may continue
// rebalancing: send collected data to other targets, rebuild slices etc
func (s *ecRebalancer) run() {
	var (
		mpath string

		wg                = sync.WaitGroup{}
		availablePaths, _ = fs.Mountpaths.Get()
		cfg               = cmn.GCO.Get()
	)

	for _, mpathInfo := range availablePaths {
		if s.mgr.xreb.Bucket() == "" {
			mpath = mpathInfo.MakePath(ec.MetaType, cmn.ProviderAIS)
		} else {
			mpath = mpathInfo.MakePathBucket(ec.MetaType, s.mgr.xreb.Bucket(), cmn.ProviderAIS)
		}
		wg.Add(1)
		go s.jog(mpath, &wg)
	}

	if cfg.Cloud.Supported {
		for _, mpathInfo := range availablePaths {
			if s.mgr.xreb.Bucket() == "" {
				mpath = mpathInfo.MakePath(ec.MetaType, cfg.Cloud.Provider)
			} else {
				mpath = mpathInfo.MakePathBucket(ec.MetaType, s.mgr.xreb.Bucket(), cfg.Cloud.Provider)
			}
			wg.Add(1)
			go s.jog(mpath, &wg)
		}
	}
	wg.Wait()
	s.mgr.changeStage(rebStageECNamespace, 0)
}

// send collected CTs to all targets with retry
func (s *ecRebalancer) exchange() error {
	const (
		retries = 3               // number of retries to send collected CT info
		sleep   = 5 * time.Second // delay between retries
	)

	globRebID := s.mgr.globRebID.Load()
	smap := s.t.GetSowner().Get()

	// TODO -- FIXME: add a helper in the cluster pkg: NodeMap => Nodes skipping self

	sendTo := make(cluster.Nodes, 0, len(smap.Tmap))
	failed := make(cluster.Nodes, 0, len(smap.Tmap))
	for _, node := range smap.Tmap {
		if node.DaemonID == s.t.Snode().DaemonID {
			continue
		}
		sendTo = append(sendTo, node)
	}

	emptyCT := make([]*rebCT, 0)
	for i := 0; i < retries; i++ {
		failed = failed[:0]
		for _, node := range sendTo {
			if s.mgr.xreb.Aborted() {
				return fmt.Errorf("%d: aborted", globRebID)
			}

			cts, ok := s.nodeData(s.t.Snode().DaemonID)
			if !ok {
				// no data collected for the target, send empty notification
				cts = emptyCT
			}

			req := pushReq{
				DaemonID: s.t.Snode().DaemonID,
				Stage:    rebStageECNamespace,
				RebID:    globRebID,
			}
			body := cmn.MustMarshal(cts)
			hdr := transport.Header{
				ObjAttrs: transport.ObjectAttrs{Size: int64(len(body))},
				Opaque:   cmn.MustMarshal(req),
			}
			rd := cmn.NewByteHandle(body)
			if err := s.data.Send(transport.Obj{Hdr: hdr}, rd, node); err != nil {
				glog.Errorf("Failed to send CTs to node %s: %v", node.DaemonID, err)
				failed = append(failed, node)
			}
			s.statsT.AddMany(
				stats.NamedVal64{Name: stats.TxRebCount, Value: 1},
				stats.NamedVal64{Name: stats.TxRebSize, Value: int64(len(body))},
			)
		}

		if len(failed) == 0 {
			s.mgr.changeStage(rebStageECDetect, 0)
			return nil
		}

		time.Sleep(sleep)
		copy(sendTo, failed)
	}

	return fmt.Errorf("Could not sent data to %d nodes", len(failed))
}

func (s *ecRebalancer) rebalanceLocalSlice(fromFQN, toFQN string, buf []byte) error {
	if _, _, err := cmn.CopyFile(fromFQN, toFQN, buf, false); err != nil {
		s.t.FSHC(err, fromFQN)
		s.t.FSHC(err, toFQN)
		return err
	}
	if rmErr := os.Remove(fromFQN); rmErr != nil { // not severe error, can continue
		glog.Errorf("Error cleaning up %q: %v", fromFQN, rmErr)
	}
	return nil
}

func (s *ecRebalancer) rebalanceLocalObject(fromMpath fs.ParsedFQN, fromFQN, toFQN string, buf []byte) error {
	lom := &cluster.LOM{T: s.t, FQN: fromFQN}
	err := lom.Init(fromMpath.Bucket, fromMpath.Provider)
	if err == nil {
		err = lom.Load()
	}
	if err != nil {
		return err
	}

	lom.Lock(true)
	lom.Uncache()
	_, err = lom.CopyObject(toFQN, buf)
	lom.Unlock(true)
	if err != nil {
		s.t.FSHC(err, fromFQN)
		s.t.FSHC(err, toFQN)
		return err
	}

	if err := os.Remove(fromFQN); err != nil {
		glog.Errorf("Failed to cleanup %q: %v", fromFQN, err)
	}
	return nil
}

// Moves local misplaced CT to correct mpath
func (s *ecRebalancer) rebalanceLocal() error {
	buf, slab := s.t.GetMem2().AllocDefault()
	defer slab.Free(buf)
	for _, act := range s.localActions {
		if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
			glog.Infof("%s Repair local %s -> %s", s.t.Snode().Name(), act.realFQN, act.hrwFQN)
		}
		mpathSrc, _, err := cluster.ResolveFQN(act.realFQN)
		if err != nil {
			return err
		}
		mpathDst, _, err := cluster.ResolveFQN(act.hrwFQN)
		if err != nil {
			return err
		}

		metaSrcFQN := mpathSrc.MpathInfo.MakePathBucketObject(ec.MetaType, mpathSrc.Bucket, mpathSrc.Provider, mpathSrc.ObjName)
		metaDstFQN := mpathDst.MpathInfo.MakePathBucketObject(ec.MetaType, mpathDst.Bucket, mpathDst.Provider, mpathDst.ObjName)
		_, _, err = cmn.CopyFile(metaSrcFQN, metaDstFQN, buf, false)
		if err != nil {
			return err
		}

		// slice case
		if act.SliceID != 0 {
			if err := s.rebalanceLocalSlice(act.realFQN, act.hrwFQN, buf); err != nil {
				os.Remove(metaDstFQN)
				return err
			}
			continue
		}

		// object/replica case
		if err := s.rebalanceLocalObject(mpathSrc, act.realFQN, act.hrwFQN, buf); err != nil {
			if rmErr := os.Remove(metaDstFQN); rmErr != nil {
				glog.Errorf("Error cleaning up %q: %v", metaDstFQN, rmErr)
			}
			return err
		}

		if err := os.Remove(metaSrcFQN); err != nil {
			glog.Errorf("Failed to cleanup %q: %v", metaSrcFQN, err)
		}
	}

	s.mgr.changeStage(rebStageECGlobRepair, 0)
	return nil
}

// Fills object properties with props that must be calculated locally
func (s *ecRebalancer) calcLocalProps(bck *cluster.Bck, obj *rebObject, smap *cluster.Smap, ecConfig *cmn.ECConf) (err error) {
	localDaemon := s.t.Snode().DaemonID
	cts := obj.newest()
	cmn.Assert(len(cts) != 0) // cannot happen
	mainSlice := cts[0]

	obj.bucket = mainSlice.Bucket
	obj.objName = mainSlice.Objname
	obj.objSize = mainSlice.ObjSize
	obj.isECCopy = ec.IsECCopy(obj.objSize, ecConfig)
	obj.dataSlices = mainSlice.DataSlices
	obj.paritySlices = mainSlice.ParitySlices
	obj.sliceSize = ec.SliceSize(obj.objSize, int(obj.dataSlices))

	ctFound := obj.foundCT()
	ctReq := obj.requiredCT()
	obj.ctExist = make([]bool, ctReq)
	obj.locCT = make(map[string]*rebCT, ctFound)

	obj.uid = uniqueWaitID(mainSlice.Bucket, obj.provider, mainSlice.Objname)
	obj.isMain = obj.mainDaemon == localDaemon

	// TODO: must check only slices of the newest object version
	// FIXME: after EC versioning is implemented
	ctCnt := int16(0)
	for _, ct := range cts {
		obj.locCT[ct.DaemonID] = ct
		if ct.DaemonID == localDaemon {
			obj.hasCT = true
		}
		if ct.DaemonID == obj.mainDaemon {
			obj.mainHasAny = true
			obj.mainSliceID = ct.SliceID
		}
		if ct.SliceID == 0 {
			obj.fullObjFound = true
		}
		obj.ctExist[ct.SliceID] = true
		if ct.SliceID != 0 {
			ctCnt++
		}
	}
	obj.hasAllSlices = ctCnt >= obj.dataSlices+obj.paritySlices

	genCount := cmn.Max(ctReq, len(smap.Tmap))
	obj.hrwTargets, err = cluster.HrwTargetList(bck.MakeUname(obj.objName), smap, genCount)
	if err != nil {
		return err
	}
	// check if HRW thinks this target must have any CT
	toCheck := ctReq - ctFound
	for _, tgt := range obj.hrwTargets[:ctReq] {
		if toCheck == 0 {
			break
		}
		if tgt.DaemonID == localDaemon {
			obj.inHrwList = true
			break
		}
		if _, ok := obj.locCT[tgt.DaemonID]; !ok {
			toCheck--
		}
	}
	// detect which target is responsible to send missing replicas to all
	// other target that miss their replicas
	for _, si := range obj.hrwTargets {
		if _, ok := obj.locCT[si.DaemonID]; ok {
			obj.sender = si
			break
		}
	}

	cmn.Assert(obj.sender != nil) // must not happen
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		if obj.sender == nil {
			glog.Infof("%s %s: hasSlice %v, fullObjExist: %v, isMain %v [mainHas: %v - %d], slice found %d vs required %d[all slices: %v], is in HRW %v",
				s.t.Snode().Name(), obj.uid, obj.hasCT, obj.fullObjFound, obj.isMain, obj.mainHasAny, obj.mainSliceID, ctFound, ctReq, obj.hasAllSlices, obj.inHrwList)
		} else {
			glog.Infof("%s %s: hasSlice %v, fullObjExist: %v, isMain %v [mainHas: %v - %d], slice found %d vs required %d[all slices: %v], is in HRW %v [sender %s]",
				s.t.Snode().Name(), obj.uid, obj.hasCT, obj.fullObjFound, obj.isMain, obj.mainHasAny, obj.mainSliceID, ctFound, ctReq, obj.hasAllSlices, obj.inHrwList, obj.sender.Name())
		}
	}
	return nil
}

// true if this target is not "default" one, and does not have any CT,
// and does not want any CT, the target can skip the object
func (s *ecRebalancer) shouldSkipObj(obj *rebObject) bool {
	return (!obj.inHrwList && !obj.hasCT) ||
		(!obj.isMain && obj.mainHasAny && obj.mainSliceID == 0 &&
			(!obj.inHrwList || obj.hasCT))
}

// Get the ordinal number of a target in HRW list of targets that have a slice.
// Returns -1 if target is not found in the list.
func (s *ecRebalancer) targetIndex(daemonID string, obj *rebObject) int {
	cnt := 0
	// always skip the "default" target
	for _, tgt := range obj.hrwTargets[1:] {
		if _, ok := obj.locCT[tgt.DaemonID]; !ok {
			continue
		}
		if tgt.DaemonID == daemonID {
			return cnt
		}
		cnt++
	}
	return -1
}

// True if local target has a slice and it should send it to "default" target
// to rebuild the full object as it is missing. Even if the target has a slice
// it may skip sending it to the main target: the case is when there are
// already 'dataSliceCount' targets are going to send their slices(by HRW).
// Trading network traffic for main target's CPU.
func (s *ecRebalancer) shouldSendSlice(obj *rebObject) (hasSlice bool, shouldSend bool) {
	if obj.isMain {
		return false, false
	}
	// First check if this target in the first 'dataSliceCount' slices.
	// Skip the first target in list for it is the main one.
	tgtIndex := s.targetIndex(s.t.Snode().DaemonID, obj)
	shouldSend = tgtIndex >= 0 && tgtIndex < int(obj.dataSlices)
	hasSlice = obj.hasCT && !obj.isMain && !obj.isECCopy && !obj.fullObjFound
	if hasSlice && (bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun) {
		locSlice := obj.locCT[s.t.Snode().DaemonID]
		glog.Infof("Should send: %s[%d] - %d : %v / %v", obj.uid, locSlice.SliceID, tgtIndex,
			hasSlice, shouldSend)
	}
	return hasSlice, shouldSend
}

// true if the object is not replicated, and this target has full object, and the
// target is not the default target, and default target does not have full object
func (s *ecRebalancer) hasFullObjMisplaced(obj *rebObject) bool {
	locCT, ok := obj.locCT[s.t.Snode().DaemonID]
	return ok && !obj.isECCopy && !obj.isMain && locCT.SliceID == 0 &&
		(!obj.mainHasAny || obj.mainSliceID != 0)
}

// true if the target needs replica: if it is default one and replica is missing,
// or the total number of replicas is less than required and this target must
// have a replica according to HRW
func (s *ecRebalancer) needsReplica(obj *rebObject) bool {
	return (obj.isMain && !obj.mainHasAny) ||
		(!obj.hasCT && obj.inHrwList)
}

// Read CT from local drive and send to another target.
// If destination is not defined the target sends its data to "default by HRW" target
func (s *ecRebalancer) sendLocalData(obj *rebObject, si ...*cluster.Snode) error {
	s.mgr.laterx.Store(true)
	ct, ok := obj.locCT[s.t.Snode().DaemonID]
	cmn.Assert(ok)
	var target *cluster.Snode
	if len(si) != 0 {
		target = si[0]
	} else {
		mainSI, ok := s.ra.smap.Tmap[obj.mainDaemon]
		cmn.Assert(ok)
		target = mainSI
	}
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("%s sending a slice/replica #%d of %s to main %s", s.t.Snode().Name(), ct.SliceID, ct.Objname, target.Name())
	}
	return s.sendFromDisk(ct, target)
}

// Sends one or more replicas of the object to fulfill EC parity requirement.
// First, check that local target is responsible for it: it must be the first
// target by HRW that has one of replicas
func (s *ecRebalancer) bcastLocalReplica(obj *rebObject) error {
	cmn.Assert(obj.sender != nil) // mustn't happen
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("%s Object %s sender %s", s.t.Snode().Name(), obj.uid, obj.sender.Name())
	}
	// Another node should send replicas, do noting
	if obj.sender.DaemonID != s.t.Snode().DaemonID {
		return nil
	}

	// calculate how many replicas the target should send
	ctDiff := obj.requiredCT() - obj.foundCT()
	if ctDiff == 0 && !obj.mainHasAny {
		// when 'main' target does not have replica but the number
		// of replicas is OK, we have to copy replica to main anyway
		ctDiff = 1
	}
	sendTo := make([]*cluster.Snode, 0, ctDiff+1)
	ct, ok := obj.locCT[s.t.Snode().DaemonID]
	cmn.Assert(ok)
	for _, si := range obj.hrwTargets {
		if _, ok := obj.locCT[si.DaemonID]; ok {
			continue
		}
		sendTo = append(sendTo, si)
		if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
			glog.Infof("%s #4.4 - sending %s a replica of %s to %s", s.t.Snode().Name(), ct.hrwFQN, ct.Objname, si.Name())
		}
		ctDiff--
		if ctDiff == 0 {
			break
		}
	}
	cmn.Assert(len(sendTo) != 0)
	if err := s.sendFromDisk(ct, sendTo...); err != nil {
		return fmt.Errorf("Failed to send %s: %v", ct.Objname, err)
	}
	return nil
}

// Return the first target in HRW list that does not have any CT
func (s *ecRebalancer) firstEmptyTgt(obj *rebObject) *cluster.Snode {
	localDaemon := s.t.Snode().DaemonID
	for i, tgt := range obj.hrwTargets {
		if _, ok := obj.locCT[tgt.DaemonID]; ok {
			continue
		}
		// must not happen
		cmn.Assert(tgt.DaemonID != localDaemon)
		// first is main, we must reach this line only if main has something
		cmn.Assert(i != 0)
		ct, ok := obj.locCT[s.t.Snode().DaemonID]
		cmn.Assert(ok && ct.SliceID != 0)
		return tgt
	}
	return nil
}

func (s *ecRebalancer) allCTReceived() bool {
	for {
		if s.mgr.xreb.Aborted() {
			return false
		}
		uid, wObj := s.waiter.nextReadyObj()
		if wObj == nil {
			break
		}
		var obj *rebObject
		batchCurr := int(s.mgr.stages.currBatch.Load())
		for j := 0; j+batchCurr < len(s.broken) && j < ecRebBatchSize; j++ {
			o := s.broken[j+batchCurr]
			if uid == uniqueWaitID(o.bucket, o.provider, o.objName) {
				obj = o
				break
			}
		}
		cmn.Assert(obj != nil)
		// Rebuild only if there were missing slices or main object.
		// Otherwise, just mark it done and continue.
		rebuildSlices := obj.isECCopy && !obj.hasAllSlices
		rebuildObject := !obj.mainHasAny && !obj.fullObjFound
		if rebuildSlices || rebuildObject {
			if err := s.rebuildAndSend(obj, wObj.cts); err != nil {
				glog.Errorf("Failed to rebuild %s: %v", uid, err)
			}
		}
		wObj.status = objDone
		s.waiter.toRebuild.Dec()
	}

	// must be the last check, because even if a target has all slices
	// it may need to rebuild and send repaired slices
	return s.waiter.waitFor.Load() == 0 && s.waiter.toRebuild.Load() == 0
}

func (s *ecRebalancer) allNodesCompletedBatch() bool {
	cnt := 0
	batchID := s.mgr.stages.currBatch.Load()
	s.mgr.stages.mtx.Lock()
	smap := s.ra.smap.Tmap
	for _, si := range smap {
		if si.ID() == s.mgr.t.Snode().ID() {
			// local target is always in the stage
			cnt++
			continue
		}
		if s.mgr.stages.isInStageBatchUnlocked(si, rebStageECBatch, batchID) {
			cnt++
		}
	}
	s.mgr.stages.mtx.Unlock()
	return cnt == len(smap)
}

func (s *ecRebalancer) waitQuiesce(cb func() bool) error {
	maxWait := s.ra.config.Rebalance.Quiesce
	aborted, timedout := s.mgr.waitQuiesce(s.ra, maxWait, cb)
	if aborted {
		return errors.New("Aborted")
	}
	if timedout {
		return cmn.NewTimeoutError("batch completion")
	}
	return nil
}

func (s *ecRebalancer) waitForFullObject(obj *rebObject, moveLocalSlice bool) error {
	if moveLocalSlice {
		// Default target has a slice, so it must be sent to another target
		// before receiving a full object from some other target
		if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
			glog.Infof("#5.4 Sending local slice before getting full object %s", obj.uid)
		}
		tgt := s.firstEmptyTgt(obj)
		cmn.Assert(tgt != nil) // must not happen
		if err := s.sendLocalData(obj, tgt); err != nil {
			return err
		}
	}
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("#5.3/4 Waiting for an object %s", obj.uid)
	}
	s.waiter.lookupCreate(obj.uid, 0, waitForReplica)
	s.waiter.updateRebuildInfo(obj.uid)
	s.waiter.toRebuild.Inc()
	return nil
}

func (s *ecRebalancer) waitForExistingSlices(obj *rebObject) (err error) {
	for _, sl := range obj.locCT {
		// case with sliceID == 0 must be processed in the beginning
		cmn.Assert(sl.SliceID != 0)

		// wait slices only from `dataSliceCount` first HRW targets
		tgtIndex := s.targetIndex(sl.DaemonID, obj)
		if tgtIndex < 0 || tgtIndex >= int(obj.dataSlices) {
			if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
				glog.Infof("#5.5 Waiting for slice %d %s - [SKIPPED %d]", sl.SliceID, obj.uid, tgtIndex)
			}
			continue
		}

		if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
			glog.Infof("#5.5 Waiting for slice %d %s", sl.SliceID, obj.uid)
		}
		s.waiter.lookupCreate(obj.uid, sl.SliceID, waitForAllSlices)
	}
	s.waiter.updateRebuildInfo(obj.uid)
	s.waiter.toRebuild.Inc()
	return nil
}

func (s *ecRebalancer) restoreReplicas(obj *rebObject) (err error) {
	if !s.needsReplica(obj) {
		return s.bcastLocalReplica(obj)
	}
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("#4 Waiting for replica %s", obj.uid)
	}
	s.waiter.lookupCreate(obj.uid, 0, waitForSingleSlice)
	return nil
}

func (s *ecRebalancer) rebalanceObject(obj *rebObject) (err error) {
	// Case #1: this target does not have to do anything
	if s.shouldSkipObj(obj) {
		if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
			glog.Infof("SKIPPING %s", obj.uid)
		}
		return nil
	}

	// Case #2: this target has someone's main object
	if s.hasFullObjMisplaced(obj) {
		return s.sendLocalData(obj)
	}

	// Case #3: this target has a slice while the main must be restored.
	// Send local slice only if this target is in `dataSliceCount` first
	// targets which have any slice.
	hasSlice, shouldSend := s.shouldSendSlice(obj)
	if !obj.fullObjFound && hasSlice {
		if shouldSend {
			return s.sendLocalData(obj)
		}
		return nil
	}

	// Case #4: object was replicated
	if obj.isECCopy {
		return s.restoreReplicas(obj)
	}

	// Case #5: the object is erasure coded

	// Case #5.1: it is not main target and has slice or does not need any
	if !obj.isMain && (obj.hasCT || !obj.inHrwList) {
		if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
			glog.Infof("#5.1 Object %s skipped", obj.uid)
		}
		return nil
	}

	// Case #5.2: it is not main target, has no slice, needs according to HRW
	// but won't receive since there are few slices outside HRW
	if !obj.isMain && !obj.hasCT && obj.inHrwList {
		if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
			glog.Infof("#5.2 Waiting for object %s", obj.uid)
		}
		s.waiter.lookupCreate(obj.uid, anySliceID, waitForSingleSlice)
		return nil
	}

	// Case #5.3: main has nothing, but full object and all slices exists
	if obj.isMain && !obj.mainHasAny && obj.fullObjFound {
		return s.waitForFullObject(obj, false)
	}

	// Case #5.4: main has a slice instead of a full object, send local
	// slice to a free target and wait for another target sends the full obj
	if obj.isMain && obj.mainHasAny && obj.mainSliceID != 0 && obj.fullObjFound {
		return s.waitForFullObject(obj, true)
	}

	// Case #5.5: it is main target and full object is missing
	if obj.isMain && !obj.fullObjFound {
		return s.waitForExistingSlices(obj)
	}

	// The last case: must be main with object. Rebuild and send missing slices
	cmn.AssertMsg(obj.isMain && obj.mainHasAny && obj.mainSliceID == 0,
		fmt.Sprintf("%s%s/%s: isMain %t - mainHasSome %t - mainID %d",
			s.mgr.t.Snode().Name(), obj.bucket, obj.objName, obj.isMain, obj.mainHasAny, obj.mainSliceID))
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("rebuilding slices of %s and send them", obj.objName)
	}
	return s.rebuildFromDisk(obj)
}

func (s *ecRebalancer) cleanupBatch() {
	s.waiter.cleanupBatch(s.broken, int(s.mgr.stages.currBatch.Load()))
	s.releaseSGLs(s.broken)
	s.ackCTs.mtx.Lock()
	for id := range s.ackCTs.ct {
		delete(s.ackCTs.ct, id)
	}
	s.ackCTs.mtx.Unlock()
}

// Wait for all targets to finish the current batch and then free allocated resources
func (s *ecRebalancer) finalizeBatch() error {
	// First, wait for all slices the local target wants to receive
	if err := s.waitQuiesce(s.allCTReceived); err != nil {
		return err
	}
	// wait until all rebiult slices are sent
	s.waitECAck()

	// mark batch done and notify other targets
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("%s batch %d done", s.t.Snode().Name(), s.mgr.stages.currBatch.Load())
	}
	s.mgr.changeStage(rebStageECBatch, s.mgr.stages.currBatch.Load())

	// wait for all targets to finish sending/receiving
	if err := s.waitQuiesce(s.allNodesCompletedBatch); err != nil {
		if _, ok := err.(*cmn.TimeoutError); ok {
			s.waiter.waitFor.Store(0)
		}
		return err
	}

	return nil
}

// Check the list of sent CTs and resent ones that did not
// receive and acknowledge from a destination target.
// Returns the number of resent CTs
func (s *ecRebalancer) retransmit() (cnt int) {
	s.ackCTs.mtx.Lock()
	defer s.ackCTs.mtx.Unlock()
	if len(s.ackCTs.ct) == 0 {
		return 0
	}

	for id, dest := range s.ackCTs.ct {
		if s.mgr.xreb.Aborted() {
			return 0
		}
		si, ok := s.ra.smap.Tmap[dest.daemonID]
		if !ok {
			glog.Errorf("Target %s not found in smap", dest.daemonID)
			continue
		}

		_, err := ec.RequestECMeta(dest.header.Bucket, dest.header.ObjName, dest.header.Provider, si)
		if err == nil {
			// the destination got new slice/replica, but failed to send
			// ACK, cleanup ACK wait right now
			delete(s.ackCTs.ct, id)
			continue
		}

		// destination still waits for the data, resend
		glog.Errorf("Did not receive ACK from %s for %s/%s. Retransmit",
			dest.daemonID, dest.header.Bucket, dest.header.ObjName)
		// TODO: resend the slice/replica with s.data.SendV

		cnt++
		// update stats with retransmitted data
		delete(s.ackCTs.ct, id)
		s.statsT.AddMany(
			stats.NamedVal64{Name: stats.TxRebCount, Value: 1},
			stats.NamedVal64{Name: stats.TxRebSize, Value: dest.header.ObjAttrs.Size},
		)
	}

	return cnt
}

func (s *ecRebalancer) allAckReceived() bool {
	if s.mgr.xreb.Aborted() {
		return false
	}
	s.ackCTs.mtx.Lock()
	cnt := len(s.ackCTs.ct)
	s.ackCTs.mtx.Unlock()
	return cnt == 0
}

func (s *ecRebalancer) waitECAck() {
	globRebID := s.mgr.globRebID.Load()
	loghdr := s.mgr.loghdr(globRebID, s.ra.smap)
	sleep := s.ra.config.Timeout.CplaneOperation // NOTE: TODO: used throughout; must be separately assigned and calibrated

	// loop without timeout - wait until all CTs put into transport
	// queue are processed (either sent or failed)
	for s.onAir.Load() > 0 {
		if s.mgr.xreb.AbortedAfter(sleep) {
			s.onAir.Store(0)
			glog.Infof("%s: abrt", loghdr)
			return
		}
	}

	// ignore erros and continue
	s.waitQuiesce(s.allAckReceived)
	if s.mgr.xreb.Aborted() {
		return
	}
	maxwt := s.ra.config.Rebalance.DestRetryTime
	maxwt += time.Duration(int64(time.Minute) * int64(s.ra.smap.CountTargets()/10))
	maxwt = cmn.MinDur(maxwt, s.ra.config.Rebalance.DestRetryTime*2)
	curwt := time.Duration(0)
	for curwt < maxwt {
		cnt := s.retransmit()
		if cnt == 0 || s.mgr.xreb.Aborted() {
			return
		}
		if s.mgr.xreb.AbortedAfter(sleep) {
			glog.Infof("%s: abrt", loghdr)
			return
		}
		curwt += sleep
		glog.Warningf("%s: retransmitted %d, more wack...", loghdr, cnt)
	}
}

// Rebalances the current batch of broken objects
func (s *ecRebalancer) rebalanceBatch(batchCurr int64) error {
	batchEnd := cmn.Min(int(batchCurr)+ecRebBatchSize, len(s.broken))
	for objIdx := int(batchCurr); objIdx < batchEnd; objIdx++ {
		if s.mgr.xreb.Aborted() {
			return fmt.Errorf("Aborted")
		}

		obj := s.broken[objIdx]
		if bool(glog.FastV(4, glog.SmoduleReb)) {
			glog.Infof("--- Starting object [%d] %s ---", objIdx, obj.uid)
		}
		cmn.Assert(len(obj.locCT) != 0) // cannot happen

		if err := s.rebalanceObject(obj); err != nil {
			return err
		}
	}
	return nil
}

// Does cluster-wide rebalance
func (s *ecRebalancer) rebalanceGlobal() (err error) {
	batchCurr := int64(0)
	batchLast := int64(len(s.broken) - 1)
	s.mgr.stages.currBatch.Store(batchCurr)
	s.mgr.stages.lastBatch.Store(batchLast)
	for batchCurr <= batchLast {
		if bool(glog.FastV(4, glog.SmoduleReb)) {
			glog.Infof("Starting batch of %d from %d", ecRebBatchSize, batchCurr)
		}

		if err = s.rebalanceBatch(batchCurr); err != nil {
			s.cleanupBatch()
			return err
		}

		s.waitECAck()
		s.mgr.stages.stage.Store(rebStageECBatch)
		err = s.finalizeBatch()
		s.cleanupBatch()
		if err != nil {
			return err
		}
		batchCurr = s.mgr.stages.currBatch.Add(int64(ecRebBatchSize))
	}
	s.mgr.changeStage(rebStageECCleanup, 0)
	return nil
}

// Free allocated memory for EC reconstruction, close opened file handles of replicas.
// Used to clean up memory after finishing a batch
func (s *ecRebalancer) releaseSGLs(objList []*rebObject) {
	batchCurr := int(s.mgr.stages.currBatch.Load())
	for i := batchCurr; i < batchCurr+ecRebBatchSize && i < len(objList); i++ {
		obj := objList[i]
		for _, sg := range obj.rebuildSGLs {
			if sg != nil {
				sg.Free()
			}
		}
		obj.rebuildSGLs = nil
		if obj.fh != nil {
			obj.fh.Close()
			obj.fh = nil
		}
	}
}

// Local target has the full object on local drive and it is "default" target.
// Just rebuild slices and send missing ones to other targets
// TODO: rebuildFromDisk, rebuildFromMem, and rebuildFromSlices shares some
// code. See what can be done to deduplicate it. Some code may go to EC package
func (s *ecRebalancer) rebuildFromDisk(obj *rebObject) (err error) {
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("%s rebuilding slices of %s and send them", s.t.Snode().Name(), obj.objName)
	}
	slice, ok := obj.locCT[s.t.Snode().DaemonID]
	cmn.Assert(ok && slice.SliceID == 0)
	padSize := obj.sliceSize*int64(obj.dataSlices) - obj.objSize
	obj.fh, err = cmn.NewFileHandle(slice.hrwFQN)
	if err != nil {
		return fmt.Errorf("Failed to open local object from %q: %v", slice.hrwFQN, err)
	}
	readers := make([]io.Reader, obj.dataSlices)
	readerSend := make([]cmn.ReadOpenCloser, obj.dataSlices)
	obj.rebuildSGLs = make([]*memsys.SGL, obj.paritySlices)
	writers := make([]io.Writer, obj.paritySlices)
	sizeLeft := obj.objSize
	for i := 0; int16(i) < obj.dataSlices; i++ {
		var reader cmn.ReadOpenCloser
		if sizeLeft < obj.sliceSize {
			reader, err = cmn.NewFileSectionHandle(obj.fh, int64(i)*obj.sliceSize, sizeLeft, padSize)
		} else {
			reader, err = cmn.NewFileSectionHandle(obj.fh, int64(i)*obj.sliceSize, obj.sliceSize, 0)
		}
		if err != nil {
			return fmt.Errorf("Failed to create file section reader for %q: %v", obj.objName, err)
		}
		readers[i] = reader
		readerSend[i] = reader
		sizeLeft -= obj.sliceSize
	}
	for i := 0; int16(i) < obj.paritySlices; i++ {
		obj.rebuildSGLs[i] = s.t.GetMem2().NewSGL(cmn.MinI64(obj.sliceSize, cmn.MiB))
		writers[i] = obj.rebuildSGLs[i]
	}

	stream, err := reedsolomon.NewStreamC(int(obj.dataSlices), int(obj.paritySlices), true, true)
	if err != nil {
		return fmt.Errorf("Failed to create initialize EC for %q: %v", obj.objName, err)
	}
	if err := stream.Encode(readers, writers); err != nil {
		return fmt.Errorf("Failed to build EC for %q: %v", obj.objName, err)
	}

	// Detect missing slices.
	// The main object that has metadata.
	ecSliceMD := obj.ctWithMD()
	cmn.Assert(ecSliceMD != nil)
	freeTargets := obj.emptyTargets(s.t.Snode())
	for idx, exists := range obj.ctExist {
		if exists {
			continue
		}
		cmn.Assert(idx != 0)              // because the full object must exist on local drive
		cmn.Assert(len(freeTargets) != 0) // 0 - means we have issue in broken objects detection
		si := freeTargets[0]
		freeTargets = freeTargets[1:]
		if idx <= int(obj.dataSlices) {
			if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
				glog.Infof("Sending %s data slice %d to %s", obj.objName, idx, si.Name())
			}
			reader := readerSend[idx-1]
			reader.Open()
			ckval, err := cksumForSlice(reader, obj.sliceSize, s.t.GetMem2())
			if err != nil {
				return fmt.Errorf("Failed to calculate checksum of %s: %v", obj.objName, err)
			}
			reader.Open()
			if err := s.sendFromReader(reader, ecSliceMD, idx, ckval, si); err != nil {
				glog.Errorf("Failed to send data slice %d[%s] to %s", idx, obj.objName, si.Name())
				// continue to fix as many as possible
				continue
			}
		} else {
			sglIdx := idx - int(obj.dataSlices) - 1
			if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
				glog.Infof("Sending %s parity slice %d[%d] to %s", obj.objName, idx, sglIdx, si.Name())
			}
			ckval, err := cksumForSlice(memsys.NewReader(obj.rebuildSGLs[sglIdx]), obj.sliceSize, s.t.GetMem2())
			if err != nil {
				return fmt.Errorf("Failed to calculate checksum of %s: %v", obj.objName, err)
			}
			reader := memsys.NewReader(obj.rebuildSGLs[sglIdx])
			if err := s.sendFromReader(reader, ecSliceMD, idx, ckval, si); err != nil {
				glog.Errorf("Failed to send parity slice %d[%s] to %s", idx, obj.objName, si.Name())
				// continue to fix as many as possible
				continue
			}
		}
	}
	return nil
}

// when local target is a default one, and the full object is missing, the target
// receives existing slices with metadata, then it rebuild the object and missing
// slices. Finally it sends rebuilt slices to other targets, for this it needs
// correct metadata. The function generates metadata for a new slice for first
// received slice
func (s *ecRebalancer) metadataForSlice(slices []*waitCT, sliceID int) *ec.Metadata {
	for _, sl := range slices {
		if !sl.recv.Load() {
			continue
		}
		var md ec.Metadata
		if err := jsoniter.Unmarshal(sl.meta, &md); err != nil {
			glog.Errorf("Invalid metadata: %v", err)
			continue
		}
		md.SliceID = sliceID
		return &md
	}
	return nil
}

// The object is misplaced and few slices are missing. The default target
// receives the object into SGL, rebuilds missing slices, and sends them
func (s *ecRebalancer) rebuildFromMem(obj *rebObject, slices []*waitCT) (err error) {
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("%s rebuilding slices of %s and send them", s.t.Snode().Name(), obj.objName)
	}
	cmn.Assert(len(slices) != 0)
	slice := slices[0]
	cmn.Assert(slice != nil && slice.sliceID == 0) // received slice must be object

	padSize := obj.sliceSize*int64(obj.dataSlices) - obj.objSize
	if padSize > 0 {
		cmn.Assert(padSize < 256)
		padding := ecPadding[:padSize]
		if _, err = slice.sgl.Write(padding); err != nil {
			return err
		}
	}

	readers := make([]io.Reader, obj.dataSlices)
	readerSGLs := make([]*memsys.SliceReader, obj.dataSlices)
	obj.rebuildSGLs = make([]*memsys.SGL, obj.paritySlices)
	writers := make([]io.Writer, obj.paritySlices)
	for i := 0; int16(i) < obj.dataSlices; i++ {
		readerSGLs[i] = memsys.NewSliceReader(slice.sgl, int64(i)*obj.sliceSize, obj.sliceSize)
		readers[i] = readerSGLs[i]
	}
	for i := 0; int16(i) < obj.paritySlices; i++ {
		obj.rebuildSGLs[i] = s.t.GetMem2().NewSGL(cmn.MinI64(obj.sliceSize, cmn.MiB))
		writers[i] = obj.rebuildSGLs[i]
	}

	stream, err := reedsolomon.NewStreamC(int(obj.dataSlices), int(obj.paritySlices), true, true)
	if err != nil {
		return fmt.Errorf("Failed to create initialize EC for %q: %v", obj.objName, err)
	}
	if err := stream.Encode(readers, writers); err != nil {
		return fmt.Errorf("Failed to build EC for %q: %v", obj.objName, err)
	}

	// detect missing slices
	// The main object that has metadata.
	ecSliceMD := obj.ctWithMD()
	cmn.Assert(ecSliceMD != nil)
	freeTargets := obj.emptyTargets(s.t.Snode())
	for idx, exists := range obj.ctExist {
		if exists {
			continue
		}
		cmn.Assert(idx != 0) // full object must exists
		cmn.Assert(len(freeTargets) != 0)
		si := freeTargets[0]
		freeTargets = freeTargets[1:]
		if idx <= int(obj.dataSlices) {
			if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
				glog.Infof("Sending %s data slice %d to %s", obj.objName, idx, si.Name())
			}
			reader := readerSGLs[idx-1]
			ckval, err := cksumForSlice(readerSGLs[idx-1], obj.sliceSize, s.t.GetMem2())
			if err != nil {
				return fmt.Errorf("Failed to calculate checksum of %s: %v", obj.objName, err)
			}
			reader.Open()
			if err := s.sendFromReader(reader, ecSliceMD, idx, ckval, si); err != nil {
				glog.Errorf("Failed to send data slice %d[%s] to %s", idx, obj.objName, si.Name())
				// keep on working to restore as many as possible
				continue
			}
		} else {
			sglIdx := idx - int(obj.dataSlices) - 1
			if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
				glog.Infof("Sending %s parity slice %d[%d] to %s", obj.objName, idx, sglIdx, si.Name())
			}
			ckval, err := cksumForSlice(memsys.NewReader(obj.rebuildSGLs[sglIdx]), obj.sliceSize, s.t.GetMem2())
			if err != nil {
				return fmt.Errorf("Failed to calculate checksum of %s: %v", obj.objName, err)
			}
			reader := memsys.NewReader(obj.rebuildSGLs[sglIdx])
			if err := s.sendFromReader(reader, ecSliceMD, idx, ckval, si); err != nil {
				glog.Errorf("Failed to send parity slice %d[%s] to %s", idx, obj.objName, si.Name())
				// keep on working to restore as many as possible
				continue
			}
		}
	}
	return nil
}

// Object is missing(and maybe a few slices as well). Default target receives all
// existing slices into SGLs, restores the object, rebuilds slices, and finally
// send missing slices to other targets
func (s *ecRebalancer) rebuildFromSlices(obj *rebObject, slices []*waitCT) (err error) {
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("%s rebuilding slices of %s and send them(mem)", s.t.Snode().Name(), obj.objName)
	}

	sliceCnt := obj.dataSlices + obj.paritySlices
	obj.rebuildSGLs = make([]*memsys.SGL, sliceCnt)
	readers := make([]io.Reader, sliceCnt)
	// since io.Reader cannot be reopened, we need to have a copy for saving object
	rereaders := make([]io.Reader, sliceCnt)
	writers := make([]io.Writer, sliceCnt)

	// put existing slices to readers list, and create SGL as writers for missing ones
	slicesFound := int16(0)
	var (
		meta  []byte
		cksum *cmn.Cksum
	)
	for _, sl := range slices {
		if !sl.recv.Load() {
			continue
		}
		id := sl.sliceID - 1
		cmn.Assert(readers[id] == nil)
		readers[id] = memsys.NewReader(sl.sgl)
		rereaders[id] = memsys.NewReader(sl.sgl)
		slicesFound++
		if meta == nil {
			meta = sl.meta
		}
	}
	cmn.Assert(meta != nil)

	var ecMD ec.Metadata
	err = jsoniter.Unmarshal(meta, &ecMD)
	cmn.Assert(err == nil)

	for i, rd := range readers {
		if rd != nil {
			continue
		}
		obj.rebuildSGLs[i] = s.t.GetMem2().NewSGL(cmn.MinI64(obj.sliceSize, cmn.MiB))
		writers[i] = obj.rebuildSGLs[i]
	}

	stream, err := reedsolomon.NewStreamC(int(obj.dataSlices), int(obj.paritySlices), true, true)
	if err != nil {
		return fmt.Errorf("Failed to create initialize EC for %q: %v", obj.objName, err)
	}
	if err := stream.Reconstruct(readers, writers); err != nil {
		return fmt.Errorf("Failed to build EC for %q: %v", obj.objName, err)
	}

	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("Saving restored full object %s[%d]", obj.objName, obj.objSize)
	}
	// Save the object and its metadata first
	srcCnt := int(obj.dataSlices)
	srcReaders := make([]io.Reader, srcCnt)
	for i := 0; i < srcCnt; i++ {
		if readers[i] != nil {
			srcReaders[i] = rereaders[i]
			continue
		}
		cmn.Assert(obj.rebuildSGLs[i] != nil)
		srcReaders[i] = memsys.NewReader(obj.rebuildSGLs[i])
	}
	src := io.MultiReader(srcReaders...)
	objMD := ecMD // copy
	objMD.SliceID = 0

	lom := &cluster.LOM{T: s.t, Objname: obj.objName}
	err = lom.Init(obj.bucket, obj.provider)
	if err != nil {
		return err
	}
	lom.Uncache()
	if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
		glog.Infof("Saving restored full object %s to %q", obj.objName, lom.FQN)
	}
	tmpFQN := fs.CSM.GenContentFQN(lom.FQN, fs.WorkfileType, "ec")
	buffer, slab := s.t.GetMem2().AllocDefault()
	if cksum, err = cmn.SaveReaderSafe(tmpFQN, lom.FQN, src, buffer, true, obj.objSize); err != nil {
		glog.Error(err)
		slab.Free(buffer)
		s.t.FSHC(err, lom.FQN)
		return err
	}

	lom.SetSize(obj.objSize)
	lom.SetCksum(cksum)
	metaFQN := lom.ParsedFQN.MpathInfo.MakePathBucketObject(ec.MetaType, obj.bucket, obj.provider, obj.objName)
	metaBuf := cmn.MustMarshal(&objMD)
	if _, err := cmn.SaveReader(metaFQN, bytes.NewReader(metaBuf), buffer, false); err != nil {
		glog.Error(err)
		slab.Free(buffer)
		if rmErr := os.Remove(lom.FQN); rmErr != nil {
			glog.Errorf("Nested error while cleaning up: %v", rmErr)
		}
		s.t.FSHC(err, metaFQN)
		return err
	}
	slab.Free(buffer)
	if err := lom.Persist(); err != nil {
		if rmErr := os.Remove(metaFQN); rmErr != nil && !os.IsNotExist(rmErr) {
			glog.Errorf("Nested error: save LOM -> remove metadata file: %v", rmErr)
		}
		if rmErr := os.Remove(lom.FQN); rmErr != nil && !os.IsNotExist(rmErr) {
			glog.Errorf("Nested error: save LOM -> remove replica: %v", rmErr)
		}
		return err
	}

	freeTargets := obj.emptyTargets(s.t.Snode())
	for i, wr := range writers {
		if wr == nil {
			continue
		}
		sliceID := i + 1
		if exists := obj.ctExist[sliceID]; exists {
			if bool(glog.FastV(4, glog.SmoduleReb)) || s.ra.dryRun {
				glog.Infof("Object %s slice %d: already exists", obj.uid, sliceID)
			}
			continue
		}
		if len(freeTargets) == 0 {
			return fmt.Errorf("Failed to send slice %d of %s - no free target", sliceID, obj.uid)
		}
		ckval, err := cksumForSlice(memsys.NewReader(obj.rebuildSGLs[i]), obj.sliceSize, s.t.GetMem2())
		if err != nil {
			return fmt.Errorf("Failed to calculate checksum of %s: %v", obj.objName, err)
		}
		reader := memsys.NewReader(obj.rebuildSGLs[i])
		si := freeTargets[0]
		freeTargets = freeTargets[1:]
		md := s.metadataForSlice(slices, sliceID)
		cmn.Assert(md != nil)

		sliceMD := ecMD // copy
		sliceMD.SliceID = sliceID
		sl := &rebCT{
			Bucket:       obj.bucket,
			Objname:      obj.objName,
			ObjSize:      sliceMD.Size,
			DaemonID:     s.t.Snode().DaemonID,
			SliceID:      int16(sliceID),
			Provider:     obj.provider,
			DataSlices:   int16(ecMD.Data),
			ParitySlices: int16(ecMD.Parity),
			meta:         &sliceMD,
		}

		if err := s.sendFromReader(reader, sl, i+1, ckval, si); err != nil {
			return fmt.Errorf("Failed to send slice %d of %s to %s: %v", i, obj.uid, si.Name(), err)
		}
	}

	return nil
}

// Default target does not have object(but it can be on another target) and
// few slices may be missing. The function detects whether it needs to reconstruct
// the object and then rebuild and send missing slices
func (s *ecRebalancer) rebuildAndSend(obj *rebObject, slices []*waitCT) error {
	// look through received slices if one of them is the object's replica
	var replica *waitCT
	recv := 0
	for _, s := range slices {
		if s.sliceID == 0 {
			replica = s
		}
		if s.recv.Load() {
			recv++
		}
	}
	cmn.Assert(recv != 0) // sanity check

	if replica != nil {
		return s.rebuildFromMem(obj, slices)
	}

	return s.rebuildFromSlices(obj, slices)
}

// Returns XXHash calculated for the reader
func cksumForSlice(reader cmn.ReadOpenCloser, sliceSize int64, mem *memsys.Mem2) (string, error) {
	reader.Open()
	buf, slab := mem.AllocForSize(sliceSize)
	defer slab.Free(buf)
	return cmn.ComputeXXHash(reader, buf)
}

//
// ctWaiter
//

func newWaiter(mem *memsys.Mem2) *ctWaiter {
	return &ctWaiter{
		objs: make(ctWaitList),
		mem:  mem,
	}
}

// object is processed, cleanup allocated memory
func (wt *ctWaiter) removeObj(uid string) {
	wt.mx.Lock()
	wt.removeObjUnlocked(uid)
	wt.mx.Unlock()
}

func (wt *ctWaiter) removeObjUnlocked(uid string) {
	wo, ok := wt.objs[uid]
	if ok {
		for _, slice := range wo.cts {
			if slice.sgl != nil {
				slice.sgl.Free()
			}
		}
		delete(wt.objs, uid)
	}
}

// final cleanup after rebalance is done
func (wt *ctWaiter) cleanup() {
	for uid := range wt.objs {
		wt.removeObj(uid)
	}
	wt.waitFor.Store(0)
	wt.toRebuild.Store(0)
}

// Range freeing: if idx is not defined, cleanup all waiting objects,
// otherwise cleanup only objects which names matches objects in range idx0..idx1
func (wt *ctWaiter) cleanupBatch(broken []*rebObject, idx ...int) {
	wt.mx.Lock()
	if len(idx) == 0 {
		for uid := range wt.objs {
			wt.removeObjUnlocked(uid)
		}
	} else {
		start := idx[0]
		for objIdx := start; objIdx < start+ecRebBatchSize; objIdx++ {
			if objIdx >= len(broken) {
				break
			}
			wt.removeObjUnlocked(broken[objIdx].uid)
		}
	}
	wt.mx.Unlock()
}

// Looks through the list of slices to wait and returns the one
// with given uid. If nothing found, it creates a new wait object and
// returns it. This case is possible when another target is faster than this
// one and starts sending slices before this target builds its list
func (wt *ctWaiter) lookupCreate(uid string, sliceID int16, waitType int) *waitCT {
	wt.mx.Lock()
	defer wt.mx.Unlock()

	wObj, ok := wt.objs[uid]
	if !ok {
		// first slice of the object, initialize everything
		slice := &waitCT{
			sliceID: sliceID,
			sgl:     wt.mem.NewSGL(32 * cmn.KiB),
		}
		wt.objs[uid] = &waitObject{wt: waitType, cts: []*waitCT{slice}}
		wt.waitFor.Inc()
		return slice
	}

	// in case of other target sent a slice before this one had initialized
	// wait structure, replace current waitType if it is not a generic one
	if waitType != waitForSingleSlice {
		wObj.wt = waitType
	}

	// check if the slice is already initialized and return it
	for _, ct := range wObj.cts {
		if ct.sliceID == anySliceID || ct.sliceID == sliceID || sliceID == anySliceID {
			return ct
		}
	}

	// slice is not in wait list yet, add it
	ct := &waitCT{
		sliceID: sliceID,
		sgl:     wt.mem.NewSGL(32 * cmn.KiB),
	}
	wt.objs[uid].cts = append(wt.objs[uid].cts, ct)
	wt.waitFor.Inc()
	return ct
}

// Updates object readiness to be rebuild (i.e., the target has received all
// required slices/replicas).
// Returns `true` if a target waits for slices only to rebuild the object,
// so the received slices should not be saved to local drives.
func (wt *ctWaiter) updateRebuildInfo(uid string) bool {
	wt.mx.Lock()
	defer wt.mx.Unlock()

	wObj, ok := wt.objs[uid]
	cmn.Assert(ok)
	if wObj.wt == waitForSingleSlice || wObj.status != objWaiting {
		// object should not be rebuilt, or it is already done: nothing to do
		return wObj.wt != waitForSingleSlice
	}

	if wObj.wt == waitForReplica {
		// For replica case, a target needs only 1 replica to start rebuilding
		for _, ct := range wObj.cts {
			if ct.sliceID == 0 && ct.recv.Load() {
				wObj.status = objReceived
				break
			}
		}
	} else {
		// For EC case, a target needs all slices to start rebuilding
		done := true
		for _, ct := range wObj.cts {
			if !ct.recv.Load() {
				done = false
				break
			}
		}
		if done {
			wObj.status = objReceived
		}
	}
	return wObj.wt != waitForSingleSlice
}

// Returns UID and data for the next object that has all slices/replicas
// received and can be rebuild.
// The number of object in `wt.objs` map is less than the number of object
// in a batch (ecRebBatchSize). So, linear algorihtm is fast enough.
func (wt *ctWaiter) nextReadyObj() (uid string, wObj *waitObject) {
	wt.mx.Lock()
	defer wt.mx.Unlock()

	for uid, obj := range wt.objs {
		if obj.status == objReceived && obj.wt != waitForSingleSlice {
			return uid, obj
		}
	}

	return "", nil
}
