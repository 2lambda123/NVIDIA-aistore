// Package s3compat provides Amazon S3 compatibility layer
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package s3compat

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
)

const defaultLastModified = 0 // When an object was not accessed yet

type (
	// List objects response
	ListObjectResult struct {
		Ns                    string     `xml:"xmlns,attr"`
		Prefix                string     `xml:"Prefix"`
		KeyCount              int        `xml:"KeyCount"` // number of objects in the response
		MaxKeys               int        `xml:"MaxKeys"`
		IsTruncated           bool       `xml:"IsTruncated"`           // true if there are more pages to read
		ContinuationToken     string     `xml:"ContinuationToken"`     // original ContinuationToken
		NextContinuationToken string     `xml:"NextContinuationToken"` // NextContinuationToken to read the next page
		Contents              []*ObjInfo `xml:"Contents"`              // list of objects
	}
	ObjInfo struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		Class        string `xml:"StorageClass"`
	}

	// Response for object copy request
	CopyObjectResult struct {
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
	}
)

func FillMsgFromS3Query(query url.Values, msg *cmn.SelectMsg) {
	mxStr := query.Get("max-keys")
	if pageSize, err := strconv.Atoi(mxStr); err == nil && pageSize > 0 {
		msg.PageSize = uint(pageSize)
	}
	if prefix := query.Get("prefix"); prefix != "" {
		msg.Prefix = prefix
	}
	var token string
	if token = query.Get("continuation-token"); token != "" {
		msg.ContinuationToken = token
	}
	// start-after makes sense only on first call. For the next call,
	// when continuation-token is set, start-after is ignored
	if after := query.Get("start-after"); after != "" && token == "" {
		msg.StartAfter = after
	}
}

func NewListObjectResult() *ListObjectResult {
	return &ListObjectResult{
		Ns:       s3Namespace,
		MaxKeys:  1000,
		Contents: make([]*ObjInfo, 0),
	}
}

func (r *ListObjectResult) MustMarshal() []byte {
	b, err := xml.Marshal(r)
	cmn.AssertNoErr(err)
	return []byte(xml.Header + string(b))
}

func (r *ListObjectResult) Add(entry *cmn.BucketEntry, smsg *cmn.SelectMsg) {
	r.Contents = append(r.Contents, entryToS3(entry, smsg))
}

func entryToS3(entry *cmn.BucketEntry, smsg *cmn.SelectMsg) *ObjInfo {
	objInfo := &ObjInfo{
		Key:          entry.Name,
		LastModified: entry.Atime,
		ETag:         entry.Checksum,
		Size:         entry.Size,
	}
	// Some S3 clients do not tolerate empty or missing LastModified, so fill it
	// with a zero time if the object was not accessed yet
	if objInfo.LastModified == "" {
		objInfo.LastModified = cmn.FormatUnixNano(defaultLastModified, smsg.TimeFormat)
	}
	return objInfo
}

func (r *ListObjectResult) FillFromAisBckList(bckList *cmn.BucketList, smsg *cmn.SelectMsg) {
	r.KeyCount = len(bckList.Entries)
	r.IsTruncated = bckList.ContinuationToken != ""
	r.ContinuationToken = bckList.ContinuationToken
	for _, e := range bckList.Entries {
		r.Add(e, smsg)
	}
}

func FormatTime(t time.Time) string {
	s := t.UTC().Format(time.RFC1123)
	return strings.Replace(s, "UTC", "GMT", 1) // expects: "%a, %d %b %Y %H:%M:%S GMT"
}

func lomCksum(lom *cluster.LOM) string {
	if v, exists := lom.GetCustomMD(cluster.SourceObjMD); exists && v == cluster.SourceAmazonObjMD {
		if v, exists := lom.GetCustomMD(cluster.MD5ObjMD); exists {
			return v
		}
	}
	if cksum := lom.Cksum(); cksum != nil && cksum.Type() == cmn.ChecksumMD5 {
		return cksum.Value()
	}
	return ""
}

func SetHeaderFromLOM(header http.Header, lom *cluster.LOM, size int64) {
	if cksumValue := lomCksum(lom); cksumValue != "" {
		header.Set(headerETag, cksumValue)
	}
	header.Set(headerAtime, FormatTime(lom.Atime()))
	header.Set(cmn.HeaderContentLength, strconv.FormatInt(size, 10))
	header.Set(cmn.HeaderContentType, cmn.ContentBinary)
	header.Set(headerVersion, lom.Version())
}

func SetETLHeader(header http.Header, lom *cluster.LOM) {
	header.Set(headerAtime, FormatTime(lom.Atime()))
	header.Set(cmn.HeaderContentType, cmn.ContentBinary)
	header.Set(headerVersion, lom.Version())
}

func (r *CopyObjectResult) MustMarshal() []byte {
	b, err := xml.Marshal(r)
	cmn.AssertNoErr(err)
	return []byte(xml.Header + string(b))
}
