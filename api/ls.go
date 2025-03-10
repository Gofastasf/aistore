// Package api provides native Go-based API/SDK over HTTP(S).
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package api

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
)

const (
	maxListPageRetries = 3

	msgpBufSize = 16 * cos.KiB
)

type (
	LsoCounter struct {
		callback  LsoCB
		startTime int64 // time operation started
		callAfter int64 // callback after
		count     int
		done      bool
	}
	LsoCB func(*LsoCounter)

	// additional and optional list-objects args (compare with: GetArgs, PutArgs)
	ListArgs struct {
		Callback  LsoCB
		Header    http.Header // to optimize listing very large buckets, e.g.: Header.Set(apc.HdrInventory, "true")
		CallAfter time.Duration
		Limit     int64
	}
)

// ListBuckets returns buckets for provided query, where
// - `fltPresence` is one of { apc.FltExists, apc.FltPresent, ... } - see api/apc/query.go
// - ListBuckets utilizes `cmn.QueryBcks` - control structure that's practically identical to `cmn.Bck`,
// except for the fact that some or all its fields can be empty (to facilitate the corresponding query).
// See also: QueryBuckets, ListObjects
func ListBuckets(bp BaseParams, qbck cmn.QueryBcks, fltPresence int) (cmn.Bcks, error) {
	q := make(url.Values, 4)
	q.Set(apc.QparamFltPresence, strconv.Itoa(fltPresence))
	qbck.AddToQuery(q)

	bp.Method = http.MethodGet
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = apc.URLPathBuckets.S
		// NOTE: bucket name
		// - qbck.IsBucket() to differentiate between list-objects and list-buckets (operations)
		// - list-buckets own correctness (see QueryBuckets below)
		reqParams.Body = cos.MustMarshal(apc.ActMsg{Action: apc.ActList, Name: qbck.Name})
		reqParams.Header = http.Header{cos.HdrContentType: []string{cos.ContentJSON}}
		reqParams.Query = q
	}
	bcks := cmn.Bcks{}
	_, err := reqParams.DoReqAny(&bcks)
	FreeRp(reqParams)
	if err != nil {
		return nil, err
	}
	return bcks, nil
}

// QueryBuckets is a little convenience helper. It returns true if the selection contains
// at least one bucket that satisfies the (qbck) criteria.
// - `fltPresence` - as per QparamFltPresence enum (see api/apc/query.go)
func QueryBuckets(bp BaseParams, qbck cmn.QueryBcks, fltPresence int) (bool, error) {
	bcks, err := ListBuckets(bp, qbck, fltPresence)
	return len(bcks) > 0, err
}

// ListObjects returns a list of objects in a bucket - a slice of structures in the
// `cmn.LsoRes` that look like `cmn.LsoEnt`.
//
// The `numObjects` argument is the maximum number of objects to be returned
// (where 0 (zero) means returning all objects in the bucket).
//
// This API supports numerous options and flags. In particular, `apc.LsoMsg`
// structure supports "opening" objects formatted as one of the supported
// archival types and include contents of archived directories in generated
// result sets.
//
// In addition, `lsmsg` (`apc.LsoMsg`) provides options (flags) to optimize
// the request's latency, to list anonymous public-access Cloud buckets, and more.
// Further details at `api/apc/lsmsg.go` source.
//
// AIS supports listing buckets that have millions of objects.
// For large and very large buckets, it is strongly recommended to use the
// `ListObjectsPage` API - effectively, an iterator returning _next_
// listed page along with associated _continuation token_.
//
// See also:
// - docs/cli/* for CLI usage examples
// - `apc.LsoMsg`
// - `api.ListObjectsPage`
func ListObjects(bp BaseParams, bck cmn.Bck, lsmsg *apc.LsoMsg, args ListArgs) (*cmn.LsoRes, error) {
	q := qalloc()
	reqParams := lsoReq(bp, bck, &args, q)
	if lsmsg == nil {
		lsmsg = &apc.LsoMsg{}
	} else {
		lsmsg.UUID, lsmsg.ContinuationToken = "", "" // new
	}
	lst, err := lso(reqParams, lsmsg, args)

	freeMbuf(reqParams.buf)
	FreeRp(reqParams)
	qfree(q)
	return lst, err
}

func lsoReq(bp BaseParams, bck cmn.Bck, args *ListArgs, q url.Values) *ReqParams {
	hdr := args.Header
	if hdr == nil {
		hdr = make(http.Header, 2)
	}

	// NOTE:
	// unlike S3 API (that aistore also provides), native Go-based API always utilizes
	// message pack serialization (of the list-objects results), with performance
	// improvement that proved to be _significant_, esp. in large-scale benchmarks

	hdr.Set(cos.HdrAccept, cos.ContentMsgPack)
	hdr.Set(cos.HdrContentType, cos.ContentJSON)
	bp.Method = http.MethodGet
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = apc.URLPathBuckets.Join(bck.Name)
		reqParams.Header = hdr
		bck.SetQuery(q)
		reqParams.Query = q
		reqParams.buf = allocMbuf() // msgpack
	}
	return reqParams
}

// `toRead` holds the remaining number of objects to list (that is, unless we are listing
// the entire bucket). Each iteration lists a page of objects and reduces `toRead`
// accordingly. When the latter gets below page size, we perform the final
// iteration for the reduced page.
func lso(reqParams *ReqParams, lsmsg *apc.LsoMsg, args ListArgs) (lst *cmn.LsoRes, _ error) {
	var (
		ctx     *LsoCounter
		toRead  = args.Limit
		listAll = args.Limit == 0
	)
	if args.Callback != nil {
		ctx = &LsoCounter{startTime: mono.NanoTime(), callback: args.Callback, count: -1}
		ctx.callAfter = ctx.startTime + args.CallAfter.Nanoseconds()
	}
	for pageNum := 1; listAll || toRead > 0; pageNum++ {
		if !listAll {
			lsmsg.PageSize = toRead
		}
		actMsg := apc.ActMsg{Action: apc.ActList, Value: lsmsg}
		reqParams.Body = cos.MustMarshal(actMsg)

		page, err := lsoPage(reqParams)
		if err != nil {
			return nil, err
		}
		if pageNum == 1 {
			lst = page
			lsmsg.UUID = page.UUID
			debug.Assert(cos.IsValidUUID(lst.UUID), lst.UUID)
		} else {
			lst.Entries = append(lst.Entries, page.Entries...)
			lst.ContinuationToken = page.ContinuationToken
			lst.Flags |= page.Flags
			debug.Assert(lst.UUID == page.UUID, lst.UUID, page.UUID)
		}
		if ctx != nil && ctx.mustCall() {
			ctx.count = len(lst.Entries)
			if page.ContinuationToken == "" {
				ctx.finish()
			}
			ctx.callback(ctx)
		}
		if page.ContinuationToken == "" { // listed all pages
			break
		}
		toRead = max(toRead-int64(len(page.Entries)), 0)
		lsmsg.ContinuationToken = page.ContinuationToken
	}
	return lst, nil
}

// w/ limited retry and increasing timeout
func lsoPage(reqParams *ReqParams) (_ *cmn.LsoRes, err error) {
	for range maxListPageRetries {
		page := &cmn.LsoRes{}
		if _, err = reqParams.DoReqAny(page); err == nil {
			return page, nil
		}
		if !cos.IsClientTimeout(err) {
			break
		}
		client := *reqParams.BaseParams.Client
		client.Timeout += client.Timeout >> 1
		reqParams.BaseParams.Client = &client
	}
	return nil, err
}

// ListObjectsPage returns the first page of bucket objects.
// On success the function updates `lsmsg.ContinuationToken` which client then can reuse
// to fetch the next page.
// See also:
// - docs/cli/* for CLI usage examples
// - `apc.LsoMsg`
// - `api.ListObjects`
func ListObjectsPage(bp BaseParams, bck cmn.Bck, lsmsg *apc.LsoMsg, args ListArgs) (*cmn.LsoRes, error) {
	q := qalloc()
	reqParams := lsoReq(bp, bck, &args, q)
	if lsmsg == nil {
		lsmsg = &apc.LsoMsg{}
	}
	actMsg := apc.ActMsg{Action: apc.ActList, Value: lsmsg}
	reqParams.Body = cos.MustMarshal(actMsg)

	// no need to preallocate bucket entries slice (msgpack does it)
	page := &cmn.LsoRes{}
	_, err := reqParams.DoReqAny(page)

	freeMbuf(reqParams.buf)
	FreeRp(reqParams)
	qfree(q)
	if err != nil {
		return nil, err
	}
	lsmsg.UUID = page.UUID
	lsmsg.ContinuationToken = page.ContinuationToken
	return page, nil
}

////////////////
// LsoCounter //
////////////////

func (ctx *LsoCounter) IsFinished() bool       { return ctx.done }
func (ctx *LsoCounter) Elapsed() time.Duration { return mono.Since(ctx.startTime) }
func (ctx *LsoCounter) Count() int             { return ctx.count }

// private

func (ctx *LsoCounter) mustCall() bool {
	return ctx.callAfter == ctx.startTime /*immediate*/ ||
		mono.NanoTime() >= ctx.callAfter
}

func (ctx *LsoCounter) finish() { ctx.done = true }
