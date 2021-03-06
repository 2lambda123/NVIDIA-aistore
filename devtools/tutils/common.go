// Package tutils provides common low-level utilities for all aistore unit and integration tests
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package tutils

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/containers"
	"github.com/NVIDIA/aistore/devtools"
	"github.com/NVIDIA/aistore/devtools/readers"
	"github.com/NVIDIA/aistore/devtools/tassert"
)

// TODO: remove hardcoding, should be same as `proxyIDFname` from `ais` package.
const proxyIDFname = ".ais.proxy_id"

type SkipTestArgs struct {
	Bck                   cmn.Bck
	RequiredDeployment    ClusterType
	MinTargets            int
	MinProxies            int
	MinMountpaths         int
	RequiresRemoteCluster bool
	RequiresAuth          bool
	Long                  bool
	RemoteBck             bool
	CloudBck              bool
	K8s                   bool
	Local                 bool
}

// Generates an object name that hashes to a different target than `baseName`.
func GenerateNotConflictingObjectName(baseName, newNamePrefix string, bck cmn.Bck, smap *cluster.Smap) string {
	// Init digests - HrwTarget() requires it
	smap.InitDigests()

	newName := newNamePrefix

	cbck := cluster.NewBckEmbed(bck)
	baseNameHrw, _ := cluster.HrwTarget(cbck.MakeUname(baseName), smap)
	newNameHrw, _ := cluster.HrwTarget(cbck.MakeUname(newName), smap)

	for i := 0; baseNameHrw == newNameHrw; i++ {
		newName = newNamePrefix + strconv.Itoa(i)
		newNameHrw, _ = cluster.HrwTarget(cbck.MakeUname(newName), smap)
	}
	return newName
}

func GenerateNonexistentBucketName(prefix string, baseParams api.BaseParams) (string, error) {
	for i := 0; i < 100; i++ {
		bck := cmn.Bck{
			Name:     prefix + cos.RandString(8),
			Provider: cmn.ProviderAIS,
		}
		_, err := api.HeadBucket(baseParams, bck)
		if err == nil {
			continue
		}
		errHTTP, ok := err.(*cmn.ErrHTTP)
		if !ok {
			return "", fmt.Errorf("error generating bucket name: expected error of type *cmn.ErrHTTP, but got: %T", err)
		}
		if errHTTP.Status == http.StatusNotFound {
			return bck.Name, nil
		}

		return "", fmt.Errorf("error generating bucket name: unexpected HEAD request error: %v", err)
	}

	return "", errors.New("error generating bucket name: too many tries gave no result")
}

func CheckSkip(tb testing.TB, args SkipTestArgs) {
	var smap *cluster.Smap
	if args.RequiresRemoteCluster && RemoteCluster.UUID == "" {
		tb.Skipf("%s requires remote cluster", tb.Name())
	}
	if args.RequiresAuth && AuthToken == "" {
		tb.Skipf("%s requires authentication token", tb.Name())
	}
	if args.Long && testing.Short() {
		tb.Skipf("skipping %s in short mode", tb.Name())
	}
	if args.RemoteBck {
		proxyURL := GetPrimaryURL()
		if !isRemoteBucket(tb, proxyURL, args.Bck) {
			tb.Skipf("%s requires a remote bucket", tb.Name())
		}
	}
	if args.CloudBck {
		proxyURL := GetPrimaryURL()
		if !isCloudBucket(tb, proxyURL, args.Bck) {
			tb.Skipf("%s requires a cloud bucket", tb.Name())
		}
	}

	switch args.RequiredDeployment {
	case ClusterTypeK8s:
		// NOTE: The test suite doesn't have to be deployed on K8s, the cluster has to be.
		isK8s, err := isClusterK8s()
		if err != nil {
			tb.Fatalf("Unrecognized error upon checking K8s deployment; err: %v", err)
		}
		if !isK8s {
			tb.Skipf("%s requires Kubernetes", tb.Name())
		}
	case ClusterTypeLocal:
		isLocal, err := isClusterLocal()
		tassert.CheckFatal(tb, err)
		if !isLocal {
			tb.Skipf("%s requires local deployment", tb.Name())
		}
	case ClusterTypeDocker:
		if !containers.DockerRunning() {
			tb.Skipf("%s requires docker deployment", tb.Name())
		}
	}

	if args.MinTargets > 0 || args.MinMountpaths > 0 || args.MinProxies > 0 {
		smap = GetClusterMap(tb, GetPrimaryURL())
	}

	if args.MinTargets > 0 {
		if smap.CountTargets() < args.MinTargets {
			tb.Skipf("%s requires at least %d targets (have %d)",
				tb.Name(), args.MinTargets, smap.CountTargets())
		}
	}

	if args.MinProxies > 0 {
		if smap.CountProxies() < args.MinProxies {
			tb.Skipf("%s requires at least %d proxies (have %d)",
				tb.Name(), args.MinProxies, smap.CountProxies())
		}
	}

	if args.MinMountpaths > 0 {
		targets := smap.Tmap.ActiveNodes()
		proxyURL := GetPrimaryURL()
		baseParams := BaseAPIParams(proxyURL)
		mpList, _ := api.GetMountpaths(baseParams, targets[0])
		if l := len(mpList.Available); l < args.MinMountpaths {
			tb.Skipf("%s requires at least %d mountpaths (have %d)", tb.Name(), args.MinMountpaths, l)
		}
	}
}

func BckExists(tb testing.TB, proxyURL string, bck cmn.Bck) bool {
	exists, err := devtools.BckExists(DevtoolsCtx, proxyURL, bck)
	tassert.CheckFatal(tb, err)
	return exists
}

func isRemoteBucket(tb testing.TB, proxyURL string, bck cmn.Bck) bool {
	if !bck.IsRemote() {
		return false
	}
	return BckExists(tb, proxyURL, bck)
}

func isCloudBucket(tb testing.TB, proxyURL string, bck cmn.Bck) bool {
	if !bck.IsCloud() {
		return false
	}
	return BckExists(tb, proxyURL, bck)
}

func PutObjRR(baseParams api.BaseParams, bck cmn.Bck, objName string, objSize int64, cksumType string) error {
	reader, err := readers.NewRandReader(objSize, cksumType)
	if err != nil {
		return err
	}
	putArgs := api.PutObjectArgs{
		BaseParams: baseParams,
		Bck:        bck,
		Object:     objName,
		Cksum:      reader.Cksum(),
		Reader:     reader,
	}
	return api.PutObject(putArgs)
}

func PutRR(tb testing.TB, baseParams api.BaseParams, objSize int64, cksumType string,
	bck cmn.Bck, dir string, objCount int) []string {
	objNames := make([]string, objCount)
	for i := 0; i < objCount; i++ {
		fname := cos.RandString(20)
		objName := filepath.Join(dir, fname)
		objNames[i] = objName
		// FIXME: Separate RandReader per object created inside PutObjRR to workaround
		// https://github.com/golang/go/issues/30597
		err := PutObjRR(baseParams, bck, objName, objSize, cksumType)
		tassert.CheckFatal(tb, err)
	}

	return objNames
}

func NewClientWithProxy(proxyURL string) *http.Client {
	transport := cmn.NewTransport(transportArgs)
	prxURL, _ := url.Parse(proxyURL)
	transport.Proxy = http.ProxyURL(prxURL)
	return &http.Client{
		Transport: transport,
		Timeout:   transportArgs.Timeout,
	}
}

func isClusterK8s() (isK8s bool, err error) {
	// NOTE: The test suite doesn't have to be deployed on K8s, the cluster has to be.
	_, err = api.ETLList(BaseAPIParams(GetPrimaryURL()))
	isK8s = err == nil
	// HACK: Check based on error message. Unfortunately, there is no relevant HTTP code.
	if err != nil && strings.Contains(err.Error(), "requires Kubernetes") {
		err = nil
	}
	return
}

func isClusterLocal() (isLocal bool, err error) {
	var (
		primaryURL = GetPrimaryURL()
		smap       *cluster.Smap
		baseParams = BaseAPIParams(primaryURL)
		config     *cmn.Config
		fileData   []byte
	)
	if smap, err = api.GetClusterMap(baseParams); err != nil {
		return
	}
	if config, err = api.GetDaemonConfig(baseParams, smap.Primary); err != nil {
		return
	}
	fileData, err = ioutil.ReadFile(path.Join(config.ConfigDir, proxyIDFname))
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return
	}

	isLocal = strings.TrimSpace(string(fileData)) == smap.Primary.ID()
	return
}
