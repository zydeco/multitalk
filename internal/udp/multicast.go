//
// Copyright (c) 2009-2020 Rob Braun <bbraun@synack.net> and others
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions
// are met:
// 1. Redistributions of source code must retain the above copyright
//    notice, this list of conditions and the following disclaimer.
// 2. Redistributions in binary form must reproduce the above copyright
//    notice, this list of conditions and the following disclaimer in the
//    documentation and/or other materials provided with the distribution.
// 3. Neither the name of Rob Braun nor the names of his contributors
//    may be used to endorse or promote products derived from this software
//    without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
// AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
// IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
// ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
// LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
// CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
// SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
// INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
// CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
// ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
// POSSIBILITY OF SUCH DAMAGE.
//
package udp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"sync"

	"github.com/sfiera/multitalk/pkg/aarp"
	"github.com/sfiera/multitalk/pkg/ddp"
	"github.com/sfiera/multitalk/pkg/ethernet"
	"github.com/sfiera/multitalk/pkg/ethertalk"
)

var (
	address = &net.UDPAddr{
		IP:   net.ParseIP("239.192.76.84"),
		Port: 1954,
	}

	defaultNet = uint16(0xff00)
)

type (
	LTOUHeader struct {
		Pid              uint32
		DstNode, SrcNode uint8
		Kind             uint8
	}
	LTOUPacket struct {
		LTOUHeader
		Data []byte
	}

	bridge struct {
		pid   int
		iface *net.Interface
		eth   ethernet.Addr
		conn  *net.UDPConn

		nodes   map[uint8]bool
		nodesMu sync.Mutex
	}
)

func Multicast(iface string) (
	send chan<- ethertalk.Packet,
	recv <-chan ethertalk.Packet,
	_ error,
) {
	i, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, nil, fmt.Errorf("interface %s: %s", iface, err.Error())
	}

	b := bridge{
		pid:   os.Getpid(),
		iface: i,
		nodes: map[uint8]bool{},
	}
	copy(b.eth[:], i.HardwareAddr)

	b.conn, err = net.ListenMulticastUDP("udp", i, address)
	if err != nil {
		return nil, nil, fmt.Errorf("listen %s: %s", iface, err.Error())
	}

	sendCh := make(chan ethertalk.Packet)
	recvCh := make(chan ethertalk.Packet)

	go b.recv(sendCh, recvCh)
	go b.send(recvCh)
	return sendCh, recvCh, nil
}

func (b *bridge) recv(sendCh <-chan ethertalk.Packet, recvCh chan<- ethertalk.Packet) {
	for packet := range sendCh {
		conv, resp := b.etherTalkToUDP(packet)
		if resp != nil {
			recvCh <- *resp
			continue
		} else if conv == nil {
			fmt.Fprintf(os.Stderr, "send udp: conversion failed\n")
			continue
		}

		buf := bytes.NewBuffer([]byte{})

		err := binary.Write(buf, binary.BigEndian, conv.LTOUHeader)
		if err != nil {
			fmt.Fprintf(os.Stderr, "write udp header: %s\n", err.Error())
			continue
		}

		n, err := buf.Write(conv.Data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "write udp body: %s\n", err.Error())
			continue
		} else if n < len(conv.Data) {
			fmt.Fprintf(os.Stderr, "write udp body: incomplete write (%d < %d)\n", n, len(conv.Data))
			continue
		}

		_, err = b.conn.WriteToUDP(buf.Bytes(), address)
		if err != nil {
			fmt.Fprintf(os.Stderr, "send udp: %s\n", err.Error())
		}
	}
}

func (b *bridge) etherTalkToUDP(packet ethertalk.Packet) (
	converted *LTOUPacket,
	response *ethertalk.Packet,
) {
	switch packet.SNAPProto {
	case ethertalk.AppleTalkProto:
		return b.ddpToUDP(packet)
	case ethertalk.AARPProto:
		return b.aarpToUDP(packet)
	default:
		return nil, nil
	}
}

func (b *bridge) ddpToUDP(packet ethertalk.Packet) (
	converted *LTOUPacket,
	response *ethertalk.Packet,
) {
	ext := ddp.ExtPacket{}
	err := ddp.ExtUnmarshal(packet.Data, &ext)
	if err != nil {
		return nil, nil
	}

	reg := ddp.Packet{
		Header: ddp.Header{
			Size:    5 + uint16(len(ext.Data)),
			DstPort: ext.DstPort,
			SrcPort: ext.SrcPort,
			Proto:   ext.Proto,
		},
		Data: ext.Data,
	}
	data, err := ddp.Marshal(reg)
	if err != nil {
		return nil, nil
	}

	return &LTOUPacket{
		LTOUHeader: LTOUHeader{
			Pid:     uint32(b.pid),
			DstNode: ext.DstNode,
			SrcNode: ext.SrcNode,
			Kind:    0x01,
		},
		Data: data,
	}, nil
}

func (b *bridge) aarpToUDP(packet ethertalk.Packet) (
	converted *LTOUPacket,
	response *ethertalk.Packet,
) {
	a := aarp.Packet{}
	err := aarp.Unmarshal(packet.Data, &a)
	if err != nil {
		return nil, nil
	}

	kind := uint8(0)
	switch a.Opcode {
	case aarp.ProbeOp:
		kind = 0x81 // “Is this AppleTalk node ID in use by anyone?”
	case aarp.ResponseOp:
		kind = 0x82 // “Yes, sorry, I’m already using that node ID.”

	case aarp.RequestOp:
		// Request to map an AppleTalk address to a hardware address (MAC).
		// Don’t translate to UDP, since there’s no corresponding request.
		// Check if the target machine is one that has broadcast UDP packets.
		// If it has, then report this machine’s hardware address as the
		// target for the queried AppleTalk address.
		if !b.isProxyForNode(a.Dst.Proto.Node) {
			return nil, nil
		}
		resp, err := ethertalk.AARP(b.eth, aarp.Response(aarp.AddrPair{
			Hardware: b.eth,
			Proto:    a.Dst.Proto,
		}, a.Src))
		if err != nil {
			return nil, nil
		}
		return nil, resp

	default:
		return nil, nil
	}

	return &LTOUPacket{
		LTOUHeader: LTOUHeader{
			Pid:     uint32(b.pid),
			DstNode: a.Dst.Proto.Node,
			SrcNode: a.Src.Proto.Node,
			Kind:    kind,
		},
	}, nil
}

func (b *bridge) isProxyForNode(node uint8) bool {
	b.nodesMu.Lock()
	defer b.nodesMu.Unlock()
	return b.nodes[node]
}

func (b *bridge) markProxyForNode(node uint8) {
	b.nodesMu.Lock()
	defer b.nodesMu.Unlock()
	b.nodes[node] = true
}

func (b *bridge) send(recvCh chan<- ethertalk.Packet) {
	bin := make([]byte, 700)
	for {
		n, addr, err := b.conn.ReadFromUDP(bin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "udp recv: %s\n", err.Error())
			os.Exit(1)
		}

		r := bytes.NewReader(bin[:n])
		packet := LTOUPacket{}
		err = binary.Read(r, binary.BigEndian, &packet.LTOUHeader)
		if err != nil {
			continue
		}

		if b.isSender(addr, packet) {
			// If this bridge sent the packet, avoid a loop by ignoring
			// it when it’s received back again via multicast.
			continue
		}

		packet.Data, err = ioutil.ReadAll(r)
		if err != nil {
			continue
		}

		out := b.udpToEtherTalk(addr, packet)
		if out != nil {
			b.markProxyForNode(packet.SrcNode)
			recvCh <- *out
		}
	}
}

func (b *bridge) isSender(from *net.UDPAddr, packet LTOUPacket) bool {
	if packet.Pid != uint32(b.pid) {
		return false
	}
	addrs, err := b.iface.Addrs()
	if err != nil {
		return true
	}
	for _, addr := range addrs {
		if ip, ok := addr.(*net.IPAddr); ok {
			if ip.IP.Equal(from.IP) {
				return true
			}
		}
	}
	return false
}

func (b *bridge) udpToEtherTalk(addr *net.UDPAddr, packet LTOUPacket) *ethertalk.Packet {
	switch packet.Kind {
	case 0x01:
		return b.udpToDDP(addr, packet)
	case 0x02:
		return b.udpToExtDDP(addr, packet)
	case 0x81:
		return b.udpToProbe(addr, packet)
	case 0x82:
		return b.udpToAck(addr, packet)
	default:
		return nil
	}
}

func (b *bridge) udpToDDP(addr *net.UDPAddr, packet LTOUPacket) *ethertalk.Packet {
	d := ddp.Packet{}
	err := ddp.Unmarshal(packet.Data, &d)
	if err != nil {
		return nil
	}

	ext := ddp.ExtPacket{
		ExtHeader: ddp.ExtHeader{
			Size:    d.Size + 8,
			DstNet:  0,
			DstNode: packet.DstNode,
			DstPort: d.DstPort,
			SrcNet:  defaultNet,
			SrcNode: packet.SrcNode,
			SrcPort: d.SrcPort,
			Proto:   d.Proto,
		},
		Data: d.Data,
	}

	out, err := ethertalk.AppleTalk(b.eth, ext)
	if err != nil {
		return nil
	}
	return out
}

func (b *bridge) udpToExtDDP(addr *net.UDPAddr, packet LTOUPacket) *ethertalk.Packet {
	d := ddp.ExtPacket{}
	err := ddp.ExtUnmarshal(packet.Data, &d)
	if err != nil {
		return nil
	}
	out, err := ethertalk.AppleTalk(b.eth, d)
	if err != nil {
		return nil
	}
	return out
}

func (b *bridge) udpToProbe(addr *net.UDPAddr, packet LTOUPacket) *ethertalk.Packet {
	out, err := ethertalk.AARP(
		b.eth,
		aarp.Probe(b.eth, aarp.AtalkAddr{Network: defaultNet, Node: packet.DstNode}),
	)
	if err != nil {
		return nil
	}
	return out
}

func (b *bridge) udpToAck(addr *net.UDPAddr, packet LTOUPacket) *ethertalk.Packet {
	out, err := ethertalk.AARP(b.eth, aarp.Response(
		aarp.AddrPair{
			Hardware: b.eth,
			Proto:    aarp.AtalkAddr{Network: defaultNet, Node: packet.SrcNode},
		},
		aarp.AddrPair{
			Hardware: b.eth,
			Proto:    aarp.AtalkAddr{Network: defaultNet, Node: packet.DstNode},
		},
	))
	if err != nil {
		return nil
	}
	return out
}
