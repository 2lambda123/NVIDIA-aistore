// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"context"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/ais/s3compat"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/ec"
	"github.com/NVIDIA/aistore/fs"
)

// PUT s3/bckName/objName
func (t *targetrunner) s3Handler(w http.ResponseWriter, r *http.Request) {
	apiItems, err := t.checkRESTItems(w, r, 0, true, cmn.URLPathS3.L)
	if err != nil {
		return
	}

	switch r.Method {
	case http.MethodHead:
		t.headObjS3(w, r, apiItems)
	case http.MethodGet:
		t.getObjS3(w, r, apiItems)
	case http.MethodPut:
		t.putObjS3(w, r, apiItems)
	case http.MethodDelete:
		t.delObjS3(w, r, apiItems)
	default:
		cmn.WriteErr405(w, r, http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodPut)
	}
}

func (t *targetrunner) copyObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	if len(items) < 2 {
		t.writeErr(w, r, errS3Obj)
		return
	}
	src := r.Header.Get(s3compat.HeaderObjSrc)
	src = strings.Trim(src, "/") // in AWS examples the path starts with "/"
	parts := strings.SplitN(src, "/", 2)
	if len(parts) < 2 {
		t.writeErr(w, r, errS3Obj)
		return
	}
	bckSrc := cluster.NewBck(parts[0], cmn.ProviderAIS, cmn.NsGlobal)
	objSrc := strings.Trim(parts[1], "/")
	if err := bckSrc.Init(t.owner.bmd); err != nil {
		t.writeErr(w, r, err)
		return
	}
	lom := cluster.AllocLOM(objSrc)
	defer cluster.FreeLOM(lom)
	if err := lom.Init(bckSrc.Bck); err != nil {
		if _, ok := err.(*cmn.ErrRemoteBucketDoesNotExist); ok {
			t.BMDVersionFixup(r, cmn.Bck{}, true /* sleep */)
			err = lom.Init(bckSrc.Bck)
		}
		if err != nil {
			t.writeErr(w, r, err)
		}
		return
	}
	if err := lom.Load(false /*cache it*/, false /*locked*/); err != nil {
		t.writeErr(w, r, err)
		return
	}
	bckDst := cluster.NewBck(items[0], cmn.ProviderAIS, cmn.NsGlobal)
	if err := bckDst.Init(t.owner.bmd); err != nil {
		t.writeErr(w, r, err)
		return
	}

	coi := copyObjInfo{t: t}
	coi.BckTo = bckDst
	objName := path.Join(items[1:]...)
	if _, err := coi.copyObject(lom, objName); err != nil {
		t.writeErr(w, r, err)
		return
	}

	var cksumValue string
	if cksum := lom.Cksum(); cksum != nil && cksum.Type() == cos.ChecksumMD5 {
		cksumValue = cksum.Value()
	}
	result := s3compat.CopyObjectResult{
		LastModified: s3compat.FormatTime(lom.Atime()),
		ETag:         cksumValue,
	}
	w.Write(result.MustMarshal())
}

func (t *targetrunner) directPutObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	started := time.Now()
	if cs := fs.GetCapStatus(); cs.OOS {
		t.writeErr(w, r, cs.Err)
		return
	}
	bck := cluster.NewBck(items[0], cmn.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(t.owner.bmd); err != nil {
		t.writeErr(w, r, err)
		return
	}
	if len(items) < 2 {
		t.writeErr(w, r, errS3Obj)
		return
	}
	var err error
	objName := path.Join(items[1:]...)
	lom := cluster.AllocLOM(objName)
	defer cluster.FreeLOM(lom)
	if err = lom.Init(bck.Bck); err != nil {
		if _, ok := err.(*cmn.ErrRemoteBucketDoesNotExist); ok {
			t.BMDVersionFixup(r, cmn.Bck{}, true /*sleep*/)
			err = lom.Init(bck.Bck)
		}
		if err != nil {
			t.writeErr(w, r, err)
			return
		}
	}
	if lom.Bck().IsAIS() && lom.VersionConf().Enabled {
		lom.Load(true /*cache it*/, false /*locked*/) // load it to see the current version
	}
	lom.SetAtimeUnix(started.UnixNano())

	// TODO: lom.SetCustomMD(cluster.AmazonMD5ObjMD, checksum)

	if errCode, err := t.doPut(r, lom, started); err != nil {
		t.fsErr(err, lom.FQN)
		t.writeErr(w, r, err, errCode)
		return
	}
	s3compat.SetHeaderFromLOM(w.Header(), lom, 0)
}

// PUT s3/bckName/objName
func (t *targetrunner) putObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	if r.Header.Get(s3compat.HeaderObjSrc) == "" {
		t.directPutObjS3(w, r, items)
		return
	}
	t.copyObjS3(w, r, items)
}

// GET s3/<bucket-name/<object-name>[?uuid=<etl-uuid>]
func (t *targetrunner) getObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	if len(items) < 2 {
		t.writeErr(w, r, errS3Obj)
		return
	}
	started := time.Now()
	bck := cluster.NewBck(items[0], cmn.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(t.owner.bmd); err != nil {
		t.writeErr(w, r, err)
		return
	}
	var (
		err     error
		objSize int64

		objName = path.Join(items[1:]...)
	)
	uuid := r.URL.Query().Get(cmn.URLParamUUID)
	if uuid != "" {
		t.doETL(w, r, uuid, bck, objName)
		return
	}

	lom := cluster.AllocLOM(objName)
	defer cluster.FreeLOM(lom)
	if err = lom.Init(bck.Bck); err != nil {
		if _, ok := err.(*cmn.ErrRemoteBucketDoesNotExist); ok {
			t.BMDVersionFixup(r, cmn.Bck{}, true /* sleep */)
			err = lom.Init(bck.Bck)
		}
		if err != nil {
			t.writeErr(w, r, err)
		}
		return
	}
	goi := allocGetObjInfo()
	{
		goi.started = started
		goi.t = t
		goi.lom = lom
		goi.w = w
		goi.ctx = context.Background()
		goi.ranges = cmn.RangesQuery{Range: r.Header.Get(cmn.HeaderRange), Size: objSize}
	}
	if sent, errCode, err := goi.getObject(); err != nil {
		if sent {
			// Cannot send error message at this point so we just glog.
			glog.Errorf("GET %s: %v", lom, err)
		} else {
			t.writeErr(w, r, err, errCode)
		}
	}
	objSize = lom.Size()
	s3compat.SetHeaderFromLOM(w.Header(), lom, objSize)
	freeGetObjInfo(goi)
}

// HEAD s3/bckName/objName
func (t *targetrunner) headObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	var err error
	if len(items) < 2 {
		t.writeErr(w, r, errS3Obj)
		return
	}
	bucket, objName := items[0], path.Join(items[1:]...)
	bck := cluster.NewBck(bucket, cmn.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(t.owner.bmd); err != nil {
		t.writeErr(w, r, err)
		return
	}
	lom := cluster.AllocLOM(objName)
	defer cluster.FreeLOM(lom)
	if err = lom.Init(bck.Bck); err != nil {
		if _, ok := err.(*cmn.ErrRemoteBucketDoesNotExist); ok {
			t.BMDVersionFixup(r, cmn.Bck{}, true /* sleep */)
			err = lom.Init(bck.Bck)
		}
		if err != nil {
			t.writeErr(w, r, err)
		}
		return
	}

	if err = lom.Load(true /*cache it*/, false /*locked*/); err != nil && !cmn.IsObjNotExist(err) {
		t.writeErr(w, r, err)
		return
	}

	exists := err == nil
	if !exists {
		err := cmn.NewNotFoundError("object %s/%s", bucket, objName)
		t.writeErr(w, r, err, http.StatusNotFound)
		return
	}

	if isETLRequest(r.URL.Query()) {
		s3compat.SetETLHeader(w.Header(), lom)
		return
	}
	s3compat.SetHeaderFromLOM(w.Header(), lom, lom.Size())
}

// DEL s3/bckName/objName
func (t *targetrunner) delObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	bck := cluster.NewBck(items[0], cmn.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(t.owner.bmd); err != nil {
		t.writeErr(w, r, err)
		return
	}
	if len(items) < 2 {
		t.writeErr(w, r, errS3Obj)
		return
	}
	objName := path.Join(items[1:]...)
	lom := cluster.AllocLOM(objName)
	defer cluster.FreeLOM(lom)
	if err := lom.Init(bck.Bck); err != nil {
		t.writeErr(w, r, err)
		return
	}
	errCode, err := t.DeleteObject(context.Background(), lom, false)
	if err != nil {
		if errCode == http.StatusNotFound {
			err := cmn.NewNotFoundError("object %s/%s", lom.Bck(), lom.ObjName)
			t.writeErrSilent(w, r, err, http.StatusNotFound)
		} else {
			t.writeErrStatusf(w, r, errCode, "error deleting %s: %v", lom, err)
		}
		return
	}
	// EC cleanup if EC is enabled
	ec.ECM.CleanupObject(lom)
}
