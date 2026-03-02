package wsdriver

import (
	"encoding/binary"
	"testing"

	assert "github.com/stretchr/testify/require"
)

func buildRequestMessage(version uint8, header uint8, requestID uint32, method uint16, path string, body []byte) []byte {
	msg := make([]byte, 0, 8+len(path)+1+len(body))
	msg = append(msg, version)
	msg = append(msg, header)
	var rid [4]byte
	binary.BigEndian.PutUint32(rid[:], requestID)
	msg = append(msg, rid[:]...)
	var m [2]byte
	binary.BigEndian.PutUint16(m[:], method)
	msg = append(msg, m[:]...)
	msg = append(msg, []byte(path)...)
	msg = append(msg, 0) // null terminator
	msg = append(msg, body...)
	return msg
}

func TestRequestMethodString(t *testing.T) {
	assert.Equal(t, "GET", RequestGet.String())
	assert.Equal(t, "HEAD", RequestHead.String())
	assert.Equal(t, "POST", RequestPost.String())
	assert.Equal(t, "PUT", RequestPut.String())
	assert.Equal(t, "DELETE", RequestDelete.String())
	assert.Equal(t, "CONNECT", RequestConnect.String())
	assert.Equal(t, "OPTIONS", RequestOptions.String())
	assert.Equal(t, "TRACE", RequestTrace.String())
	assert.Equal(t, "PATCH", RequestPatch.String())
	assert.Equal(t, "UNKNOWN", RequestMethod(9).String())
	assert.Equal(t, "UNKNOWN", RequestMethod(255).String())
}

func TestParseRequestV1_ValidGetNoCompression(t *testing.T) {
	// Header: MsgType=Request(0), Compression=None(0), IsJSON=false, IsWdError=false
	header := uint8(0x00)
	data := buildRequestMessage(1, header, 42, uint16(RequestGet), "session/abc/url", nil)

	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.NoError(t, err)
	assert.Equal(t, uint8(1), req.Version)
	assert.Equal(t, MessageTypeRequest, req.Header.MessageType)
	assert.Equal(t, CompressionNone, req.Header.CompressionType)
	assert.False(t, req.Header.IsJSON)
	assert.False(t, req.Header.IsWsdriverError)
	assert.Equal(t, uint32(42), req.RequestID)
	assert.Equal(t, RequestGet, req.RequestMethod)
	assert.Equal(t, "session/abc/url", req.RequestPath)
	assert.Empty(t, req.Buffer)
}

func TestParseRequestV1_ValidPostWithJSON(t *testing.T) {
	// Header: MsgType=Request(0), Compression=None(0), IsJSON=true, IsWdError=false
	header := uint8(1 << 1) // 0x02
	body := []byte(`{"url":"http://example.com"}`)
	data := buildRequestMessage(1, header, 100, uint16(RequestPost), "session/abc/url", body)

	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.NoError(t, err)
	assert.True(t, req.Header.IsJSON)
	assert.Equal(t, RequestPost, req.RequestMethod)
	assert.Equal(t, "session/abc/url", req.RequestPath)
	assert.Equal(t, body, req.Buffer)
}

func TestParseRequestV1_GzipCompressionHeader(t *testing.T) {
	// Header: MsgType=Request(0), Compression=GZIP(1), IsJSON=true, IsWdError=false
	header := uint8((1 << 2) | (1 << 1)) // 0x06
	body := []byte("compressed-data")
	data := buildRequestMessage(1, header, 1, uint16(RequestPost), "session/abc/element", body)

	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.NoError(t, err)
	assert.Equal(t, CompressionGZIP, req.Header.CompressionType)
	assert.Equal(t, body, req.Buffer)
}

func TestParseRequestV1_ZstdCompressionHeader(t *testing.T) {
	// Header: MsgType=Request(0), Compression=ZSTD(2), IsJSON=true, IsWdError=false
	header := uint8((2 << 2) | (1 << 1)) // 0x0A
	data := buildRequestMessage(1, header, 1, uint16(RequestPost), "session/abc/element", []byte("zstd-data"))

	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.NoError(t, err)
	assert.Equal(t, CompressionZSTD, req.Header.CompressionType)
}

func TestParseRequestV1_WsdriverErrorFlag(t *testing.T) {
	// Header: MsgType=Request(0), Compression=None(0), IsJSON=false, IsWdError=true
	header := uint8(0x01)
	data := buildRequestMessage(1, header, 1, uint16(RequestGet), "status", nil)

	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.NoError(t, err)
	assert.True(t, req.Header.IsWsdriverError)
}

func TestParseRequestV1_AllMethods(t *testing.T) {
	methods := []RequestMethod{
		RequestGet, RequestHead, RequestPost, RequestPut,
		RequestDelete, RequestConnect, RequestOptions, RequestTrace, RequestPatch,
	}
	for _, m := range methods {
		data := buildRequestMessage(1, 0x00, 1, uint16(m), "status", nil)
		var req RequestMessage
		err := ParseRequestV1(data, &req)
		assert.NoError(t, err)
		assert.Equal(t, m, req.RequestMethod)
	}
}

func TestParseRequestV1_LargeRequestID(t *testing.T) {
	data := buildRequestMessage(1, 0x00, 0xFFFFFFFF, uint16(RequestGet), "status", nil)
	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.NoError(t, err)
	assert.Equal(t, uint32(0xFFFFFFFF), req.RequestID)
}

func TestParseRequestV1_ErrorTooShort(t *testing.T) {
	data := []byte{1, 0}
	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "message too short")
}

func TestParseRequestV1_ErrorInvalidVersion(t *testing.T) {
	data := buildRequestMessage(2, 0x00, 1, 0, "status", nil)
	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid message version")
}

func TestParseRequestV1_ErrorInvalidMessageType(t *testing.T) {
	// Header: MsgType=Response(1), rest zero
	header := uint8(1 << 4) // 0x10
	data := buildRequestMessage(1, header, 1, 0, "status", nil)
	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid message type")
}

func TestParseRequestV1_ErrorUnsupportedCompression(t *testing.T) {
	// Header: MsgType=Request(0), Compression=3(invalid)
	header := uint8(3 << 2) // 0x0C
	data := buildRequestMessage(1, header, 1, 0, "status", nil)
	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported compression type")
}

func TestParseRequestV1_ErrorInvalidRequestMethod(t *testing.T) {
	data := buildRequestMessage(1, 0x00, 1, 9, "status", nil)
	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid request method")
}

func TestParseRequestV1_ErrorNoNullTerminator(t *testing.T) {
	// Build message manually without null terminator
	msg := []byte{1, 0x00, 0, 0, 0, 1, 0, 0, 's', 't', 'a', 't', 'u', 's'}
	var req RequestMessage
	err := ParseRequestV1(msg, &req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no null terminator")
}

func TestParseRequestV1_ErrorEmptyRequestPath(t *testing.T) {
	// null terminator immediately at byte 8
	msg := []byte{1, 0x00, 0, 0, 0, 1, 0, 0, 0}
	var req RequestMessage
	err := ParseRequestV1(msg, &req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected empty request path")
}

func TestParseRequestV1_ErrorPathStartsWithDot(t *testing.T) {
	data := buildRequestMessage(1, 0x00, 1, 0, "./status", nil)
	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "can't start with '.' or '/'")
}

func TestParseRequestV1_ErrorPathStartsWithSlash(t *testing.T) {
	data := buildRequestMessage(1, 0x00, 1, 0, "/status", nil)
	var req RequestMessage
	err := ParseRequestV1(data, &req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "can't start with '.' or '/'")
}

func TestParseRequestV1_Reuse(t *testing.T) {
	var req RequestMessage

	data1 := buildRequestMessage(1, 0x02, 10, uint16(RequestPost), "session/1/url", []byte(`{"url":"a"}`))
	err := ParseRequestV1(data1, &req)
	assert.NoError(t, err)
	assert.Equal(t, uint32(10), req.RequestID)
	assert.Equal(t, "session/1/url", req.RequestPath)

	data2 := buildRequestMessage(1, 0x00, 20, uint16(RequestDelete), "session/2/cookie", nil)
	err = ParseRequestV1(data2, &req)
	assert.NoError(t, err)
	assert.Equal(t, uint32(20), req.RequestID)
	assert.Equal(t, "session/2/cookie", req.RequestPath)
	assert.False(t, req.Header.IsJSON)
}
