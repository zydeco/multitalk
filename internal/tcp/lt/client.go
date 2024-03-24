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

// Communicates with TCP servers
package lt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"go.uber.org/zap"

	"github.com/sfiera/multitalk/internal/bridge"
	"github.com/sfiera/multitalk/pkg/llap"
	"github.com/sfiera/multitalk/pkg/ltou"
)

type client struct {
	conn net.Conn
}

func TCPClient(server string) (bridge.Bridge, error) {
	conn, err := net.Dial("tcp", server)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %s", server, err.Error())
	}
	return &client{conn}, nil
}

func (c *client) Start(ctx context.Context, log *zap.Logger) (
	send chan<- llap.Packet,
	recv <-chan llap.Packet,
) {
	log = log.With(zap.String("bridge", "tcp-localtalk"))
	sendCh := make(chan llap.Packet)
	recvCh := make(chan llap.Packet)
	go c.capture(ctx, log, recvCh)
	go c.transmit(ctx, log, sendCh)
	return sendCh, recvCh
}

func (c *client) transmit(ctx context.Context, log *zap.Logger, sendCh <-chan llap.Packet) {
	for packet := range sendCh {
		data, err := ltou.Marshal(ltou.Packet{
			Header: ltou.Header{Pid: 1337},
			LLAP:   packet,
		})
		if err != nil {
			log.With(zap.Error(err)).Error("marshal failed")
			continue
		}

		_ = binary.Write(c.conn, binary.BigEndian, uint16(len(data)))
		_, _ = c.conn.Write(data)
	}
}

func (c *client) capture(ctx context.Context, log *zap.Logger, recvCh chan<- llap.Packet) {
	defer close(recvCh)
	go func() {
		<-ctx.Done()
		c.conn.Close()
	}()
	log = log.With(zap.Stringer("remoteAddr", c.conn.RemoteAddr()))

	for {
		// receive a frame and send it out on the net
		length := uint16(0)
		err := binary.Read(c.conn, binary.BigEndian, &length)
		if errors.Is(err, io.EOF) {
			log.Info("closed")
			return
		} else if err != nil {
			log.With(zap.Error(err)).Error("recv length failed")
			return
		}

		if length > 4096 {
			log.With(zap.Error(err), zap.Uint16("length", length)).Error("invalid length")
			continue
		}
		// DebugLog("receiving packet of length: %u\n", length);

		data := make([]byte, length)
		_, err = c.conn.Read(data)
		if err != nil {
			log.With(zap.Error(err)).Error("recv packet failed")
			return
		}
		// DebugLog("Successfully received packet\n%s", "");

		packet := ltou.Packet{}
		err = ltou.Unmarshal(data, &packet)
		if err != nil {
			log.With(zap.Error(err)).Error("unmarshal failed")
			continue
		}

		recvCh <- packet.LLAP
	}
}
