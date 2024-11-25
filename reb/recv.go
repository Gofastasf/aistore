// Package reb provides global cluster-wide rebalance upon adding/removing storage nodes.
/*
 * Copyright (c) 2018-2024, NVIDIA CORPORATION. All rights reserved.
 */
package reb

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/ec"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/transport"
)

// TODO: currently, cannot return errors from the receive handlers, here and elsewhere
//       (see `_regRecv` for "static lifecycle")

func (reb *Reb) _recvErr(err error) error {
	if err == nil {
		return nil
	}
	if xreb := reb.xctn(); xreb != nil {
		xreb.Abort(err)
	}
	return nil
}

func (reb *Reb) recvObj(hdr *transport.ObjHdr, objReader io.Reader, err error) error {
	defer transport.DrainAndFreeReader(objReader)
	if err != nil {
		nlog.Errorln(err)
		return err
	}
	reb.lastrx.Store(mono.NanoTime())

	smap, err := reb._waitForSmap()
	if err != nil {
		return reb._recvErr(err)
	}
	unpacker := cos.NewUnpacker(hdr.Opaque)
	act, err := unpacker.ReadByte()
	if err != nil {
		nlog.Errorf("g[%d]: failed to recv recv-obj action (regular or EC): %v", reb.RebID(), err)
		return reb._recvErr(err)
	}
	if act == rebMsgRegular {
		err := reb.recvObjRegular(hdr, smap, unpacker, objReader)
		return reb._recvErr(err)
	}
	debug.Assertf(act == rebMsgEC, "act=%d", act)
	err = reb.recvECData(hdr, unpacker, objReader)
	return reb._recvErr(err)
}

func (reb *Reb) recvAck(hdr *transport.ObjHdr, _ io.Reader, err error) error {
	if err != nil {
		nlog.Errorln(err)
		return err
	}
	reb.lastrx.Store(mono.NanoTime())

	unpacker := cos.NewUnpacker(hdr.Opaque)
	act, err := unpacker.ReadByte()
	switch {
	case err != nil:
		err := fmt.Errorf("g[%d]: failed to read ACK message type: %v", reb.RebID(), err)
		return reb._recvErr(err)
	case act == rebMsgEC:
		err := reb.recvECAck(hdr, unpacker)
		return reb._recvErr(err)
	case act == rebMsgRegular:
		err := reb.recvRegularAck(hdr, unpacker)
		return reb._recvErr(err)
	default:
		err := fmt.Errorf("g[%d]: invalid ACK message type '%d' (expecting '%d')", reb.RebID(), act, rebMsgRegular)
		return reb._recvErr(err)
	}
}

func (reb *Reb) recvStageNtfn(hdr *transport.ObjHdr, _ io.Reader, errRx error) error {
	if errRx != nil {
		nlog.Errorf("%s g[%d]: stage err %v", core.T, reb.RebID(), errRx)
		return errRx
	}
	ntfn, err := reb.decodeStageNtfn(hdr.Opaque)
	if err != nil {
		return reb._recvErr(err)
	}

	reb.lastrx.Store(mono.NanoTime())

	var (
		rebID      = reb.RebID()
		rsmap      = reb.smap.Load()
		otherStage = stages[ntfn.stage]
		xreb       = reb.xctn()
	)
	if xreb == nil {
		if reb.stages.stage.Load() != rebStageInactive {
			nlog.Errorln(reb.logHdr(rebID, rsmap), "nil rebalancing xaction")
		}
		return nil
	}
	if xreb.IsAborted() {
		return nil
	}

	// TODO: see "static lifecycle" comment above

	// eq
	if rebID == ntfn.rebID {
		reb.stages.setStage(ntfn.daemonID, ntfn.stage)
		if ntfn.stage == rebStageAbort {
			err := fmt.Errorf("abort stage notification from %s(%s)", meta.Tname(ntfn.daemonID), otherStage)
			xreb.Abort(cmn.NewErrAborted(xreb.Name(), reb.logHdr(rebID, rsmap), err))
		}
		return nil
	}
	// other's old
	if rebID > ntfn.rebID {
		nlog.Warningln(reb.logHdr(rebID, rsmap), "stage notification from",
			meta.Tname(ntfn.daemonID), "at stage", otherStage+":", reb.warnID(ntfn.rebID, ntfn.daemonID))
		return nil
	}

	xreb.Abort(cmn.NewErrAborted(xreb.Name(), reb.logHdr(rebID, rsmap), err))
	return nil
}

//
// regular (non-EC) receive
//

func (reb *Reb) recvObjRegular(hdr *transport.ObjHdr, smap *meta.Smap, unpacker *cos.ByteUnpack, objReader io.Reader) error {
	ack := &regularAck{}
	if err := unpacker.ReadAny(ack); err != nil {
		nlog.Errorf("g[%d]: failed to parse ACK: %v", reb.RebID(), err)
		return err
	}
	if ack.rebID != reb.RebID() {
		nlog.Warningln("received", hdr.Cname(), reb.warnID(ack.rebID, ack.daemonID))
		return nil
	}
	tsid := ack.daemonID // the sender
	// Rx
	lom := core.AllocLOM(hdr.ObjName)
	defer core.FreeLOM(lom)
	if err := lom.InitBck(&hdr.Bck); err != nil {
		nlog.Errorln(err)
		return nil
	}

	// log warn
	if stage := reb.stages.stage.Load(); stage >= rebStageFin {
		if stage > rebStageFin {
			warn := fmt.Sprintf("%s g[%d]: post stage-fin receive from %s %s (stage %s)", core.T, ack.rebID, meta.Tname(tsid), lom, stages[stage])
			nlog.Warningln(warn)
		}
	} else if stage < rebStageTraverse {
		nlog.Errorf("%s g[%d]: early receive from %s %s (stage %s)", core.T, reb.RebID(), meta.Tname(tsid), lom, stages[stage])
	}

	lom.CopyAttrs(&hdr.ObjAttrs, true /*skip-checksum*/) // see "PUT is a no-op"
	xreb := reb.xctn()
	if xreb.IsAborted() {
		return nil
	}
	params := core.AllocPutParams()
	{
		params.WorkTag = fs.WorkfilePut
		params.Reader = io.NopCloser(objReader)
		params.OWT = cmn.OwtRebalance
		params.Cksum = hdr.ObjAttrs.Cksum
		params.Atime = lom.Atime()
		params.Xact = xreb
	}
	erp := core.T.PutObject(lom, params)
	core.FreePutParams(params)
	if erp != nil {
		nlog.Errorln(erp)
		return erp
	}
	// stats
	xreb.InObjsAdd(1, hdr.ObjAttrs.Size)

	// ACK
	tsi := smap.GetTarget(tsid)
	if tsi == nil {
		err := fmt.Errorf("g[%d]: %s is not in the %s", reb.RebID(), meta.Tname(tsid), smap)
		nlog.Errorln(err)
		return err
	}
	if stage := reb.stages.stage.Load(); stage < rebStageFinStreams && stage != rebStageInactive {
		ack := &regularAck{rebID: reb.RebID(), daemonID: core.T.SID()}
		hdr.Opaque = ack.NewPack()
		hdr.ObjAttrs.Size = 0
		if err := reb.dm.ACK(hdr, nil, tsi); err != nil {
			nlog.Errorln(err)
			return err
		}
	}
	return nil
}

func (reb *Reb) recvRegularAck(hdr *transport.ObjHdr, unpacker *cos.ByteUnpack) error {
	ack := &regularAck{}
	if err := unpacker.ReadAny(ack); err != nil {
		return fmt.Errorf("g[%d]: failed to unpack regular ACK: %v", reb.RebID(), err)
	}
	if ack.rebID == 0 {
		return fmt.Errorf("g[%d]: invalid g[0] ACK from %s", reb.RebID(), meta.Tname(ack.daemonID))
	}
	if ack.rebID != reb.rebID.Load() {
		nlog.Warningln("ACK from", ack.daemonID, "[", reb.warnID(ack.rebID, ack.daemonID), "]")
		return nil
	}

	lom := core.AllocLOM(hdr.ObjName)
	if err := lom.InitBck(&hdr.Bck); err != nil {
		core.FreeLOM(lom)
		nlog.Errorln(err)
		return nil
	}

	// [NOTE]
	// - remove migrated object and copies (unless disallowed by feature flag)
	// - free pending (original) transmitted LOM
	reb.ackLomAck(lom)
	core.FreeLOM(lom)

	return nil
}

//
// EC receive
//

func (reb *Reb) recvECAck(hdr *transport.ObjHdr, unpacker *cos.ByteUnpack) (err error) {
	ack := &ecAck{}
	err = unpacker.ReadAny(ack)
	if err != nil {
		nlog.Errorf("g[%d]: failed to unpack EC ACK for %s: %v", reb.RebID(), hdr.Cname(), err)
	}
	return
}

// Receive MD update. Handling includes partially updating local information:
// only the list of daemons and the _main_ target.
func receiveMD(req *stageNtfn, hdr *transport.ObjHdr) error {
	ctMeta, err := core.NewCTFromBO(&hdr.Bck, hdr.ObjName, core.T.Bowner(), fs.ECMetaType)
	if err != nil {
		return err
	}
	md, err := ec.LoadMetadata(ctMeta.FQN())
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return err
	}
	if md.Generation != req.md.Generation {
		return nil
	}
	md.FullReplica = req.md.FullReplica
	md.Daemons = req.md.Daemons
	mdBytes := md.NewPack()

	return ctMeta.Write(bytes.NewReader(mdBytes), -1, "" /*work fqn*/)
}

func (reb *Reb) receiveCT(req *stageNtfn, hdr *transport.ObjHdr, reader io.Reader) error {
	ct, err := core.NewCTFromBO(&hdr.Bck, hdr.ObjName, core.T.Bowner(), fs.ECSliceType)
	if err != nil {
		return err
	}
	md, err := detectLocalCT(req, ct)
	if err != nil {
		nlog.Errorf("%s: %v", ct.FQN(), err)
		return err
	}
	// Fix the metadata: update CT locations
	delete(req.md.Daemons, req.daemonID)
	if md != nil && req.md.Generation < md.Generation {
		// Local CT is newer - do not save anything
		return nil
	}
	// Check for slice conflict
	workFQN, moveTo, err := reb.renameLocalCT(req, ct, md)
	if err != nil {
		return err
	}
	req.md.FullReplica = core.T.SID()
	req.md.Daemons[core.T.SID()] = uint16(req.md.SliceID)
	if moveTo != nil {
		req.md.Daemons[moveTo.ID()] = uint16(md.SliceID)
	}
	// Save received CT to local drives
	err = reb.saveCTToDisk(req, hdr, reader)
	if err != nil {
		if errRm := cos.RemoveFile(ct.FQN()); errRm != nil {
			nlog.Errorln(err, "nested err: failed to remove", ct.FQN(), "[", errRm, "]")
		}
		if moveTo != nil {
			if errMv := os.Rename(workFQN, ct.FQN()); errMv != nil {
				nlog.Errorln(err, "nested err: failed to rename slice", ct.FQN(), "[", errMv, "]")
			}
		}
		return err
	}
	// Send local slice
	if moveTo != nil {
		req.md.SliceID = md.SliceID
		if err = reb.sendFromDisk(ct, req.md, moveTo, workFQN); err != nil {
			nlog.Errorln("failed to move slice to", moveTo, "[", err, "]")
		}
	}
	// Broadcast updated MD
	ntfnMD := stageNtfn{daemonID: core.T.SID(), stage: rebStageTraverse, rebID: reb.rebID.Load(), md: req.md, action: rebActUpdateMD}
	nodes := req.md.RemoteTargets()

	err = nil // keep the first errSend (TODO: count failures)
	for _, tsi := range nodes {
		if moveTo != nil && moveTo.ID() == tsi.ID() {
			continue
		}
		xreb := reb.xctn()
		if xreb.IsAborted() {
			break
		}
		o := transport.AllocSend()
		o.Hdr = transport.ObjHdr{ObjName: ct.ObjectName(), ObjAttrs: cmn.ObjAttrs{Size: 0}}
		o.Hdr.Bck.Copy(ct.Bck().Bucket())
		o.Hdr.Opaque = ntfnMD.NewPack(rebMsgEC)
		if errSend := reb.dm.Send(o, nil, tsi); errSend != nil && err == nil {
			// TODO: consider r.AddErr(errSend)
			err = fmt.Errorf("%s %s: failed to send updated EC MD: %v", core.T, xreb.ID(), err)
		}
	}
	return err
}

// receiving EC CT
func (reb *Reb) recvECData(hdr *transport.ObjHdr, unpacker *cos.ByteUnpack, reader io.Reader) error {
	req := &stageNtfn{}
	err := unpacker.ReadAny(req)
	if err != nil {
		err = fmt.Errorf("%s recvECData: invalid stage notification from t[%s] for %s: %v", core.T, hdr.SID, hdr.Cname(), err)
		return err
	}
	if req.rebID != reb.rebID.Load() {
		nlog.Warningf("%s: not yet started or already finished rebalancing (%d, %d) - dropping EC MD for %s from t[%s]",
			core.T, req.rebID, reb.rebID.Load(), hdr.Cname(), hdr.SID)
		return nil
	}
	if req.action == rebActUpdateMD {
		err := receiveMD(req, hdr)
		if err != nil {
			nlog.Errorf("Warning: %s g[%d]: failed to receive EC MD from t[%s] for %s: [%v]", core.T, req.rebID, hdr.SID, hdr.Cname(), err)
		}
		return nil
	}
	if err := reb.receiveCT(req, hdr, reader); err != nil {
		err = fmt.Errorf("%s g[%d]: failed to receive CT from t[%s] for %s: %v", core.T, req.rebID, hdr.SID, hdr.Cname(), err)
		nlog.Errorln(err)
		return err
	}
	return nil
}
