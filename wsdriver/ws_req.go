package wsdriver

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

const ExpectedMessageVersion = 1

type MessageType uint8

const (
	MessageTypeRequest  MessageType = 0
	MessageTypeResponse MessageType = 1
)

type CompressionType uint8

const (
	CompressionNone CompressionType = 0
	CompressionGZIP CompressionType = 1
	CompressionZSTD CompressionType = 2
)

type Header struct {
	MessageType     MessageType     // 4 bits
	CompressionType CompressionType // 2 bits
	IsJSON          bool            // 1 bit
	IsWsdriverError bool            // 1 bit
}

type RequestMethod uint16

// Order:
// https://datatracker.ietf.org/doc/html/rfc7231#section-4
// https://datatracker.ietf.org/doc/html/rfc5789#section-2
const (
	RequestGet     RequestMethod = 0
	RequestHead    RequestMethod = 1
	RequestPost    RequestMethod = 2
	RequestPut     RequestMethod = 3
	RequestDelete  RequestMethod = 4
	RequestConnect RequestMethod = 5
	RequestOptions RequestMethod = 6
	RequestTrace   RequestMethod = 7
	RequestPatch   RequestMethod = 8
)

var methodNames = [...]string{
	RequestGet:     "GET",
	RequestHead:    "HEAD",
	RequestPost:    "POST",
	RequestPut:     "PUT",
	RequestDelete:  "DELETE",
	RequestConnect: "CONNECT",
	RequestOptions: "OPTIONS",
	RequestTrace:   "TRACE",
	RequestPatch:   "PATCH",
}

func (rm RequestMethod) String() string {
	if rm > 8 {
		return "UNKNOWN"
	}
	return methodNames[rm]
}

type RequestMessage struct {
	Version       uint8         // Bytes: 0-1
	Header        Header        // Bytes: 1-2
	RequestID     uint32        // Bytes: 2-6
	RequestMethod RequestMethod // Bytes: 6-8
	RequestPath   string        // Bytes: 8+ Null-terminated string content (without the null byte)
	Buffer        []byte        // Bytes: 9+[RequestPath lengh]. Remainings, compressed with "Header.CompressionType"
}

type ResponseMessage struct {
	Version        uint8  // Bytes: 0-1
	Header         Header // Bytes: 1-2
	RequestID      uint32 // Bytes: 2-6
	ResponseStatus uint16 // Bytes: 6-8
	RequestPath    []byte // Bytes: 8-9 '\0'
	Buffer         []byte // Bytes: 9+ Response body. Remainings, uncompressed
}

const minMessageSize = 1 + 1 + 4 + 2 + 1

func ParseRequestV1(data []byte, req *RequestMessage) error {
	if len(data) < minMessageSize {
		return fmt.Errorf("message too short: %d bytes, minimum %d", len(data), minMessageSize)
	}

	// Parse version byte
	req.Version = data[0]

	if req.Version != ExpectedMessageVersion {
		return fmt.Errorf("invalid message version. Expected '%d', got '%d'", ExpectedMessageVersion, req.Version)
	}

	// Parse header byte: [MsgType(4) | CompType(2) | IsJSON | IsWdError]
	h := data[1]
	req.Header.MessageType = MessageType((h >> 4) & 0x0f)
	req.Header.CompressionType = CompressionType((h >> 2) & 0x03)
	req.Header.IsJSON = (h>>1)&0x01 != 0
	req.Header.IsWsdriverError = h&0x01 != 0

	if req.Header.MessageType != MessageTypeRequest {
		return fmt.Errorf("invalid message type. Expected '%d', got '%d'", MessageTypeRequest, req.Header.MessageType)
	}

	if req.Header.CompressionType > 2 {
		return fmt.Errorf("unsupported compression type. Expected 0-2, got '%d'", req.Header.CompressionType)
	}

	req.RequestID = binary.BigEndian.Uint32(data[2:6])
	req.RequestMethod = RequestMethod(binary.BigEndian.Uint16(data[6:8]))

	if req.RequestMethod > 8 {
		return fmt.Errorf("invalid request method. Expected 0-8, got '%d'", req.RequestMethod)
	}

	requestPathLength := bytes.IndexByte(data[8:], 0)
	if requestPathLength < 0 {
		return fmt.Errorf("no null terminator found in url-path")
	}
	if requestPathLength == 0 {
		return fmt.Errorf("unexpected empty request path")
	}
	req.RequestPath = string(data[8 : 8+requestPathLength])
	if req.RequestPath[0] == '.' || req.RequestPath[0] == '/' {
		return fmt.Errorf("invalid request path. It can't start with '.' or '/'")
	}
	req.Buffer = data[9+requestPathLength:]

	return nil
}
