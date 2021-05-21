// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"context"
	"encoding"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/ec"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/xaction/xreg"
)

//
// PUT, GET, APPEND, and COPY object
//

type (
	putObjInfo struct {
		started time.Time // started time of receiving - used to calculate the recv duration
		t       *targetrunner
		lom     *cluster.LOM
		// Reader with the content of the object.
		r io.ReadCloser
		// If available (not `none`), can be validated and will be stored with the object
		// see `writeToFile` method
		cksumToUse *cos.Cksum
		// object size aka Content-Length
		size int64
		// Context used when putting the object which should be contained in
		// cloud bucket. It usually contains credentials to access the cloud.
		ctx context.Context
		// FQN which is used only temporarily for receiving file. After
		// successful receive is renamed to actual FQN.
		workFQN string
		// Determines the receive type of the request.
		recvType cluster.RecvType
		// if true, poi won't erasure-encode an object when finalizing
		skipEC bool
	}

	getObjInfo struct {
		started time.Time // started time of receiving - used to calculate the recv duration
		t       *targetrunner
		lom     *cluster.LOM
		// Writer where the object will be written.
		w io.Writer
		// Context used when receiving the object which is contained in cloud
		// bucket. It usually contains credentials to access the cloud.
		ctx context.Context
		// Contains object range query
		ranges cmn.RangesQuery
		// Determines if it is GFN request
		isGFN bool
		// true: chunked transfer (en)coding as per https://tools.ietf.org/html/rfc7230#page-36
		chunked bool
	}

	// Contains information packed in append handle.
	handleInfo struct {
		nodeID       string
		filePath     string
		partialCksum *cos.CksumHash
	}

	appendObjInfo struct {
		started time.Time // started time of receiving - used to calculate the recv duration
		t       *targetrunner
		lom     *cluster.LOM

		// Reader with the content of the object.
		r io.ReadCloser
		// Object size aka Content-Length.
		size int64
		// Append/Flush operation.
		op string
		hi handleInfo // Information contained in handle.

		cksum *cos.Cksum // Expected checksum of the final object.
	}

	copyObjInfo struct {
		cluster.CopyObjectParams
		t           *targetrunner
		localOnly   bool // copy locally with no HRW=>target
		finalize    bool // copies and EC (as in poi.finalize())
		promoteFile bool // Determines if we are promoting a file.
	}
)

////////////////
// PUT OBJECT //
////////////////

func (poi *putObjInfo) putObject() (errCode int, err error) {
	debug.Assert(cluster.RegularPut <= poi.recvType && poi.recvType <= cluster.Migrated)

	lom := poi.lom
	// optimize out if the checksums do match
	if !poi.cksumToUse.IsEmpty() {
		if lom.Cksum().Equal(poi.cksumToUse) {
			if glog.FastV(4, glog.SmoduleAIS) {
				glog.Infof("%s is valid %s: PUT is a no-op", lom, poi.cksumToUse)
			}
			cos.DrainReader(poi.r)
			return 0, nil
		}
	}

	if !daemon.dryRun.disk {
		if err := poi.writeToFile(); err != nil {
			return http.StatusInternalServerError, err
		}
		if errCode, err := poi.finalize(); err != nil {
			return errCode, err
		}
	}
	if poi.recvType == cluster.RegularPut {
		delta := time.Since(poi.started)
		poi.t.statsT.AddMany(
			stats.NamedVal64{Name: stats.PutCount, Value: 1},
			stats.NamedVal64{Name: stats.PutLatency, Value: int64(delta)},
		)
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Infof("PUT %s: %s", lom, delta)
		}
	}
	return 0, nil
}

func (poi *putObjInfo) finalize() (errCode int, err error) {
	if errCode, err = poi.tryFinalize(); err != nil {
		if err1 := fs.Access(poi.workFQN); err1 == nil || !os.IsNotExist(err1) {
			if err1 == nil {
				err1 = err
			}
			poi.t.fsErr(err1, poi.workFQN)
			if err2 := cos.RemoveFile(poi.workFQN); err2 != nil {
				glog.Errorf("Nested error: %s => (remove %s => err: %v)", err1, poi.workFQN, err2)
			}
		}
		poi.lom.Uncache(true /*delDirty*/)
		return
	}
	if !poi.skipEC {
		if ecErr := ec.ECM.EncodeObject(poi.lom); ecErr != nil && ecErr != ec.ErrorECDisabled {
			err = ecErr
			return
		}
	}

	poi.t.putMirror(poi.lom)
	return
}

// poi.workFQN => LOM
func (poi *putObjInfo) tryFinalize() (errCode int, err error) {
	var (
		lom = poi.lom
		bck = lom.Bck()
		bmd = poi.t.owner.bmd.Get()
	)
	// remote versioning
	if bck.IsRemote() && poi.recvType == cluster.RegularPut {
		var version string
		version, errCode, err = poi.putRemote()
		if err != nil {
			glog.Errorf("PUT %s: %v", lom, err)
			return
		}
		if lom.VersionConf().Enabled {
			lom.SetVersion(version)
		}
	}
	if _, present := bmd.Get(bck); !present {
		err = fmt.Errorf("PUT %s: bucket %s does not exist", lom, bck)
		errCode = http.StatusBadRequest
		return
	}
	if poi.recvType == cluster.ColdGet {
		debug.Assert(!lom.TryLock(true)) // cold GET: caller must take a lock
	} else {
		lom.Lock(true)
		defer lom.Unlock(true)
	}
	// ais versioning
	if bck.IsAIS() && lom.VersionConf().Enabled {
		// NOTE: the caller is expected to load it and get the current version, if exists
		if poi.recvType == cluster.RegularPut || lom.Version(true) == "" {
			if err = lom.IncVersion(); err != nil {
				return
			}
		}
	}
	if err = cos.Rename(poi.workFQN, lom.FQN); err != nil {
		err = fmt.Errorf(cmn.FmtErrFailed, poi.t.si, "rename", lom, err)
		return
	}
	if lom.HasCopies() {
		// TODO: recover
		if errdc := lom.DelAllCopies(); errdc != nil {
			glog.Errorf("PUT %s: failed to delete old copies [%v], proceeding to PUT anyway...", lom, errdc)
		}
	}
	err = lom.Persist(true)
	if err != nil {
		lom.Uncache(true /*delDirty*/)
	}
	return
}

func (poi *putObjInfo) putRemote() (version string, errCode int, err error) {
	var (
		lom     = poi.lom
		backend = poi.t.Backend(lom.Bck())
	)
	fh, err := cos.NewFileHandle(poi.workFQN)
	if err != nil {
		err = fmt.Errorf(cmn.FmtErrFailed, poi.t.Snode(), "open", poi.workFQN, err)
		return
	}

	version, errCode, err = backend.PutObj(poi.ctx, fh, lom)
	if !lom.Bck().IsRemoteAIS() {
		customMD := cos.SimpleKVs{cluster.SourceObjMD: backend.Provider()}
		if version != "" {
			customMD[cluster.VersionObjMD] = version
		}
		lom.SetCustomMD(customMD)
	}
	return
}

// NOTE: LOM is updated on the end of the call with proper size and checksum.
// NOTE: `roi.r` is closed on the end of the call.
func (poi *putObjInfo) writeToFile() (err error) {
	var (
		written int64
		file    *os.File
		buf     []byte
		slab    *memsys.Slab
		reader  = poi.r
		writer  io.Writer
		writers = make([]io.Writer, 0, 4)
		cksums  = struct {
			store *cos.CksumHash // store with LOM
			given *cos.CksumHash // compute additionally
			expct *cos.Cksum     // and validate against `expct` if required/available
		}{}
		conf = poi.lom.CksumConf()
	)
	if daemon.dryRun.disk {
		return
	}
	if file, err = poi.lom.CreateFile(poi.workFQN); err != nil {
		return
	}
	writer = cos.WriterOnly{Writer: file} // Hiding `ReadFrom` for `*os.File` introduced in Go1.15.
	if poi.size == 0 {
		buf, slab = poi.t.gmm.Alloc()
	} else {
		buf, slab = poi.t.gmm.Alloc(poi.size)
	}
	// cleanup
	defer func() { // free & cleanup on err
		slab.Free(buf)
		if err == nil {
			cos.Close(reader)
			return
		}
		reader.Close()
		if file != nil {
			if nestedErr := file.Close(); nestedErr != nil {
				glog.Errorf("Nested (%v): failed to close received object %s, err: %v",
					err, poi.workFQN, nestedErr)
			}
		}
		if nestedErr := cos.RemoveFile(poi.workFQN); nestedErr != nil {
			glog.Errorf("Nested (%v): failed to remove %s, err: %v", err, poi.workFQN, nestedErr)
		}
	}()
	// checksums
	if conf.Type == cos.ChecksumNone {
		poi.lom.SetCksum(cos.NoneCksum)
		goto write
	}
	if poi.recvType == cluster.Migrated && !conf.ShouldValidate() && !poi.cksumToUse.IsEmpty() {
		// if migration validation is not configured we can just take
		// the checksum that has arrived with the object (and compute it if not present)
		poi.lom.SetCksum(poi.cksumToUse)
		goto write
	}

	// compute checksum and save it as part of the object metadata
	cksums.store = cos.NewCksumHash(conf.Type)
	writers = append(writers, cksums.store.H)
	if conf.ShouldValidate() && !poi.cksumToUse.IsEmpty() {
		// if validate-cold-get and the cksum is provided we should also check md5 hash (aws, gcp)
		// or if the object is migrated, and `conf.ValidateObjMove` we should check with existing checksum
		cksums.expct = poi.cksumToUse
		cksums.given = cos.NewCksumHash(poi.cksumToUse.Type())
		writers = append(writers, cksums.given.H)
	}

write:
	if len(writers) == 0 {
		written, err = io.CopyBuffer(writer, reader, buf)
	} else {
		writers = append(writers, writer)
		written, err = io.CopyBuffer(cos.NewWriterMulti(writers...), reader, buf)
	}
	if err != nil {
		return
	}
	// validate
	if cksums.given != nil {
		cksums.given.Finalize()
		if !cksums.given.Equal(cksums.expct) {
			err = cos.NewBadDataCksumError(cksums.expct, &cksums.given.Cksum, poi.lom.String())
			poi.t.statsT.AddMany(
				stats.NamedVal64{Name: stats.ErrCksumCount, Value: 1},
				stats.NamedVal64{Name: stats.ErrCksumSize, Value: written},
			)
			return
		}
	}
	if err = cos.FlushClose(file); err != nil {
		err = fmt.Errorf(cmn.FmtErrFailed, poi.t.si, "close poi temp", poi.workFQN, err)
		file = nil // see defer
		return
	}
	// ok
	poi.lom.SetSize(written)
	if cksums.store != nil {
		cksums.store.Finalize()
		poi.lom.SetCksum(&cksums.store.Cksum)
	}
	return
}

////////////////
// GET OBJECT //
////////////////

func (goi *getObjInfo) getObject() (sent bool, errCode int, err error) {
	var (
		cs                                            fs.CapStatus
		doubleCheck, retry, retried, coldGet, capRead bool
	)
	// under lock: lom init, restore from cluster
	goi.lom.Lock(false)
do:
	// all subsequent checks work with disks - skip all if dryRun.disk=true
	if daemon.dryRun.disk {
		goto get
	}
	err = goi.lom.Load(true /*cache it*/, true /*locked*/)
	if err != nil {
		coldGet = cmn.IsObjNotExist(err)
		if !coldGet {
			goi.lom.Unlock(false)
			return sent, http.StatusInternalServerError, err
		}
		capRead = true // set flag to avoid calling later
		cs = fs.GetCapStatus()
		if cs.OOS {
			// Immediate return for no space left to restore object.
			goi.lom.Unlock(false)
			return sent, http.StatusInsufficientStorage, cs.Err
		}
	}

	if coldGet && goi.lom.Bck().IsAIS() {
		// try lookup and restore
		goi.lom.Unlock(false)
		doubleCheck, errCode, err = goi.tryRestoreObject()
		if doubleCheck && err != nil {
			lom2 := &cluster.LOM{ObjName: goi.lom.ObjName}
			er2 := lom2.Init(goi.lom.Bucket())
			if er2 == nil {
				er2 = lom2.Load(true /*cache it*/, false /*locked*/)
				if er2 == nil {
					goi.lom = lom2
					err = nil
				}
			}
		}
		if err != nil {
			return
		}
		goi.lom.Lock(false)
		goto get
	}
	// exists && remote|cloud: check ver if requested
	if !coldGet && goi.lom.Bck().IsRemote() {
		if goi.lom.Version() != "" && goi.lom.VersionConf().ValidateWarmGet {
			goi.lom.Unlock(false)
			if coldGet, errCode, err = goi.t.CheckRemoteVersion(goi.ctx, goi.lom); err != nil {
				goi.lom.Uncache(true /*delDirty*/)
				return
			}
			goi.lom.Lock(false)
		}
	}

	// checksum validation, if requested
	if !coldGet && goi.lom.CksumConf().ValidateWarmGet {
		coldGet, errCode, err = goi.tryRecoverObject()
		if err != nil {
			if !coldGet {
				goi.lom.Unlock(false)
				glog.Error(err)
				return
			}
			glog.Errorf("%v - proceeding to execute cold GET from %s", err, goi.lom.Bck())
		}
	}

	// 3. coldget
	if coldGet {
		if !capRead {
			capRead = true
			cs = fs.GetCapStatus()
		}
		if cs.OOS {
			// No space left to prefetch object.
			goi.lom.Unlock(false)
			return sent, http.StatusInsufficientStorage, cs.Err
		}
		goi.lom.SetAtimeUnix(goi.started.UnixNano())
		if errCode, err := goi.t.GetCold(goi.ctx, goi.lom, cluster.GetCold); err != nil {
			return sent, errCode, err
		}
		goi.t.putMirror(goi.lom)
	}

	// 4. get locally and stream back
get:
	retry, sent, errCode, err = goi.finalize(coldGet)
	if retry && !retried {
		glog.Warningf("GET %s: uncaching and retrying...", goi.lom)
		retried = true
		goi.lom.Uncache(true /*delDirty*/)
		goto do
	}

	goi.lom.Unlock(false)
	return
}

// validate checksum; if corrupted try to recover from other replicas or EC slices
func (goi *getObjInfo) tryRecoverObject() (coldGet bool, code int, err error) {
	var (
		lom     = goi.lom
		retried bool
	)
retry:
	err = lom.ValidateMetaChecksum()
	if err == nil {
		err = lom.ValidateContentChecksum()
	}
	if err == nil {
		return
	}
	code = http.StatusInternalServerError
	if _, ok := err.(*cos.ErrBadCksum); !ok {
		return
	}
	if !lom.Bck().IsAIS() {
		coldGet = true
		return
	}

	glog.Warning(err)
	redundant := lom.HasCopies() || lom.Bprops().EC.Enabled
	//
	// return err if there's no redundancy OR already recovered once (and failed)
	//
	if retried || !redundant {
		//
		// TODO: mark `deleted` and postpone actual deletion
		//
		if erl := lom.Remove(); erl != nil {
			glog.Warningf("%s: failed to remove corrupted %s, err: %v", goi.t.si, lom, erl)
		}
		return
	}
	//
	// try to recover from BAD CHECKSUM
	//
	cos.RemoveFile(lom.FQN) // TODO: ditto

	if lom.HasCopies() {
		retried = true
		goi.lom.Unlock(false)
		// lookup and restore the object from local replicas
		restored := lom.RestoreObjectFromAny()
		goi.lom.Lock(false)
		if restored {
			glog.Warningf("%s: recovered corrupted %s from local replica", goi.t.si, lom)
			code = 0
			goto retry
		}
	}
	if lom.Bprops().EC.Enabled {
		retried = true
		goi.lom.Unlock(false)
		cos.RemoveFile(lom.FQN)
		_, code, err = goi.tryRestoreObject()
		goi.lom.Lock(false)
		if err == nil {
			glog.Warningf("%s: recovered corrupted %s from EC slices", goi.t.si, lom)
			code = 0
			goto retry
		}
	}

	// TODO: ditto
	if erl := lom.Remove(); erl != nil {
		glog.Warningf("%s: failed to remove corrupted %s, err: %v", goi.t.si, lom, erl)
	}
	return
}

// an attempt to restore an object that is missing in the ais bucket - from:
// 1) local FS
// 2) other FSes or targets when resilvering (rebalancing) is running (aka GFN)
// 3) other targets if the bucket erasure coded
// 4) Cloud
func (goi *getObjInfo) tryRestoreObject() (doubleCheck bool, errCode int, err error) {
	var (
		tsi, gfnNode         *cluster.Snode
		smap                 = goi.t.owner.smap.get()
		tname                = goi.t.si.String()
		marked               = xreg.GetResilverMarked()
		interrupted, running = marked.Interrupted, marked.Xact != nil
		gfnActive            = goi.t.gfn.local.active()
		ecEnabled            = goi.lom.Bprops().EC.Enabled
	)
	tsi, err = cluster.HrwTarget(goi.lom.Uname(), &smap.Smap, true /*include maintenance*/)
	if err != nil {
		return
	}
	if interrupted || running || gfnActive {
		if goi.lom.RestoreObjectFromAny() { // get-from-neighbor local (mountpaths) variety
			if glog.FastV(4, glog.SmoduleAIS) {
				glog.Infof("%s restored", goi.lom)
			}
			return
		}
		doubleCheck = running
	}

	// FIXME: if there're not enough EC targets to restore a "sliced" object,
	// we might be able to restore it if it was replicated. In this case even
	// just one additional target might be sufficient. This won't succeed if
	// an object was sliced, neither will ecmanager.RestoreObject(lom)
	enoughECRestoreTargets := goi.lom.Bprops().EC.RequiredRestoreTargets() <= smap.CountActiveTargets()

	// cluster-wide lookup ("get from neighbor")
	marked = xreg.GetRebMarked()
	interrupted, running = marked.Interrupted, marked.Xact != nil
	if running {
		doubleCheck = true
	}
	gfnActive = goi.t.gfn.global.active()
	if running && tsi.ID() != goi.t.si.ID() {
		if goi.t.LookupRemoteSingle(goi.lom, tsi) {
			gfnNode = tsi
			goto gfn
		}
	}
	if running || !enoughECRestoreTargets || ((interrupted || gfnActive) && !ecEnabled) {
		gfnNode = goi.t.lookupRemoteAll(goi.lom, smap)
	}

gfn:
	if gfnNode != nil {
		if goi.getFromNeighbor(goi.lom, gfnNode) {
			if glog.FastV(4, glog.SmoduleAIS) {
				glog.Infof("%s: GFN %s <= %s", tname, goi.lom, gfnNode)
			}
			return
		}
	}

	// restore from existing EC slices if possible
	if ecErr := ec.ECM.RestoreObject(goi.lom); ecErr == nil {
		ecErr = goi.lom.Load(true /*cache it*/, false /*locked*/) // TODO: optimize locking
		debug.AssertNoErr(ecErr)
		if ecErr == nil {
			if glog.FastV(4, glog.SmoduleAIS) {
				glog.Infof("%s: EC-recovered %s", tname, goi.lom)
			}
			return
		}
		err = fmt.Errorf(cmn.FmtErrFailed, tname, "load EC-recovered", goi.lom, ecErr)
	} else if ecErr != ec.ErrorECDisabled {
		err = fmt.Errorf(cmn.FmtErrFailed, tname, "EC-recover", goi.lom, ecErr)
	}

	if err != nil {
		err = fmt.Errorf("object %s/%s not found, err: %v", goi.lom.Bck(), goi.lom.ObjName, err)
	} else {
		err = cmn.NewNotFoundError("object %s/%s", goi.lom.Bck(), goi.lom.ObjName)
	}
	errCode = http.StatusNotFound
	return
}

func (goi *getObjInfo) getFromNeighbor(lom *cluster.LOM, tsi *cluster.Snode) (ok bool) {
	header := make(http.Header)
	header.Add(cmn.HdrCallerID, goi.t.SID())
	header.Add(cmn.HdrCallerName, goi.t.Sname())
	query := url.Values{}
	query.Set(cmn.URLParamIsGFNRequest, "true")
	query = cmn.AddBckToQuery(query, lom.Bucket())
	reqArgs := cmn.ReqArgs{
		Method: http.MethodGet,
		Base:   tsi.URL(cmn.NetworkIntraData),
		Header: header,
		Path:   cmn.URLPathObjects.Join(lom.BckName(), lom.ObjName),
		Query:  query,
	}
	config := cmn.GCO.Get()
	req, _, cancel, err := reqArgs.ReqWithTimeout(config.Timeout.SendFile.D())
	if err != nil {
		glog.Errorf("failed to create request, err: %v", err)
		return
	}
	defer cancel()

	resp, err := goi.t.client.data.Do(req) // nolint:bodyclose // closed by `poi.putObject`
	if err != nil {
		glog.Errorf("GFN failure, URL %q, err: %v", reqArgs.URL(), err)
		return
	}
	lom.FromHTTPHdr(resp.Header)
	workFQN := fs.CSM.GenContentFQN(lom, fs.WorkfileType, fs.WorkfileRemote)
	poi := &putObjInfo{
		t:        goi.t,
		lom:      lom,
		r:        resp.Body,
		recvType: cluster.Migrated,
		workFQN:  workFQN,
	}
	if _, err := poi.putObject(); err != nil {
		glog.Error(err)
		return
	}
	ok = true
	return
}

func (goi *getObjInfo) finalize(coldGet bool) (retry, sent bool, errCode int, err error) {
	var (
		file    *os.File
		sgl     *memsys.SGL
		slab    *memsys.Slab
		buf     []byte
		reader  io.Reader
		hdr     http.Header // if it is http request we will write also header
		written int64
	)
	defer func() {
		if file != nil {
			cos.Close(file)
		}
		if buf != nil {
			slab.Free(buf)
		}
		if sgl != nil {
			sgl.Free()
		}
	}()

	// loopback if disk IO is disabled
	if daemon.dryRun.disk {
		if err = cos.FloodWriter(goi.w, daemon.dryRun.size); err != nil {
			err = fmt.Errorf("dry-run: failed to send random response, err: %v", err)
			errCode = http.StatusInternalServerError
			goi.t.statsT.Add(stats.ErrGetCount, 1)
			return
		}
		delta := time.Since(goi.started)
		goi.t.statsT.AddMany(
			stats.NamedVal64{Name: stats.GetCount, Value: 1},
			stats.NamedVal64{Name: stats.GetLatency, Value: int64(delta)},
		)
		return
	}

	if rw, ok := goi.w.(http.ResponseWriter); ok {
		hdr = rw.Header()
	}

	fqn := goi.lom.FQN
	if !coldGet && !goi.isGFN {
		// best-effort GET load balancing (see also mirror.findLeastUtilized())
		fqn = goi.lom.LBGet()
	}
	file, err = os.Open(fqn)
	if err != nil {
		if os.IsNotExist(err) {
			errCode = http.StatusNotFound
			retry = true // (!lom.IsAIS() || lom.ECEnabled() || GFN...)
		} else {
			goi.t.fsErr(err, fqn)
			err = fmt.Errorf("%s: %w", goi.lom, err)
			errCode = http.StatusInternalServerError
		}
		return
	}

	var (
		r    *cmn.HTTPRange
		size = goi.lom.Size()
	)
	if goi.ranges.Size > 0 {
		size = goi.ranges.Size
	}

	if hdr != nil {
		ranges, err := cmn.ParseMultiRange(goi.ranges.Range, size)
		if err != nil {
			if err == cmn.ErrNoOverlap {
				hdr.Set(cmn.HdrContentRange, fmt.Sprintf("%s*/%d", cmn.HdrContentRangeValPrefix, size))
			}
			return false, sent, http.StatusRequestedRangeNotSatisfiable, err
		}

		if len(ranges) > 0 {
			if len(ranges) > 1 {
				err = fmt.Errorf(cmn.FmtErrUnsupported, goi.t.Snode(), "multi-range")
				errCode = http.StatusRequestedRangeNotSatisfiable
				return false, sent, errCode, err
			}
			r = &ranges[0]

			hdr.Set(cmn.HdrAcceptRanges, "bytes")
			hdr.Set(cmn.HdrContentRange, r.ContentRange(size))
		}
	}

	cksumConf := goi.lom.CksumConf()
	cksumRange := cksumConf.Type != cos.ChecksumNone && r != nil && cksumConf.EnableReadRange

	if hdr != nil {
		if !goi.lom.Cksum().IsEmpty() && !cksumRange {
			cksumType, cksumValue := goi.lom.Cksum().Get()
			hdr.Set(cmn.HdrObjCksumType, cksumType)
			hdr.Set(cmn.HdrObjCksumVal, cksumValue)
		}
		if goi.lom.Version() != "" {
			hdr.Set(cmn.HdrObjVersion, goi.lom.Version())
		}
		hdr.Set(cmn.HdrObjSize, strconv.FormatInt(goi.lom.Size(), 10))
		hdr.Set(cmn.HdrObjAtime, cos.UnixNano2S(goi.lom.AtimeUnix()))
		if r != nil {
			hdr.Set(cmn.HdrContentLength, strconv.FormatInt(r.Length, 10))
		} else {
			hdr.Set(cmn.HdrContentLength, strconv.FormatInt(size, 10))
		}
	}

	w := goi.w
	if r == nil {
		reader = file
		if goi.chunked {
			// Explicitly hiding `ReadFrom` implemented for `http.ResponseWriter`
			// so the `sendfile` syscall won't be used.
			w = cos.WriterOnly{Writer: goi.w}
			buf, slab = goi.t.gmm.Alloc(goi.lom.Size())
		}
	} else {
		buf, slab = goi.t.gmm.Alloc(r.Length)
		reader = io.NewSectionReader(file, r.Start, r.Length)
		if cksumRange {
			var cksum *cos.CksumHash
			sgl = slab.MMSA().NewSGL(r.Length, slab.Size())
			if _, cksum, err = cos.CopyAndChecksum(sgl, reader, buf, cksumConf.Type); err != nil {
				return
			}
			hdr.Set(cmn.HdrObjCksumVal, cksum.Value())
			hdr.Set(cmn.HdrObjCksumType, cksumConf.Type)
			reader = io.NewSectionReader(file, r.Start, r.Length)
		}
	}

	sent = true // At this point we mark the object as sent, regardless of the outcome.
	written, err = io.CopyBuffer(w, reader, buf)
	if err != nil {
		if cmn.IsErrConnectionReset(err) {
			return
		}
		goi.t.fsErr(err, fqn)
		goi.t.statsT.Add(stats.ErrGetCount, 1)
		err := fmt.Errorf(cmn.FmtErrFailed, goi.t.si, "GET", fqn, err)
		return retry, sent, http.StatusInternalServerError, err
	}

	// GFN: atime must be already set
	if !coldGet && !goi.isGFN {
		goi.lom.Load(false /*cache it*/, true /*locked*/)
		goi.lom.SetAtimeUnix(goi.started.UnixNano())
		goi.lom.ReCache(true) // GFN and cold GETs already did this
	}

	// Update objects which were sent during GFN. Thanks to this we will not
	// have to resend them in rebalance. In case of a race between rebalance
	// and GFN, the former wins and it will result in double send.
	if goi.isGFN {
		goi.t.rebManager.FilterAdd([]byte(goi.lom.Uname()))
	}

	delta := time.Since(goi.started)
	if glog.FastV(4, glog.SmoduleAIS) {
		s := fmt.Sprintf("GET: %s(%s), %s", goi.lom, cos.B2S(written, 1), delta)
		if coldGet {
			s += " (cold)"
		}
		glog.Infoln(s)
	}
	goi.t.statsT.AddMany(
		stats.NamedVal64{Name: stats.GetThroughput, Value: written},
		stats.NamedVal64{Name: stats.GetLatency, Value: int64(delta)},
		stats.NamedVal64{Name: stats.GetCount, Value: 1},
	)
	return
}

///////////////////
// APPEND OBJECT //
///////////////////

func (aoi *appendObjInfo) appendObject() (newHandle string, errCode int, err error) {
	filePath := aoi.hi.filePath
	switch aoi.op {
	case cmn.AppendOp:
		var f *os.File
		if filePath == "" {
			filePath = fs.CSM.GenContentFQN(aoi.lom, fs.WorkfileType, fs.WorkfileAppend)
			f, err = aoi.lom.CreateFile(filePath)
			if err != nil {
				errCode = http.StatusInternalServerError
				return
			}
			aoi.hi.partialCksum = cos.NewCksumHash(aoi.lom.CksumConf().Type)
		} else {
			f, err = os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, cos.PermRWR)
			if err != nil {
				errCode = http.StatusInternalServerError
				return
			}
			cos.Assert(aoi.hi.partialCksum != nil)
		}

		var (
			buf  []byte
			slab *memsys.Slab
		)
		if aoi.size == 0 {
			buf, slab = aoi.t.gmm.Alloc()
		} else {
			buf, slab = aoi.t.gmm.Alloc(aoi.size)
		}

		w := cos.NewWriterMulti(f, aoi.hi.partialCksum.H)
		_, err = io.CopyBuffer(w, aoi.r, buf)

		slab.Free(buf)
		cos.Close(f)
		if err != nil {
			errCode = http.StatusInternalServerError
			return
		}

		newHandle = combineAppendHandle(aoi.t.si.ID(), filePath, aoi.hi.partialCksum)
	case cmn.FlushOp:
		if filePath == "" {
			err = errors.New("handle not provided")
			errCode = http.StatusBadRequest
			return
		}
		cos.Assert(aoi.hi.partialCksum != nil)
		aoi.hi.partialCksum.Finalize()
		partialCksum := aoi.hi.partialCksum.Clone()
		if aoi.cksum != nil && !partialCksum.Equal(aoi.cksum) {
			err = cos.NewBadDataCksumError(partialCksum, aoi.cksum)
			errCode = http.StatusInternalServerError
			return
		}
		params := cluster.PromoteFileParams{
			SrcFQN:    filePath,
			Bck:       aoi.lom.Bck(),
			ObjName:   aoi.lom.ObjName,
			Cksum:     partialCksum,
			Overwrite: true,
			KeepOrig:  false,
		}
		if _, err := aoi.t.PromoteFile(params); err != nil {
			return "", 0, err
		}
	default:
		cos.AssertMsg(false, aoi.op)
	}

	delta := time.Since(aoi.started)
	aoi.t.statsT.AddMany(
		stats.NamedVal64{Name: stats.AppendCount, Value: 1},
		stats.NamedVal64{Name: stats.AppendLatency, Value: int64(delta)},
	)
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("PUT %s: %s", aoi.lom, delta)
	}
	return
}

func parseAppendHandle(handle string) (hi handleInfo, err error) {
	if handle == "" {
		return
	}
	p := strings.SplitN(handle, "|", 4)
	if len(p) != 4 {
		return hi, fmt.Errorf("invalid handle provided: %q", handle)
	}
	hi.partialCksum = cos.NewCksumHash(p[2])
	buf, err := base64.StdEncoding.DecodeString(p[3])
	if err != nil {
		return hi, err
	}
	err = hi.partialCksum.H.(encoding.BinaryUnmarshaler).UnmarshalBinary(buf)
	if err != nil {
		return hi, err
	}
	hi.nodeID = p[0]
	hi.filePath = p[1]
	return
}

func combineAppendHandle(nodeID, filePath string, partialCksum *cos.CksumHash) string {
	buf, err := partialCksum.H.(encoding.BinaryMarshaler).MarshalBinary()
	cos.AssertNoErr(err)
	cksumTy := partialCksum.Type()
	cksumBinary := base64.StdEncoding.EncodeToString(buf)
	return nodeID + "|" + filePath + "|" + cksumTy + "|" + cksumBinary
}

/////////////////
// COPY OBJECT //
/////////////////

func (coi *copyObjInfo) copyObject(src *cluster.LOM, objNameTo string) (size int64, err error) {
	debug.Assert(coi.DP == nil)
	if src.Bck().IsRemote() || coi.BckTo.IsRemote() {
		// There will be no logic to create local copies etc, we can simply use copyReader
		coi.DP = &cluster.LomReader{}
		return coi.copyReader(src, objNameTo)
	}
	si := coi.t.si
	if !coi.localOnly {
		smap := coi.t.owner.smap.Get()
		if si, err = cluster.HrwTarget(coi.BckTo.MakeUname(objNameTo), smap); err != nil {
			return
		}
	}
	// remote copy
	if si.ID() != coi.t.si.ID() {
		params := allocSendParams()
		{
			params.ObjNameTo = objNameTo
			params.Tsi = si
			params.DM = coi.DM
			params.NoVersion = !src.Bucket().Equal(coi.BckTo.Bck) // no versioning when buckets differ
		}
		size, err = coi.putRemote(src, params)
		freeSendParams(params)
		return
	}
	// dry-run
	if coi.DryRun {
		// TODO: replace with something similar to src.FQN == dst.FQN, but dstBck might not exist.
		if src.Bucket().Equal(coi.BckTo.Bck) && src.ObjName == objNameTo {
			return 0, nil
		}
		return src.Size(), nil
	}
	// local copy
	dst := cluster.AllocLOM(objNameTo)
	defer cluster.FreeLOM(dst)
	err = dst.Init(coi.BckTo.Bck)
	if err != nil {
		return
	}
	if src.FQN == dst.FQN { // resilvering with a single mountpath?
		return
	}

	exclusive := src.Uname() == dst.Uname()
	src.Lock(exclusive)
	defer src.Unlock(exclusive)
	if err = src.Load(false /*cache it*/, true /*locked*/); err != nil {
		if !cmn.IsObjNotExist(err) {
			err = fmt.Errorf("%s: err: %v", src, err)
		}
		return
	}

	// unless overwriting the source w-lock the destination (see `exclusive`)
	if src.Uname() != dst.Uname() {
		dst.Lock(true)
		defer dst.Unlock(true)
		if err = dst.Load(false /*cache it*/, true /*locked*/); err == nil {
			if src.Cksum().Equal(dst.Cksum()) {
				return
			}
		} else if cmn.IsErrBucketNought(err) {
			return
		}
	}
	dst2, err2 := src.CopyObject(dst.FQN, coi.Buf)
	if err2 == nil {
		size = src.Size()
		if coi.finalize {
			coi.t.putMirror(dst2)
		}
	}
	err = err2
	cluster.FreeLOM(dst2)
	return
}

/////////////////
// COPY READER //
/////////////////

// copyReader puts a new object to a cluster, according to a reader taken from coi.DP.Reader(lom) The reader returned
// from coi.DP is responsible for any locking or source LOM, if necessary. If the reader doesn't take any locks, it has
// to consider object content changing in the middle of copying.
//
// LOM can be meta of a cloud object. It creates some problems. However, it's DP who is responsible for providing a reader,
// so DP should tak any steps necessary to do so. It includes handling cold get, warm get etc.
//
// If destination bucket is remote bucket, copyReader will always create a cached copy of an object on one of the
// targets as well as make put to the relevant backend provider.
// TODO: Make it possible to skip caching an object from a cloud bucket.
func (coi *copyObjInfo) copyReader(lom *cluster.LOM, objNameTo string) (size int64, err error) {
	var (
		reader cos.ReadOpenCloser
		si     *cluster.Snode
	)
	debug.Assert(coi.DP != nil)
	if si, err = cluster.HrwTarget(coi.BckTo.MakeUname(objNameTo), coi.t.owner.smap.Get()); err != nil {
		return
	}

	if si.ID() != coi.t.si.ID() {
		params := allocSendParams()
		{
			params.ObjNameTo = objNameTo
			params.Tsi = si
			params.DM = coi.DM
			params.NoVersion = !lom.Bucket().Equal(coi.BckTo.Bck) // no versioning when buckets differ
		}
		size, err = coi.putRemote(lom, params)
		freeSendParams(params)
		return
	}

	if err = lom.Load(false /*cache it*/, false /*locked*/); err != nil {
		return
	}
	// DryRun: just get a reader and discard it. Init on dstLOM would cause and error as dstBck doesn't exist.
	if coi.DryRun {
		return coi.dryRunCopyReader(lom)
	}
	dst := cluster.AllocLOM(objNameTo)
	defer cluster.FreeLOM(dst)
	if err = dst.Init(coi.BckTo.Bck); err != nil {
		return
	}
	if lom.Bucket().Equal(coi.BckTo.Bck) {
		dst.SetVersion(lom.Version())
	}

	if reader, _, err = coi.DP.Reader(lom); err != nil {
		return 0, err
	}

	// Set the correct recvType: some transactions must update the object
	// in the Cloud(if destination is a Cloud bucket).
	recvType := cluster.Migrated
	if coi.DM != nil {
		recvType = coi.DM.RecvType()
	}
	params := cluster.PutObjectParams{
		Tag:      "copy-dp",
		Reader:   reader,
		RecvType: recvType,
	}
	if err := coi.t.PutObject(dst, params); err != nil {
		return 0, err
	}

	return lom.Size(), nil
}

func (coi *copyObjInfo) dryRunCopyReader(lom *cluster.LOM) (size int64, err error) {
	debug.Assert(coi.DryRun)
	debug.Assert(coi.DP != nil)

	var reader io.ReadCloser
	if reader, _, err = coi.DP.Reader(lom); err != nil {
		return 0, err
	}
	defer reader.Close()
	return io.Copy(io.Discard, reader)
}

// PUT object onto designated target
func (coi *copyObjInfo) putRemote(lom *cluster.LOM, params *cluster.SendToParams) (size int64, err error) {
	if coi.DM != nil {
		// We need to clone the `lom` as we will use in async operation.
		// The `lom` is freed on callback in `_sendObjDM`.
		lom = lom.Clone(lom.FQN)
	}

	if coi.DP == nil {
		// XXX: Kind of hacky way of determining if the `lom` is object or just
		//  a regular file. Ideally, the parameter shouldn't be `lom` at all
		//  but rather something more general like `cluster.CT`.
		var reader cos.ReadOpenCloser
		if !coi.promoteFile {
			lom.Lock(false)
			if err := lom.Load(false /*cache it*/, true /*locked*/); err != nil {
				lom.Unlock(false)
				return 0, nil
			}
			if coi.DryRun {
				lom.Unlock(false)
				return lom.Size(), nil
			}
			fh, err := cos.NewFileHandle(lom.FQN)
			if err != nil {
				lom.Unlock(false)
				return 0, fmt.Errorf(cmn.FmtErrFailed, coi.t.Snode(), "open", lom.FQN, err)
			}
			size = lom.Size()
			reader = cos.NewDeferROC(fh, func() { lom.Unlock(false) })
		} else {
			debug.Assert(!coi.DryRun)
			fh, err := cos.NewFileHandle(lom.FQN)
			if err != nil {
				return 0, fmt.Errorf(cmn.FmtErrFailed, coi.t.Snode(), "open", lom.FQN, err)
			}
			fi, err := fh.Stat()
			if err != nil {
				fh.Close()
				return 0, fmt.Errorf("failed to stat %s: %w", lom.FQN, err)
			}
			size = fi.Size()
			reader = fh
		}

		params.Reader = reader
		params.HdrMeta = lom
	} else {
		if params.Reader, params.HdrMeta, err = coi.DP.Reader(lom); err != nil {
			return
		}
		if coi.DryRun {
			size, err = io.Copy(io.Discard, params.Reader)
			cos.Close(params.Reader)
			return
		}
		if size = params.HdrMeta.Size(); size < 0 {
			// NOTE: Return the current size as resulting (transformed) size isn't known.
			size = lom.Size()
		}
	}
	debug.Assert(params.HdrMeta != nil)
	if params.NoVersion {
		params.HdrMeta = cmn.NewHdrMetaCustomVersion(params.HdrMeta, "")
	}
	params.BckTo = coi.BckTo
	if params.DM != nil { // Either send via stream...
		err = _sendObjDM(lom, params)
	} else { // ... or via remote PUT call.
		err = coi.t._sendPUT(lom, params)
	}
	return
}

// streaming send via bundle.DataMover
func _sendObjDM(lom *cluster.LOM, params *cluster.SendToParams) error {
	o := transport.AllocSend()
	hdr, meta := &o.Hdr, params.HdrMeta
	{
		hdr.Bck = params.BckTo.Bck
		hdr.ObjName = params.ObjNameTo
		hdr.ObjAttrs.Size = meta.Size()
		hdr.ObjAttrs.Atime = meta.AtimeUnix()
		if cksum := meta.Cksum(); cksum != nil {
			hdr.ObjAttrs.CksumType, hdr.ObjAttrs.CksumValue = cksum.Get()
		}
		hdr.ObjAttrs.Version = meta.Version()
	}
	o.Callback = func(_ transport.ObjHdr, _ io.ReadCloser, _ interface{}, _ error) {
		cluster.FreeLOM(lom)
	}
	return params.DM.Send(o, params.Reader, params.Tsi)
}

///////////////
// mem pools //
///////////////

var (
	goiPool, poiPool, coiPool, sndPool sync.Pool

	goi0 getObjInfo
	poi0 putObjInfo
	coi0 copyObjInfo
	snd0 cluster.SendToParams
)

func allocGetObjInfo() (a *getObjInfo) {
	if v := goiPool.Get(); v != nil {
		a = v.(*getObjInfo)
		return
	}
	return &getObjInfo{}
}

func freeGetObjInfo(a *getObjInfo) {
	*a = goi0
	goiPool.Put(a)
}

func allocPutObjInfo() (a *putObjInfo) {
	if v := poiPool.Get(); v != nil {
		a = v.(*putObjInfo)
		return
	}
	return &putObjInfo{}
}

func freePutObjInfo(a *putObjInfo) {
	*a = poi0
	poiPool.Put(a)
}

func allocCopyObjInfo() (a *copyObjInfo) {
	if v := coiPool.Get(); v != nil {
		a = v.(*copyObjInfo)
		return
	}
	return &copyObjInfo{}
}

func freeCopyObjInfo(a *copyObjInfo) {
	*a = coi0
	coiPool.Put(a)
}

func allocSendParams() (a *cluster.SendToParams) {
	if v := sndPool.Get(); v != nil {
		a = v.(*cluster.SendToParams)
		return
	}
	return &cluster.SendToParams{}
}

func freeSendParams(a *cluster.SendToParams) {
	*a = snd0
	sndPool.Put(a)
}
