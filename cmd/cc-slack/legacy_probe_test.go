package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/daemonkit/paths"
)

const (
	legacyProtocol = 2
	frameHello     = 1
	frameHelloAck  = 2
	frameRequest   = 3
	frameResponse  = 4
	frameHeader    = 32
)

func TestProtocolTwoBuildReadsTheReleaseItHandsOffTo(t *testing.T) {
	startDaemon(t)
	socket, err := paths.Socket(serviceLabel)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	writeLegacyFrame(t, conn, frameHello, 0, "", []byte(`{"protocol":2,"lane":"control"}`))
	var ack struct {
		Protocol uint16 `json:"protocol"`
		Rejected bool   `json:"rejected"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(readLegacyFrame(t, conn, frameHelloAck), &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Rejected || ack.Protocol != legacyProtocol {
		t.Fatalf("ack = %+v, want a protocol-2 acceptance", ack)
	}

	writeLegacyFrame(t, conn, frameRequest, 1, "daemon.health", nil)
	var response struct {
		Payload struct {
			PID    int    `json:"pid"`
			Detail []byte `json:"detail"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(readLegacyFrame(t, conn, frameResponse), &response); err != nil {
		t.Fatal(err)
	}
	var detail daemon.HealthDetail
	if err := json.Unmarshal(response.Payload.Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.RuntimeBuild != appVersion || response.Payload.PID != os.Getpid() {
		t.Fatalf("health = pid %d release %q, want pid %d release %q", response.Payload.PID, detail.RuntimeBuild, os.Getpid(), appVersion)
	}
}

func writeLegacyFrame(t *testing.T, conn net.Conn, kind byte, id uint64, op string, payload []byte) {
	t.Helper()
	body := make([]byte, frameHeader+len(op)+len(payload))
	copy(body, "DKS1")
	binary.BigEndian.PutUint16(body[4:6], legacyProtocol)
	body[6] = kind
	body[7] = 1
	binary.BigEndian.PutUint64(body[8:16], id)
	binary.BigEndian.PutUint16(body[28:30], uint16(len(op)))
	copy(body[frameHeader:], op)
	copy(body[frameHeader+len(op):], payload)
	packet := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	if _, err := conn.Write(append(packet, body...)); err != nil {
		t.Fatal(err)
	}
}

func readLegacyFrame(t *testing.T, conn net.Conn, kind byte) []byte {
	t.Helper()
	for {
		var prefix [4]byte
		if _, err := io.ReadFull(conn, prefix[:]); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		body := make([]byte, binary.BigEndian.Uint32(prefix[:]))
		if _, err := io.ReadFull(conn, body); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if version := binary.BigEndian.Uint16(body[4:6]); version != legacyProtocol {
			t.Fatalf("frame version = %d, want %d", version, legacyProtocol)
		}
		if body[6] != kind {
			continue
		}
		opLen := int(binary.BigEndian.Uint16(body[28:30]))
		tenantLen := int(binary.BigEndian.Uint16(body[30:32]))
		return body[frameHeader+opLen+tenantLen:]
	}
}
