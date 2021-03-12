// Package tutils provides common low-level utilities for all aistore unit and integration tests
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package tutils

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/jsp"
	"github.com/NVIDIA/aistore/containers"
	"github.com/NVIDIA/aistore/devtools"
	"github.com/NVIDIA/aistore/devtools/tlog"
	"github.com/NVIDIA/aistore/memsys"
)

const (
	defaultProxyURL = "http://localhost:8080"      // the url for the cluster's proxy (local)
	dockerEnvFile   = "/tmp/docker_ais/deploy.env" // filepath of Docker deployment config
)

const (
	registerTimeout = time.Minute * 2
	bucketTimeout   = time.Minute
)

type (
	// command used to restore a node
	RestoreCmd struct {
		Node *cluster.Snode
		Cmd  string
		Args []string
		PID  int
	}
	ClusterType string
)

// Cluster type used for test
const (
	ClusterTypeLocal  ClusterType = "local"
	ClusterTypeDocker ClusterType = "docker"
	ClusterTypeK8s    ClusterType = "k8s"
)

var (
	proxyURLReadOnly string          // user-defined primary proxy URL - it is read-only variable and tests mustn't change it
	pmapReadOnly     cluster.NodeMap // initial proxy map - it is read-only variable
	testClusterType  ClusterType     // AIS cluster type - it is read-only variable

	restoreNodesOnce sync.Once             // Ensures that the initialization happens only once.
	restoreNodes     map[string]RestoreCmd // initial proxy and target nodes => command to restore them

	transportArgs = cmn.TransportArgs{
		Timeout:          600 * time.Second,
		IdleConnsPerHost: 100,
		UseHTTPProxyEnv:  true,
	}
	HTTPClient *http.Client

	RemoteCluster struct {
		UUID  string
		Alias string
		URL   string
	}
	AuthToken string

	MMSA *memsys.MMSA

	DevtoolsCtx *devtools.Ctx
)

func init() {
	MMSA = memsys.DefaultPageMM()
	envURL := os.Getenv(cmn.EnvVars.Endpoint)
	// Since tests do not have access to cluster configuration, the tests
	// detect client type by the primary proxy URL passed by a user.
	// Certificate check is always disabled.
	transportArgs.UseHTTPS = cos.IsHTTPS(envURL)
	transportArgs.SkipVerify = cos.IsParseBool(os.Getenv(cmn.EnvVars.SkipVerifyCrt))
	HTTPClient = cmn.NewClient(transportArgs)

	DevtoolsCtx = &devtools.Ctx{
		Client: HTTPClient,
		Log:    tlog.Logf,
	}
}

// InitLocalCluster initializes tutils component with AIS cluster that must be either:
//  1. the cluster must be deployed locally using `make deploy` command and accessible @ localhost:8080, or
//  2. cluster deployed on local docker environment, or
//  3. provided via `AIS_ENDPOINT` environment variable
// In addition, try to query remote AIS cluster that may or may not be locally deployed as well.
func InitLocalCluster() {
	var (
		// Gets the fields from the .env file from which the docker was deployed
		envVars = cos.ParseEnvVariables(dockerEnvFile)
		// Host IP and port of primary cluster
		primaryHostIP, port = envVars["PRIMARY_HOST_IP"], envVars["PORT"]

		clusterType = ClusterTypeLocal
		proxyURL    = defaultProxyURL
	)

	if containers.DockerRunning() {
		clusterType = ClusterTypeDocker
		proxyURL = "http://" + primaryHostIP + ":" + port
	}

	// This is needed for testing on Kubernetes if we want to run 'make test-XXX'
	// Many of the other packages do not accept the 'url' flag
	if cliAISURL := os.Getenv(cmn.EnvVars.Endpoint); cliAISURL != "" {
		if !strings.HasPrefix(cliAISURL, "http") {
			cliAISURL = "http://" + cliAISURL
		}
		proxyURL = cliAISURL
	}

	err := InitCluster(proxyURL, clusterType)
	if err == nil {
		initRemoteCluster() // remote AIS that optionally may be run locally as well and used for testing
		return
	}
	fmt.Printf("Error: %s\n", strings.TrimSuffix(err.Error(), "\n"))
	fmt.Println("Environment variables:")
	fmt.Printf("\t%s:\t%s\n", cmn.EnvVars.Endpoint, os.Getenv(cmn.EnvVars.Endpoint))
	fmt.Printf("\t%s:\t%s\n", cmn.EnvVars.PrimaryID, os.Getenv(cmn.EnvVars.PrimaryID))
	fmt.Printf("\t%s:\t%s\n", cmn.EnvVars.SkipVerifyCrt, os.Getenv(cmn.EnvVars.SkipVerifyCrt))
	fmt.Printf("\t%s:\t%s\n", cmn.EnvVars.UseHTTPS, os.Getenv(cmn.EnvVars.UseHTTPS))
	if len(envVars) > 0 {
		fmt.Println("Docker Environment:")
		for k, v := range envVars {
			fmt.Printf("\t%s:\t%s\n", k, v)
		}
	}
	cos.Exitf("")
}

// InitCluster initializes the environment necessary for testing against an AIS cluster.
// NOTE: the function is also used for testing by NVIDIA/ais-k8s Operator
func InitCluster(proxyURL string, clusterType ClusterType) (err error) {
	proxyURLReadOnly = proxyURL
	testClusterType = clusterType
	if err = initProxyURL(); err != nil {
		return
	}
	initPmap()
	initAuthToken()
	return
}

func initProxyURL() (err error) {
	// Discover if a proxy is ready to accept requests.
	err = cmn.NetworkCallWithRetry(&cmn.CallWithRetryArgs{
		Call:    func() (int, error) { return 0, GetProxyReadiness(proxyURLReadOnly) },
		SoftErr: 5,
		HardErr: 5,
		Sleep:   5 * time.Second,
		Action:  fmt.Sprintf("check proxy readiness at %s", proxyURLReadOnly),
	})
	if err != nil {
		err = fmt.Errorf("failed to successfully check readiness of a proxy at %s; err %v", proxyURLReadOnly, err)
		return
	}

	if testClusterType == ClusterTypeK8s {
		// For kubernetes cluster, we use LoadBalancer service to expose the proxies.
		// `proxyURLReadOnly` will point to LoadBalancer service, and we need not get primary URL.
		return
	}

	// Primary proxy can change if proxy tests are run and
	// no new cluster is re-deployed before each test.
	// Finds who is the current primary proxy.
	primary, err := GetPrimaryProxy(proxyURLReadOnly)
	if err != nil {
		err = fmt.Errorf("failed to get primary proxy info from %s; err %v", proxyURLReadOnly, err)
		return err
	}
	proxyURLReadOnly = primary.URL(cmn.NetworkPublic)
	return
}

func initPmap() {
	baseParams := BaseAPIParams(proxyURLReadOnly)
	smap, err := waitForStartup(baseParams)
	cos.AssertNoErr(err)
	pmapReadOnly = smap.Pmap
}

func initRemoteCluster() {
	aisInfo, err := api.GetRemoteAIS(BaseAPIParams(proxyURLReadOnly))
	if err != nil {
		if !errors.Is(err, io.EOF) {
			fmt.Fprintf(os.Stderr, "failed to query remote ais cluster: %v\n", err)
		}
		return
	}
	for _, clusterInfo := range aisInfo {
		if !clusterInfo.Online {
			continue
		}
		// TODO: use actual UUID (for now it doesn't work correctly as
		//  proxy may not have full information about the remote cluster)
		RemoteCluster.UUID = clusterInfo.Alias
		RemoteCluster.Alias = clusterInfo.Alias
		RemoteCluster.URL = clusterInfo.URL
		break
	}
}

func initNodeCmd() {
	baseParams := BaseAPIParams(proxyURLReadOnly)
	smap, err := waitForStartup(baseParams)
	cos.AssertNoErr(err)
	restoreNodes = make(map[string]RestoreCmd, smap.CountProxies()+smap.CountTargets())
	for _, node := range smap.Pmap {
		restoreNodes[node.ID()] = GetRestoreCmd(node)
	}

	for _, node := range smap.Tmap {
		restoreNodes[node.ID()] = GetRestoreCmd(node)
	}
}

func initAuthToken() {
	home, err := os.UserHomeDir()
	cos.AssertNoErr(err)
	tokenPath := filepath.Join(home, ".ais", "token")

	var token cmn.TokenMsg
	jsp.LoadMeta(tokenPath, &token)

	AuthToken = token.Token
}
