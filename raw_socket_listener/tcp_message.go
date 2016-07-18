package rawSocket

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"github.com/buger/gor/proto"
	"log"
	"net"
	"strconv"
	"time"
)

var _ = log.Println

// TCPMessage ensure that all TCP packets for given request is received, and processed in right sequence
// Its needed because all TCP message can be fragmented or re-transmitted
//
// Each TCP Packet have 2 ids: acknowledgment - message_id, and sequence - packet_id
// Message can be compiled from unique packets with same message_id which sorted by sequence
// Message is received if we didn't receive any packets for 2000ms
type TCPMessage struct {
	Seq         uint32
	Ack         uint32
	ResponseAck uint32
	ResponseID  tcpID
	DataAck     uint32
	DataSeq     uint32

	AssocMessage *TCPMessage
	Start        time.Time
	End          time.Time
	IsIncoming   bool

	packets []*TCPPacket

	delChan chan *TCPMessage

	/* HTTP specific variables */
	methodType    httpMethodType
	bodyType      httpBodyType
	expectType    httpExpectType
	seqMissing    bool
	headerPacket  int
	contentLength int
	complete      bool
}

// NewTCPMessage pointer created from a Acknowledgment number and a channel of messages readuy to be deleted
func NewTCPMessage(Seq, Ack uint32, IsIncoming bool) (msg *TCPMessage) {
	msg = &TCPMessage{Seq: Seq, Ack: Ack, IsIncoming: IsIncoming}
	msg.Start = time.Now()

	return
}

// Bytes return message content
func (t *TCPMessage) Bytes() (output []byte) {
	for _, p := range t.packets {
		output = append(output, p.Data...)
	}

	return output
}

// Size returns total body size
func (t *TCPMessage) BodySize() (size int) {
	if len(t.packets) == 0 || t.headerPacket == -1 {
		return 0
	}

	size += len(proto.Body(t.packets[t.headerPacket].Data))

	for _, p := range t.packets[t.headerPacket + 1:] {
		size += len(p.Data)
	}

	return
}

// Size returns total size of message
func (t *TCPMessage) Size() (size int) {
	if len(t.packets) == 0 {
		return 0
	}

	for _, p := range t.packets {
		size += len(p.Data)
	}

	return
}

// AddPacket to the message and ensure packet uniqueness
// TCP allows that packet can be re-send multiple times
func (t *TCPMessage) AddPacket(packet *TCPPacket) {
	packetFound := false

	for _, pkt := range t.packets {
		if packet.Seq == pkt.Seq {
			packetFound = true
			break
		}
	}

	if !packetFound {
		// Packets not always captured in same Seq order, and sometimes we need to prepend
		if len(t.packets) == 0 || packet.Seq > t.packets[len(t.packets)-1].Seq {
			t.packets = append(t.packets, packet)
		} else if packet.Seq < t.packets[0].Seq {
			t.packets = append([]*TCPPacket{packet}, t.packets...)
			t.Seq = packet.Seq // Message Seq should indicated starting seq
		} else { // insert somewhere in the middle...
			for i, p := range t.packets {
				if packet.Seq < p.Seq {
					t.packets = append(t.packets[:i], append([]*TCPPacket{packet}, t.packets[i:]...)...)
					break
				}
			}
		}

		if t.IsIncoming {
			t.End = time.Now()
		} else {
			t.End = time.Now().Add(time.Millisecond)
		}

		if packet.OrigAck != 0 {
			t.DataAck = packet.OrigAck
		}
	}

	t.checkSeqIntegrity()
	t.updateHeadersPacket()
	t.updateMethodType()
	t.updateBodyType()
	t.checkIfComplete()
	t.check100Continue()
}

// Check if there is missing packet
func (t *TCPMessage) checkSeqIntegrity() {
	if len(t.packets) == 1 {
		t.seqMissing = false
	}

	for i, p := range t.packets {
		// If final packet
		if len(t.packets) == i+1 {
			t.seqMissing = false
			return
		}
		np := t.packets[i+1]

		nextSeq := p.Seq + uint32(len(p.Data))

		if np.Seq != nextSeq {
			if t.expectType == httpExpect100Continue {
				if np.Seq != nextSeq+22 {
					t.seqMissing = true
					return
				}
			} else {
				t.seqMissing = true
				return
			}
		}
	}

	t.seqMissing = false
}

var bEmptyLine = []byte("\r\n\r\n")
var bChunkEnd = []byte("0\r\n\r\n")

func (t *TCPMessage) updateHeadersPacket() {
	if len(t.packets) == 1 {
		t.headerPacket = -1
	}

	if t.headerPacket != -1 {
		return
	}

	if t.seqMissing {
		return
	}

	for i, p := range t.packets {
		if bytes.LastIndex(p.Data, bEmptyLine) != -1 {
			t.headerPacket = i
			return
		}
	}

	return
}

// isMultipart returns true if message contains from multiple tcp packets
func (t *TCPMessage) checkIfComplete() {
	if t.seqMissing || t.headerPacket == -1 {
		return
	}

	if t.methodType == httpMethodNotFound {
		return
	}

	// Responses can be emitted only if we found request
	if !t.IsIncoming && t.AssocMessage == nil {
		return
	}

	// If one GET, OPTIONS, or HEAD request
	if t.methodType == httpMethodWithoutBody {
		t.complete = true
	} else {
		switch t.bodyType {
		case httpBodyEmpty:
			t.complete = true
		case httpBodyContentLength:
			if t.contentLength == 0 || t.contentLength == t.BodySize() {
				t.complete = true
			}
		case httpBodyChunked:
			lastPacket := t.packets[len(t.packets)-1]
			if bytes.LastIndex(lastPacket.Data, bChunkEnd) != -1 {
				t.complete = true
			}
		}
	}
}

type httpMethodType uint8

const (
	httpMethodNotSet      httpMethodType = 0
	httpMethodWithBody    httpMethodType = 1
	httpMethodWithoutBody httpMethodType = 2
	httpMethodNotFound    httpMethodType = 3
)

var methodsWithBody = [][]byte{
	[]byte("POST"),
	[]byte("PUT"),
	[]byte("PATCH"),
	[]byte("CONNECT"),
}

func (t *TCPMessage) updateMethodType() {
	// if there is cache
	if t.methodType != httpMethodNotSet && t.methodType != httpMethodNotFound {
		return
	}

	d := t.packets[0].Data

	// Minimum length fo request: GET / HTTP/1.1\r\n

	if len(d) < 16 {
		t.methodType = httpMethodNotFound
		return
	}

	if t.IsIncoming {
		var method []byte

		if mIdx := bytes.IndexByte(d[:8], ' '); mIdx != -1 {
			method = d[:mIdx]

			// Check that after method we have absolute or relative path
			switch d[mIdx+1] {
			case '/', 'h', '*':
			default:
				t.methodType = httpMethodNotFound
				return
			}
		} else {
			t.methodType = httpMethodNotFound
			return
		}

		for _, m := range methodsWithBody {
			if len(m) == len(method) && bytes.Equal(m, method) {
				t.methodType = httpMethodWithBody
				return
			}
		}

		t.methodType = httpMethodWithoutBody
	} else {
		if !bytes.Equal(d[:6], []byte("HTTP/1")) {
			t.methodType = httpMethodNotFound
			return
		}

		t.methodType = httpMethodWithBody
	}
}

type httpBodyType uint8

const (
	httpBodyNotSet        httpBodyType = 0
	httpBodyEmpty         httpBodyType = 1
	httpBodyContentLength httpBodyType = 2
	httpBodyChunked       httpBodyType = 3
)

func (t *TCPMessage) updateBodyType() {
	// if there is cache
	if t.bodyType != httpBodyNotSet {
		return
	}

	// Headers not received
	if t.headerPacket == -1 {
		return
	}

	switch t.methodType {
	case httpMethodNotFound:
		return
	case httpMethodWithoutBody:
		t.bodyType = httpBodyEmpty
		return
	case httpMethodWithBody:
		var lengthB, encB []byte

		for _, p := range t.packets[:t.headerPacket+1] {
			lengthB = proto.Header(p.Data, []byte("Content-Length"))

			if len(lengthB) > 0 {
				break
			}
		}

		if len(lengthB) > 0 {
			t.bodyType = httpBodyContentLength
			t.contentLength, _ = strconv.Atoi(string(lengthB))
			return
		} else {
			for _, p := range t.packets[:t.headerPacket+1] {
				encB = proto.Header(p.Data, []byte("Transfer-Encoding"))

				if len(encB) > 0 {
					t.bodyType = httpBodyChunked
					return
				}
			}
		}
	}

	t.bodyType = httpBodyEmpty
}

type httpExpectType uint8

const (
	httpExpectNotSet      httpExpectType = 0
	httpExpectEmpty       httpExpectType = 1
	httpExpect100Continue httpExpectType = 2
)

var bExpectHeader = []byte("Expect:")
var bExpect100Value = []byte("100-continue")

func (t *TCPMessage) check100Continue() {
	if t.expectType != httpExpectNotSet || len(t.packets[0].Data) < 25 {
		return
	}

	if t.methodType != httpMethodWithBody {
		return
	}

	if t.seqMissing || t.headerPacket == -1 {
		return
	}

	last := t.packets[len(t.packets)-1]
	// reading last 4 bytes for double CRLF
	if !bytes.HasSuffix(last.Data, bEmptyLine) {
		return
	}

	for _, p := range t.packets[:t.headerPacket+1] {
		if h := proto.Header(p.Data, bExpectHeader); len(h) > 0 {
			if bytes.Equal(bExpect100Value, h) {
				t.expectType = httpExpect100Continue
			}
			return
		}
	}

	t.expectType = httpExpectEmpty
}

func (t *TCPMessage) setAssocMessage(m *TCPMessage) {
	t.AssocMessage = m
	t.checkIfComplete()
}

// UpdateResponseAck should be called after packet is added
func (t *TCPMessage) UpdateResponseAck() uint32 {
	lastPacket := t.packets[len(t.packets)-1]
	respAck := lastPacket.Seq + uint32(len(lastPacket.Data))

	if t.ResponseAck != respAck {
		t.ResponseAck = lastPacket.Seq + uint32(len(lastPacket.Data))

		// We swappwed src and dst port
		copy(t.ResponseID[:16], lastPacket.Addr)
		copy(t.ResponseID[16:], lastPacket.Raw[2:4]) // Src port
		copy(t.ResponseID[18:], lastPacket.Raw[0:2]) // Dest port
		binary.BigEndian.PutUint32(t.ResponseID[20:24], t.ResponseAck)
	}

	return t.ResponseAck
}

func (t *TCPMessage) UUID() []byte {
	var key []byte

	if t.IsIncoming {
		// log.Println("UUID:", t.Ack, t.Start.UnixNano())
		key = strconv.AppendInt(key, t.Start.UnixNano(), 10)
		key = strconv.AppendUint(key, uint64(t.Ack), 10)
	} else {
		// log.Println("RequestMessage:", t.AssocMessage.Ack, t.AssocMessage.Start.UnixNano())
		key = strconv.AppendInt(key, t.AssocMessage.Start.UnixNano(), 10)
		key = strconv.AppendUint(key, uint64(t.AssocMessage.Ack), 10)
	}

	uuid := make([]byte, 40)
	sha := sha1.Sum(key)
	hex.Encode(uuid, sha[:20])

	return uuid
}

func (t *TCPMessage) ID() tcpID {
	return t.packets[0].ID
}

func (t *TCPMessage) IP() net.IP {
	return net.IP(t.packets[0].Addr)
}
