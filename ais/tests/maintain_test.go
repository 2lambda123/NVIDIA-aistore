// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/devtools/readers"
	"github.com/NVIDIA/aistore/devtools/tassert"
	"github.com/NVIDIA/aistore/devtools/tlog"
	"github.com/NVIDIA/aistore/devtools/tutils"
)

func TestMaintenanceOnOff(t *testing.T) {
	proxyURL := tutils.RandomProxyURL(t)
	smap := tutils.GetClusterMap(t, proxyURL)

	// Invalid target case
	msg := &cmn.ActValRmNode{DaemonID: "fakeID", SkipRebalance: true}
	_, err := api.StartMaintenance(baseParams, msg)
	tassert.Fatalf(t, err != nil, "Maintenance for invalid daemon ID succeeded")

	mntTarget, _ := smap.GetRandTarget()
	msg.DaemonID = mntTarget.ID()
	baseParams := tutils.BaseAPIParams(proxyURL)
	_, err = api.StartMaintenance(baseParams, msg)
	tassert.CheckFatal(t, err)
	smap, err = tutils.WaitForClusterState(proxyURL, "target in maintenance",
		smap.Version, smap.CountActiveProxies(), smap.CountActiveTargets()-1)
	tassert.CheckFatal(t, err)
	_, err = api.StopMaintenance(baseParams, msg)
	tassert.CheckFatal(t, err)
	_, err = tutils.WaitForClusterState(proxyURL, "target is back",
		smap.Version, smap.CountActiveProxies(), smap.CountTargets())
	tassert.CheckFatal(t, err)
	_, err = api.StopMaintenance(baseParams, msg)
	tassert.Fatalf(t, err != nil, "Canceling maintenance must fail for 'normal' daemon")
}

func TestMaintenanceListObjects(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		bck = cmn.Bck{Name: "maint-list", Provider: cmn.ProviderAIS}
		m   = &ioContext{
			t:         t,
			num:       1500,
			fileSize:  cos.KiB,
			fixedSize: true,
			bck:       bck,
			proxyURL:  proxyURL,
		}
		proxyURL    = tutils.RandomProxyURL(t)
		baseParams  = tutils.BaseAPIParams(proxyURL)
		origEntries = make(map[string]*cmn.BucketEntry, 1500)
	)

	m.saveClusterState()
	tutils.CreateFreshBucket(t, proxyURL, bck, nil)

	m.puts()
	// 1. Perform list-object and populate entries map
	msg := &cmn.SelectMsg{}
	msg.AddProps(cmn.GetPropsChecksum, cmn.GetPropsVersion, cmn.GetPropsCopies, cmn.GetPropsSize)
	bckList, err := api.ListObjects(baseParams, bck, msg, 0)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(bckList.Entries) == m.num, "list-object should return %d objects - returned %d", m.num, len(bckList.Entries))
	for _, entry := range bckList.Entries {
		origEntries[entry.Name] = entry
	}

	// 2. Put a random target under maintenanace
	tsi, _ := m.smap.GetRandTarget()
	tlog.Logf("Put target maintenanace %s\n", tsi)
	actVal := &cmn.ActValRmNode{DaemonID: tsi.ID(), SkipRebalance: false}
	rebID, err := api.StartMaintenance(baseParams, actVal)
	tassert.CheckFatal(t, err)

	defer func() {
		rebID, err = api.StopMaintenance(baseParams, actVal)
		tassert.CheckFatal(t, err)
		_, err = tutils.WaitForClusterState(proxyURL, "target is back",
			m.smap.Version, m.smap.CountActiveProxies(), m.smap.CountTargets())
		args := api.XactReqArgs{ID: rebID, Timeout: rebalanceTimeout}
		_, err = api.WaitForXaction(baseParams, args)
		tassert.CheckFatal(t, err)
	}()

	m.smap, err = tutils.WaitForClusterState(proxyURL, "target in maintenance",
		m.smap.Version, m.smap.CountActiveProxies(), m.smap.CountActiveTargets()-1)
	tassert.CheckFatal(t, err)

	// Wait for reb to complete
	args := api.XactReqArgs{ID: rebID, Timeout: rebalanceTimeout}
	_, err = api.WaitForXaction(baseParams, args)
	tassert.CheckFatal(t, err)

	// 3. Check if we can list all the objects
	bckList, err = api.ListObjects(baseParams, bck, msg, 0)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(bckList.Entries) == m.num, "list-object should return %d objects - returned %d", m.num, len(bckList.Entries))
	for _, entry := range bckList.Entries {
		origEntry, ok := origEntries[entry.Name]
		tassert.Fatalf(t, ok, "object %s missing in original entries", entry.Name)
		if entry.Checksum != origEntry.Checksum ||
			entry.Version != origEntry.Version ||
			entry.Flags != origEntry.Flags ||
			entry.Copies != origEntry.Copies {
			t.Errorf("some fields of object %q, don't match: %#v v/s %#v ", entry.Name, entry, origEntry)
		}
	}
}

// TODO: Run only with long tests when the test is stable.
func TestMaintenanceMD(t *testing.T) {
	// NOTE: This function requires local deployment as it checks local file system for VMDs.
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeLocal})

	var (
		proxyURL   = tutils.RandomProxyURL(t)
		smap       = tutils.GetClusterMap(t, proxyURL)
		baseParams = tutils.BaseAPIParams(proxyURL)

		dcmTarget, _  = smap.GetRandTarget()
		allTgtsMpaths = tutils.GetTargetsMountpaths(t, smap, baseParams)
	)

	cmd := tutils.GetRestoreCmd(dcmTarget)
	msg := &cmn.ActValRmNode{DaemonID: dcmTarget.ID(), SkipRebalance: true}
	_, err := api.Decommission(baseParams, msg)
	tassert.CheckError(t, err)
	_, err = tutils.WaitForClusterState(proxyURL, "target decommission", smap.Version, smap.CountActiveProxies(),
		smap.CountTargets()-1)
	tassert.CheckFatal(t, err)

	vmdTargets := countVMDTargets(allTgtsMpaths)
	tassert.Errorf(t, vmdTargets == smap.CountTargets()-1, "expected VMD to be found on %d targets, got %d.",
		smap.CountTargets()-1, vmdTargets)

	err = tutils.RestoreNode(cmd, false, "target")
	tassert.CheckFatal(t, err)
	_, err = tutils.WaitForClusterState(proxyURL, "target decommission",
		smap.Version, smap.CountActiveProxies(), smap.CountTargets())
	tassert.CheckFatal(t, err)
	args := api.XactReqArgs{Kind: cmn.ActRebalance, Timeout: rebalanceTimeout}
	_, err = api.WaitForXaction(baseParams, args)
	tassert.CheckError(t, err)

	smap = tutils.GetClusterMap(t, proxyURL)
	vmdTargets = countVMDTargets(allTgtsMpaths)
	tassert.Errorf(t, vmdTargets == smap.CountTargets(),
		"expected VMD to be found on all %d targets after joining cluster, got %d",
		smap.CountTargets(), vmdTargets)
}

func TestMaintenanceDecommissionRebalance(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeLocal, Long: true})

	var (
		proxyURL   = tutils.RandomProxyURL(t)
		smap       = tutils.GetClusterMap(t, proxyURL)
		baseParams = tutils.BaseAPIParams(proxyURL)
		objCount   = 100
		objPath    = "ic-decomm/"
		fileSize   = cos.KiB

		dcmTarget, _    = smap.GetRandTarget()
		origTargetCount = smap.CountTargets()
		origProxyCount  = smap.CountActiveProxies()
		bck             = cmn.Bck{Name: t.Name(), Provider: cmn.ProviderAIS}
	)

	tutils.CreateFreshBucket(t, proxyURL, bck, nil)
	for i := 0; i < objCount; i++ {
		objName := fmt.Sprintf("%sobj%04d", objPath, i)
		r, _ := readers.NewRandReader(int64(fileSize), cos.ChecksumXXHash)
		err := api.PutObject(api.PutObjectArgs{
			BaseParams: baseParams,
			Bck:        bck,
			Object:     objName,
			Reader:     r,
			Size:       uint64(fileSize),
		})
		tassert.CheckFatal(t, err)
	}

	cmd := tutils.GetRestoreCmd(dcmTarget)
	msg := &cmn.ActValRmNode{DaemonID: dcmTarget.ID(), CleanData: true}
	rebID, err := api.Decommission(baseParams, msg)
	tassert.CheckError(t, err)
	_, err = tutils.WaitForClusterState(proxyURL, "target decommission",
		smap.Version, origProxyCount, origTargetCount-1, dcmTarget.ID())
	tassert.CheckFatal(t, err)

	tutils.WaitForRebalanceByID(t, baseParams, rebID, rebalanceTimeout)
	msgList := &cmn.SelectMsg{Prefix: objPath}
	bucketList, err := api.ListObjects(baseParams, bck, msgList, 0)
	tassert.CheckError(t, err)
	if bucketList != nil && len(bucketList.Entries) != objCount {
		t.Errorf("Invalid number of objects: %d, expected %d", len(bucketList.Entries), objCount)
	}

	tlog.Logf("wait for node is gone...\n")
	err = tutils.WaitForNodeToTerminate(cmd.PID, 10*time.Second)
	tassert.CheckError(t, err)
	// TODO: something is going on inside a cluster after the node is dead.
	// If the test restarts the node immediately, the new node reports many
	// `pipe broken` and `EOF` messages that results in rebalance fails and
	// the test does as well. Adding a Sleep (10-20 seconds, 3 seconds is
	// insufficient) fixes the test.
	time.Sleep(time.Second * 10)

	smap = tutils.GetClusterMap(t, proxyURL)
	err = tutils.RestoreNode(cmd, false, "target")
	tassert.CheckFatal(t, err)
	smap, err = tutils.WaitForClusterState(proxyURL, "target restore",
		smap.Version, 0, 0)
	tassert.CheckFatal(t, err)

	// If any node is in maintenance, cancel the maintenance mode
	var dcm *cluster.Snode
	for _, node := range smap.Tmap {
		if smap.PresentInMaint(node) {
			dcm = node
			break
		}
	}
	if dcm != nil {
		tlog.Logf("Canceling maintenance for %s\n", dcm.ID())
		args := api.XactReqArgs{Kind: cmn.ActRebalance}
		err = api.AbortXaction(baseParams, args)
		tassert.CheckError(t, err)
		val := &cmn.ActValRmNode{DaemonID: dcm.ID()}
		rebID, err = api.StopMaintenance(baseParams, val)
		tassert.CheckError(t, err)
		tutils.WaitForRebalanceByID(t, baseParams, rebID, rebalanceTimeout)
	} else {
		args := api.XactReqArgs{Kind: cmn.ActRebalance, Timeout: rebalanceTimeout}
		_, err = api.WaitForXaction(baseParams, args)
		tassert.CheckError(t, err)
	}

	bucketList, err = api.ListObjects(baseParams, bck, msgList, 0)
	tassert.CheckError(t, err)
	if bucketList != nil && len(bucketList.Entries) != objCount {
		t.Errorf("Invalid number of objects: %d, expected %d", len(bucketList.Entries), objCount)
	}
}

func countVMDTargets(tsMpaths map[string][]string) (total int) {
	for _, mpaths := range tsMpaths {
		for _, mpath := range mpaths {
			if _, err := os.Stat(filepath.Join(mpath, cmn.VmdFname)); err == nil {
				total++
				break
			}
		}
	}
	return
}

func TestMaintenanceRebalance(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})
	var (
		bck = cmn.Bck{Name: "maint-reb", Provider: cmn.ProviderAIS}
		m   = &ioContext{
			t:               t,
			num:             30,
			fileSize:        512,
			fixedSize:       true,
			bck:             bck,
			numGetsEachFile: 1,
			proxyURL:        proxyURL,
		}
		actVal     = &cmn.ActValRmNode{}
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
	)

	m.saveClusterState()
	tutils.CreateFreshBucket(t, proxyURL, bck, nil)
	origProxyCnt, origTargetCount := m.smap.CountActiveProxies(), m.smap.CountActiveTargets()

	m.puts()
	tsi, _ := m.smap.GetRandTarget()
	tlog.Logf("Removing target %s\n", tsi)
	restored := false
	actVal.DaemonID = tsi.ID()
	rebID, err := api.StartMaintenance(baseParams, actVal)
	tassert.CheckError(t, err)
	defer func() {
		if !restored {
			rebID, err := api.StopMaintenance(baseParams, actVal)
			tassert.CheckError(t, err)
			_, err = tutils.WaitForClusterState(
				proxyURL,
				"to target joined the cluster",
				m.smap.Version, origProxyCnt, origTargetCount,
			)
			tassert.CheckFatal(t, err)
			tutils.WaitForRebalanceByID(t, baseParams, rebID, time.Minute)
		}
		tutils.ClearMaintenance(baseParams, tsi)
	}()
	tlog.Logf("Wait for rebalance %s\n", rebID)
	tutils.WaitForRebalanceByID(t, baseParams, rebID, time.Minute)

	smap, err := tutils.WaitForClusterState(
		proxyURL,
		"to target removed from the cluster",
		m.smap.Version, origProxyCnt, origTargetCount-1, tsi.ID(),
	)
	tassert.CheckFatal(t, err)
	m.smap = smap

	m.gets()
	m.ensureNoErrors()

	rebID, err = api.StopMaintenance(baseParams, actVal)
	tassert.CheckFatal(t, err)
	restored = true
	smap, err = tutils.WaitForClusterState(
		proxyURL,
		"to target joined the cluster",
		m.smap.Version, origProxyCnt, origTargetCount,
	)
	tassert.CheckFatal(t, err)
	m.smap = smap

	tutils.WaitForRebalanceByID(t, baseParams, rebID, time.Minute)
}

func TestMaintenanceGetWhileRebalance(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})
	var (
		bck = cmn.Bck{Name: "maint-get-reb", Provider: cmn.ProviderAIS}
		m   = &ioContext{
			t:               t,
			num:             5000,
			fileSize:        1024,
			fixedSize:       true,
			bck:             bck,
			numGetsEachFile: 1,
			proxyURL:        proxyURL,
		}
		actVal     = &cmn.ActValRmNode{}
		proxyURL   = tutils.RandomProxyURL(t)
		baseParams = tutils.BaseAPIParams(proxyURL)
	)

	m.saveClusterState()
	tutils.CreateFreshBucket(t, proxyURL, bck, nil)
	origProxyCnt, origTargetCount := m.smap.CountActiveProxies(), m.smap.CountActiveTargets()

	m.puts()
	go m.getsUntilStop()
	stopped := false

	tsi, _ := m.smap.GetRandTarget()
	tlog.Logf("Removing target %s\n", tsi)
	restored := false
	actVal.DaemonID = tsi.ID()
	rebID, err := api.StartMaintenance(baseParams, actVal)
	tassert.CheckFatal(t, err)
	defer func() {
		if !stopped {
			m.stopGets()
		}
		if !restored {
			rebID, err := api.StopMaintenance(baseParams, actVal)
			tassert.CheckFatal(t, err)
			_, err = tutils.WaitForClusterState(
				proxyURL,
				"to target joined the cluster",
				m.smap.Version, origProxyCnt, origTargetCount,
			)
			tassert.CheckFatal(t, err)
			tutils.WaitForRebalanceByID(t, baseParams, rebID, time.Minute)
		}
		tutils.ClearMaintenance(baseParams, tsi)
	}()
	tlog.Logf("Wait for rebalance %s\n", rebID)
	tutils.WaitForRebalanceByID(t, baseParams, rebID, time.Minute)

	smap, err := tutils.WaitForClusterState(
		proxyURL,
		"target removed from the cluster",
		m.smap.Version, origProxyCnt, origTargetCount-1, tsi.ID(),
	)
	tassert.CheckFatal(t, err)
	m.smap = smap

	m.stopGets()
	stopped = true
	m.ensureNoErrors()

	rebID, err = api.StopMaintenance(baseParams, actVal)
	tassert.CheckFatal(t, err)
	restored = true
	smap, err = tutils.WaitForClusterState(
		proxyURL,
		"to target joined the cluster",
		m.smap.Version, origProxyCnt, origTargetCount,
	)
	tassert.CheckFatal(t, err)
	m.smap = smap
	tutils.WaitForRebalanceByID(t, baseParams, rebID, time.Minute)
}

func TestNodeShutdown(t *testing.T) {
	for _, ty := range []string{cmn.Proxy, cmn.Target} {
		t.Run(ty, func(t *testing.T) {
			testNodeShutdown(t, ty)
		})
	}
}

func testNodeShutdown(t *testing.T, nodeType string) {
	const nodeOffTimeout = 10 * time.Second
	var (
		proxyURL = tutils.GetPrimaryURL()
		smap     = tutils.GetClusterMap(t, proxyURL)
		node     *cluster.Snode
		err      error
		pdc, tdc int

		origProxyCnt    = smap.CountActiveProxies()
		origTargetCount = smap.CountActiveTargets()
	)

	if nodeType == cmn.Proxy {
		node, err = smap.GetRandProxy(true)
		pdc = 1
	} else {
		node, err = smap.GetRandTarget()
		tdc = 1
	}
	tassert.CheckFatal(t, err)

	// 1. Shutdown a random node.
	pid, cmd, err := tutils.ShutdownNode(t, baseParams, node)
	tassert.CheckFatal(t, err)
	if nodeType == cmn.Target {
		tutils.WaitForRebalanceToComplete(t, baseParams)
	}

	// 2. Make sure the node has been shut down.
	err = tutils.WaitForNodeToTerminate(pid, nodeOffTimeout)
	tassert.CheckError(t, err)
	_, err = tutils.WaitForClusterState(proxyURL, "shutdown node",
		smap.Version, origProxyCnt-pdc, origTargetCount-tdc, node.DaemonID)
	tassert.CheckError(t, err)

	// 3. Start node again.
	tlog.Logf("Starting the node %s[%s]...\n", node, nodeType)
	err = tutils.RestoreNode(cmd, false, nodeType)
	tassert.CheckError(t, err)
	smap, err = tutils.WaitForClusterState(proxyURL, "restart node",
		smap.Version, origProxyCnt-pdc, origTargetCount-tdc)
	tassert.CheckError(t, err)
	tassert.Errorf(t, smap.GetNode(node.DaemonID).Flags.IsSet(cluster.SnodeMaintenance),
		"node should be in maintenance after starting")

	// 4. Remove the node from maintenance.
	_, err = api.StopMaintenance(baseParams, &cmn.ActValRmNode{DaemonID: node.DaemonID})
	tassert.CheckError(t, err)
	_, err = tutils.WaitForClusterState(proxyURL, "remove node from maintenance",
		smap.Version, origProxyCnt, origTargetCount)
	tassert.CheckError(t, err)

	if nodeType == cmn.Target {
		tutils.WaitForRebalanceToComplete(t, baseParams)
	}
}

func TestShutdownListObjects(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	const nodeOffTimeout = 10 * time.Second
	var (
		bck = cmn.Bck{Name: "shutdown-list", Provider: cmn.ProviderAIS}
		m   = &ioContext{
			t:         t,
			num:       1500,
			fileSize:  cos.KiB,
			fixedSize: true,
			bck:       bck,
			proxyURL:  proxyURL,
		}
		proxyURL    = tutils.RandomProxyURL(t)
		baseParams  = tutils.BaseAPIParams(proxyURL)
		origEntries = make(map[string]*cmn.BucketEntry, 1500)
	)

	m.saveClusterState()
	origTargetCount := m.smap.CountActiveTargets()
	tutils.CreateFreshBucket(t, proxyURL, bck, nil)
	m.puts()

	// 1. Perform list-object and populate entries map.
	msg := &cmn.SelectMsg{}
	msg.AddProps(cmn.GetPropsChecksum, cmn.GetPropsCopies, cmn.GetPropsSize)
	bckList, err := api.ListObjects(baseParams, bck, msg, 0)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(bckList.Entries) == m.num, "list-object should return %d objects - returned %d", m.num, len(bckList.Entries))
	for _, entry := range bckList.Entries {
		origEntries[entry.Name] = entry
	}

	// 2. Shut down a random target.
	tsi, _ := m.smap.GetRandTarget()
	pid, cmd, err := tutils.ShutdownNode(t, baseParams, tsi)
	tassert.CheckFatal(t, err)

	// Restore target after test is over.
	t.Cleanup(func() {
		tlog.Logf("Restoring %s\n", tsi.Name())
		err = tutils.RestoreNode(cmd, false, cmn.Target)
		tassert.CheckError(t, err)
		_, err = tutils.WaitForClusterState(proxyURL, "target is back",
			m.smap.Version, 0, origTargetCount-1)
		tassert.CheckError(t, err)

		// Remove the node from maintenance.
		_, err = api.StopMaintenance(baseParams, &cmn.ActValRmNode{DaemonID: tsi.DaemonID})
		tassert.CheckError(t, err)
		_, err = tutils.WaitForClusterState(proxyURL, "remove node from maintenance",
			m.smap.Version, 0, origTargetCount)
		tassert.CheckError(t, err)

		tutils.WaitForRebalanceToComplete(t, baseParams)
	})

	// Wait for reb, shutdown to complete.
	tutils.WaitForRebalanceToComplete(t, baseParams)
	tassert.CheckError(t, err)
	err = tutils.WaitForNodeToTerminate(pid, nodeOffTimeout)
	tassert.CheckError(t, err)
	m.smap, err = tutils.WaitForClusterState(proxyURL, "target in maintenance",
		m.smap.Version, 0, origTargetCount-1, tsi.ID())
	tassert.CheckError(t, err)

	// 3. Check if we can list all the objects.
	tlog.Logln("Listing objects")
	bckList, err = api.ListObjects(baseParams, bck, msg, 0)
	tassert.CheckError(t, err)
	tassert.Errorf(t, len(bckList.Entries) == m.num, "list-object should return %d objects - returned %d", m.num, len(bckList.Entries))
	for _, entry := range bckList.Entries {
		origEntry, ok := origEntries[entry.Name]
		tassert.Errorf(t, ok, "object %s missing in original entries", entry.Name)
		if entry.Version != origEntry.Version ||
			entry.Flags != origEntry.Flags ||
			entry.Copies != origEntry.Copies {
			t.Errorf("some fields of object %q, don't match: %#v v/s %#v ", entry.Name, entry, origEntry)
		}
	}
}
