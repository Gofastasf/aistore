// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2024, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"net/http"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/tracing"
)

type global struct {
	client struct {
		control *http.Client // http client for intra-cluster comm
		data    *http.Client // http client to execute target <=> target GET & PUT (object)
	}
	netServ struct {
		pub      *netServer
		control  *netServer
		data     *netServer
		pubExtra []*netServer
	}
}

var g global

func handlePub(path string, handler func(http.ResponseWriter, *http.Request)) {
	for _, v := range htverbs {
		g.netServ.pub.muxers[v].HandleFunc(path, handler)
		if !cos.IsLastB(path, '/') {
			g.netServ.pub.muxers[v].HandleFunc(path+"/", handler)
		}
	}
}

func handleControl(path string, handler func(http.ResponseWriter, *http.Request)) {
	for _, v := range htverbs {
		g.netServ.control.muxers[v].HandleFunc(path, handler)
		if !cos.IsLastB(path, '/') {
			g.netServ.control.muxers[v].HandleFunc(path+"/", handler)
		}
	}
}

func handleData(path string, handler func(http.ResponseWriter, *http.Request)) {
	for _, v := range htverbs {
		g.netServ.data.muxers[v].HandleFunc(path, handler)
		if !cos.IsLastB(path, '/') {
			g.netServ.data.muxers[v].HandleFunc(path+"/", handler)
		}
	}
}

func initCtrlClient(config *cmn.Config) {
	const (
		defaultControlWriteBufferSize = 16 * cos.KiB // for more defaults see cmn/network.go
		defaultControlReadBufferSize  = 16 * cos.KiB
	)
	cargs := cmn.TransportArgs{
		Timeout:          config.Client.Timeout.D(),
		WriteBufferSize:  defaultControlWriteBufferSize,
		ReadBufferSize:   defaultControlReadBufferSize,
		IdleConnTimeout:  config.Net.HTTP.IdleConnTimeout.D(),
		IdleConnsPerHost: config.Net.HTTP.MaxIdleConnsPerHost,
		MaxIdleConns:     config.Net.HTTP.MaxIdleConns,
	}
	if config.Net.HTTP.UseHTTPS {
		g.client.control = cmn.NewIntraClientTLS(cargs, config)
	} else {
		g.client.control = cmn.NewClient(cargs)
	}
}

// wbuf/rbuf - when not configured use AIS defaults (to override the usual 4KB)
func initDataClient(config *cmn.Config) {
	wbuf, rbuf := config.Net.HTTP.WriteBufferSize, config.Net.HTTP.ReadBufferSize
	if wbuf == 0 {
		wbuf = cmn.DefaultWriteBufferSize
	}
	if rbuf == 0 {
		rbuf = cmn.DefaultReadBufferSize
	}
	cargs := cmn.TransportArgs{
		Timeout:          config.Client.TimeoutLong.D(),
		WriteBufferSize:  wbuf,
		ReadBufferSize:   rbuf,
		IdleConnTimeout:  config.Net.HTTP.IdleConnTimeout.D(),
		IdleConnsPerHost: config.Net.HTTP.MaxIdleConnsPerHost,
		MaxIdleConns:     config.Net.HTTP.MaxIdleConns,
	}
	if config.Net.HTTP.UseHTTPS {
		g.client.data = cmn.NewIntraClientTLS(cargs, config)
	} else {
		g.client.data = cmn.NewClient(cargs)
	}

	// The g.client.data is used for the AWS MPT/presigned URL features.
	// Enable tracing on the data client to capture traces for related AWS client calls.
	g.client.data = tracing.NewTraceableClient(g.client.data)
}

func shuthttp() {
	config := cmn.GCO.Get()
	g.netServ.pub.shutdown(config)
	for _, server := range g.netServ.pubExtra {
		server.shutdown(config)
	}
	if config.HostNet.UseIntraControl {
		g.netServ.control.shutdown(config)
	}
	if config.HostNet.UseIntraData {
		g.netServ.data.shutdown(config)
	}
}
