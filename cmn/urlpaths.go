// Package cmn provides common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2021, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

type URLPath struct {
	L []string
	S string
}

func urlpath(words ...string) URLPath { return URLPath{L: words, S: JoinWords(words...)} }

var (
	URLPathS3 = urlpath(S3) // URLPath{[]string{S3}, S3}

	URLPathBuckets   = urlpath(Version, Buckets)
	URLPathObjects   = urlpath(Version, Objects)
	URLPathEC        = urlpath(Version, EC)
	URLPathNotifs    = urlpath(Version, Notifs)
	URLPathTxn       = urlpath(Version, Txn)
	URLPathXactions  = urlpath(Version, Xactions)
	URLPathIC        = urlpath(Version, IC)
	URLPathHealth    = urlpath(Version, Health)
	URLPathMetasync  = urlpath(Version, Metasync)
	URLPathRebalance = urlpath(Version, Rebalance)

	URLPathCluster        = urlpath(Version, Cluster)
	URLPathClusterProxy   = urlpath(Version, Cluster, Proxy)
	URLPathClusterUserReg = urlpath(Version, Cluster, UserRegister)
	URLPathClusterAutoReg = urlpath(Version, Cluster, AutoRegister)
	URLPathClusterKalive  = urlpath(Version, Cluster, Keepalive)
	URLPathClusterDaemon  = urlpath(Version, Cluster, Daemon)
	URLPathClusterSetConf = urlpath(Version, Cluster, ActSetConfig)
	URLPathClusterAttach  = urlpath(Version, Cluster, ActAttach)
	URLPathClusterDetach  = urlpath(Version, Cluster, ActDetach)

	URLPathDaemon        = urlpath(Version, Daemon)
	URLPathDaemonProxy   = urlpath(Version, Daemon, Proxy)
	URLPathDaemonSetConf = urlpath(Version, Daemon, ActSetConfig)
	URLPathDaemonUserReg = urlpath(Version, Daemon, UserRegister)
	URLPathDaemonUnreg   = urlpath(Version, Daemon, Unregister)

	URLPathReverse       = urlpath(Version, Reverse)
	URLPathReverseDaemon = urlpath(Version, Reverse, Daemon)

	URLPathVote        = urlpath(Version, Vote)
	URLPathVoteInit    = urlpath(Version, Vote, Init)
	URLPathVoteProxy   = urlpath(Version, Vote, Proxy)
	URLPathVoteVoteres = urlpath(Version, Vote, Voteres)

	URLPathdSort        = urlpath(Version, Sort)
	URLPathdSortInit    = urlpath(Version, Sort, Init)
	URLPathdSortStart   = urlpath(Version, Sort, Start)
	URLPathdSortList    = urlpath(Version, Sort, List)
	URLPathdSortAbort   = urlpath(Version, Sort, Abort)
	URLPathdSortShards  = urlpath(Version, Sort, Shards)
	URLPathdSortRecords = urlpath(Version, Sort, Records)
	URLPathdSortMetrics = urlpath(Version, Sort, Metrics)
	URLPathdSortAck     = urlpath(Version, Sort, FinishedAck)
	URLPathdSortRemove  = urlpath(Version, Sort, Remove)

	URLPathDownload       = urlpath(Version, Download)
	URLPathDownloadAbort  = urlpath(Version, Download, Abort)
	URLPathDownloadRemove = urlpath(Version, Download, Remove)

	URLPathQuery        = urlpath(Version, Query)
	URLPathQueryInit    = urlpath(Version, Query, Init)
	URLPathQueryPeek    = urlpath(Version, Query, Peek)
	URLPathQueryDiscard = urlpath(Version, Query, Discard)
	URLPathQueryNext    = urlpath(Version, Query, Next)
	URLPathQueryWorker  = urlpath(Version, Query, WorkerOwner)

	URLPathETL       = urlpath(Version, ETL)
	URLPathETLInit   = urlpath(Version, ETL, ETLInit)
	URLPathETLStop   = urlpath(Version, ETL, ETLStop)
	URLPathETLList   = urlpath(Version, ETL, ETLList)
	URLPathETLLogs   = urlpath(Version, ETL, ETLLogs)
	URLPathETLObject = urlpath(Version, ETL, ETLObject)
	URLPathETLBuild  = urlpath(Version, ETL, ETLBuild)

	URLPathTokens   = urlpath(Version, Tokens) // authn
	URLPathUsers    = urlpath(Version, Users)
	URLPathClusters = urlpath(Version, Clusters)
	URLPathRoles    = urlpath(Version, Roles)
)
