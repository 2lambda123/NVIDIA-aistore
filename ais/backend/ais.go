// Package backend contains implementation of various backend providers.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package backend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/fs"
)

type (
	remAISCluster struct {
		url  string
		smap *cluster.Smap
		m    *AISBackendProvider

		uuid string
		bp   api.BaseParams
	}
	AISBackendProvider struct {
		t      cluster.Target
		mu     *sync.RWMutex
		remote map[string]*remAISCluster // by UUID:  1 to 1
		alias  map[string]string         // by alias: many to 1 UUID
	}
)

// interface guard
var _ cluster.BackendProvider = (*AISBackendProvider)(nil)

// TODO: house-keep refreshing remote Smap
// TODO: utilize m.remote[uuid].smap to load balance and retry disconnects

func NewAIS(t cluster.Target) *AISBackendProvider {
	return &AISBackendProvider{
		t:      t,
		mu:     &sync.RWMutex{},
		remote: make(map[string]*remAISCluster),
		alias:  make(map[string]string),
	}
}

func (r *remAISCluster) String() string {
	var aliases []string
	for alias, uuid := range r.m.alias {
		if uuid == r.smap.UUID {
			aliases = append(aliases, alias)
		}
	}
	return fmt.Sprintf("remote cluster (url: %s, aliases: %q, uuid: %v, smap: %s)", r.url, aliases, r.smap.UUID, r.smap)
}

// NOTE: this and the next method are part of the of the *extended* AIS cloud API
//       in addition to the basic GetObj, et al.

// apply new or updated (attach, detach) cmn.BackendConfAIS configuration
func (m *AISBackendProvider) Apply(v interface{}, action string) error {
	var (
		cfg         = cmn.GCO.Get()
		clusterConf = cmn.BackendConfAIS{}
		err         = cos.MorphMarshal(v, &clusterConf)
	)
	if err != nil {
		return fmt.Errorf("invalid ais cloud config (%+v, %T), err: %v", v, v, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// detach
	if action == cmn.ActDetach {
		for alias, uuid := range m.alias {
			if _, ok := clusterConf[alias]; !ok {
				if _, ok = clusterConf[uuid]; !ok {
					delete(m.alias, alias)
					delete(m.remote, uuid)
				}
			}
		}
		return nil
	}

	// init and attach
	for alias, clusterURLs := range clusterConf {
		remAis := &remAISCluster{}
		if offline, err := remAis.init(alias, clusterURLs, cfg); err != nil { // and check connectivity
			if offline {
				continue
			}
			return err
		}
		if err := m.add(remAis, alias); err != nil {
			return err
		}
	}
	return nil
}

// At the same time a cluster may have registered both types of remote AIS
// clusters(HTTP and HTTPS). So, the method must use both kind of clients and
// select the correct one at the moment it sends a request.
func (m *AISBackendProvider) GetInfo(clusterConf cmn.BackendConfAIS) (cia cmn.BackendInfoAIS) {
	var (
		cfg         = cmn.GCO.Get()
		httpClient  = cmn.NewClient(cmn.TransportArgs{Timeout: cfg.Client.Timeout.D()})
		httpsClient = cmn.NewClient(cmn.TransportArgs{
			Timeout:    cfg.Client.Timeout.D(),
			UseHTTPS:   true,
			SkipVerify: cfg.Net.HTTP.SkipVerify,
		})
	)
	cia = make(cmn.BackendInfoAIS, len(m.remote))
	m.mu.RLock()
	defer m.mu.RUnlock()
	for uuid, remAis := range m.remote {
		var (
			aliases []string
			info    = &cmn.RemoteAISInfo{}
		)
		client := httpClient
		if cos.IsHTTPS(remAis.url) {
			client = httpsClient
		}
		info.URL = remAis.url
		for a, u := range m.alias {
			if uuid == u {
				aliases = append(aliases, a)
			}
		}
		if len(aliases) == 1 {
			info.Alias = aliases[0]
		} else if len(aliases) > 1 {
			info.Alias = fmt.Sprintf("%v", aliases)
		}
		// online?
		if smap, err := api.GetClusterMap(api.BaseParams{Client: client, URL: remAis.url}); err == nil {
			if smap.UUID != uuid {
				glog.Errorf("%s: unexpected (or changed) uuid %q", remAis, smap.UUID)
				continue
			}
			info.Online = true
			if smap.Version < remAis.smap.Version {
				glog.Errorf("%s: detected older Smap %s - proceeding to override anyway", remAis, smap)
			}
			remAis.smap = smap
		}
		info.Primary = remAis.smap.Primary.String()
		info.Smap = remAis.smap.Version
		info.Targets = int32(remAis.smap.CountActiveTargets())
		cia[uuid] = info
	}
	// defunct
	for alias, clusterURLs := range clusterConf {
		if _, ok := m.alias[alias]; !ok {
			if _, ok = m.remote[alias]; !ok {
				info := &cmn.RemoteAISInfo{}
				info.URL = fmt.Sprintf("%v", clusterURLs)
				cia[alias] = info
			}
		}
	}
	return
}

// A list of remote AIS URLs can contains both HTTP and HTTPS links at the
// same time. So, the method must use both kind of clients and select the
// correct one at the moment it sends a request. First successful request
// saves the good client for the future usage.
func (r *remAISCluster) init(alias string, confURLs []string, cfg *cmn.Config) (offline bool, err error) {
	var (
		url           string
		remSmap, smap *cluster.Smap
		httpClient    = cmn.NewClient(cmn.TransportArgs{Timeout: cfg.Client.Timeout.D()})
		httpsClient   = cmn.NewClient(cmn.TransportArgs{
			Timeout:    cfg.Client.Timeout.D(),
			UseHTTPS:   true,
			SkipVerify: cfg.Net.HTTP.SkipVerify,
		})
	)
	for _, u := range confURLs {
		client := httpClient
		if cos.IsHTTPS(u) {
			client = httpsClient
		}
		if smap, err = api.GetClusterMap(api.BaseParams{Client: client, URL: u}); err != nil {
			glog.Warningf("remote cluster failing to reach %q via %s, err: %v", alias, u, err)
			continue
		}
		if remSmap == nil {
			remSmap, url = smap, u
			continue
		}
		if remSmap.UUID != smap.UUID {
			err = fmt.Errorf("%q(%v) references two different clusters: uuid=%q vs uuid=%q",
				alias, confURLs, remSmap.UUID, smap.UUID)
			return
		}
		if remSmap.Version < smap.Version {
			remSmap, url = smap, u
		}
	}
	if remSmap == nil {
		err = fmt.Errorf("remote cluster failed to reach %q via any/all of the configured URLs %v", alias, confURLs)
		offline = true
		return
	}
	r.smap, r.url = remSmap, url
	if cos.IsHTTPS(url) {
		r.bp = api.BaseParams{Client: httpsClient, URL: url}
	} else {
		r.bp = api.BaseParams{Client: httpClient, URL: url}
	}
	r.uuid = remSmap.UUID
	return
}

// NOTE: supporting remote attachments both by alias and by UUID interchangeably,
//       with mappings: 1(uuid) to 1(cluster) and 1(alias) to 1(cluster)
func (m *AISBackendProvider) add(newAis *remAISCluster, newAlias string) (err error) {
	if remAis, ok := m.remote[newAlias]; ok {
		return fmt.Errorf("cannot attach %s: alias %q is already in use as uuid for %s",
			newAlias, newAlias, remAis)
	}
	newAis.m = m
	tag := "added"
	if newAlias == newAis.smap.UUID {
		// not an alias
		goto ad
	}
	// existing
	if remAis, ok := m.remote[newAis.smap.UUID]; ok {
		// can re-alias existing remote cluster
		for alias, uuid := range m.alias {
			if uuid == newAis.smap.UUID {
				delete(m.alias, alias)
			}
		}
		m.alias[newAlias] = newAis.smap.UUID // alias
		if newAis.url != remAis.url {
			glog.Warningf("%s: different new URL %s - overriding", remAis, newAis)
		}
		if newAis.smap.Version < remAis.smap.Version {
			glog.Errorf("%s: detected older Smap %s - proceeding to override anyway", remAis, newAis)
		}
		tag = "updated"
		goto ad
	}
	if uuid, ok := m.alias[newAlias]; ok {
		remAis, ok := m.remote[uuid]
		if !ok {
			delete(m.alias, newAlias)
		} else {
			return fmt.Errorf("cannot attach %s: alias %q is already in use for %s", newAis, newAlias, remAis)
		}
	}
	m.alias[newAlias] = newAis.smap.UUID
ad:
	m.remote[newAis.smap.UUID] = newAis
	glog.Infof("%s %s", newAis, tag)
	return
}

func (m *AISBackendProvider) remoteCluster(uuid string) (*remAISCluster, error) {
	m.mu.RLock()
	remAis, ok := m.remote[uuid]
	if !ok {
		// double take (see "for user convenience" above)
		orig := uuid
		if uuid, ok = m.alias[uuid /*alias?*/]; !ok {
			m.mu.RUnlock()
			return nil, cmn.NewNotFoundError("UUID or alias of a remote cluster %q", orig)
		}
		remAis, ok = m.remote[uuid]
		cos.Assert(ok)
	}
	m.mu.RUnlock()
	return remAis, nil
}

func prepareBck(bck cmn.Bck) cmn.Bck {
	bck.Ns.UUID = ""
	return bck
}

func extractErrCode(e error) (int, error) {
	if e == nil {
		return http.StatusOK, nil
	}
	if httpErr := cmn.Err2HTTPErr(e); httpErr != nil {
		return httpErr.Status, httpErr
	}
	return http.StatusInternalServerError, e
}

/////////////////////
// BackendProvider //
/////////////////////

func (m *AISBackendProvider) Provider() string  { return cmn.ProviderAIS }
func (m *AISBackendProvider) MaxPageSize() uint { return cmn.DefaultListPageSizeAIS }

func (m *AISBackendProvider) CreateBucket(_ context.Context, _ *cluster.Bck) (errCode int, err error) {
	cos.Assert(false) // Bucket creation happens only with reverse proxy to AIS cluster.
	return 0, nil
}

func (m *AISBackendProvider) HeadBucket(_ context.Context, remoteBck *cluster.Bck) (bckProps cos.SimpleKVs, errCode int, err error) {
	debug.Assert(remoteBck.Provider == cmn.ProviderAIS)

	aisCluster, err := m.remoteCluster(remoteBck.Ns.UUID)
	if err != nil {
		return nil, errCode, err
	}
	bck := prepareBck(remoteBck.Bck)
	p, err := api.HeadBucket(aisCluster.bp, bck)
	if err != nil {
		errCode, err = extractErrCode(err)
		return bckProps, errCode, err
	}
	bckProps = make(cos.SimpleKVs)
	err = cmn.IterFields(p, func(uniqueTag string, field cmn.IterField) (e error, b bool) {
		bckProps[uniqueTag] = fmt.Sprintf("%v", field.Value())
		return nil, false
	})
	debug.AssertNoErr(err)
	return bckProps, 0, nil
}

func (m *AISBackendProvider) ListObjects(_ context.Context, remoteBck *cluster.Bck, msg *cmn.SelectMsg) (bckList *cmn.BucketList, errCode int, err error) {
	debug.Assert(remoteBck.Provider == cmn.ProviderAIS)

	aisCluster, err := m.remoteCluster(remoteBck.Ns.UUID)
	if err != nil {
		return nil, errCode, err
	}

	remoteMsg := msg.Clone()
	remoteMsg.PageSize = calcPageSize(remoteMsg.PageSize, m.MaxPageSize())

	// TODO: Currently we cannot remember the `UUID` from remote cluster and
	//  embed it into `ContinuationToken`. The problem is that when local data
	//  is needed then all targets list cloud objects and currently we don't
	//  support listing objects (AIS bucket) with same `UUID` from multiple clients.
	//
	// Clearing `remoteMsg.UUID` is necessary otherwise the remote cluster
	// will think that it already knows this UUID and problems will arise.
	remoteMsg.UUID = ""

	bck := prepareBck(remoteBck.Bck)
	bckList, err = api.ListObjectsPage(aisCluster.bp, bck, remoteMsg)
	if err != nil {
		errCode, err = extractErrCode(err)
		return bckList, errCode, err
	}
	// Set original UUID of the request (UUID of remote cluster is already
	// embedded into `ContinuationToken`).
	bckList.UUID = msg.UUID
	return bckList, 0, nil
}

func (m *AISBackendProvider) listBucketsCluster(uuid string, query cmn.QueryBcks) (buckets cmn.BucketNames, err error) {
	var (
		aisCluster  *remAISCluster
		remoteQuery = cmn.QueryBcks{Provider: cmn.ProviderAIS, Ns: cmn.Ns{Name: query.Ns.Name}}
	)
	if aisCluster, err = m.remoteCluster(uuid); err != nil {
		return
	}
	buckets, err = api.ListBuckets(aisCluster.bp, remoteQuery)
	if err != nil {
		_, err = extractErrCode(err)
		return nil, err
	}
	for i, bck := range buckets {
		bck.Ns.UUID = uuid // if `uuid` is alias we need to preserve it
		buckets[i] = bck
	}
	return buckets, nil
}

func (m *AISBackendProvider) ListBuckets(_ context.Context, query cmn.QueryBcks) (buckets cmn.BucketNames, errCode int, err error) {
	if !query.Ns.IsAnyRemote() {
		buckets, err = m.listBucketsCluster(query.Ns.UUID, query)
	} else {
		for uuid := range m.remote {
			remoteBcks, tryErr := m.listBucketsCluster(uuid, query)
			buckets = append(buckets, remoteBcks...)
			if tryErr != nil {
				err = tryErr
			}
		}
	}
	errCode, err = extractErrCode(err)
	return
}

func (m *AISBackendProvider) HeadObj(_ context.Context, lom *cluster.LOM) (objHdrMeta cos.SimpleKVs, errCode int, err error) {
	remoteBck := lom.Bucket()
	aisCluster, err := m.remoteCluster(remoteBck.Ns.UUID)
	if err != nil {
		return nil, errCode, err
	}
	bck := prepareBck(remoteBck)
	p, err := api.HeadObject(aisCluster.bp, bck, lom.ObjName)
	if err != nil {
		errCode, err = extractErrCode(err)
		return nil, errCode, err
	}
	objHdrMeta = make(cos.SimpleKVs)
	err = cmn.IterFields(p, func(tag string, field cmn.IterField) (e error, b bool) {
		headerName := cmn.PropToHeader(tag)
		objHdrMeta[headerName] = fmt.Sprintf("%v", field.Value())
		return nil, false
	})
	debug.AssertNoErr(err)
	return objHdrMeta, 0, nil
}

func (m *AISBackendProvider) GetObj(_ context.Context, lom *cluster.LOM) (errCode int, err error) {
	remoteBck := lom.Bucket()
	aisCluster, err := m.remoteCluster(remoteBck.Ns.UUID)
	if err != nil {
		return errCode, err
	}

	bck := prepareBck(remoteBck)
	r, err := api.GetObjectReader(aisCluster.bp, bck, lom.ObjName)
	if err != nil {
		return extractErrCode(err)
	}

	params := cluster.PutObjectParams{
		Tag:      fs.WorkfileColdget,
		Reader:   r,
		RecvType: cluster.ColdGet,
	}
	err = m.t.PutObject(lom, params)
	return extractErrCode(err)
}

func (m *AISBackendProvider) GetObjReader(_ context.Context, lom *cluster.LOM) (r io.ReadCloser, expectedCksm *cos.Cksum,
	errCode int, err error) {
	remoteBck := lom.Bucket()
	aisCluster, err := m.remoteCluster(remoteBck.Ns.UUID)
	if err != nil {
		return nil, nil, errCode, err
	}

	r, err = api.GetObjectReader(aisCluster.bp, remoteBck, lom.ObjName)
	errCode, err = extractErrCode(err)
	return r, nil, errCode, err
}

func (m *AISBackendProvider) PutObj(ctx context.Context, r io.ReadCloser, lom *cluster.LOM) (version string, errCode int, err error) {
	remoteBck := lom.Bucket()
	aisCluster, err := m.remoteCluster(remoteBck.Ns.UUID)
	if err != nil {
		cos.Close(r)
		return "", errCode, err
	}
	var (
		bck  = prepareBck(lom.Bucket())
		args = api.PutObjectArgs{
			BaseParams: aisCluster.bp,
			Bck:        bck,
			Object:     lom.ObjName,
			Cksum:      lom.Cksum(),
			Reader:     r.(cos.ReadOpenCloser),
			Size:       uint64(lom.Size(true)), // It's special because it's still workfile.
		}
	)
	if err = api.PutObject(args); err != nil {
		errCode, err = extractErrCode(err)
		return "", errCode, err
	}
	// NOTE: the caller is expected to load it and get the current version, if exists
	return lom.Version(true), 0, nil
}

func (m *AISBackendProvider) DeleteObj(_ context.Context, lom *cluster.LOM) (errCode int, err error) {
	remoteBck := lom.Bucket()
	aisCluster, err := m.remoteCluster(remoteBck.Ns.UUID)
	if err != nil {
		return errCode, err
	}
	bck := prepareBck(remoteBck)
	err = api.DeleteObject(aisCluster.bp, bck, lom.ObjName)
	return extractErrCode(err)
}
