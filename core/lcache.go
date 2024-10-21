// Package core provides core metadata and in-cluster API
/*
 * Copyright (c) 2018-2024, NVIDIA CORPORATION. All rights reserved.
 */
package core

import (
	"runtime"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/memsys"
)

// throttle tunables
const (
	throttleBatch = 0xf // each goroutine independently

	skipEvictThreashold = 20 // likely not running when above
	maxEvictThreashold  = 60 // never running when above

	maxTimeWithNoEvictions = 16 * time.Hour
)

type (
	// g.lchk
	lchk struct {
		timeout time.Duration
		last    time.Time
		total   atomic.Int64
		rc      atomic.Int32
		running atomic.Bool
	}
	// HK flush & evict
	evct struct {
		parent *lchk
		mi     *fs.Mountpath
		wg     *sync.WaitGroup
		now    time.Time
		d      time.Duration
		pct    int
		// runtime
		cache   *sync.Map
		evicted int64
	}
	// termination
	term struct {
		mi *fs.Mountpath
		wg *sync.WaitGroup
	}
	// uncache buckets
	rmbcks struct {
		mi   *fs.Mountpath
		wg   *sync.WaitGroup
		bcks []*meta.Bck
		pct  int
		// runtime
		nd    int
		cache *sync.Map
	}
)

// g.lchk
func (lchk *lchk) init(timeout time.Duration) {
	lchk.running.Store(false)
	lchk.timeout = timeout

	lchk.last = time.Now()
	hk.Reg("lcache"+hk.NameSuffix, lchk.housekeep, timeout)
}

// evict bucket
// TODO: consider dropping caches when > maxEvictThreashold; take in account time spent in
func UncacheBcks(wg *sync.WaitGroup, bcks ...*meta.Bck) bool {
	g.lchk.rc.Inc()
	defer g.lchk.rc.Dec()

	// mem pressure
	if g.lchk.mempDropAll() {
		return true // dropped all caches, nothing to do
	}

	var (
		avail = fs.GetAvail()
		pct   = _throttlePct()
	)
	nlog.Infoln("uncache:", bcks[0].String(), "throttle:", pct)
	if num := len(bcks); num > 1 {
		nlog.Infoln("uncache multiple:", bcks[1].String(), "..., num:", num)
	}
	if pct > maxEvictThreashold {
		nlog.Warningln("high utilization and/or load average:", pct)
	}
	for _, mi := range avail {
		wg.Add(1)
		u := &rmbcks{
			mi:   mi,
			wg:   wg,
			bcks: bcks,
			pct:  pct,
		}
		go u.do()
	}
	return false
}

func (u *rmbcks) do() {
	defer u.wg.Done()

	for idx := range cos.MultiHashMapCount {
		if !u.mi.IsAvail() {
			return
		}
		u.cache = u.mi.LomCaches.Get(idx)
		u.cache.Range(u.f)
	}
}

func (u *rmbcks) f(hkey, value any) bool {
	lmd := value.(*lmeta)
	if lmd.uname == nil {
		return true
	}
	b, _ := cmn.ParseUname(*lmd.uname)
	for _, rmb := range u.bcks {
		if !rmb.Eq(&b) {
			continue
		}
		lmd2, _ := u.cache.LoadAndDelete(hkey)
		if lmd2 == lmd {
			*lmd = lom0.md
		}
		// throttle
		u.nd++
		if u.nd&throttleBatch == throttleBatch {
			// compare with _throttle(evct.pct)
			// (and note: caller's waiting on wg)
			if u.pct >= maxEvictThreashold {
				time.Sleep(fs.Throttle10ms)
			} else {
				runtime.Gosched()
			}
		}
		break
	}
	return true
}

// evict mountpath (see also: mempDropAll)
func UncacheMountpath(mi *fs.Mountpath) {
	for idx := range cos.MultiHashMapCount {
		cache := mi.LomCaches.Get(idx)
		cache.Clear()
	}
}

func lcacheIdx(digest uint64) int { return int(digest & cos.MultiHashMapMask) }

//
// term
//

func (lchk *lchk) term() {
	var (
		avail = fs.GetAvail()
		wg    = &sync.WaitGroup{}
	)
	lchk.rc.Inc()
	defer lchk.rc.Dec()
	for _, mi := range avail {
		wg.Add(1)
		term := &term{
			mi: mi,
			wg: wg,
		}
		go term.do()
	}
	wg.Wait()
}

func (term *term) do() {
	defer term.wg.Done()
	for idx := range cos.MultiHashMapCount {
		cache := term.mi.LomCaches.Get(idx)
		cache.Range(term.f)
	}
}

func (*term) f(_, value any) bool {
	md := value.(*lmeta)
	if md.uname == nil {
		return true
	}
	lif := LIF{uname: *md.uname, lid: md.lid}
	lom, err := lif.LOM()
	if err != nil {
		return true
	}
	if lom.WritePolicy() == apc.WriteNever {
		return true
	}
	if md.Atime < 0 {
		// prefetched, not yet accessed
		mdTime := -md.Atime
		_flushAtime(md, time.Unix(0, mdTime), mdTime)
		return true
	}
	if md.isDirty() || md.atimefs != uint64(md.Atime) {
		_flushAtime(md, time.Unix(0, md.Atime), md.Atime)
	}
	return true
}

//
// HK evict
//

func (lchk *lchk) housekeep(int64) time.Duration {
	// refresh
	config := cmn.GCO.Get()
	lchk.timeout = config.Timeout.ObjectMD.D()

	// concurrent term, uncache-bck, etc.
	rc := lchk.rc.Load()
	if rc > 0 {
		nlog.Warningln("(not) running now, rc:", rc)
		return lchk.timeout
	}

	// mem pressure
	if lchk.mempDropAll() {
		return lchk.timeout
	}

	// load, utilization
	pct := _throttlePct()
	if pct > maxEvictThreashold {
		nlog.Warningln("not running: throttle [", pct, "greater than max", maxEvictThreashold, "]")
		return min(lchk.timeout>>1, time.Hour)
	}
	now := time.Now()
	if pct > skipEvictThreashold {
		if now.Sub(lchk.last) < min(maxTimeWithNoEvictions, max(lchk.timeout, time.Hour)*8) {
			nlog.Warningln("not running: throttle [", pct, "greater than", skipEvictThreashold, "]")
			return min(lchk.timeout>>1, time.Hour)
		}
	}

	// still running?
	if !lchk.running.CAS(false, true) {
		nlog.Warningln("(not) running now")
		return lchk.timeout
	}

	// finally, run
	nlog.Infoln("hk begin")
	lchk.last = now
	go lchk.evict(lchk.timeout, now, pct)

	return lchk.timeout
}

func (lchk *lchk) mempDropAll() bool /*dropped*/ {
	p := g.pmm.Pressure()
	switch p {
	case memsys.OOM, memsys.PressureExtreme:
		nlog.ErrorDepth(1, "oom [", p, "] - dropping all caches")
		lchk._drop()
		lchk.last = time.Now()
		return true
	case memsys.PressureHigh:
		nlog.Warningln("high memory pressure")
	}
	return false
}

func (*lchk) _drop() {
	avail := fs.GetAvail()
	for _, mi := range avail {
		UncacheMountpath(mi)
	}
}

func (lchk *lchk) evict(timeout time.Duration, now time.Time, pct int) {
	defer lchk.running.Store(false)
	var (
		avail   = fs.GetAvail()
		wg      = &sync.WaitGroup{}
		evicted = g.tstats.Get(LcacheEvictedCount)
	)
	lchk.total.Store(0)
	for _, mi := range avail {
		wg.Add(1)
		evct := &evct{
			parent: lchk,
			mi:     mi,
			wg:     wg,
			now:    now,
			d:      timeout,
			pct:    pct,
		}
		go evct.do()
	}
	wg.Wait()
	evicted = g.tstats.Get(LcacheEvictedCount) - evicted
	nlog.Infoln("hk done:", lchk.total.Load(), evicted)
}

func (evct *evct) do() {
	defer evct.wg.Done()
	for idx := range cos.MultiHashMapCount {
		if !evct.mi.IsAvail() {
			return
		}
		cache := evct.mi.LomCaches.Get(idx)
		evct.cache = cache
		cache.Range(evct.f)
		if evct.parent.rc.Load() > 0 {
			break
		}
	}
}

func (evct *evct) f(hkey, value any) bool {
	evct.parent.total.Inc()

	md := value.(*lmeta)
	mdTime := md.Atime
	if mdTime < 0 {
		// prefetched, not yet accessed
		mdTime = -mdTime
	}

	atime := time.Unix(0, mdTime)
	elapsed := evct.now.Sub(atime)
	if elapsed < evct.d {
		return evct.parent.rc.Load() == 0
	}

	// flush
	if md.isDirty() || md.atimefs != uint64(md.Atime) {
		_flushAtime(md, atime, mdTime)
	}

	// evict
	lmd2, _ := evct.cache.LoadAndDelete(hkey)
	if lmd2 == md {
		*md = lom0.md // zero out
	}
	g.tstats.Inc(LcacheEvictedCount)

	// throttle
	evct.evicted++
	if evct.evicted&throttleBatch == throttleBatch {
		_throttle(evct.pct)
	}

	return evct.parent.rc.Load() == 0
}

func _flushAtime(md *lmeta, atime time.Time, mdTime int64) {
	if md.uname == nil {
		return
	}
	lif := LIF{uname: *md.uname, lid: md.lid}
	lom, err := lif.LOM()
	if err != nil {
		return
	}
	if lom.WritePolicy() == apc.WriteNever {
		return
	}
	if err = lom.flushAtime(atime); err != nil {
		g.tstats.Inc(LcacheErrCount)
		T.FSHC(err, lom.Mountpath(), lom.FQN)
		return
	}

	// stats
	g.tstats.Inc(LcacheFlushColdCount)

	if !md.isDirty() {
		return
	}

	// special [dirty] case: clear and flush
	md.Atime = mdTime
	md.atimefs = uint64(mdTime)
	lom.md = *md

	buf := lom.pack()
	if err = fs.SetXattr(lom.FQN, XattrLOM, buf); err != nil {
		T.FSHC(err, lom.Mountpath(), lom.FQN)
	} else {
		for copyFQN := range lom.md.copies {
			if copyFQN == lom.FQN {
				continue
			}
			if err = fs.SetXattr(copyFQN, XattrLOM, buf); err != nil {
				g.tstats.Inc(LcacheErrCount)
				nlog.Errorln("set-xattr [", copyFQN, err, "]")
				break
			}
		}
	}
	g.smm.Free(buf)
	FreeLOM(lom)
}

//
// throttle
//

// [NOTE]:
// - artificially reducing `maxload` to maybe wait longer for truly idle ("nothing running") state
// - OTOH, see `maxTimeWithNoEvictions`
func _throttlePct() int {
	var (
		util, lavg = T.MaxUtilLoad()
		cpus       = runtime.NumCPU()
		maxload    = max((cpus>>1)-(cpus>>3), 1)
	)
	if lavg >= float64(maxload) {
		return 100
	}
	ru := cos.RatioPct(100, 2, util)
	rl := cos.RatioPct(int64(10*maxload), 1, int64(10*lavg))
	return int(max(ru, rl))
}

func _throttle(pct int) {
	if pct < 10 {
		runtime.Gosched()
	} else {
		time.Sleep(time.Duration(pct) * time.Millisecond)
	}
}
