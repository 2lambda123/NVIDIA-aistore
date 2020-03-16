// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
)

type (
	txn interface {
		// accessors
		uuid() string
		started(tm time.Time)
		String() string
		fired() (err error)
		fire(err error) // fires only once
		callback(caller string, msgInt *actionMsgInternal, err error, args ...interface{})
	}
	transactions struct {
		sync.Mutex
		t *targetrunner
		m map[string]txn // by txn.uuid
	}
	txnBase struct { // generic base
		txn
		sync.RWMutex
		uid       string
		start     time.Time
		action    string
		smapVer   int64
		kind      string
		bmdVer    int64
		initiator string
		err       error
	}
	txnBckBase struct {
		txnBase
		bck cluster.Bck
	}
	//
	// concrete transaction types
	//
	txnCreateBucket struct {
		txnBckBase
	}
	txnMakeNCopies struct {
		txnBckBase
		curCopies int64
		newCopies int64
	}
	txnSetBucketProps struct {
		txnBckBase
		bprops *cmn.BucketProps
		nprops *cmn.BucketProps
	}
	txnRenameBucket struct {
		txnBckBase
		bckFrom *cluster.Bck
		bckTo   *cluster.Bck
	}
)

var (
	errTxnTimeout = errors.New("timeout")
	errNil        = errors.New("nil")
)

//////////////////
// transactions //
//////////////////

func (txns *transactions) init(t *targetrunner) {
	txns.t = t
	txns.m = make(map[string]txn, 4)
}

func (txns *transactions) begin(txn txn) error {
	txns.Lock()
	defer txns.Unlock()
	if x, ok := txns.m[txn.uuid()]; ok {
		return fmt.Errorf("%s: %s already started (duplicate uuid?)", txns.t.si, x)
	}
	txn.started(time.Now())
	txns.m[txn.uuid()] = txn
	return nil
}

func (txns *transactions) find(uuid string, remove bool) (txn txn, err error) {
	var ok bool
	txns.Lock()
	if txn, ok = txns.m[uuid]; !ok {
		err = fmt.Errorf("%s: Txn[%s] doesn't exist (aborted?)", txns.t.si, uuid)
	} else if remove {
		delete(txns.m, uuid)
	}
	txns.Unlock()
	return
}

func (txns *transactions) callback(caller string, msgInt *actionMsgInternal, err error, args ...interface{}) {
	txns.Lock()
	for _, txn := range txns.m {
		if err := txn.fired(); err != errNil {
			continue // only once
		}
		txn.callback(caller, msgInt, err, args...)
	}
	txns.Unlock()
}

// given txn, wait for its completion, handle timeout, and ultimately remove
func (txns *transactions) wait(txn txn, timeout time.Duration) (err error) {
	sleep := cmn.MinDuration(100*time.Millisecond, timeout/10)
	for i := sleep; i < timeout; i += sleep {
		if err = txn.fired(); err != errNil {
			txns.find(txn.uuid(), true /* remove */)
			return
		}
		// aborted?
		if _, err = txns.find(txn.uuid(), false); err != nil {
			return
		}
		time.Sleep(sleep)
	}
	txns.find(txn.uuid(), true /* remove */)
	return errTxnTimeout
}

// TODO -- FIXME: register with hk to cleanup orphaned transactions

/////////////
// txnBase //
/////////////

func (txn *txnBase) uuid() string         { return txn.uid }
func (txn *txnBase) started(tm time.Time) { txn.start = tm }

func (txn *txnBase) fired() (err error) {
	txn.RLock()
	err = txn.err
	txn.RUnlock()
	return
}

func (txn *txnBase) fire(err error) {
	txn.Lock()
	txn.err = err
	txn.Unlock()
}

////////////////
// txnBckBase //
////////////////

func (txn *txnBckBase) String() string {
	tm := cmn.FormatTimestamp(txn.start)
	return fmt.Sprintf("txn-%s[%s-(v%d, v%d)-%s-%s-%s], bucket %s",
		txn.kind, txn.uid, txn.smapVer, txn.bmdVer, txn.action, txn.initiator, tm, txn.bck.Name)
}

func (txn *txnBckBase) callback(caller string, msgInt *actionMsgInternal, err error, args ...interface{}) {
	if txn.initiator != caller || msgInt.TxnID != txn.uuid() {
		return
	}
	bmd, _ := args[0].(*bucketMD)
	cmn.Assert(bmd.version() > txn.bmdVer)
	txn.fire(err)
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%s: callback fired (BMD v%d, err %v)", txn, bmd.version(), err)
	}
}

/////////////////////
// txnCreateBucket //
/////////////////////

var _ txn = &txnCreateBucket{}

// c-tor
func newTxnCreateBucket(uuid, action string, smapVer, bmdVer int64, initiator string, bck *cluster.Bck) *txnCreateBucket {
	return &txnCreateBucket{
		txnBckBase{
			txnBase{
				uid:       uuid,
				action:    action,
				smapVer:   smapVer,
				kind:      "crb",
				bmdVer:    bmdVer,
				initiator: initiator,
				err:       errNil, // NOTE: another kind of nil (here and elsewhere)
			},
			*bck,
		},
	}
}

////////////////////
// txnMakeNCopies //
////////////////////

var _ txn = &txnMakeNCopies{}

// c-tor
func newTxnMakeNCopies(uuid, action string, smapVer, bmdVer int64, initiator string, bck *cluster.Bck, c, n int64) *txnMakeNCopies {
	return &txnMakeNCopies{
		txnBckBase{
			txnBase{
				uid:       uuid,
				action:    action,
				smapVer:   smapVer,
				kind:      "mnc",
				bmdVer:    bmdVer,
				initiator: initiator,
				err:       errNil,
			},
			*bck,
		},
		c,
		n,
	}
}

func (txn *txnMakeNCopies) String() string {
	s := txn.txnBckBase.String()
	return fmt.Sprintf("%s, copies %d => %d", s, txn.curCopies, txn.newCopies)
}

///////////////////////
// txnSetBucketProps //
///////////////////////

var _ txn = &txnSetBucketProps{}

// c-tor
func newTxnSetBucketProps(uuid, action string, smapVer, bmdVer int64, initiator string, bck *cluster.Bck,
	nprops *cmn.BucketProps) *txnSetBucketProps {
	cmn.Assert(bck.Props != nil)
	bprops := bck.Props.Clone()
	return &txnSetBucketProps{
		txnBckBase{
			txnBase{
				uid:       uuid,
				action:    action,
				smapVer:   smapVer,
				kind:      "mnc",
				bmdVer:    bmdVer,
				initiator: initiator,
				err:       errNil,
			},
			*bck,
		},
		bprops,
		nprops,
	}
}

/////////////////////
// txnRenameBucket //
/////////////////////

var _ txn = &txnRenameBucket{}

// c-tor
func newTxnRenameBucket(uuid, action string, smapVer, bmdVer int64, initiator string,
	bckFrom, bckTo *cluster.Bck) *txnRenameBucket {
	return &txnRenameBucket{
		txnBckBase{
			txnBase{
				uid:       uuid,
				action:    action,
				smapVer:   smapVer,
				kind:      "rnb",
				bmdVer:    bmdVer,
				initiator: initiator,
				err:       errNil,
			},
			*bckFrom,
		},
		bckFrom,
		bckTo,
	}
}

///////////////////
// txnCopyBucket //
///////////////////
