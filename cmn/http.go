// Package cmn provides common constants, types, and utilities for AIS clients
// and AIStore.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/3rdparty/golang/mux"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	jsoniter "github.com/json-iterator/go"
)

const (
	HdrRange                 = "Range" // Ref: https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35
	HdrRangeValPrefix        = "bytes="
	HdrContentRange          = "Content-Range"
	HdrContentRangeValPrefix = "bytes " // Ref: https://tools.ietf.org/html/rfc7233#section-4.2
	HdrAcceptRanges          = "Accept-Ranges"
	HdrContentType           = "Content-Type"
	HdrContentLength         = "Content-Length"
	HdrAccept                = "Accept"
	HdrLocation              = "Location"
	HdrETag                  = "ETag" // Ref: https://developer.mozilla.org/en-US/docs/Web/HTTP/Hdrs/ETag
	HdrError                 = "Hdr-Error"
)

// Ref: https://www.iana.org/assignments/media-types/media-types.xhtml
const (
	ContentJSON    = "application/json"
	ContentMsgPack = "application/msgpack"
	ContentXML     = "application/xml"
	ContentBinary  = "application/octet-stream"
)

type (
	// HTTPRange specifies the byte range to be sent to the client.
	HTTPRange struct {
		Start, Length int64
	}

	RangesQuery struct {
		Range string
		Size  int64
	}

	// ReqArgs specifies HTTP request that we want to send.
	ReqArgs struct {
		Method string      // GET, POST, ...
		Header http.Header // request headers
		Base   string      // base URL: http://xyz.abc
		Path   string      // path URL: /x/y/z
		Query  url.Values  // query: ?a=x&b=y
		Body   []byte      // body for [POST, PUT, ...]
		// BodyR is an alternative to `Body` to avoid unnecessary allocations
		// when body for [POST, PUT, ...] is in stored `io.Reader`.
		// If non-nil and implements `io.Closer`, it will be closed by `client.Do`,
		// even on errors.
		BodyR io.Reader
	}

	HTTPMuxers map[string]*mux.ServeMux // by http.Method

	CallWithRetryArgs struct {
		Call    func() (int, error)
		IsFatal func(error) bool

		Action string
		Caller string

		SoftErr uint // How many retires on ConnectionRefused or ConnectionReset error.
		HardErr uint // How many retries on any other error.
		Sleep   time.Duration

		Verbosity int  // Determine the verbosity level.
		BackOff   bool // If requests should be retried less and less often.
		IsClient  bool // true: client (e.g. tutils, etc.)
	}
)

const (
	CallWithRetryLogVerbose = iota
	CallWithRetryLogQuiet
	CallWithRetryLogOff
)

// ErrNoOverlap is returned by serveContent's parseRange if first-byte-pos of
// all of the byte-range-spec values is greater than the content size.
var ErrNoOverlap = errors.New("invalid range: failed to overlap")

// PrependProtocol prepends protocol in URL in case it is missing.
// By default it adds `http://` as prefix to the URL.
func PrependProtocol(url string, protocol ...string) string {
	if url == "" || strings.Contains(url, "://") {
		return url
	}
	proto := httpProto
	if len(protocol) == 1 {
		proto = protocol[0]
	}
	return proto + "://" + url
}

// MatchRESTItems splits url path and matches the parts against specified `items`.
// If `splitAfter` is true all items will be split, otherwise the
// rest of the path will be split only to `itemsAfter` items.
// Returns all items that follow the specified `items`.
func MatchRESTItems(unescapedPath string, itemsAfter int, splitAfter bool, items []string) ([]string, error) {
	var split []string
	escaped := html.EscapeString(unescapedPath)
	if len(escaped) > 0 && escaped[0] == '/' {
		escaped = escaped[1:] // remove leading slash
	}
	if splitAfter {
		split = strings.Split(escaped, "/")
	} else {
		split = strings.SplitN(escaped, "/", len(items)+cos.Max(1, itemsAfter))
	}
	apiItems := split[:0] // filtering without allocation
	for _, item := range split {
		if item != "" { // omit empty
			apiItems = append(apiItems, item)
		}
	}
	if len(apiItems) < len(items) {
		return nil, fmt.Errorf("expected %d items, but got: %d", len(items), len(apiItems))
	}

	for idx, item := range items {
		if item != apiItems[idx] {
			return nil, fmt.Errorf("expected %s in path, but got: %s", item, apiItems[idx])
		}
	}

	apiItems = apiItems[len(items):]
	if len(apiItems) < itemsAfter {
		return nil, fmt.Errorf("path is too short: got %d items, but expected %d",
			len(apiItems)+len(items), itemsAfter+len(items))
	} else if len(apiItems) > itemsAfter && !splitAfter {
		return nil, fmt.Errorf("path is too long: got %d items, but expected %d",
			len(apiItems)+len(items), itemsAfter+len(items))
	}

	return apiItems, nil
}

func ReadBytes(r *http.Request) (b []byte, err error) {
	var e error

	b, e = io.ReadAll(r.Body)
	if e != nil {
		err = fmt.Errorf("failed to read %s request, err: %v", r.Method, e)
		if e == io.EOF {
			trailer := r.Trailer.Get("Error")
			if trailer != "" {
				err = fmt.Errorf("failed to read %s request, err: %v, trailer: %s", r.Method, e, trailer)
			}
		}
	}
	cos.Close(r.Body)

	return b, err
}

func ReadJSON(w http.ResponseWriter, r *http.Request, out interface{}, optional ...bool) error {
	defer cos.Close(r.Body)
	if err := jsoniter.NewDecoder(r.Body).Decode(out); err != nil {
		if len(optional) > 0 && optional[0] && err == io.EOF {
			return nil
		}
		s := fmt.Sprintf("failed to json-unmarshal %s request, err: %v [%T]", r.Method, err, out)
		if _, file, line, ok := runtime.Caller(1); ok {
			f := filepath.Base(file)
			s += fmt.Sprintf("(%s, #%d)", f, line)
		}
		WriteErrMsg(w, r, s)
		return err
	}
	return nil
}

// WriteJSON writes a struct or byte slice to an HTTP response.
// If `v` is a byte slice, it is passed as-is(already JSON-encoded data).
// In other cases, `v` is encoded to JSON and then passed.
// The function returns an error if writing to the response fails.
func WriteJSON(w http.ResponseWriter, v interface{}) error {
	w.Header().Set(HdrContentType, ContentJSON)
	if b, ok := v.([]byte); ok {
		_, err := w.Write(b)
		return err
	}
	return jsoniter.NewEncoder(w).Encode(v)
}

func (u *ReqArgs) URL() string {
	url := cos.JoinPath(u.Base, u.Path)
	query := u.Query.Encode()
	if query != "" {
		url += "?" + query
	}
	return url
}

func (u *ReqArgs) Req() (*http.Request, error) {
	r := u.BodyR
	if r == nil && u.Body != nil {
		r = bytes.NewBuffer(u.Body)
	}

	req, err := http.NewRequest(u.Method, u.URL(), r)
	if err != nil {
		return nil, err
	}

	if u.Header != nil {
		copyHeaders(u.Header, &req.Header)
	}
	return req, nil
}

// ReqWithCancel creates request with ability to cancel it.
func (u *ReqArgs) ReqWithCancel() (*http.Request, context.Context, context.CancelFunc, error) {
	req, err := u.Req()
	if err != nil {
		return nil, nil, nil, err
	}
	if u.Method == http.MethodPost || u.Method == http.MethodPut {
		req.Header.Set(HdrContentType, ContentJSON)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	return req, ctx, cancel, nil
}

func (u *ReqArgs) ReqWithTimeout(timeout time.Duration) (*http.Request, context.Context, context.CancelFunc, error) {
	req, err := u.Req()
	if err != nil {
		return nil, nil, nil, err
	}
	if u.Method == http.MethodPost || u.Method == http.MethodPut {
		req.Header.Set(HdrContentType, ContentJSON)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req = req.WithContext(ctx)
	return req, ctx, cancel, nil
}

// Copies headers from original request(from client) to
// a new one(inter-cluster call)
func copyHeaders(src http.Header, dst *http.Header) {
	for k, values := range src {
		for _, v := range values {
			dst.Set(k, v)
		}
	}
}

func (r HTTPRange) ContentRange(size int64) string {
	return fmt.Sprintf("%s%d-%d/%d", HdrContentRangeValPrefix, r.Start, r.Start+r.Length-1, size)
}

// ParseMultiRange parses a Range header string as per RFC 7233.
// ErrNoOverlap is returned if none of the ranges overlap.
func ParseMultiRange(s string, size int64) (ranges []HTTPRange, err error) {
	if s == "" {
		return nil, nil // header not present
	}

	if !strings.HasPrefix(s, HdrRangeValPrefix) {
		return nil, errors.New("invalid range")
	}

	noOverlap := false
	for _, ra := range strings.Split(s[len(HdrRangeValPrefix):], ",") {
		ra = strings.TrimSpace(ra)
		if ra == "" {
			continue
		}

		i := strings.Index(ra, "-")
		if i < 0 {
			return nil, errors.New("invalid range")
		}

		start, end := strings.TrimSpace(ra[:i]), strings.TrimSpace(ra[i+1:])

		var r HTTPRange
		if start == "" {
			// If no start is specified, end specifies the range start relative
			// to the end of the file, and we are dealing with <suffix-length>
			// which has to be a non-negative integer as per RFC 7233 Section 2.1 "Byte-Ranges".
			if end == "" || end[0] == '-' {
				return nil, errors.New("invalid range")
			}
			i, err := strconv.ParseInt(end, 10, 64)
			if i < 0 || err != nil {
				return nil, errors.New("invalid range")
			}

			if i > size {
				i = size
			}
			r.Start = size - i
			r.Length = size - r.Start
		} else {
			i, err := strconv.ParseInt(start, 10, 64)
			if err != nil || i < 0 {
				return nil, errors.New("invalid range")
			}

			if i >= size {
				// If the range begins after the size of the content,
				// then it does not overlap.
				noOverlap = true
				continue
			}

			r.Start = i
			if end == "" {
				// If no end is specified, range extends to end of the file.
				r.Length = size - r.Start
			} else {
				i, err := strconv.ParseInt(end, 10, 64)
				if err != nil || r.Start > i {
					return nil, errors.New("invalid range")
				}
				if i >= size {
					i = size - 1
				}
				r.Length = i - r.Start + 1
			}
		}
		ranges = append(ranges, r)
	}

	if noOverlap && len(ranges) == 0 {
		// The specified ranges did not overlap with the content.
		return nil, ErrNoOverlap
	}
	return ranges, nil
}

func RangeHdr(start, length int64) (hdr http.Header) {
	if start == 0 && length == 0 {
		return hdr
	}
	hdr = make(http.Header, 1)
	hdr.Add(HdrRange, fmt.Sprintf("%s%d-%d", HdrRangeValPrefix, start, start+length-1))
	return
}

func ToHTTPHdr(meta ObjHeaderMetaProvider, hdrs ...http.Header) (hdr http.Header) {
	if len(hdrs) == 0 || hdrs[0] == nil {
		hdr = make(http.Header, 4)
	} else {
		hdr = hdrs[0]
	}
	if !meta.Cksum().IsEmpty() {
		ty, val := meta.Cksum().Get()
		hdr.Set(HdrObjCksumType, ty)
		hdr.Set(HdrObjCksumVal, val)
	}
	if meta.AtimeUnix() != 0 {
		hdr.Set(HdrObjAtime, cos.UnixNano2S(meta.AtimeUnix()))
	}
	for k, v := range meta.CustomMD() {
		hdr.Add(HdrObjCustomMD, strings.Join([]string{k, v}, "="))
	}
	return hdr
}

////////////////
// HTTPMuxers //
////////////////

// interface guard
var _ http.Handler = HTTPMuxers{}

// ServeHTTP dispatches the request to the handler whose
// pattern most closely matches the request URL.
func (m HTTPMuxers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if sm, ok := m[r.Method]; ok {
		sm.ServeHTTP(w, r)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
}

func NetworkCallWithRetry(args *CallWithRetryArgs) (err error) {
	debug.Assert(args.SoftErr > 0 || args.HardErr > 0)

	if args.Sleep == 0 {
		if args.IsClient {
			args.Sleep = time.Second / 2
		} else {
			config := GCO.Get()
			debug.Assert(config.Timeout.CplaneOperation > 0)
			args.Sleep = config.Timeout.CplaneOperation / 4
		}
	}

	callerStr := ""
	if args.Caller != "" {
		callerStr = args.Caller + ": "
	}
	if args.Action == "" {
		args.Action = "call"
	}

	var (
		sleep                        = args.Sleep
		hardErrCnt, softErrCnt, iter uint
		nonEmptyErr                  error
	)
	for hardErrCnt, softErrCnt, iter = uint(0), uint(0), uint(1); ; iter++ {
		var status int
		if status, err = args.Call(); err == nil {
			if args.Verbosity < CallWithRetryLogOff && (hardErrCnt > 0 || softErrCnt > 0) {
				glog.Warningf(
					"%s Successful %s, after errors (softErr: %d, hardErr: %d, last err: %v)",
					callerStr, args.Action, softErrCnt, hardErrCnt, nonEmptyErr)
			}
			return
		}
		nonEmptyErr = err
		if args.IsFatal != nil && args.IsFatal(err) {
			return
		}

		if args.Verbosity < CallWithRetryLogQuiet {
			glog.Errorf("%s Failed to %s, err: %v (iter: %d, status code: %d)", callerStr, args.Action, err, iter, status)
		}
		if IsErrConnectionRefused(err) || IsErrConnectionReset(err) {
			softErrCnt++
		} else {
			hardErrCnt++
		}
		if args.BackOff && iter > 1 {
			if args.IsClient {
				sleep = cos.MinDuration(sleep+(args.Sleep/2), 4*time.Second)
			} else {
				config := GCO.Get()
				debug.Assert(config.Timeout.MaxKeepalive > 0)
				sleep = cos.MinDuration(sleep+(args.Sleep/2), config.Timeout.MaxKeepalive)
			}
		}
		if hardErrCnt > args.HardErr || softErrCnt > args.SoftErr {
			break
		}
		time.Sleep(sleep)
	}

	// Just print once summary of the errors. No need to repeat the log for Verbose setting.
	if args.Verbosity == CallWithRetryLogQuiet {
		glog.Errorf("%sFailed to %s (softErr: %d, hardErr: %d, last err: %v)", callerStr, args.Action, softErrCnt, hardErrCnt, err)
	}
	return
}
