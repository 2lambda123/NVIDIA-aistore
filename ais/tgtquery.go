// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/nl"
	"github.com/NVIDIA/aistore/query"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xaction/xreg"
)

// There are 3 methods exposed by targets:
// * Peek(n): get next n objects from a target query, but keep the results in memory.
//   Subsequent Peek(n) request returns the same objects.
// * Discard(n): forget first n elements from a target query.
// * Next(n): Peek(n) + Discard(n)

func (t *targetrunner) queryHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		t.httpqueryget(w, r)
	case http.MethodPost:
		t.httpquerypost(w, r)
	case http.MethodPut:
		t.httpqueryput(w, r)
	default:
		cmn.InvalidHandlerWithMsg(w, r, "invalid method for /query path")
	}
}

func (t *targetrunner) httpquerypost(w http.ResponseWriter, r *http.Request) {
	if _, err := t.checkRESTItems(w, r, 0, false, cmn.URLPathQueryInit.L); err != nil {
		return
	}
	var (
		handle = r.Header.Get(cmn.HeaderHandle) // TODO: should it be from header or from body?
		msg    = &query.InitMsg{}
	)
	if err := cmn.ReadJSON(w, r, msg); err != nil {
		return
	}

	q, err := query.NewQueryFromMsg(t, &msg.QueryMsg)
	if err != nil {
		t.writeErr(w, r, err)
		return
	}

	var (
		ctx = context.Background()
		// TODO: we should use `q` directly instead of passing everything in
		//  additional, redundant `SelectMsg`.
		smsg = &cmn.SelectMsg{
			UUID:   handle,
			Prefix: q.ObjectsSource.Prefix,
			Props:  q.Select.Props,
		}
	)
	if q.Cached {
		smsg.Flags = cmn.SelectCached
	}

	xact, isNew, err := xreg.RenewQuery(ctx, t, q, smsg)
	if err != nil {
		t.writeErr(w, r, err)
		return
	}
	if !isNew {
		return
	}

	xact.AddNotif(&xaction.NotifXact{
		NotifBase: nl.NotifBase{When: cluster.UponTerm, Dsts: []string{equalIC}, F: t.callerNotifyFin},
		Xact:      xact,
	})
	go xact.Run()
}

func (t *targetrunner) httpqueryget(w http.ResponseWriter, r *http.Request) {
	apiItems, err := t.checkRESTItems(w, r, 1, false, cmn.URLPathQuery.L)
	if err != nil {
		return
	}

	switch apiItems[0] {
	case cmn.Next, cmn.Peek:
		t.httpquerygetobjects(w, r)
	case cmn.WorkerOwner:
		t.httpquerygetworkertarget(w, r)
	default:
		t.writeErrf(w, r, "unknown path /%s/%s/%s", cmn.Version, cmn.Query, apiItems[0])
	}
}

// /v1/query/worker
// TODO: change an endpoint and use the logic when #833 is done
func (t *targetrunner) httpquerygetworkertarget(w http.ResponseWriter, _ *http.Request) {
	w.Write([]byte(t.si.DaemonID))
}

func (t *targetrunner) httpquerygetobjects(w http.ResponseWriter, r *http.Request) {
	var entries []*cmn.BucketEntry

	apiItems, err := t.checkRESTItems(w, r, 1, false, cmn.URLPathQuery.L)
	if err != nil {
		return
	}

	msg := &query.NextMsg{}
	if err := cmn.ReadJSON(w, r, msg); err != nil {
		return
	}
	if msg.Handle == "" {
		t.writeErr(w, r, errQueryHandle, http.StatusBadRequest)
		return
	}
	resultSet := query.Registry.Get(msg.Handle)
	if resultSet == nil {
		t.queryDoesntExist(w, r, msg.Handle)
		return
	}

	switch apiItems[0] {
	case cmn.Next:
		entries, err = resultSet.NextN(msg.Size)
	case cmn.Peek:
		entries, err = resultSet.PeekN(msg.Size)
	default:
		t.writeErrf(w, r, "invalid %s/%s/%s", cmn.Version, cmn.Query, apiItems[0])
		return
	}

	if err != nil && err != io.EOF {
		t.writeErr(w, r, err, http.StatusInternalServerError)
		return
	}

	objList := &cmn.BucketList{Entries: entries}
	if strings.Contains(r.Header.Get(cmn.HeaderAccept), cmn.ContentMsgPack) {
		t.writeMsgPack(w, r, objList, "query_objects")
		return
	}
	t.writeJSON(w, r, objList, "query_objects")
}

// v1/query/discard/handle/value
func (t *targetrunner) httpqueryput(w http.ResponseWriter, r *http.Request) {
	apiItems, err := t.checkRESTItems(w, r, 2, false, cmn.URLPathQueryDiscard.L)
	if err != nil {
		return
	}

	handle, value := apiItems[0], apiItems[1]
	resultSet := query.Registry.Get(handle)
	if resultSet == nil {
		t.queryDoesntExist(w, r, handle)
		return
	}

	resultSet.DiscardUntil(value)
}

func (t *targetrunner) queryDoesntExist(w http.ResponseWriter, r *http.Request, handle string) {
	t.writeErrSilentf(w, r, http.StatusNotFound, "%s: handle %q not found", t.Sname(), handle)
}
