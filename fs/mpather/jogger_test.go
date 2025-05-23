// Package mpather provides per-mountpath concepts.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package mpather_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/fs/mpather"
	"github.com/NVIDIA/aistore/tools"
	"github.com/NVIDIA/aistore/tools/tassert"
)

func TestJoggerGroup(t *testing.T) {
	var (
		desc = tools.ObjectsDesc{
			CTs: []tools.ContentTypeDesc{
				{Type: fs.WorkfileType, ContentCnt: 10},
				{Type: fs.ObjectType, ContentCnt: 500},
			},
			MountpathsCnt: 10,
			ObjectSize:    cos.KiB,
		}
		out     = tools.PrepareObjects(t, desc)
		counter = atomic.NewInt32(0)
	)
	defer os.RemoveAll(out.Dir)

	opts := &mpather.JgroupOpts{
		Bck: out.Bck,
		CTs: []string{fs.ObjectType},
		VisitObj: func(_ *core.LOM, buf []byte) error {
			tassert.Errorf(t, len(buf) == 0, "buffer expected to be empty")
			counter.Inc()
			return nil
		},
	}
	jg := mpather.NewJoggerGroup(opts, cmn.GCO.Get(), nil)
	jg.Run()
	<-jg.ListenFinished()

	tassert.Errorf(
		t, int(counter.Load()) == len(out.FQNs[fs.ObjectType]),
		"invalid number of objects visited (%d vs %d)", counter.Load(), len(out.FQNs[fs.ObjectType]),
	)

	err := jg.Stop()
	tassert.CheckFatal(t, err)
}

func TestJoggerGroupLoad(t *testing.T) {
	var (
		desc = tools.ObjectsDesc{
			CTs: []tools.ContentTypeDesc{
				{Type: fs.WorkfileType, ContentCnt: 10},
				{Type: fs.ObjectType, ContentCnt: 500},
			},
			MountpathsCnt: 10,
			ObjectSize:    cos.KiB,
		}
		out     = tools.PrepareObjects(t, desc)
		counter = atomic.NewInt32(0)
	)
	defer os.RemoveAll(out.Dir)

	opts := &mpather.JgroupOpts{
		Bck: out.Bck,
		CTs: []string{fs.ObjectType},
		VisitObj: func(lom *core.LOM, buf []byte) error {
			tassert.Errorf(t, lom.Lsize() == desc.ObjectSize, "incorrect object size (lom probably not loaded)")
			tassert.Errorf(t, len(buf) == 0, "buffer expected to be empty")
			counter.Inc()
			return nil
		},
		DoLoad: mpather.Load,
	}
	jg := mpather.NewJoggerGroup(opts, cmn.GCO.Get(), nil)

	jg.Run()
	<-jg.ListenFinished()

	tassert.Errorf(
		t, int(counter.Load()) == len(out.FQNs[fs.ObjectType]),
		"invalid number of objects visited (%d vs %d)", counter.Load(), len(out.FQNs[fs.ObjectType]),
	)

	err := jg.Stop()
	tassert.CheckFatal(t, err)
}

func TestJoggerGroupError(t *testing.T) {
	var (
		desc = tools.ObjectsDesc{
			CTs: []tools.ContentTypeDesc{
				{Type: fs.ObjectType, ContentCnt: 50},
			},
			MountpathsCnt: 4,
			ObjectSize:    cos.KiB,
		}
		out     = tools.PrepareObjects(t, desc)
		counter = atomic.NewInt32(0)
	)
	defer os.RemoveAll(out.Dir)

	opts := &mpather.JgroupOpts{
		Bck: out.Bck,
		CTs: []string{fs.ObjectType},
		VisitObj: func(_ *core.LOM, _ []byte) error {
			counter.Inc()
			return errors.New("oops")
		},
	}
	jg := mpather.NewJoggerGroup(opts, cmn.GCO.Get(), nil)
	jg.Run()
	<-jg.ListenFinished()

	tassert.Errorf(
		t, int(counter.Load()) <= desc.MountpathsCnt,
		"joggers should not visit more than #mountpaths objects",
	)

	err := jg.Stop()
	tassert.Errorf(t, err != nil && strings.Contains(err.Error(), "oops"), "expected an error")
}

// This test checks if single LOM error will cause all joggers to stop.
func TestJoggerGroupOneErrorStopsAll(t *testing.T) {
	var (
		totalObjCnt = 5000
		mpathsCnt   = 4
		failAt      = int32(totalObjCnt/mpathsCnt) / 5 // Fail more or less at 20% of objects jogged.
		desc        = tools.ObjectsDesc{
			CTs: []tools.ContentTypeDesc{
				{Type: fs.ObjectType, ContentCnt: totalObjCnt},
			},
			MountpathsCnt: mpathsCnt,
			ObjectSize:    cos.KiB,
		}
		out = tools.PrepareObjects(t, desc)

		mpaths      = fs.GetAvail()
		counters    = make(map[string]*atomic.Int32, len(mpaths))
		failOnMpath *fs.Mountpath
		failed      atomic.Bool
	)
	defer os.RemoveAll(out.Dir)

	for _, failOnMpath = range mpaths {
		counters[failOnMpath.Path] = atomic.NewInt32(0)
	}

	opts := &mpather.JgroupOpts{
		Bck: out.Bck,
		CTs: []string{fs.ObjectType},
		VisitObj: func(lom *core.LOM, _ []byte) error {
			cnt := counters[lom.Mountpath().Path].Inc()

			// Fail only once, on one mpath.
			if cnt == failAt && failed.CAS(false, true) {
				failOnMpath = lom.Mountpath()
				return errors.New("oops")
			}
			return nil
		},
	}
	jg := mpather.NewJoggerGroup(opts, cmn.GCO.Get(), nil)
	jg.Run()
	<-jg.ListenFinished()

	for mpath, counter := range counters {
		// Expected at least one object to be skipped at each mountpath, when error occurred at 20% of objects jogged.
		visitCount := counter.Load()
		if mpath == failOnMpath.Path {
			tassert.Fatalf(t, visitCount == failAt, "jogger on fail mpath %q expected to visit %d: visited %d",
				mpath, failAt, visitCount)
		}
		tassert.Errorf(t, int(visitCount) <= out.MpathObjectsCnt[mpath],
			"jogger on mpath %q expected to visit at most %d, visited %d",
			mpath, out.MpathObjectsCnt[mpath], counter.Load())
	}

	err := jg.Stop()
	tassert.Errorf(t, err != nil && strings.Contains(err.Error(), "oops"), "expected an error")
}

func TestJoggerGroupMultiContentTypes(t *testing.T) {
	var (
		cts  = []string{fs.ObjectType, fs.ECSliceType, fs.ECMetaType}
		desc = tools.ObjectsDesc{
			CTs: []tools.ContentTypeDesc{
				{Type: fs.WorkfileType, ContentCnt: 10},
				{Type: fs.ObjectType, ContentCnt: 541},
				{Type: fs.ECSliceType, ContentCnt: 244},
				{Type: fs.ECMetaType, ContentCnt: 405},
			},
			MountpathsCnt: 10,
			ObjectSize:    cos.KiB,
		}
		out = tools.PrepareObjects(t, desc)
	)
	defer os.RemoveAll(out.Dir)

	counters := make(map[string]*atomic.Int32, len(cts))
	for _, ct := range cts {
		counters[ct] = atomic.NewInt32(0)
	}
	opts := &mpather.JgroupOpts{
		Bck: out.Bck,
		CTs: cts,
		VisitObj: func(_ *core.LOM, buf []byte) error {
			tassert.Errorf(t, len(buf) == 0, "buffer expected to be empty")
			counters[fs.ObjectType].Inc()
			return nil
		},
		VisitCT: func(ct *core.CT, buf []byte) error {
			tassert.Errorf(t, len(buf) == 0, "buffer expected to be empty")
			counters[ct.ContentType()].Inc()
			return nil
		},
	}
	jg := mpather.NewJoggerGroup(opts, cmn.GCO.Get(), nil)
	jg.Run()
	<-jg.ListenFinished()

	// NOTE: No need to check `fs.WorkfileType == 0` since we would get panic when
	//  increasing the counter (counter for `fs.WorkfileType` is not allocated).
	for _, ct := range cts {
		tassert.Errorf(
			t, int(counters[ct].Load()) == len(out.FQNs[ct]),
			"invalid number of %q visited (%d vs %d)", ct, counters[ct].Load(), len(out.FQNs[ct]),
		)
	}

	err := jg.Stop()
	tassert.CheckFatal(t, err)
}
