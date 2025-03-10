// Package xs is a collection of eXtended actions (xactions), including multi-object
// operations, list-objects, (cluster) rebalance and (target) resilver, ETL, and more.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package xs

import (
	"sync"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/xact/xreg"
)

// target i/f (ais/tgtimpl)
var (
	gcoi COI
)

// mem pool
var (
	coiPool sync.Pool
	coi0    CoiParams
)

// for additional startup-time reg-s see lru, downloader, ec
func Preg() {
	xreg.RegNonBckXact(&eleFactory{})
}

func Treg(coi COI) {
	xreg.RegNonBckXact(&eleFactory{})

	xreg.RegNonBckXact(&resFactory{})
	xreg.RegNonBckXact(&rebFactory{})
	xreg.RegNonBckXact(&etlFactory{})

	xreg.RegBckXact(&bmvFactory{})
	xreg.RegBckXact(&evdFactory{kind: apc.ActEvictObjects})
	xreg.RegBckXact(&evdFactory{kind: apc.ActDeleteObjects})
	xreg.RegBckXact(&prfFactory{})

	xreg.RegNonBckXact(&nsummFactory{})

	xreg.RegBckXact(&proFactory{})
	xreg.RegBckXact(&llcFactory{})

	gcoi = coi
	xreg.RegBckXact(&tcbFactory{kind: apc.ActCopyBck})
	xreg.RegBckXact(&tcbFactory{kind: apc.ActETLBck})
	xreg.RegBckXact(&tcoFactory{streamingF: streamingF{kind: apc.ActETLObjects}})
	xreg.RegBckXact(&tcoFactory{streamingF: streamingF{kind: apc.ActCopyObjects}})

	xreg.RegBckXact(&archFactory{streamingF: streamingF{kind: apc.ActArchive}})
	xreg.RegBckXact(&lsoFactory{streamingF: streamingF{kind: apc.ActList}})

	xreg.RegBckXact(&blobFactory{})
}

//
// CoiParams pool
//

func AllocCOI() (a *CoiParams) {
	if v := coiPool.Get(); v != nil {
		a = v.(*CoiParams)
		return
	}
	return &CoiParams{}
}

func FreeCOI(a *CoiParams) {
	*a = coi0
	coiPool.Put(a)
}
