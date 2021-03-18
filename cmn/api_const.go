// Package cmn provides common constants, types, and utilities for AIS clients
// and AIStore.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"time"
)

// module names
const (
	DSortName          = "dSort"
	DSortNameLowercase = "dsort"
)

// ActionMsg.Action
// includes Xaction.Kind == ActionMsg.Action (when the action is asynchronous)
const (
	ActShutdown       = "shutdown"
	ActDecommission   = "decommission" // Decommission all deamons in cluster
	ActRebalance      = "rebalance"
	ActResilver       = "resilver"
	ActLRU            = "lru"
	ActCreateBck      = "create_bck"
	ActDestroyBck     = "destroy_bck"     // Destroy bucket data and metadata
	ActAddRemoteBck   = "add_remotebck"   // Register (existing) remote bucket into AIS
	ActEvictRemoteBck = "evict_remotebck" // Evict remote bucket's data; TODO cleanup BMD as well
	ActMoveBck        = "move_bck"
	ActCopyBck        = "copybck"
	ActETLBck         = "etlbck"
	ActSetConfig      = "setconfig"
	ActResetConfig    = "resetconfig"
	ActSetBprops      = "setbprops"
	ActResetBprops    = "resetbprops"
	ActResyncBprops   = "resyncbprops"
	ActList           = "list"
	ActQueryObjects   = "queryobj"
	ActInvalListCache = "invallistobjcache"
	ActSummary        = "summary"
	ActRenameObject   = "renameobj"
	ActPromote        = "promote"
	ActEvictObjects   = "evictobj"
	ActDelete         = "delete"
	ActPrefetch       = "prefetch"
	ActDownload       = "download"
	ActRegTarget      = "regtarget"
	ActRegProxy       = "regproxy"
	ActNewPrimary     = "newprimary"
	ActElection       = "election"
	ActPutCopies      = "putcopies"
	ActMakeNCopies    = "makencopies"
	ActLoadLomCache   = "loadlomcache"
	ActECGet          = "ecget"    // erasure decode objects
	ActECPut          = "ecput"    // erasure encode objects
	ActECRespond      = "ecresp"   // respond to other targets' EC requests
	ActECEncode       = "ecencode" // erasure code a bucket
	ActStartGFN       = "metasync_start_gfn"
	ActAttach         = "attach"
	ActDetach         = "detach"
	// Node maintenance
	ActStartMaintenance = "startmaintenance"  // put into maintenance state
	ActStopMaintenance  = "stopmaintenance"   // cancel maintenance state
	ActDecommissionNode = "decommission_node" // start rebalance and remove node from Smap when it finishes
	ActShutdownNode     = "shutdown_node"     // shutdown a specific node
	// IC
	ActSendOwnershipTbl  = "ic_send_ownership_tbl"
	ActListenToNotif     = "watch_xaction"
	ActMergeOwnershipTbl = "merge_ownership_tbl"
	ActRegGlobalXaction  = "reg_global_xaction"
)

const (
	// Actions to manipulate mountpaths (/v1/daemon/mountpaths)
	ActMountpathEnable  = "enable"
	ActMountpathDisable = "disable"
	ActMountpathAdd     = "add"
	ActMountpathRemove  = "remove"

	// Actions on xactions
	ActXactStop  = Stop
	ActXactStart = Start

	// auxiliary
	ActTransient = "transient" // transient - in-memory only
)

// xaction begin-commit phases
const (
	ActBegin  = "begin"
	ActCommit = "commit"
	ActAbort  = "abort"
)

// Header Key conventions:
//  - starts with a prefix "ais-",
//  - all words separated with "-": no dots and underscores.

// User/client header keys.
const (
	// Bucket props headers.
	headerPrefix           = "ais-"
	HeaderBucketProps      = headerPrefix + "bucket-props"
	HeaderOrigURLBck       = headerPrefix + "original-url"       // See: BucketProps.Extra.HTTP.OrigURLBck
	HeaderCloudRegion      = headerPrefix + "cloud-region"       // See: BucketProps.Extra.AWS.CloudRegion
	HeaderBucketVerEnabled = headerPrefix + "versioning-enabled" // Enable/disable object versioning in a bucket.

	HeaderBucketCreated   = headerPrefix + "created"        // Bucket creation time.
	HeaderBackendProvider = headerPrefix + "provider"       // ProviderAmazon et al. - see cmn/bucket.go.
	HeaderRemoteOffline   = headerPrefix + "remote-offline" // When accessing cached remote bucket with no backend connectivity.

	// Object props headers.
	HeaderObjCksumType = headerPrefix + "checksum-type"  // Checksum type, one of SupportedChecksums().
	HeaderObjCksumVal  = headerPrefix + "checksum-value" // Checksum value.
	HeaderObjAtime     = headerPrefix + "atime"          // Object access time.
	HeaderObjCustomMD  = headerPrefix + "custom-md"      // Object custom metadata.
	HeaderObjSize      = headerPrefix + "size"           // Object size (bytes).
	HeaderObjVersion   = headerPrefix + "version"        // Object version/generation - ais or cloud.
	HeaderObjECMeta    = headerPrefix + "ec-meta"        // Info about EC object/slice/replica.

	// Append object header.
	HeaderAppendHandle = headerPrefix + "append-handle"

	// Query objects handle header.
	HeaderHandle = headerPrefix + "query-handle"

	// Reverse proxy headers.
	HeaderNodeID  = headerPrefix + "node-id"
	HeaderNodeURL = headerPrefix + "node-url"
)

// AuthN consts
const (
	HeaderAuthorization      = "Authorization" // https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Authorization
	AuthenticationTypeBearer = "Bearer"
)

// Internal header keys.
const (
	// Intra cluster headers.
	HeaderCallerID          = headerPrefix + "caller-id" // Marker of intra-cluster request.
	HeaderPutterID          = headerPrefix + "putter-id" // Marker of inter-cluster PUT object request.
	HeaderCallerName        = headerPrefix + "caller-name"
	HeaderCallerSmapVersion = headerPrefix + "caller-smap-ver"

	// Stream related headers.
	HeaderSessID   = headerPrefix + "session-id"
	HeaderCompress = headerPrefix + "compress" // LZ4Compression, etc.
)

// Configuration and bucket properties
const (
	PropBucketAccessAttrs  = "access"             // Bucket access attributes.
	PropBucketVerEnabled   = "versioning.enabled" // Enable/disable object versioning in a bucket.
	PropBucketCreated      = "created"            // Bucket creation time.
	PropBackendBck         = "backend_bck"
	PropBackendBckName     = PropBackendBck + ".name"
	PropBackendBckProvider = PropBackendBck + ".provider"
)

// HeaderCompress enum (supported compression algorithms)
const (
	LZ4Compression = "lz4"
)

// URL Query "?name1=val1&name2=..."
// Query parameter name conventions:
//  - contains only alpha-numeric characters,
//  - words must be separated with underscore "_".

// User/client query params.
const (
	URLParamWhat            = "what"         // "smap" | "bmd" | "config" | "stats" | "xaction" ...
	URLParamProps           = "props"        // e.g. "checksum, size"|"atime, size"|"cached"|"bucket, size"| ...
	URLParamCheckExists     = "check_cached" // true: check if object exists
	URLParamHealthReadiness = "readiness"    // true: check if node can accept HTTP(S) requests
	URLParamUUID            = "uuid"
	URLParamRegex           = "regex" // dsort/downloader regex

	// Bucket related query params.
	URLParamProvider  = "provider" // backend provider
	URLParamNamespace = "namespace"
	URLParamBucketTo  = "bck_to"
	URLParamKeepBckMD = "keep_md"

	// Object related query params.
	URLParamAppendType   = "append_type"
	URLParamAppendHandle = "append_handle"

	// HTTP bucket support.
	URLParamOrigURL = "original_url"
)

// Internal query params.
const (
	URLParamCheckExistsAny   = "cea" // true: lookup object in all mountpaths (NOTE: compare with URLParamCheckExists)
	URLParamProxyID          = "pid" // ID of the redirecting proxy.
	URLParamPrimaryCandidate = "can" // ID of the candidate for the primary proxy.
	URLParamForce            = "frc" // true: force the operation (e.g., shutdown primary and the entire cluster)
	URLParamPrepare          = "prp" // true: request belongs to the "prepare" phase of the primary proxy election
	URLParamNonElectable     = "nel" // true: proxy is non-electable for the primary role
	URLParamUnixTime         = "utm" // Unix time: number of nanoseconds elapsed since 01/01/70 UTC.
	URLParamIsGFNRequest     = "gfn" // true if the request is a Get-From-Neighbor
	URLParamSilent           = "sln" // true: destination should not log errors (HEAD request)
	URLParamRebStatus        = "rbs" // true: get detailed rebalancing status
	URLParamRebData          = "rbd" // true: get EC rebalance data (pulling data if push way fails)
	URLParamTaskAction       = "tac" // "start", "status", "result"
	URLParamClusterInfo      = "cii" // true: Health to return ais.clusterInfo
	URLParamRecvType         = "rtp" // to tell real PUT from migration PUT

	// dsort
	URLParamTotalCompressedSize       = "tcs"
	URLParamTotalInputShardsExtracted = "tise"
	URLParamTotalUncompressedSize     = "tunc"

	// 2PC transactions - control plane
	URLParamNetwTimeout  = "xnt" // [begin, start-commit] timeout
	URLParamHostTimeout  = "xht" // [begin, txn-done] timeout
	URLParamWaitMetasync = "xwm" // true: wait for metasync (used only when there's an alternative)

	// Notification target's node ID (usually, the node that initiates the operation).
	URLParamNotifyMe = "nft"
)

// URLParamAppendType enum
const (
	AppendOp = "append"
	FlushOp  = "flush"
)

// URLParamTaskAction enum
const (
	TaskStart  = Start
	TaskStatus = "status"
	TaskResult = "result"
)

// URLParamWhat enum

// User/client "what" values.
const (
	GetWhatBMD           = "bmd"
	GetWhatConfig        = "config"
	GetWhatClusterConfig = "cluster_config"
	GetWhatDaemonStatus  = "status"
	GetWhatDiskStats     = "disk"
	GetWhatMountpaths    = "mountpaths"
	GetWhatRemoteAIS     = "remote"
	GetWhatSmap          = "smap"
	GetWhatSmapVote      = "smapvote"
	GetWhatSnode         = "snode"
	GetWhatStats         = "stats"
	GetWhatStatus        = "status" // IC status by uuid.
	GetWhatSysInfo       = "sysinfo"
	GetWhatTargetIPs     = "target_ips"
)

// Internal "what" values.
const (
	GetWhatXactStats      = "getxstats" // stats: xaction by uuid
	GetWhatQueryXactStats = "qryxstats" // stats: all matching xactions
	GetWhatICBundle       = "ic_bundle"
)

// SelectMsg.Props enum
// NOTE: DO NOT forget update `GetPropsAll` constant when a prop is added/removed.
const (
	GetPropsName     = "name"
	GetPropsSize     = "size"
	GetPropsVersion  = "version"
	GetPropsChecksum = "checksum"
	GetPropsAtime    = "atime"
	GetPropsCached   = "cached"
	GetTargetURL     = "target_url"
	GetPropsStatus   = "status"
	GetPropsCopies   = "copies"
	GetPropsEC       = "ec"
)

// BucketEntry.Flags field
const (
	EntryStatusBits = 5                          // N bits
	EntryStatusMask = (1 << EntryStatusBits) - 1 // mask for N low bits
)

const (
	// Status
	ObjStatusOK = iota
	ObjStatusMoved
	ObjStatusDeleted // TODO: reserved for future when we introduce delayed delete of the object/bucket

	// Flags
	EntryIsCached = 1 << (EntryStatusBits + 1)
)

// List objects default page size
const (
	DefaultListPageSizeAIS = 10000
)

// RESTful URL path: l1/l2/l3
const (
	// l1
	Version = "v1"
	// l2
	Buckets   = "buckets"
	Objects   = "objects"
	EC        = "ec"
	Download  = "download"
	Daemon    = "daemon"
	Cluster   = "cluster"
	Tokens    = "tokens"
	Metasync  = "metasync"
	Health    = "health"
	Vote      = "vote"
	ObjStream = "objstream"
	MsgStream = "msgstream"
	Reverse   = "reverse"
	Rebalance = "rebalance"
	Xactions  = "xactions"
	S3        = "s3"
	Txn       = "txn"      // 2PC
	Notifs    = "notifs"   // intra-cluster notifications
	Users     = "users"    // AuthN
	Clusters  = "clusters" // AuthN
	Roles     = "roles"    // AuthN
	Query     = "query"
	IC        = "ic" // information center

	// l3
	SyncSmap     = "syncsmap"
	Keepalive    = "keepalive"
	UserRegister = "register" // node register by admin (manual)
	AutoRegister = "autoreg"  // node register itself into the primary proxy (automatic)
	Unregister   = "unregister"
	Voteres      = "result"
	VoteInit     = "init"
	Mountpaths   = "mountpaths"

	// common
	Init     = "init"
	Start    = "start"
	Stop     = "stop"
	Abort    = "abort"
	Sort     = "sort"
	Finished = "finished"
	Progress = "progress"

	// dSort, downloader, query
	Metrics     = "metrics"
	Records     = "records"
	Shards      = "shards"
	FinishedAck = "finished_ack"
	List        = "list"
	Remove      = "remove"
	Next        = "next"
	Peek        = "peek"
	Discard     = "discard"
	WorkerOwner = "worker" // TODO: it should be removed once get-next-bytes endpoint is ready

	// ETL
	ETL       = "etl"
	ETLInit   = Init
	ETLBuild  = "build"
	ETLList   = List
	ETLLogs   = "logs"
	ETLObject = "object"
	ETLStop   = Stop
	ETLHealth = "health"
)

const (
	Proxy  = "proxy"
	Target = "target"
)

// Compression enum
const (
	CompressAlways = "always"
	CompressNever  = "never"
)

// timeouts for intra-cluster requests
const (
	DefaultTimeout = time.Duration(-1)
	LongTimeout    = time.Duration(-2)
)

// metadata write policy
type MDWritePolicy string

func (mdw MDWritePolicy) IsImmediate() bool { return mdw == WriteDefault || mdw == WriteImmediate }

const (
	WriteImmediate = MDWritePolicy("immediate") // immediate write (default)
	WriteDelayed   = MDWritePolicy("delayed")   // cache and flush when not accessed for a while (lom_cache_hk.go)
	WriteNever     = MDWritePolicy("never")     // transient - in-memory only

	WriteDefault = MDWritePolicy("") // equivalent to immediate writing (WriteImmediate)
)

var (
	SupportedWritePolicy = []string{string(WriteImmediate), string(WriteDelayed), string(WriteNever)}
	SupportedCompression = []string{CompressNever, CompressAlways}
)
