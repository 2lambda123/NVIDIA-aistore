// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/authn"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/dsort"
	"github.com/NVIDIA/aistore/nl"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/sys"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xaction/xreg"
)

const (
	whatRenamedLB = "renamedlb"
	ciePrefix     = "cluster integrity error: cie#"
	listBuckets   = "listBuckets"
)

const (
	fmtNotRemote  = "%q appears to be ais bucket (expecting remote)"
	fmtUnknownQue = "unexpected query [what=%s]"
)

type (
	ClusterMountpathsRaw struct {
		Targets cos.JSONRawMsgs `json:"targets"`
	}
	reverseProxy struct {
		cloud   *httputil.ReverseProxy // unmodified GET requests => storage.googleapis.com
		primary struct {
			sync.Mutex
			rp  *httputil.ReverseProxy // modify cluster-level md => current primary gateway
			url string                 // URL of the current primary
		}
		nodes sync.Map // map of reverse proxies keyed by node DaemonIDs
	}

	singleRProxy struct {
		rp *httputil.ReverseProxy
		u  *url.URL
	}

	// proxy runner
	proxyrunner struct {
		httprunner
		authn      *authManager
		metasyncer *metasyncer
		rproxy     reverseProxy
		notifs     notifs
		ic         ic
		reg        struct {
			mtx  sync.RWMutex
			pool nodeRegPool
		}
		qm queryMem
	}
)

// interface guard
var (
	_ cos.Runner = (*proxyrunner)(nil)
	_ electable  = (*proxyrunner)(nil)
)

func (p *proxyrunner) initClusterCIDR() {
	if nodeCIDR := os.Getenv("AIS_CLUSTER_CIDR"); nodeCIDR != "" {
		_, network, err := net.ParseCIDR(nodeCIDR)
		p.si.LocalNet = network
		cos.AssertNoErr(err)
		glog.Infof("local network: %+v", *network)
	}
}

// start proxy runner
func (p *proxyrunner) Run() error {
	config := cmn.GCO.Get()
	p.httprunner.init(config)
	p.httprunner.electable = p
	p.owner.bmd = newBMDOwnerPrx(config)

	p.owner.bmd.init() // initialize owner and load BMD

	cluster.Init(nil /*cluster.Target*/)

	// startup sequence - see earlystart.go for the steps and commentary
	p.bootstrap()

	p.authn = &authManager{
		tokens:        make(map[string]*authn.Token),
		revokedTokens: make(map[string]bool),
		version:       1,
	}

	p.rproxy.init()

	p.notifs.init(p)
	p.ic.init(p)
	p.qm.init()

	//
	// REST API: register proxy handlers and start listening
	//
	networkHandlers := []networkHandler{
		{r: cmn.Reverse, h: p.reverseHandler, net: accessNetPublic},

		// pubnet handlers: cluster must be started
		{r: cmn.Buckets, h: p.bucketHandler, net: accessNetPublic},
		{r: cmn.Objects, h: p.objectHandler, net: accessNetPublic},
		{r: cmn.Download, h: p.downloadHandler, net: accessNetPublic},
		{r: cmn.Query, h: p.queryHandler, net: accessNetPublic},
		{r: cmn.ETL, h: p.etlHandler, net: accessNetPublic},
		{r: cmn.Sort, h: p.dsortHandler, net: accessNetPublic},

		{r: cmn.IC, h: p.ic.handler, net: accessNetIntraControl},
		{r: cmn.Daemon, h: p.daemonHandler, net: accessNetPublicControl},
		{r: cmn.Cluster, h: p.clusterHandler, net: accessNetPublicControl},
		{r: cmn.Tokens, h: p.tokenHandler, net: accessNetPublic},

		{r: cmn.Metasync, h: p.metasyncHandler, net: accessNetIntraControl},
		{r: cmn.Health, h: p.healthHandler, net: accessNetPublicControl},
		{r: cmn.Vote, h: p.voteHandler, net: accessNetIntraControl},

		{r: cmn.Notifs, h: p.notifs.handler, net: accessNetIntraControl},

		{r: "/" + cmn.S3, h: p.s3Handler, net: accessNetPublic},

		{r: "/", h: p.httpCloudHandler, net: accessNetPublic},
	}

	p.registerNetworkHandlers(networkHandlers)

	glog.Infof("%s: [%s net] listening on: %s", p.si, cmn.NetworkPublic, p.si.PublicNet.DirectURL)
	if p.si.PublicNet.DirectURL != p.si.IntraControlNet.DirectURL {
		glog.Infof("%s: [%s net] listening on: %s", p.si, cmn.NetworkIntraControl, p.si.IntraControlNet.DirectURL)
	}
	if p.si.PublicNet.DirectURL != p.si.IntraDataNet.DirectURL {
		glog.Infof("%s: [%s net] listening on: %s", p.si, cmn.NetworkIntraData, p.si.IntraDataNet.DirectURL)
	}

	dsort.RegisterNode(p.owner.smap, p.owner.bmd, p.si, nil, p.statsT)
	return p.httprunner.run()
}

func (p *proxyrunner) sendKeepalive(timeout time.Duration) (status int, err error) {
	smap := p.owner.smap.get()
	if smap != nil && smap.isPrimary(p.si) {
		return
	}
	return p.httprunner.sendKeepalive(timeout)
}

func (p *proxyrunner) joinCluster(primaryURLs ...string) (status int, err error) {
	var query url.Values
	if smap := p.owner.smap.get(); smap.isPrimary(p.si) {
		return 0, fmt.Errorf("%s should not be joining: is primary, %s", p.si, smap.StringEx())
	}
	if cmn.GCO.Get().Proxy.NonElectable {
		query = url.Values{cmn.URLParamNonElectable: []string{"true"}}
	}
	res := p.join(query, primaryURLs...)
	defer _freeCallRes(res)
	if res.err != nil {
		status, err = res.status, res.err
		return
	}
	// not being sent at cluster startup and keepalive
	if len(res.bytes) == 0 {
		return
	}
	err = p.applyRegMeta(res.bytes, "")
	return
}

func (p *proxyrunner) applyRegMeta(body []byte, caller string) (err error) {
	regMeta, msg, err := p._applyRegMeta(body, caller)
	if err != nil {
		return err
	}
	// BMD
	if err = p.receiveBMD(regMeta.BMD, msg, caller); err != nil {
		if !isErrDowngrade(err) {
			glog.Errorf(cmn.FmtErrFailed, p.si, "sync", regMeta.BMD, err)
		}
	} else {
		glog.Infof("%s: synch %s", p.si, regMeta.BMD)
	}

	if err = p.receiveSmap(regMeta.Smap, msg, caller, p.smapOnUpdate); err != nil {
		if !isErrDowngrade(err) {
			glog.Errorf(cmn.FmtErrFailed, p.si, "sync", regMeta.Smap, err)
		}
	} else {
		glog.Infof("%s: synch %s", p.si, regMeta.Smap)
	}
	return
}

// stop proxy runner and return => rungroup.run
func (p *proxyrunner) Stop(err error) {
	var (
		s         string
		smap      = p.owner.smap.get()
		isPrimary = smap.isPrimary(p.si)
		f         = glog.Infof
	)
	if isPrimary {
		s = " (primary)"
	}
	if err != nil {
		f = glog.Warningf
	}
	f("Stopping %s%s, err: %v", p.si, s, err)
	xreg.AbortAll()
	p.httprunner.stop(!isPrimary && smap.isValid() && err != errShutdown /* rm from Smap*/)
}

////////////////////////////////////////
// http /bucket and /objects handlers //
////////////////////////////////////////

func (p *proxyrunner) parseAPIBckObj(w http.ResponseWriter, r *http.Request, bckArgs *bckInitArgs, origURLBck ...string) (bck *cluster.Bck, objName string, err error) {
	request := apiRequest{after: 2, prefix: cmn.URLPathObjects.L}
	if err = p.parseAPIRequest(w, r, &request); err != nil {
		return
	}
	bckArgs.bck = request.bck
	bck, err = bckArgs.initAndTry(request.bck.Name, origURLBck...)
	return bck, request.items[1], err
}

// verb /v1/buckets/
func (p *proxyrunner) bucketHandler(w http.ResponseWriter, r *http.Request) {
	if !p.ClusterStartedWithRetry() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		p.httpbckget(w, r)
	case http.MethodDelete:
		p.httpbckdelete(w, r)
	case http.MethodPost:
		p.httpbckpost(w, r)
	case http.MethodHead:
		p.httpbckhead(w, r)
	case http.MethodPatch:
		p.httpbckpatch(w, r)
	default:
		cmn.WriteErr405(w, r, http.MethodDelete, http.MethodGet, http.MethodHead,
			http.MethodPatch, http.MethodPost)
	}
}

// verb /v1/objects/
func (p *proxyrunner) objectHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.httpobjget(w, r)
	case http.MethodPut:
		p.httpobjput(w, r)
	case http.MethodDelete:
		p.httpobjdelete(w, r)
	case http.MethodPost:
		p.httpobjpost(w, r)
	case http.MethodHead:
		p.httpobjhead(w, r)
	default:
		cmn.WriteErr405(w, r, http.MethodDelete, http.MethodGet, http.MethodHead,
			http.MethodPost, http.MethodPut)
	}
}

// GET /v1/buckets[/bucket-name]
func (p *proxyrunner) httpbckget(w http.ResponseWriter, r *http.Request) {
	apiItems, err := p.checkRESTItems(w, r, 0, true, cmn.URLPathBuckets.L)
	if err != nil {
		return
	}

	var msg cmn.ActionMsg
	if err := cmn.ReadJSON(w, r, &msg); err != nil {
		return
	}

	var bckName string
	if len(apiItems) > 0 {
		bckName = apiItems[0]
	}

	var queryBcks cmn.QueryBcks
	if queryBcks, err = newQueryBcksFromQuery(bckName, r.URL.Query()); err != nil {
		p.writeErr(w, r, err)
		return
	}

	switch msg.Action {
	case cmn.ActList:
		p.handleList(w, r, queryBcks, &msg)
	case cmn.ActSummary:
		p.bucketSummary(w, r, queryBcks, &msg)
	default:
		p.writeErrAct(w, r, msg.Action)
	}
}

func (p *proxyrunner) handleList(w http.ResponseWriter, r *http.Request, queryBcks cmn.QueryBcks, msg *cmn.ActionMsg) {
	if queryBcks.Name == "" {
		if err := p.checkACL(r.Header, nil, cmn.AccessListBuckets); err != nil {
			p.writeErr(w, r, err, http.StatusUnauthorized)
			return
		}

		p.listBuckets(w, r, queryBcks, msg)
	} else {
		var (
			err error
			bck = cluster.NewBckEmbed(cmn.Bck(queryBcks))
		)
		bckArgs := bckInitArgs{p: p, w: w, r: r, msg: msg, perms: cmn.AccessObjLIST, tryOnlyRem: true, bck: bck}
		if bck, err = bckArgs.initAndTry(queryBcks.Name); err != nil {
			return
		}
		begin := mono.NanoTime()
		p.listObjects(w, r, bck, msg, begin)
	}
}

// GET /v1/objects/bucket-name/object-name
func (p *proxyrunner) httpobjget(w http.ResponseWriter, r *http.Request, origURLBck ...string) {
	started := time.Now()
	bckArgs := bckInitArgs{p: p, w: w, r: r, perms: cmn.AccessGET, tryOnlyRem: true}
	bck, objName, err := p.parseAPIBckObj(w, r, &bckArgs, origURLBck...)
	if err != nil {
		return
	}

	smap := p.owner.smap.get()
	si, err := cluster.HrwTarget(bck.MakeUname(objName), &smap.Smap)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%s %s/%s => %s", r.Method, bck.Name, objName, si)
	}
	redirectURL := p.redirectURL(r, si, started, cmn.NetworkIntraData)
	http.Redirect(w, r, redirectURL, http.StatusMovedPermanently)
	p.statsT.Add(stats.GetCount, 1)
}

// PUT /v1/objects/bucket-name/object-name
func (p *proxyrunner) httpobjput(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var (
		si       *cluster.Snode
		nodeID   string
		perms    cmn.AccessAttrs
		err      error
		smap     = p.owner.smap.get()
		query    = r.URL.Query()
		appendTy = query.Get(cmn.URLParamAppendType)
	)
	if appendTy == "" {
		perms = cmn.AccessPUT
	} else {
		perms = cmn.AccessAPPEND
		var hi handleInfo
		hi, err = parseAppendHandle(query.Get(cmn.URLParamAppendHandle))
		if err != nil {
			p.writeErr(w, r, err)
			return
		}
		nodeID = hi.nodeID
	}

	bckArgs := bckInitArgs{p: p, w: w, r: r, perms: perms, tryOnlyRem: true}
	bck, objName, err := p.parseAPIBckObj(w, r, &bckArgs)
	if err != nil {
		return
	}

	if nodeID == "" {
		si, err = cluster.HrwTarget(bck.MakeUname(objName), &smap.Smap)
		if err != nil {
			p.writeErr(w, r, err)
			return
		}
	} else {
		si = smap.GetTarget(nodeID)
		if si == nil {
			err = &errNodeNotFound{"PUT failure", nodeID, p.si, smap}
			p.writeErr(w, r, err)
			return
		}
	}

	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%s %s/%s => %s (append: %v)", r.Method, bck.Name, objName, si, appendTy != "")
	}
	redirectURL := p.redirectURL(r, si, started, cmn.NetworkIntraData)
	http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)

	if appendTy == "" {
		p.statsT.Add(stats.PutCount, 1)
	} else {
		p.statsT.Add(stats.AppendCount, 1)
	}
}

// DELETE /v1/objects/bucket-name/object-name
func (p *proxyrunner) httpobjdelete(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	bckArgs := bckInitArgs{p: p, w: w, r: r, perms: cmn.AccessObjDELETE, tryOnlyRem: true}
	bck, objName, err := p.parseAPIBckObj(w, r, &bckArgs)
	if err != nil {
		return
	}

	smap := p.owner.smap.get()
	si, err := cluster.HrwTarget(bck.MakeUname(objName), &smap.Smap)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%s %s/%s => %s", r.Method, bck.Name, objName, si)
	}
	redirectURL := p.redirectURL(r, si, started, cmn.NetworkIntraControl)
	http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)

	p.statsT.Add(stats.DeleteCount, 1)
}

// DELETE { action } /v1/buckets
func (p *proxyrunner) httpbckdelete(w http.ResponseWriter, r *http.Request) {
	var (
		err     error
		msg     = cmn.ActionMsg{}
		request = &apiRequest{msg: &msg, after: 1, prefix: cmn.URLPathBuckets.L}
	)
	if err = p.parseAPIRequest(w, r, request); err != nil {
		return
	}

	var (
		bck     = request.bck
		query   = r.URL.Query()
		perms   = cmn.AccessDestroyBucket
		errCode int
	)
	if msg.Action == cmn.ActDelete || msg.Action == cmn.ActEvictObjects {
		perms = cmn.AccessObjDELETE
	}

	bckArgs := bckInitArgs{p: p, w: w, r: r, msg: &msg, perms: perms, tryOnlyRem: true, bck: bck}
	if msg.Action == cmn.ActEvictRemoteBck {
		bck, errCode, err = bckArgs.init(bck.Name)
		if errCode == http.StatusNotFound {
			return // remote bucket not in BMD - ignore error
		}
		if err != nil {
			p.writeErr(w, r, err, errCode)
		}
	} else if bck, err = bckArgs.initAndTry(bck.Name); err != nil {
		return
	}

	switch msg.Action {
	case cmn.ActEvictRemoteBck:
		if bck.IsAIS() {
			p.writeErrf(w, r, fmtNotRemote, bck.Name)
			return
		}
		keepMD := cos.IsParseBool(r.URL.Query().Get(cmn.URLParamKeepBckMD))
		// HDFS buckets will always keep metadata so they can re-register later
		if bck.IsHDFS() || keepMD {
			if err := p.destroyBucketData(&msg, bck); err != nil {
				p.writeErr(w, r, err)
			}
			return
		}
		fallthrough // fallthrough
	case cmn.ActDestroyBck:
		if p.forwardCP(w, r, &msg, bck.Name) {
			return
		}
		if bck.IsRemoteAIS() {
			if err := p.reverseReqRemote(w, r, &msg, bck.Bck); err != nil {
				return
			}
		}
		if err := p.destroyBucket(&msg, bck); err != nil {
			if _, ok := err.(*cmn.ErrBucketDoesNotExist); ok { // race
				glog.Infof("%s: %s already %q-ed, nothing to do", p.si, bck, msg.Action)
			} else {
				p.writeErr(w, r, err)
			}
		}
	case cmn.ActDelete, cmn.ActEvictObjects:
		var xactID string
		if msg.Action == cmn.ActEvictObjects && bck.IsAIS() {
			p.writeErrf(w, r, fmtNotRemote, bck.Name)
			return
		}

		if xactID, err = p.doListRange(http.MethodDelete, bck.Name, &msg, query); err != nil {
			p.writeErr(w, r, err)
			return
		}
		w.Write([]byte(xactID))
	default:
		p.writeErrAct(w, r, msg.Action)
	}
}

// PUT /v1/metasync
func (p *proxyrunner) metasyncHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		cmn.WriteErr405(w, r, http.MethodPut)
		return
	}
	smap := p.owner.smap.get()
	if smap.isPrimary(p.si) {
		const txt = "is primary, cannot be on the receiving side of metasync"
		if xact := xreg.GetXactRunning(cmn.ActElection); xact != nil {
			p.writeErrf(w, r, "%s: %s [%s, %s]", p.si, txt, smap, xact)
		} else {
			p.writeErrf(w, r, "%s: %s, %s", p.si, txt, smap)
		}
		return
	}
	payload := make(msPayload)
	if err := payload.unmarshal(r.Body, "metasync put"); err != nil {
		cmn.WriteErr(w, r, err)
		return
	}

	var (
		errs   []error
		caller = r.Header.Get(cmn.HeaderCallerName)
	)

	newConf, msgConf, err := p.extractConfig(payload, caller)
	if err != nil {
		errs = append(errs, err)
	} else if newConf != nil {
		if err = p.receiveConfig(newConf, msgConf, caller); err != nil && !isErrDowngrade(err) {
			errs = append(errs, err)
		}
	}

	newSmap, msgSmap, err := p.extractSmap(payload, caller)
	if err != nil {
		errs = append(errs, err)
	} else if newSmap != nil {
		if err = p.receiveSmap(newSmap, msgSmap, caller, p.smapOnUpdate); err != nil && !isErrDowngrade(err) {
			errs = append(errs, err)
		}
	}

	newRMD, msgRMD, err := p.extractRMD(payload, caller)
	if err != nil {
		errs = append(errs, err)
	} else if newRMD != nil {
		if err = p.receiveRMD(newRMD, msgRMD, caller); err != nil {
			errs = append(errs, err)
		}
	}

	newBMD, msgBMD, err := p.extractBMD(payload, caller)
	if err != nil {
		errs = append(errs, err)
	} else if newBMD != nil {
		if err = p.receiveBMD(newBMD, msgBMD, caller); err != nil && !isErrDowngrade(err) {
			errs = append(errs, err)
		}
	}

	revokedTokens, err := p.extractRevokedTokenList(payload, caller)
	if err != nil {
		errs = append(errs, err)
	} else {
		p.authn.updateRevokedList(revokedTokens)
	}

	if len(errs) > 0 {
		p.writeErrf(w, r, "%v", errs)
		return
	}
}

func (p *proxyrunner) syncNewICOwners(smap, newSmap *smapX) {
	if !smap.IsIC(p.si) || !newSmap.IsIC(p.si) {
		return
	}

	for _, psi := range newSmap.Pmap {
		if p.si.ID() != psi.ID() && newSmap.IsIC(psi) && !smap.IsIC(psi) {
			go func(psi *cluster.Snode) {
				if err := p.ic.sendOwnershipTbl(psi); err != nil {
					glog.Errorf("%s: failed to send ownership table to %s, err:%v", p.si, psi, err)
				}
			}(psi)
		}
	}
}

// GET /v1/health
func (p *proxyrunner) healthHandler(w http.ResponseWriter, r *http.Request) {
	if !p.NodeStarted() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if responded := p.healthByExternalWD(w, r); responded {
		return
	}
	// cluster info piggy-back
	getCii := cos.IsParseBool(r.URL.Query().Get(cmn.URLParamClusterInfo))
	if getCii {
		cii := &clusterInfo{}
		cii.fill(&p.httprunner)
		_ = p.writeJSON(w, r, cii, "cluster-info")
		return
	}
	smap := p.owner.smap.get()
	if !smap.isValid() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	// non-primary will keep returning 503 until cluster starts up
	if !smap.isPrimary(p.si) && !p.ClusterStarted() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// POST { action } /v1/buckets[/bucket-name]
func (p *proxyrunner) httpbckpost(w http.ResponseWriter, r *http.Request) {
	apiItems, err := p.checkRESTItems(w, r, 1, true, cmn.URLPathBuckets.L)
	if err != nil {
		return
	}

	var msg cmn.ActionMsg
	if cmn.ReadJSON(w, r, &msg) != nil {
		return
	}

	bucket := apiItems[0]
	p.hpostBucket(w, r, &msg, bucket)
}

func (p *proxyrunner) hpostBucket(w http.ResponseWriter, r *http.Request, msg *cmn.ActionMsg, bucket string) {
	query := r.URL.Query()
	bck, err := newBckFromQuery(bucket, query)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	if bck.Bck.IsRemoteAIS() {
		// forward to remote AIS as is, with a few distinct exceptions
		switch msg.Action {
		case cmn.ActInvalListCache:
			break
		default:
			p.reverseReqRemote(w, r, msg, bck.Bck)
			return
		}
	}
	if msg.Action == cmn.ActCreateBck {
		p.hpostCreateBucket(w, r, msg, bck)
		return
	}
	// only the primary can do metasync
	xactDtor := xaction.XactsDtor[msg.Action]
	if xactDtor.Metasync {
		if p.forwardCP(w, r, msg, bucket) {
			return
		}
	}

	// Initialize bucket, try creating if it's a cloud bucket.
	args := bckInitArgs{p: p, w: w, r: r, bck: bck, msg: msg, tryOnlyRem: true}
	if bck, err = args.initAndTry(bck.Name); err != nil {
		return
	}

	//
	// {action} on bucket
	//
	switch msg.Action {
	case cmn.ActMoveBck:
		bckFrom := bck
		bckTo, err := newBckFromQueryUname(query, cmn.URLParamBucketTo, true /*required*/)
		if err != nil {
			p.writeErr(w, r, err)
			return
		}
		if !bckFrom.IsAIS() && !bckFrom.HasBackendBck() {
			p.writeErrf(w, r, "cannot rename bucket %q, it is not an AIS bucket", bckFrom)
			return
		}
		if bckTo.IsRemote() {
			p.writeErrf(w, r, "destination bucket %q must be an AIS bucket", bckTo)
			return
		}
		if bckFrom.Name == bckTo.Name {
			p.writeErrf(w, r, "cannot rename bucket %q as %q", bckFrom, bckTo)
			return
		}

		bckFrom.Provider = cmn.ProviderAIS
		bckTo.Provider = cmn.ProviderAIS

		if _, present := p.owner.bmd.get().Get(bckTo); present {
			err := cmn.NewErrorBucketAlreadyExists(bckTo.Bck)
			p.writeErr(w, r, err)
			return
		}
		glog.Infof("%s bucket %s => %s", msg.Action, bckFrom, bckTo)
		var xactID string
		if xactID, err = p.renameBucket(bckFrom, bckTo, msg); err != nil {
			p.writeErr(w, r, err)
			return
		}
		w.Write([]byte(xactID))
	case cmn.ActCopyBck, cmn.ActETLBck:
		var (
			internalMsg = &cmn.Bck2BckMsg{}
			bckTo       *cluster.Bck
		)
		switch msg.Action {
		case cmn.ActETLBck:
			if err := cos.MorphMarshal(msg.Value, internalMsg); err != nil {
				p.writeErrMsg(w, r, "request body can't be empty")
				return
			}
			if err := internalMsg.Validate(); err != nil {
				p.writeErr(w, r, err)
				return
			}
		case cmn.ActCopyBck:
			cpyBckMsg := &cmn.CopyBckMsg{}
			if err = cos.MorphMarshal(msg.Value, cpyBckMsg); err != nil {
				return
			}
			internalMsg.DryRun = cpyBckMsg.DryRun
			internalMsg.Prefix = cpyBckMsg.Prefix
		}

		userBckTo, err := newBckFromQueryUname(query, cmn.URLParamBucketTo, true /*required*/)
		if err != nil {
			p.writeErr(w, r, err)
			return
		}
		if bck.Equal(userBckTo, false, true) {
			p.writeErrf(w, r, "cannot %s bucket %q onto itself", msg.Action, bucket)
			return
		}
		var (
			bckToArgs = bckInitArgs{p: p, w: w, r: r, bck: userBckTo, perms: cmn.AccessPUT}
			errCode   int
		)
		if bckTo, errCode, err = bckToArgs.init(userBckTo.Name); err != nil && errCode != http.StatusNotFound {
			p.writeErr(w, r, err, errCode)
			return
		}
		if errCode == http.StatusNotFound && userBckTo.IsCloud() {
			// If userBckTo is a cloud bucket that doesn't exist (in the BMD) - try registering on the fly
			if bckTo, err = bckToArgs.try(); err != nil {
				return
			}
		}
		if bckTo == nil {
			// It is a non existing ais bucket.
			bckTo = userBckTo
		}
		if bckTo.IsHTTP() {
			p.writeErrf(w, r, "cannot %s HTTP bucket %q - the operation is not supported",
				msg.Action, bucket)
			return
		}

		glog.Infof("%s bucket %s => %s", msg.Action, bck, bckTo)

		var xactID string
		if xactID, err = p.bucketToBucketTxn(bck, bckTo, msg, internalMsg.DryRun); err != nil {
			p.writeErr(w, r, err)
			return
		}

		w.Write([]byte(xactID))
	case cmn.ActAddRemoteBck:
		// TODO: choose the best permission
		if err := p.checkACL(r.Header, nil, cmn.AccessCreateBucket); err != nil {
			p.writeErr(w, r, err, http.StatusUnauthorized)
			return
		}
		if err := p.createBucket(msg, bck); err != nil {
			errCode := http.StatusInternalServerError
			if _, ok := err.(*cmn.ErrBucketAlreadyExists); ok {
				errCode = http.StatusConflict
			}
			p.writeErr(w, r, err, errCode)
			return
		}
	case cmn.ActPrefetch:
		// TODO: GET vs SYNC?
		if bck.IsAIS() {
			p.writeErrf(w, r, fmtNotRemote, bucket)
			return
		}
		var xactID string
		if xactID, err = p.doListRange(http.MethodPost, bucket, msg, query); err != nil {
			p.writeErr(w, r, err)
			return
		}
		w.Write([]byte(xactID))
	case cmn.ActInvalListCache:
		p.qm.c.invalidate(bck.Bck)
	case cmn.ActMakeNCopies:
		var xactID string
		if xactID, err = p.makeNCopies(msg, bck); err != nil {
			p.writeErr(w, r, err)
			return
		}
		w.Write([]byte(xactID))
	case cmn.ActECEncode:
		var xactID string
		if xactID, err = p.ecEncode(bck, msg); err != nil {
			p.writeErr(w, r, err)
			return
		}
		w.Write([]byte(xactID))
	default:
		p.writeErrAct(w, r, msg.Action)
	}
}

func (p *proxyrunner) hpostCreateBucket(w http.ResponseWriter, r *http.Request, msg *cmn.ActionMsg, bck *cluster.Bck) {
	bucket := bck.Name
	err := p.checkACL(r.Header, nil, cmn.AccessCreateBucket)
	if err != nil {
		p.writeErr(w, r, err, http.StatusUnauthorized)
		return
	}
	if err = bck.Validate(); err != nil {
		p.writeErr(w, r, err)
		return
	}
	if p.forwardCP(w, r, msg, bucket) {
		return
	}
	if bck.Provider == "" {
		bck.Provider = cmn.ProviderAIS
	}
	if bck.IsHDFS() && msg.Value == nil {
		p.writeErr(w, r,
			errors.New("property 'extra.hdfs.ref_directory' must be specified when creating HDFS bucket"))
		return
	}
	if msg.Value != nil {
		propsToUpdate := cmn.BucketPropsToUpdate{}
		if err := cos.MorphMarshal(msg.Value, &propsToUpdate); err != nil {
			p.writeErr(w, r, err)
			return
		}

		// Make and validate new bucket props.
		bck.Props = defaultBckProps(bckPropsArgs{bck: bck})
		bck.Props, err = p.makeNewBckProps(bck, &propsToUpdate, true /*creating*/)
		if err != nil {
			p.writeErr(w, r, err)
			return
		}

		if bck.HasBackendBck() {
			// Initialize backend bucket.
			backend := cluster.BackendBck(bck)
			if err = backend.InitNoBackend(p.owner.bmd); err != nil {
				if _, ok := err.(*cmn.ErrRemoteBucketDoesNotExist); !ok {
					p.writeErrf(w, r,
						"cannot create %s: failing to initialize backend %s, err: %v",
						bck, backend, err)
					return
				}
				args := bckInitArgs{p: p, w: w, r: r, bck: backend, msg: msg}
				if _, err = args.try(); err != nil {
					return
				}
			}
		}

		// Send full props to the target. Required for HDFS provider.
		msg.Value = bck.Props
	}
	if err := p.createBucket(msg, bck); err != nil {
		errCode := http.StatusInternalServerError
		if _, ok := err.(*cmn.ErrBucketAlreadyExists); ok {
			errCode = http.StatusConflict
		}
		p.writeErr(w, r, err, errCode)
	}
}

func (p *proxyrunner) listObjects(w http.ResponseWriter, r *http.Request, bck *cluster.Bck, amsg *cmn.ActionMsg,
	begin int64) {
	var (
		err     error
		bckList *cmn.BucketList
		smsg    = cmn.SelectMsg{}
		smap    = p.owner.smap.get()
	)
	if err := cos.MorphMarshal(amsg.Value, &smsg); err != nil {
		p.writeErr(w, r, err)
		return
	}
	if smap.CountActiveTargets() < 1 {
		p.writeErrMsg(w, r, "no registered targets yet")
		return
	}

	// If props were not explicitly specified always return default ones.
	if smsg.Props == "" {
		smsg.AddProps(cmn.GetPropsDefault...)
	}

	// Vanilla HTTP buckets do not support remote listing
	if bck.IsHTTP() {
		smsg.SetFlag(cmn.SelectCached)
	}

	locationIsAIS := bck.IsAIS() || smsg.IsFlagSet(cmn.SelectCached)
	if smsg.UUID == "" {
		var nl nl.NotifListener
		smsg.UUID = cos.GenUUID()
		if locationIsAIS || smsg.NeedLocalMD() {
			nl = xaction.NewXactNL(smsg.UUID,
				cmn.ActList, &smap.Smap, nil, bck.Bck)
		} else {
			// random target to execute `list-objects` on a Cloud bucket
			si, _ := smap.GetRandTarget()
			nl = xaction.NewXactNL(smsg.UUID, cmn.ActList,
				&smap.Smap, cluster.NodeMap{si.ID(): si}, bck.Bck)
		}
		nl.SetHrwOwner(&smap.Smap)
		p.ic.registerEqual(regIC{nl: nl, smap: smap, msg: amsg})
	}

	if p.ic.reverseToOwner(w, r, smsg.UUID, amsg) {
		return
	}

	if locationIsAIS {
		bckList, err = p.listObjectsAIS(bck, smsg)
	} else {
		bckList, err = p.listObjectsRemote(bck, smsg)
		// TODO: `status == http.StatusGone` At this point we know that this
		//  cloud bucket exists and is offline. We should somehow try to list
		//  cached objects. This isn't easy as we basically need to start a new
		//  xaction and return new `UUID`.
	}
	if err != nil {
		p.writeErr(w, r, err)
		return
	}

	cos.Assert(bckList != nil)

	if strings.Contains(r.Header.Get(cmn.HeaderAccept), cmn.ContentMsgPack) {
		if !p.writeMsgPack(w, r, bckList, "list_objects") {
			return
		}
	} else if !p.writeJSON(w, r, bckList, "list_objects") {
		return
	}

	delta := mono.Since(begin)
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("LIST: bck: %q, token: %q, %s", bck, bckList.ContinuationToken, delta)
	}

	// Free memory allocated for temporary slice immediately as it can take up to a few GB
	bckList.Entries = bckList.Entries[:0]
	bckList.Entries = nil
	bckList = nil

	p.statsT.AddMany(
		stats.NamedVal64{Name: stats.ListCount, Value: 1},
		stats.NamedVal64{Name: stats.ListLatency, Value: int64(delta)},
	)
}

// bucket == "": all buckets for a given provider
func (p *proxyrunner) bucketSummary(w http.ResponseWriter, r *http.Request, queryBcks cmn.QueryBcks, amsg *cmn.ActionMsg) {
	var (
		err       error
		uuid      string
		summaries cmn.BucketsSummaries
		smsg      = cmn.BucketSummaryMsg{}
	)

	if err := cos.MorphMarshal(amsg.Value, &smsg); err != nil {
		p.writeErr(w, r, err)
		return
	}

	if queryBcks.Name != "" {
		bck := cluster.NewBckEmbed(cmn.Bck(queryBcks))
		bckArgs := bckInitArgs{p: p, w: w, r: r, msg: amsg, perms: cmn.AccessBckHEAD, tryOnlyRem: true, bck: bck}
		if _, err = bckArgs.initAndTry(queryBcks.Name); err != nil {
			return
		}
	}

	id := r.URL.Query().Get(cmn.URLParamUUID)
	if id != "" {
		smsg.UUID = id
	}

	if summaries, uuid, err = p.gatherBucketSummary(queryBcks, &smsg); err != nil {
		p.writeErr(w, r, err)
		return
	}

	// uuid == "" means that async runner has completed and the result is available
	// otherwise it is an ID of a still running task
	if uuid != "" {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(uuid))
		return
	}

	p.writeJSON(w, r, summaries, "bucket_summary")
}

func (p *proxyrunner) gatherBucketSummary(bck cmn.QueryBcks, msg *cmn.BucketSummaryMsg) (
	summaries cmn.BucketsSummaries, uuid string, err error) {
	var (
		isNew, q = p.initAsyncQuery(cmn.Bck(bck), msg, cos.GenUUID())
		config   = cmn.GCO.Get()
		smap     = p.owner.smap.get()
		aisMsg   = p.newAmsgActVal(cmn.ActSummary, msg, smap)
	)
	args := allocBcastArgs()
	args.req = cmn.ReqArgs{
		Method: http.MethodGet,
		Path:   cmn.URLPathBuckets.Join(bck.Name),
		Query:  q,
		Body:   cos.MustMarshal(aisMsg),
	}
	args.smap = smap
	args.timeout = config.Timeout.MaxHostBusy + config.Timeout.MaxKeepalive
	results := p.bcastGroup(args)
	allOK, _, err := p.checkBckTaskResp(msg.UUID, results)
	if err != nil {
		return nil, "", err
	}

	// some targets are still executing their tasks, or it is the request to start
	// an async task - return only uuid
	if !allOK || isNew {
		return nil, msg.UUID, nil
	}

	// all targets are ready, prepare the final result
	q = url.Values{}
	q = cmn.AddBckToQuery(q, cmn.Bck(bck))
	q.Set(cmn.URLParamTaskAction, cmn.TaskResult)
	q.Set(cmn.URLParamSilent, "true")
	args.req.Query = q
	args.fv = func() interface{} { return &cmn.BucketsSummaries{} }
	summaries = make(cmn.BucketsSummaries, 0)
	results = p.bcastGroup(args)
	freeBcastArgs(args)
	for _, res := range results {
		if res.err != nil {
			err = res.error()
			freeCallResults(results)
			return nil, "", err
		}
		targetSummary := res.v.(*cmn.BucketsSummaries)
		for _, bckSummary := range *targetSummary {
			summaries = summaries.Aggregate(bckSummary)
		}
	}
	freeCallResults(results)
	return summaries, "", nil
}

// POST { action } /v1/objects/bucket-name[/object-name]
func (p *proxyrunner) httpobjpost(w http.ResponseWriter, r *http.Request) {
	var msg cmn.ActionMsg
	if cmn.ReadJSON(w, r, &msg) != nil {
		return
	}
	request := &apiRequest{after: 1, prefix: cmn.URLPathObjects.L}
	if msg.Action == cmn.ActRenameObject {
		request.after = 2
	}
	if err := p.parseAPIRequest(w, r, request); err != nil {
		return
	}

	// TODO: revisit versus cloud bucket not being present, see p.tryBckInit
	bck := request.bck
	if err := bck.Init(p.owner.bmd); err != nil {
		p.writeErr(w, r, err)
		return
	}
	switch msg.Action {
	case cmn.ActRenameObject:
		if err := p.checkACL(r.Header, bck, cmn.AccessObjMOVE); err != nil {
			p.writeErr(w, r, err, http.StatusUnauthorized)
			return
		}
		if bck.IsRemote() {
			p.writeErrActf(w, r, msg.Action, "not supported for remote buckets (%s)", bck)
			return
		}
		if bck.Props.EC.Enabled {
			p.writeErrActf(w, r, msg.Action, "not supported for erasure-coded  buckets (%s)", bck)
			return
		}
		p.objRename(w, r, bck, request.items[1], &msg)
		return
	case cmn.ActPromote:
		if err := p.checkACL(r.Header, bck, cmn.AccessPROMOTE); err != nil {
			p.writeErr(w, r, err, http.StatusUnauthorized)
			return
		}
		if !filepath.IsAbs(msg.Name) {
			p.writeErrMsg(w, r, "source must be an absolute path")
			return
		}
		p.promoteFQN(w, r, bck, &msg)
		return
	default:
		p.writeErrAct(w, r, msg.Action)
	}
}

// HEAD /v1/buckets/bucket-name
func (p *proxyrunner) httpbckhead(w http.ResponseWriter, r *http.Request) {
	request := apiRequest{after: 1, prefix: cmn.URLPathBuckets.L}
	if err := p.parseAPIRequest(w, r, &request); err != nil {
		return
	}

	args := bckInitArgs{p: p, w: w, r: r, tryOnlyRem: true, bck: request.bck, perms: cmn.AccessBckHEAD}
	bck, err := args.initAndTry(request.bck.Name)
	if err != nil {
		return
	}

	if bck.IsAIS() || !args.exists {
		p.bucketPropsToHdr(bck, w.Header())
		return
	}

	cloudProps, statusCode, err := p.headRemoteBck(*bck.RemoteBck(), nil)
	if err != nil {
		// TODO: what if HEAD fails
		p.writeErr(w, r, err, statusCode)
		return
	}

	if p.forwardCP(w, r, nil, "httpheadbck") {
		return
	}

	ctx := &bmdModifier{
		pre:        p._bckHeadPre,
		final:      p._syncBMDFinal,
		msg:        &cmn.ActionMsg{Action: cmn.ActResyncBprops},
		bcks:       []*cluster.Bck{bck},
		cloudProps: cloudProps,
	}
	_, err = p.owner.bmd.modify(ctx)
	if err != nil {
		debug.AssertNoErr(err)
		p.writeErr(w, r, err, http.StatusNotFound)
		return
	}
	p.bucketPropsToHdr(bck, w.Header())
}

func (p *proxyrunner) _bckHeadPre(ctx *bmdModifier, clone *bucketMD) error {
	var (
		bck             = ctx.bcks[0]
		bprops, present = clone.Get(bck)
	)
	if !present {
		return cmn.NewErrorBucketDoesNotExist(bck.Bck)
	}
	nprops := mergeRemoteBckProps(bprops, ctx.cloudProps)
	if nprops.Equal(bprops) {
		glog.Warningf("%s: Cloud bucket %s properties are already in-sync, nothing to do", p.si, bck)
		ctx.terminate = true
		return nil
	}
	clone.set(bck, nprops)
	return nil
}

// PATCH /v1/buckets/bucket-name
func (p *proxyrunner) httpbckpatch(w http.ResponseWriter, r *http.Request) {
	var (
		err           error
		propsToUpdate cmn.BucketPropsToUpdate
		msg           = &cmn.ActionMsg{Value: &propsToUpdate}
		request       = &apiRequest{after: 1, prefix: cmn.URLPathBuckets.L, msg: &msg}
	)
	if err = p.parseAPIRequest(w, r, request); err != nil {
		return
	}
	if p.forwardCP(w, r, msg, "httpbckpatch") {
		return
	}

	bck := request.bck
	perms := cmn.AccessPATCH
	if propsToUpdate.Access != nil {
		perms |= cmn.AccessBckSetACL
	}
	args := bckInitArgs{p: p, w: w, r: r, bck: bck, msg: msg, skipBackend: true, tryOnlyRem: true, perms: perms}
	if bck, err = args.initAndTry(bck.Name); err != nil {
		return
	}

	if err = p.checkAction(msg, cmn.ActSetBprops, cmn.ActResetBprops); err != nil {
		p.writeErr(w, r, err)
		return
	}

	var xactID string
	if xactID, err = p.setBucketProps(w, r, msg, bck, &propsToUpdate); err != nil {
		p.writeErr(w, r, err)
		return
	}
	w.Write([]byte(xactID))
}

// HEAD /v1/objects/bucket-name/object-name
func (p *proxyrunner) httpobjhead(w http.ResponseWriter, r *http.Request, origURLBck ...string) {
	var (
		started = time.Now()
		bckArgs = bckInitArgs{p: p, w: w, r: r, perms: cmn.AccessObjHEAD, tryOnlyRem: true}
	)

	bck, objName, err := p.parseAPIBckObj(w, r, &bckArgs, origURLBck...)
	if err != nil {
		return
	}

	smap := p.owner.smap.get()
	si, err := cluster.HrwTarget(bck.MakeUname(objName), &smap.Smap)
	if err != nil {
		p.writeErr(w, r, err, http.StatusInternalServerError)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%s %s/%s => %s", r.Method, bck.Name, objName, si)
	}
	redirectURL := p.redirectURL(r, si, started, cmn.NetworkIntraControl)
	http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
}

//============================
//
// supporting methods and misc
//
//============================
// forward control plane request to the current primary proxy
// return: forf (forwarded or failed) where forf = true means exactly that: forwarded or failed
func (p *proxyrunner) forwardCP(w http.ResponseWriter, r *http.Request, msg *cmn.ActionMsg,
	s string, origBody ...[]byte) (forf bool) {
	var (
		body []byte
		smap = p.owner.smap.get()
	)
	if !smap.isValid() {
		errmsg := fmt.Sprintf("%s must be starting up: cannot execute", p.si)
		if msg != nil {
			p.writeErrStatusf(w, r, http.StatusServiceUnavailable, "%s %s: %s", errmsg, msg.Action, s)
		} else {
			p.writeErrStatusf(w, r, http.StatusServiceUnavailable, "%s %q", errmsg, s)
		}
		return true
	}
	if p.inPrimaryTransition.Load() {
		p.writeErrStatusf(w, r, http.StatusServiceUnavailable,
			"%s is in transition, cannot process the request", p.si)
		return true
	}
	if smap.isPrimary(p.si) {
		return
	}
	// We must **not** send any request body when doing HEAD request.
	// Otherwise, the request can be rejected and terminated.
	if r.Method != http.MethodHead {
		if len(origBody) > 0 && len(origBody[0]) > 0 {
			body = origBody[0]
		} else if msg != nil {
			body = cos.MustMarshal(msg)
		}
	}
	primary := &p.rproxy.primary
	primary.Lock()
	if primary.url != smap.Primary.PublicNet.DirectURL {
		primary.url = smap.Primary.PublicNet.DirectURL
		uparsed, err := url.Parse(smap.Primary.PublicNet.DirectURL)
		cos.AssertNoErr(err)
		cfg := cmn.GCO.Get()
		primary.rp = httputil.NewSingleHostReverseProxy(uparsed)
		primary.rp.Transport = cmn.NewTransport(cmn.TransportArgs{
			UseHTTPS:   cfg.Net.HTTP.UseHTTPS,
			SkipVerify: cfg.Net.HTTP.SkipVerify,
		})
		primary.rp.ErrorHandler = p.rpErrHandler
	}
	primary.Unlock()
	if len(body) > 0 {
		debug.AssertFunc(func() bool {
			l, _ := io.Copy(ioutil.Discard, r.Body)
			return l == 0
		})

		r.Body = ioutil.NopCloser(bytes.NewBuffer(body))
		r.ContentLength = int64(len(body)) // Directly setting `Content-Length` header.
	}
	if msg != nil {
		glog.Infof(`%s: forwarding "%s:%s" to the primary %s`, p.si, msg.Action, s, smap.Primary)
	} else {
		glog.Infof("%s: forwarding %q to the primary %s", p.si, s, smap.Primary)
	}
	primary.rp.ServeHTTP(w, r)
	return true
}

// reverse-proxy request
func (p *proxyrunner) reverseNodeRequest(w http.ResponseWriter, r *http.Request, si *cluster.Snode) {
	parsedURL, err := url.Parse(si.URL(cmn.NetworkPublic))
	cos.AssertNoErr(err)
	p.reverseRequest(w, r, si.ID(), parsedURL)
}

func (p *proxyrunner) reverseRequest(w http.ResponseWriter, r *http.Request, nodeID string, parsedURL *url.URL) {
	rproxy := p.rproxy.loadOrStore(nodeID, parsedURL, p.rpErrHandler)
	rproxy.ServeHTTP(w, r)
}

func (p *proxyrunner) reverseReqRemote(w http.ResponseWriter, r *http.Request, msg *cmn.ActionMsg,
	bck cmn.Bck) (err error) {
	var (
		remoteUUID = bck.Ns.UUID
		query      = r.URL.Query()

		v, configured = cmn.GCO.Get().Backend.ProviderConf(cmn.ProviderAIS)
	)

	if !configured {
		err = errors.New("ais remote cloud is not configured")
		p.writeErr(w, r, err)
		return err
	}

	aisConf := cmn.BackendConfAIS{}
	cos.MustMorphMarshal(v, &aisConf)
	urls, exists := aisConf[remoteUUID]
	if !exists {
		err = cmn.NewNotFoundError("remote UUID/alias %q", remoteUUID)
		p.writeErr(w, r, err)
		return err
	}

	cos.Assert(len(urls) > 0)
	u, err := url.Parse(urls[0])
	if err != nil {
		p.writeErr(w, r, err)
		return err
	}
	if msg != nil {
		body := cos.MustMarshal(msg)
		r.Body = ioutil.NopCloser(bytes.NewReader(body))
	}

	bck.Ns.UUID = ""
	query = cmn.DelBckFromQuery(query)
	query = cmn.AddBckToQuery(query, bck)
	r.URL.RawQuery = query.Encode()
	p.reverseRequest(w, r, remoteUUID, u)
	return nil
}

func (p *proxyrunner) listBuckets(w http.ResponseWriter, r *http.Request, query cmn.QueryBcks, msg *cmn.ActionMsg) {
	bmd := p.owner.bmd.get()
	// HDFS doesn't support listing remote buckets (there are no remote buckets).
	if query.IsAIS() || query.IsHDFS() {
		bcks := p.selectBMDBuckets(bmd, query)
		p.writeJSON(w, r, bcks, listBuckets)
		return
	}
	si, err := p.owner.smap.get().GetRandTarget()
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	r.Body = ioutil.NopCloser(bytes.NewBuffer(cos.MustMarshal(msg)))
	p.reverseNodeRequest(w, r, si)
}

func (p *proxyrunner) redirectURL(r *http.Request, si *cluster.Snode, ts time.Time, netName string) (redirect string) {
	var (
		nodeURL string
		query   = url.Values{}
	)
	if p.si.LocalNet == nil {
		nodeURL = si.URL(cmn.NetworkPublic)
	} else {
		var local bool
		remote := r.RemoteAddr
		if colon := strings.Index(remote, ":"); colon != -1 {
			remote = remote[:colon]
		}
		if ip := net.ParseIP(remote); ip != nil {
			local = p.si.LocalNet.Contains(ip)
		}
		if local {
			nodeURL = si.URL(netName)
		} else {
			nodeURL = si.URL(cmn.NetworkPublic)
		}
	}
	redirect = nodeURL + r.URL.Path + "?"
	if r.URL.RawQuery != "" {
		redirect += r.URL.RawQuery + "&"
	}

	query.Set(cmn.URLParamProxyID, p.si.ID())
	query.Set(cmn.URLParamUnixTime, cos.UnixNano2S(ts.UnixNano()))
	redirect += query.Encode()
	return
}

func (p *proxyrunner) initAsyncQuery(bck cmn.Bck, msg *cmn.BucketSummaryMsg, newTaskID string) (bool, url.Values) {
	isNew := msg.UUID == ""
	q := url.Values{}
	if isNew {
		msg.UUID = newTaskID
		q.Set(cmn.URLParamTaskAction, cmn.TaskStart)
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Infof("proxy: starting new async task %s", msg.UUID)
		}
	} else {
		// First request is always 'Status' to avoid wasting gigabytes of
		// traffic in case when few targets have finished their tasks.
		q.Set(cmn.URLParamTaskAction, cmn.TaskStatus)
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Infof("proxy: reading async task %s result", msg.UUID)
		}
	}

	q = cmn.AddBckToQuery(q, bck)
	return isNew, q
}

func (p *proxyrunner) checkBckTaskResp(uuid string, results sliceResults) (allOK bool, status int, err error) {
	// check response codes of all targets
	// Target that has completed its async task returns 200, and 202 otherwise
	allOK = true
	allNotFound := true
	for _, res := range results {
		if res.status == http.StatusNotFound {
			continue
		}
		allNotFound = false
		if res.err != nil {
			allOK, status, err = false, res.status, res.err
			freeCallResults(results)
			return
		}
		if res.status != http.StatusOK {
			allOK = false
			status = res.status
			break
		}
	}
	freeCallResults(results)
	if allNotFound {
		err = cmn.NewNotFoundError("task %q", uuid)
	}
	return
}

// listObjectsAIS reads object list from all targets, combines, sorts and returns
// the final list. Excess of object entries from each target is remembered in the
// buffer (see: `queryBuffers`) so we won't request the same objects again.
func (p *proxyrunner) listObjectsAIS(bck *cluster.Bck, smsg cmn.SelectMsg) (allEntries *cmn.BucketList, err error) {
	var (
		aisMsg    *aisMsg
		args      *bcastArgs
		entries   []*cmn.BucketEntry
		results   sliceResults
		smap      = p.owner.smap.get()
		cacheID   = cacheReqID{bck: bck.Bck, prefix: smsg.Prefix}
		token     = smsg.ContinuationToken
		props     = smsg.PropsSet()
		hasEnough bool
		flags     uint32
	)
	if smsg.PageSize == 0 {
		smsg.PageSize = cmn.DefaultListPageSizeAIS
	}
	pageSize := smsg.PageSize

	// TODO: Before checking cache and buffer we should check if there is another
	//  request already in-flight that requests the same page as we do - if yes
	//  then we should just patiently wait for the cache to get populated.

	if smsg.UseCache {
		entries, hasEnough = p.qm.c.get(cacheID, token, pageSize)
		if hasEnough {
			goto end
		}
		// Request for all the props if (cache should always have all entries).
		smsg.AddProps(cmn.GetPropsAll...)
	}
	entries, hasEnough = p.qm.b.get(smsg.UUID, token, pageSize)
	if hasEnough {
		// We have enough in the buffer to fulfill the request.
		goto endWithCache
	}

	// User requested some page but we don't have enough (but we may have part
	// of the full page). Therefore, we must ask targets for page starting from
	// what we have locally, so we don't re-request the objects.
	smsg.ContinuationToken = p.qm.b.last(smsg.UUID, token)

	aisMsg = p.newAmsgActVal(cmn.ActList, &smsg, smap)
	args = allocBcastArgs()
	args.req = cmn.ReqArgs{
		Method: http.MethodGet,
		Path:   cmn.URLPathBuckets.Join(bck.Name),
		Query:  cmn.AddBckToQuery(nil, bck.Bck),
		Body:   cos.MustMarshal(aisMsg),
	}
	args.timeout = cmn.LongTimeout // TODO: should it be `Client.ListObjects`?
	args.smap = smap
	args.fv = func() interface{} { return &cmn.BucketList{} }

	// Combine the results.
	results = p.bcastGroup(args)
	freeBcastArgs(args)
	for _, res := range results {
		if res.err != nil {
			err = res.error()
			freeCallResults(results)
			return nil, err
		}
		objList := res.v.(*cmn.BucketList)
		flags |= objList.Flags
		p.qm.b.set(smsg.UUID, res.si.ID(), objList.Entries, pageSize)
	}
	freeCallResults(results)
	entries, hasEnough = p.qm.b.get(smsg.UUID, token, pageSize)
	cos.Assert(hasEnough)

endWithCache:
	if smsg.UseCache {
		p.qm.c.set(cacheID, token, entries, pageSize)
	}
end:
	if smsg.UseCache && !props.All(cmn.GetPropsAll...) {
		// Since cache keeps entries with whole subset props we must create copy
		// of the entries with smaller subset of props (if we would change the
		// props of the `entries` it would also affect entries inside cache).
		propsEntries := make([]*cmn.BucketEntry, len(entries))
		for idx := range entries {
			propsEntries[idx] = entries[idx].CopyWithProps(props)
		}
		entries = propsEntries
	}

	allEntries = &cmn.BucketList{
		UUID:    smsg.UUID,
		Entries: entries,
		Flags:   flags,
	}
	if uint(len(entries)) >= pageSize {
		allEntries.ContinuationToken = entries[len(entries)-1].Name
	}
	return allEntries, nil
}

// listObjectsRemote returns the list of objects from requested remote bucket
// (cloud or remote AIS). If request requires local data then it is broadcast
// to all targets which perform traverse on the disks, otherwise random target
// is chosen to perform cloud listing.
func (p *proxyrunner) listObjectsRemote(bck *cluster.Bck, smsg cmn.SelectMsg) (allEntries *cmn.BucketList, err error) {
	if smsg.StartAfter != "" {
		return nil, fmt.Errorf("start after for cloud buckets is not yet supported")
	}
	var (
		smap       = p.owner.smap.get()
		reqTimeout = cmn.GCO.Get().Client.ListObjects
		aisMsg     = p.newAmsgActVal(cmn.ActList, &smsg, smap)
		args       = allocBcastArgs()
		results    sliceResults
	)
	args.req = cmn.ReqArgs{
		Method: http.MethodGet,
		Path:   cmn.URLPathBuckets.Join(bck.Name),
		Query:  cmn.AddBckToQuery(nil, bck.Bck),
		Body:   cos.MustMarshal(aisMsg),
	}
	if smsg.NeedLocalMD() {
		args.timeout = reqTimeout
		args.smap = smap
		args.fv = func() interface{} { return &cmn.BucketList{} }
		results = p.bcastGroup(args)
	} else {
		nl, exists := p.notifs.entry(smsg.UUID)
		debug.Assert(exists) // NOTE: we register listobj xaction before starting to list
		for _, si := range nl.Notifiers() {
			res := p.call(callArgs{si: si, req: args.req, timeout: reqTimeout, v: &cmn.BucketList{}})
			results = make(sliceResults, 1)
			results[0] = res
			break
		}
	}
	freeBcastArgs(args)
	// Combine the results.
	bckLists := make([]*cmn.BucketList, 0, len(results))
	for _, res := range results {
		if res.status == http.StatusNotFound { // TODO -- FIXME
			continue
		}
		if res.err != nil {
			err = res.error()
			freeCallResults(results)
			return nil, err
		}
		bckLists = append(bckLists, res.v.(*cmn.BucketList))
	}
	freeCallResults(results)

	// Maximum objects in the final result page. Take all objects in
	// case of Cloud and no limit is set by a user.
	allEntries = cmn.MergeObjLists(bckLists, 0)
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("Objects after merge: %d, token: %q", len(allEntries.Entries), allEntries.ContinuationToken)
	}

	if smsg.WantProp(cmn.GetTargetURL) {
		for _, e := range allEntries.Entries {
			si, err := cluster.HrwTarget(bck.MakeUname(e.Name), &smap.Smap)
			if err == nil {
				e.TargetURL = si.URL(cmn.NetworkPublic)
			}
		}
	}

	return allEntries, nil
}

func (p *proxyrunner) objRename(w http.ResponseWriter, r *http.Request, bck *cluster.Bck,
	objName string, msg *cmn.ActionMsg) {
	started := time.Now()
	if objName == msg.Name {
		p.writeErrMsg(w, r, "the new and the current name are the same")
		return
	}
	smap := p.owner.smap.get()
	si, err := cluster.HrwTarget(bck.MakeUname(objName), &smap.Smap)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%q %s/%s => %s", msg.Action, bck.Name, objName, si)
	}

	// NOTE: Code 307 is the only way to http-redirect with the original JSON payload.
	redirectURL := p.redirectURL(r, si, started, cmn.NetworkIntraControl)
	http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)

	p.statsT.Add(stats.RenameCount, 1)
}

func (p *proxyrunner) promoteFQN(w http.ResponseWriter, r *http.Request, bck *cluster.Bck, msg *cmn.ActionMsg) {
	promoteArgs := cmn.ActValPromote{}
	if err := cos.MorphMarshal(msg.Value, &promoteArgs); err != nil {
		p.writeErr(w, r, err)
		return
	}
	var (
		started = time.Now()
		smap    = p.owner.smap.get()
	)
	// designated target ID
	if promoteArgs.Target != "" {
		tsi := smap.GetTarget(promoteArgs.Target)
		if tsi == nil {
			err := &errNodeNotFound{cmn.ActPromote + " failure", promoteArgs.Target, p.si, smap}
			p.writeErr(w, r, err)
			return
		}
		// NOTE: Code 307 is the only way to http-redirect with the original JSON payload.
		redirectURL := p.redirectURL(r, tsi, started, cmn.NetworkIntraControl)
		http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
		return
	}

	// all targets
	//
	// TODO -- FIXME: 2phase begin to check space, validate params, and check vs running xactions
	//
	query := cmn.AddBckToQuery(nil, bck.Bck)
	args := allocBcastArgs()
	args.req = cmn.ReqArgs{
		Method: http.MethodPost,
		Path:   cmn.URLPathObjects.Join(bck.Name),
		Body:   cos.MustMarshal(msg),
		Query:  query,
	}
	args.to = cluster.Targets
	results := p.bcastGroup(args)
	freeBcastArgs(args)
	for _, res := range results {
		if res.err == nil {
			continue
		}
		p.writeErr(w, r, res.error())
		break
	}
	freeCallResults(results)
}

func (p *proxyrunner) doListRange(method, bucket string, msg *cmn.ActionMsg,
	query url.Values) (xactID string, err error) {
	var (
		smap   = p.owner.smap.get()
		aisMsg = p.newAmsg(msg, smap, nil, cos.GenUUID())
		body   = cos.MustMarshal(aisMsg)
		path   = cmn.URLPathBuckets.Join(bucket)
	)
	nlb := xaction.NewXactNL(aisMsg.UUID, aisMsg.Action, &smap.Smap, nil)
	nlb.SetOwner(equalIC)
	p.ic.registerEqual(regIC{smap: smap, query: query, nl: nlb})
	args := allocBcastArgs()
	args.req = cmn.ReqArgs{Method: method, Path: path, Query: query, Body: body}
	args.smap = smap
	args.timeout = cmn.DefaultTimeout // TODO: use cmn.GCO.Get().Client.ListObjects
	results := p.bcastGroup(args)
	freeBcastArgs(args)
	for _, res := range results {
		if res.err == nil {
			continue
		}
		err = res.errorf("%s failed to %q List/Range", res.si, msg.Action)
		break
	}
	freeCallResults(results)
	xactID = aisMsg.UUID
	return
}

func (p *proxyrunner) reverseHandler(w http.ResponseWriter, r *http.Request) {
	apiItems, err := p.checkRESTItems(w, r, 1, false, cmn.URLPathReverse.L)
	if err != nil {
		return
	}

	// rewrite URL path (removing `cmn.Reverse`)
	r.URL.Path = cos.JoinWords(cmn.Version, apiItems[0])

	nodeID := r.Header.Get(cmn.HeaderNodeID)
	if nodeID == "" {
		p.writeErrMsg(w, r, "missing node ID")
		return
	}
	smap := p.owner.smap.get()
	si := smap.GetNode(nodeID)
	if si != nil {
		p.reverseNodeRequest(w, r, si)
		return
	}

	// Node not found but maybe we could contact it directly. This is for
	// special case where we need to contact target which is not part of the
	// cluster eg. mountpaths: when target is not part of the cluster after
	// removing all mountpaths.
	nodeURL := r.Header.Get(cmn.HeaderNodeURL)
	if nodeURL == "" {
		err = &errNodeNotFound{"cannot rproxy", nodeID, p.si, smap}
		p.writeErr(w, r, err, http.StatusNotFound)
		return
	}

	parsedURL, err := url.Parse(nodeURL)
	if err != nil {
		p.writeErrf(w, r, "%s: invalid URL %q for node %s", p.si, nodeURL, nodeID)
		return
	}

	p.reverseRequest(w, r, nodeID, parsedURL)
}

///////////////////////////
// http /daemon handlers //
///////////////////////////

// [METHOD] /v1/daemon
func (p *proxyrunner) daemonHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: mark internal calls with a special token?
	switch r.Method {
	case http.MethodGet:
		p.httpdaeget(w, r)
	case http.MethodPut:
		p.httpdaeput(w, r)
	case http.MethodDelete:
		p.httpdaedelete(w, r)
	case http.MethodPost:
		p.httpdaepost(w, r)
	default:
		cmn.WriteErr405(w, r, http.MethodDelete, http.MethodGet, http.MethodPost, http.MethodPut)
	}
}

func (p *proxyrunner) handlePendingRenamedLB(renamedBucket string) {
	ctx := &bmdModifier{
		pre:   p._pendingRnPre,
		final: p._syncBMDFinal,
		msg:   &cmn.ActionMsg{Value: cmn.ActMoveBck},
		bcks:  []*cluster.Bck{cluster.NewBck(renamedBucket, cmn.ProviderAIS, cmn.NsGlobal)},
	}
	_, err := p.owner.bmd.modify(ctx)
	debug.AssertNoErr(err)
}

func (p *proxyrunner) _pendingRnPre(ctx *bmdModifier, clone *bucketMD) error {
	var (
		bck            = ctx.bcks[0]
		props, present = clone.Get(bck)
	)
	if !present {
		ctx.terminate = true
		// Already removed via the the very first target calling here.
		return nil
	}
	if props.Renamed == "" {
		glog.Errorf("%s: renamed bucket %s: unexpected props %+v", p.si, bck.Name, *bck.Props)
		ctx.terminate = true
		return nil
	}
	clone.del(bck)
	return nil
}

func (p *proxyrunner) httpdaeget(w http.ResponseWriter, r *http.Request) {
	var (
		query = r.URL.Query()
		what  = query.Get(cmn.URLParamWhat)
	)
	switch what {
	case cmn.GetWhatBMD:
		if renamedBucket := query.Get(whatRenamedLB); renamedBucket != "" {
			p.handlePendingRenamedLB(renamedBucket)
		}
		fallthrough // fallthrough
	case cmn.GetWhatConfig, cmn.GetWhatSmapVote, cmn.GetWhatSnode:
		p.httprunner.httpdaeget(w, r)
	case cmn.GetWhatStats:
		ws := p.statsT.GetWhatStats()
		p.writeJSON(w, r, ws, what)
	case cmn.GetWhatSysInfo:
		p.writeJSON(w, r, sys.FetchSysInfo(), what)
	case cmn.GetWhatSmap:
		const max = 16
		var (
			smap  = p.owner.smap.get()
			sleep = cmn.GCO.Get().Timeout.CplaneOperation / 2
		)
		for i := 0; smap.validate() != nil && i < max; i++ {
			if !p.NodeStarted() {
				time.Sleep(sleep)
				smap = p.owner.smap.get()
				if err := smap.validate(); err != nil {
					glog.Errorf("%s is starting up, cannot return %s yet: %v", p.si, smap, err)
				}
				break
			}
			smap = p.owner.smap.get()
			time.Sleep(sleep)
		}
		if err := smap.validate(); err != nil {
			glog.Errorf("%s: startup is taking unusually long time: %s (%v)", p.si, smap, err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		p.writeJSON(w, r, smap, what)
	case cmn.GetWhatDaemonStatus:
		msg := &stats.DaemonStatus{
			Snode:       p.httprunner.si,
			SmapVersion: p.owner.smap.get().Version,
			SysInfo:     sys.FetchSysInfo(),
			Stats:       p.statsT.CoreStats(),
			DeployedOn:  deploymentType(),
			Version:     daemon.version,
			BuildTime:   daemon.buildTime,
		}

		p.writeJSON(w, r, msg, what)
	default:
		p.httprunner.httpdaeget(w, r)
	}
}

func (p *proxyrunner) httpdaeput(w http.ResponseWriter, r *http.Request) {
	apiItems, err := p.checkRESTItems(w, r, 0, true, cmn.URLPathDaemon.L)
	if err != nil {
		return
	}
	if err := p.checkACL(r.Header, nil, cmn.AccessAdmin); err != nil {
		p.writeErr(w, r, err, http.StatusUnauthorized)
		return
	}
	// urlpath-based actions
	if len(apiItems) > 0 {
		action := apiItems[0]
		p.daePathAction(w, r, action)
		return
	}
	// message-based actions
	var (
		msg   cmn.ActionMsg
		query = r.URL.Query()
	)
	if cmn.ReadJSON(w, r, &msg) != nil {
		return
	}
	switch msg.Action {
	case cmn.ActSetConfig: // setconfig #2 - via action message
		p.setDaemonConfigMsg(w, r, &msg)
	case cmn.ActResetConfig:
		if err := p.owner.config.resetDaemonConfig(); err != nil {
			p.writeErr(w, r, err)
		}
	case cmn.ActShutdown:
		smap := p.owner.smap.get()
		isPrimary := smap.isPrimary(p.si)
		if !isPrimary {
			p.Stop(errShutdown)
			return
		}
		force := cos.IsParseBool(query.Get(cmn.URLParamForce))
		if !force {
			p.writeErrf(w, r, "cannot shutdown primary %s (consider %s=true option)",
				p.si, cmn.URLParamForce)
			return
		}
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	default:
		p.writeErrAct(w, r, msg.Action)
	}
}

func (p *proxyrunner) daePathAction(w http.ResponseWriter, r *http.Request, action string) {
	switch action {
	case cmn.Proxy:
		p.daeSetPrimary(w, r)
	case cmn.SyncSmap:
		newsmap := &smapX{}
		if cmn.ReadJSON(w, r, newsmap) != nil {
			return
		}
		if err := newsmap.validate(); err != nil {
			p.writeErrf(w, r, "%s: invalid %s: %v", p.si, newsmap, err)
			return
		}
		if err := p.owner.smap.synchronize(p.si, newsmap); err != nil {
			p.writeErrf(w, r, cmn.FmtErrFailed, p.si, "sync", newsmap, err)
			return
		}
		glog.Infof("%s: %s %s done", p.si, cmn.SyncSmap, newsmap)
	case cmn.ActSetConfig: // setconfig #1 - via query parameters and "?n1=v1&n2=v2..."
		p.setDaemonConfigQuery(w, r)
	}
}

func (p *proxyrunner) httpdaedelete(w http.ResponseWriter, r *http.Request) {
	_, err := p.checkRESTItems(w, r, 0, false, cmn.URLPathDaemonUnreg.L)
	if err != nil {
		return
	}

	if glog.V(3) {
		glog.Infoln("sending unregister on proxy keepalive control channel")
	}

	_, ok, err := p.isDecommissionUnreg(w, r)
	if err != nil {
		cmn.WriteErr(w, r, err)
		return
	}
	// Stop keepaliving
	p.keepalive.send(kaUnregisterMsg)
	if ok {
		p.stopHTTPServer()
	}
}

func (p *proxyrunner) httpdaepost(w http.ResponseWriter, r *http.Request) {
	apiItems, err := p.checkRESTItems(w, r, 0, true, cmn.URLPathDaemon.L)
	if err != nil {
		return
	}
	if len(apiItems) == 0 || apiItems[0] != cmn.UserRegister {
		p.writeErrURL(w, r)
		return
	}
	p.keepalive.send(kaRegisterMsg)
	body, err := cmn.ReadBytes(r)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	caller := r.Header.Get(cmn.HeaderCallerName)
	if err := p.applyRegMeta(body, caller); err != nil {
		p.writeErr(w, r, err)
	}
}

func (p *proxyrunner) smapFromURL(baseURL string) (smap *smapX, err error) {
	var (
		req = cmn.ReqArgs{
			Method: http.MethodGet,
			Base:   baseURL,
			Path:   cmn.URLPathDaemon.S,
			Query:  url.Values{cmn.URLParamWhat: []string{cmn.GetWhatSmap}},
		}
		args = callArgs{req: req, timeout: cmn.DefaultTimeout, v: &smapX{}}
	)
	res := p.call(args)
	defer _freeCallRes(res)
	if res.err != nil {
		err = res.errorf("failed to get Smap from %s", baseURL)
		return
	}
	smap = res.v.(*smapX)
	if err := smap.validate(); err != nil {
		err = fmt.Errorf("%s: invalid %s from %s: %v", p.si, smap, baseURL, err)
		return nil, err
	}
	return
}

// forceful primary change - is used when the original primary network is down
// for a while and the remained nodes selected a new primary. After the
// original primary is back it does not attach automatically to the new primary
// and the cluster gets into split-brain mode. This request makes original
// primary connect to the new primary
func (p *proxyrunner) forcefulJoin(w http.ResponseWriter, r *http.Request, proxyID string) {
	newPrimaryURL := r.URL.Query().Get(cmn.URLParamPrimaryCandidate)
	glog.Infof("%s: force new primary %s (URL: %s)", p.si, proxyID, newPrimaryURL)

	if p.si.ID() == proxyID {
		glog.Warningf("%s is already the primary", p.si)
		return
	}
	smap := p.owner.smap.get()
	psi := smap.GetProxy(proxyID)
	if psi == nil && newPrimaryURL == "" {
		err := &errNodeNotFound{"failed to find new primary", proxyID, p.si, smap}
		p.writeErr(w, r, err, http.StatusNotFound)
		return
	}
	if newPrimaryURL == "" {
		newPrimaryURL = psi.IntraControlNet.DirectURL
	}
	if newPrimaryURL == "" {
		err := &errNodeNotFound{"failed to get new primary's direct URL", proxyID, p.si, smap}
		p.writeErr(w, r, err)
		return
	}
	newSmap, err := p.smapFromURL(newPrimaryURL)
	if err != nil {
		p.writeErr(w, r, err)
		return
	}
	primary := newSmap.Primary
	if proxyID != primary.ID() {
		p.writeErrf(w, r, "%s: proxy %s is not the primary, current %s", p.si, proxyID, newSmap.pp())
		return
	}

	p.metasyncer.becomeNonPrimary() // metasync to stop syncing and cancel all pending requests
	p.owner.smap.put(newSmap)
	res := p.registerToURL(primary.IntraControlNet.DirectURL, primary, cmn.DefaultTimeout, nil, false)
	if res.err != nil {
		p.writeErr(w, r, res.error())
	}
}

func (p *proxyrunner) daeSetPrimary(w http.ResponseWriter, r *http.Request) {
	var (
		prepare bool
		query   = r.URL.Query()
	)
	apiItems, err := p.checkRESTItems(w, r, 2, false, cmn.URLPathDaemon.L)
	if err != nil {
		return
	}
	proxyID := apiItems[1]
	force := cos.IsParseBool(query.Get(cmn.URLParamForce))
	// forceful primary change
	if force && apiItems[0] == cmn.Proxy {
		if smap := p.owner.smap.get(); !smap.isPrimary(p.si) {
			p.writeErr(w, r, newErrNotPrimary(p.si, smap))
		}
		p.forcefulJoin(w, r, proxyID)
		return
	}

	preparestr := query.Get(cmn.URLParamPrepare)
	if prepare, err = cos.ParseBool(preparestr); err != nil {
		p.writeErrf(w, r, "failed to parse %s URL parameter: %v", cmn.URLParamPrepare, err)
		return
	}
	if p.owner.smap.get().isPrimary(p.si) {
		p.writeErr(w, r,
			errors.New("expecting 'cluster' (RESTful) resource when designating primary proxy via API"))
		return
	}
	if p.si.ID() == proxyID {
		if !prepare {
			p.becomeNewPrimary("")
		}
		return
	}
	smap := p.owner.smap.get()
	psi := smap.GetProxy(proxyID)
	if psi == nil {
		err := &errNodeNotFound{"cannot set new primary", proxyID, p.si, smap}
		p.writeErr(w, r, err)
		return
	}
	if prepare {
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Info("Preparation step: do nothing")
		}
		return
	}
	ctx := &smapModifier{pre: func(_ *smapModifier, clone *smapX) error { clone.Primary = psi; return nil }}
	err = p.owner.smap.modify(ctx)
	debug.AssertNoErr(err)
}

func (p *proxyrunner) becomeNewPrimary(proxyIDToRemove string) {
	ctx := &smapModifier{
		pre:   p._becomePre,
		final: p._becomeFinal,
		sid:   proxyIDToRemove,
	}
	err := p.owner.smap.modify(ctx)
	cos.AssertNoErr(err)
}

func (p *proxyrunner) _becomePre(ctx *smapModifier, clone *smapX) error {
	if !clone.isPresent(p.si) {
		cos.Assertf(false, "%s must always be present in the %s", p.si, clone.pp())
	}

	if ctx.sid != "" && clone.containsID(ctx.sid) {
		// decision is made: going ahead to remove
		glog.Infof("%s: removing failed primary %s", p.si, ctx.sid)
		clone.delProxy(ctx.sid)

		// Remove reverse proxy entry for the node.
		p.rproxy.nodes.Delete(ctx.sid)
	}

	clone.Primary = clone.GetProxy(p.si.ID())
	clone.Version += 100
	clone.staffIC()
	return nil
}

func (p *proxyrunner) _becomeFinal(ctx *smapModifier, clone *smapX) {
	var (
		bmd   = p.owner.bmd.get()
		rmd   = p.owner.rmd.get()
		msg   = p.newAmsgStr(cmn.ActNewPrimary, clone, bmd)
		pairs = []revsPair{{clone, msg}, {bmd, msg}, {rmd, msg}}
	)

	config, err := p.ensureConfigPrimaryURL()
	if err != nil {
		glog.Error(err)
	}
	if config != nil {
		pairs = append(pairs, revsPair{config, msg})
	}
	glog.Infof("%s: distributing (%s, %s, %s) with newly elected primary (self)", p.si, clone, bmd, rmd)
	_ = p.metasyncer.sync(pairs...)
	p.syncNewICOwners(ctx.smap, clone)
}

func (p *proxyrunner) ensureConfigPrimaryURL() (config *globalConfig, err error) {
	config, err = p.owner.config.modify(&configModifier{pre: p._primaryURLPre})
	if err != nil {
		err = fmt.Errorf("failed to update primary URL, err: %v", err)
	}
	return
}

func (p *proxyrunner) _primaryURLPre(ctx *configModifier, clone *globalConfig) (updated bool, err error) {
	smap := p.owner.smap.get()
	debug.Assert(smap.isPrimary(p.si))
	if newURL := smap.Primary.URL(cmn.NetworkPublic); clone.Proxy.PrimaryURL != newURL {
		clone.Proxy.PrimaryURL = smap.Primary.URL(cmn.NetworkPublic)
		updated = true
	}
	return
}

// [METHOD] /v1/tokens
func (p *proxyrunner) tokenHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		p.httpTokenDelete(w, r)
	default:
		cmn.WriteErr405(w, r, http.MethodDelete)
	}
}

// [METHOD] /v1/dsort
func (p *proxyrunner) dsortHandler(w http.ResponseWriter, r *http.Request) {
	if !p.ClusterStartedWithRetry() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if err := p.checkACL(r.Header, nil, cmn.AccessAdmin); err != nil {
		p.writeErr(w, r, err, http.StatusUnauthorized)
		return
	}

	apiItems, err := cmn.MatchRESTItems(r.URL.Path, 0, true, cmn.URLPathdSort.L)
	if err != nil {
		p.writeErrURL(w, r)
		return
	}

	switch r.Method {
	case http.MethodPost:
		p.proxyStartSortHandler(w, r)
	case http.MethodGet:
		dsort.ProxyGetHandler(w, r)
	case http.MethodDelete:
		if len(apiItems) == 1 && apiItems[0] == cmn.Abort {
			dsort.ProxyAbortSortHandler(w, r)
		} else if len(apiItems) == 0 {
			dsort.ProxyRemoveSortHandler(w, r)
		} else {
			p.writeErrURL(w, r)
		}
	default:
		cmn.WriteErr405(w, r, http.MethodDelete, http.MethodGet, http.MethodPost)
	}
}

// http reverse-proxy handler, to handle unmodified requests
// (not to confuse with p.rproxy)
func (p *proxyrunner) httpCloudHandler(w http.ResponseWriter, r *http.Request) {
	if !p.ClusterStartedWithRetry() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.URL.Scheme == "" {
		p.writeErrMsg(w, r, "invalid protocol scheme ''")
		return
	}
	baseURL := r.URL.Scheme + "://" + r.URL.Host
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[HTTP CLOUD] RevProxy handler for: %s -> %s", baseURL, r.URL.Path)
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		// bck.IsHTTP()
		hbo := cmn.NewHTTPObj(r.URL)
		q := r.URL.Query()
		q.Set(cmn.URLParamOrigURL, r.URL.String())
		q.Set(cmn.URLParamProvider, cmn.ProviderHTTP)
		r.URL.Path = cmn.URLPathObjects.Join(hbo.Bck.Name, hbo.ObjName)
		r.URL.RawQuery = q.Encode()
		if r.Method == http.MethodGet {
			p.httpobjget(w, r, hbo.OrigURLBck)
		} else {
			p.httpobjhead(w, r, hbo.OrigURLBck)
		}
		return
	}
	p.writeErrf(w, r, "%q provider doesn't support %q", cmn.ProviderHTTP, r.Method)
}

/////////////////
// METASYNC RX //
/////////////////

func (p *proxyrunner) receiveRMD(newRMD *rebMD, msg *aisMsg, caller string) (err error) {
	glog.Infof(
		"[metasync] receive %s from %q (action: %q, uuid: %q)",
		newRMD.String(), caller, msg.Action, msg.UUID,
	)
	p.owner.rmd.Lock()
	rmd := p.owner.rmd.get()
	if newRMD.version() <= rmd.version() {
		p.owner.rmd.Unlock()
		if newRMD.version() < rmd.version() {
			err = newErrDowngrade(p.si, rmd.String(), newRMD.String())
		}
		return
	}
	p.owner.rmd.put(newRMD)
	p.owner.rmd.Unlock()

	// Register `nl` for rebalance is metasynced.
	smap := p.owner.smap.get()
	if smap.IsIC(p.si) && smap.CountActiveTargets() > 0 {
		nl := xaction.NewXactNL(xaction.RebID(newRMD.Version).String(),
			cmn.ActRebalance, &smap.Smap, nil)
		nl.SetOwner(equalIC)
		err := p.notifs.add(nl)
		cos.AssertNoErr(err)

		if newRMD.Resilver != "" {
			nl = xaction.NewXactNL(newRMD.Resilver, cmn.ActResilver, &smap.Smap, nil)
			nl.SetOwner(equalIC)
			err := p.notifs.add(nl)
			cos.AssertNoErr(err)
		}
	}
	return
}

func (p *proxyrunner) smapOnUpdate(newSmap, oldSmap *smapX) {
	// When some node was removed from the cluster we need to clean up the
	// reverse proxy structure.
	p.rproxy.nodes.Range(func(key, _ interface{}) bool {
		nodeID := key.(string)
		if oldSmap.containsID(nodeID) && !newSmap.containsID(nodeID) {
			p.rproxy.nodes.Delete(nodeID)
		}
		return true
	})
	p.syncNewICOwners(oldSmap, newSmap)
}

func (p *proxyrunner) receiveBMD(newBMD *bucketMD, msg *aisMsg, caller string) (err error) {
	glog.Infof(
		"[metasync] receive %s from %q (action: %q, uuid: %q)",
		newBMD.String(), caller, msg.Action, msg.UUID,
	)

	p.owner.bmd.Lock()
	bmd := p.owner.bmd.get()
	if err = bmd.validateUUID(newBMD, p.si, nil, caller); err != nil {
		cos.Assert(!p.owner.smap.get().isPrimary(p.si))
		// cluster integrity error: making exception for non-primary proxies
		glog.Errorf("%s (non-primary): %v - proceeding to override BMD", p.si, err)
	} else if newBMD.version() <= bmd.version() {
		p.owner.bmd.Unlock()
		return newErrDowngrade(p.si, bmd.String(), newBMD.String())
	}
	err = p.owner.bmd.put(newBMD)
	debug.AssertNoErr(err)
	p.owner.bmd.Unlock()
	return
}

// detectDaemonDuplicate queries osi for its daemon info in order to determine if info has changed
// and is equal to nsi
func (p *proxyrunner) detectDaemonDuplicate(osi, nsi *cluster.Snode) (bool, error) {
	si, err := p.getDaemonInfo(osi)
	if err != nil {
		return false, err
	}
	return !nsi.Equals(si), nil
}

// getDaemonInfo queries osi for its daemon info and returns it.
func (p *proxyrunner) getDaemonInfo(osi *cluster.Snode) (si *cluster.Snode, err error) {
	var (
		args = callArgs{
			si: osi,
			req: cmn.ReqArgs{
				Method: http.MethodGet,
				Path:   cmn.URLPathDaemon.S,
				Query:  url.Values{cmn.URLParamWhat: []string{cmn.GetWhatSnode}},
			},
			timeout: cmn.GCO.Get().Timeout.CplaneOperation,
			v:       &cluster.Snode{},
		}
		res = p.call(args)
	)
	defer _freeCallRes(res)
	if res.err != nil {
		return nil, res.err
	}
	return res.v.(*cluster.Snode), nil
}

func (p *proxyrunner) headRemoteBck(bck cmn.Bck, q url.Values) (header http.Header, statusCode int, err error) {
	var (
		tsi  *cluster.Snode
		path = cmn.URLPathBuckets.Join(bck.Name)
	)
	if tsi, err = p.owner.smap.get().GetRandTarget(); err != nil {
		return
	}
	q = cmn.AddBckToQuery(q, bck)

	req := cmn.ReqArgs{Method: http.MethodHead, Base: tsi.URL(cmn.NetworkIntraData), Path: path, Query: q}
	res := p.call(callArgs{si: tsi, req: req, timeout: cmn.DefaultTimeout})
	defer _freeCallRes(res)
	if res.status == http.StatusNotFound {
		err = cmn.NewErrorRemoteBucketDoesNotExist(bck)
	} else if res.status == http.StatusGone {
		err = cmn.NewErrorRemoteBucketOffline(bck)
	} else {
		err = res.err
		header = res.header
	}
	statusCode = res.status
	return
}

//////////////////
// reverseProxy //
//////////////////

func (rp *reverseProxy) init() {
	cfg := cmn.GCO.Get()
	rp.cloud = &httputil.ReverseProxy{
		Director: func(r *http.Request) {},
		Transport: cmn.NewTransport(cmn.TransportArgs{
			UseHTTPS:   cfg.Net.HTTP.UseHTTPS,
			SkipVerify: cfg.Net.HTTP.SkipVerify,
		}),
	}
}

func (rp *reverseProxy) loadOrStore(uuid string, u *url.URL, errHdlr func(w http.ResponseWriter, r *http.Request, err error)) *httputil.ReverseProxy {
	revProxyIf, exists := rp.nodes.Load(uuid)
	if exists {
		shrp := revProxyIf.(*singleRProxy)
		if shrp.u.Host == u.Host {
			return shrp.rp
		}
	}

	cfg := cmn.GCO.Get()
	rproxy := httputil.NewSingleHostReverseProxy(u)
	rproxy.Transport = cmn.NewTransport(cmn.TransportArgs{
		UseHTTPS:   cfg.Net.HTTP.UseHTTPS,
		SkipVerify: cfg.Net.HTTP.SkipVerify,
	})
	rproxy.ErrorHandler = errHdlr
	// NOTE: races are rare probably happen only when storing an entry for the first time or when URL changes.
	// Also, races don't impact the correctness as we always have latest entry for `uuid`, `URL` pair (see: L3917).
	rp.nodes.Store(uuid, &singleRProxy{rproxy, u})
	return rproxy
}

////////////////
// misc utils //
////////////////

func resolveUUIDBMD(bmds bmds) (*bucketMD, error) {
	var (
		mlist = make(map[string][]nodeRegMeta) // uuid => list(targetRegMeta)
		maxor = make(map[string]*bucketMD)     // uuid => max-ver BMD
	)
	// results => (mlist, maxor)
	for si, bmd := range bmds {
		if bmd.Version == 0 {
			continue
		}
		mlist[bmd.UUID] = append(mlist[bmd.UUID], nodeRegMeta{BMD: bmd, SI: si})

		if rbmd, ok := maxor[bmd.UUID]; !ok {
			maxor[bmd.UUID] = bmd
		} else if rbmd.Version < bmd.Version {
			maxor[bmd.UUID] = bmd
		}
	}
	cos.Assert(len(maxor) == len(mlist)) // TODO: remove
	if len(maxor) == 0 {
		return nil, errNoBMD
	}
	// by simple majority
	uuid, l := "", 0
	for u, lst := range mlist {
		if l < len(lst) {
			uuid, l = u, len(lst)
		}
	}
	for u, lst := range mlist {
		if l == len(lst) && u != uuid {
			s := fmt.Sprintf("%s: BMDs have different UUIDs with no simple majority:\n%v",
				ciError(60), mlist)
			return nil, &errBmdUUIDSplit{s}
		}
	}
	var err error
	if len(mlist) > 1 {
		s := fmt.Sprintf("%s: BMDs have different UUIDs with simple majority: %s:\n%v",
			ciError(70), uuid, mlist)
		err = &errTgtBmdUUIDDiffer{s}
	}
	bmd := maxor[uuid]
	cos.Assert(bmd.UUID != "")
	return bmd, err
}

func ciError(num int) string {
	const s = "[%s%d - for details, see %s/blob/master/docs/troubleshooting.md]"
	return fmt.Sprintf(s, ciePrefix, num, cmn.GithubHome)
}
