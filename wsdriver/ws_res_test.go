package wsdriver

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	assert "github.com/stretchr/testify/require"
)

func makeHTTPResponse(statusCode int, contentType string, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func parseResponseHeader(data []byte) (version uint8, header uint8, requestID uint32, statusCode uint16, body []byte) {
	version = data[0]
	header = data[1]
	requestID = binary.BigEndian.Uint32(data[2:6])
	statusCode = binary.BigEndian.Uint16(data[6:8])
	// data[8] is the null byte for empty url path
	body = data[9:]
	return
}

func TestWriteResponse_NoCompression(t *testing.T) {
	wsBuffer := new(bytes.Buffer)
	httpBuffer := new(bytes.Buffer)
	httpResp := makeHTTPResponse(200, "application/json; charset=utf-8", `{"value":null}`)

	err := WriteResponse(wsBuffer, httpBuffer, httpResp, 42, SupportedEncoding{})
	assert.NoError(t, err)

	data := wsBuffer.Bytes()
	version, header, reqID, status, body := parseResponseHeader(data)

	assert.Equal(t, uint8(1), version)
	assert.Equal(t, MessageTypeResponse, MessageType((header>>4)&0x0f))
	assert.Equal(t, CompressionNone, CompressionType((header>>2)&0x03))
	assert.True(t, (header>>1)&0x01 != 0, "IsJSON should be set")
	assert.Equal(t, uint32(42), reqID)
	assert.Equal(t, uint16(200), status)
	assert.Equal(t, `{"value":null}`, string(body))
}

func TestWriteResponse_NonJSONContentType(t *testing.T) {
	wsBuffer := new(bytes.Buffer)
	httpBuffer := new(bytes.Buffer)
	httpResp := makeHTTPResponse(200, "text/plain", "hello")

	err := WriteResponse(wsBuffer, httpBuffer, httpResp, 1, SupportedEncoding{})
	assert.NoError(t, err)

	header := wsBuffer.Bytes()[1]
	assert.False(t, (header>>1)&0x01 != 0, "IsJSON should not be set for text/plain")
}

func TestWriteResponse_GzipCompression(t *testing.T) {
	wsBuffer := new(bytes.Buffer)
	httpBuffer := new(bytes.Buffer)

	// Body must exceed compressionThresholdBytes (1024)
	largeBody := strings.Repeat("a", 2000)
	httpResp := makeHTTPResponse(200, "application/json", largeBody)

	err := WriteResponse(wsBuffer, httpBuffer, httpResp, 5, SupportedEncoding{IsGzipSupported: true})
	assert.NoError(t, err)

	data := wsBuffer.Bytes()
	header := data[1]
	assert.Equal(t, CompressionGZIP, CompressionType((header>>2)&0x03))

	// Decompress and verify
	compressedBody := data[9:]
	gr, err := gzip.NewReader(bytes.NewReader(compressedBody))
	assert.NoError(t, err)
	decompressed, err := io.ReadAll(gr)
	assert.NoError(t, err)
	assert.Equal(t, largeBody, string(decompressed))
}

func TestWriteResponse_ZstdCompression(t *testing.T) {
	wsBuffer := new(bytes.Buffer)
	httpBuffer := new(bytes.Buffer)

	largeBody := strings.Repeat("b", 2000)
	httpResp := makeHTTPResponse(200, "application/json", largeBody)

	err := WriteResponse(wsBuffer, httpBuffer, httpResp, 7, SupportedEncoding{IsZstdSupported: true})
	assert.NoError(t, err)

	data := wsBuffer.Bytes()
	header := data[1]
	assert.Equal(t, CompressionZSTD, CompressionType((header>>2)&0x03))

	// Decompress and verify
	compressedBody := data[9:]
	zr, err := zstd.NewReader(bytes.NewReader(compressedBody))
	assert.NoError(t, err)
	decompressed, err := io.ReadAll(zr)
	assert.NoError(t, err)
	assert.Equal(t, largeBody, string(decompressed))
}

func TestWriteResponse_ZstdPreferredOverGzip(t *testing.T) {
	wsBuffer := new(bytes.Buffer)
	httpBuffer := new(bytes.Buffer)

	largeBody := strings.Repeat("c", 2000)
	httpResp := makeHTTPResponse(200, "text/plain", largeBody)

	err := WriteResponse(wsBuffer, httpBuffer, httpResp, 1, SupportedEncoding{IsGzipSupported: true, IsZstdSupported: true})
	assert.NoError(t, err)

	header := wsBuffer.Bytes()[1]
	assert.Equal(t, CompressionZSTD, CompressionType((header>>2)&0x03))
}

func TestWriteResponse_NoCompressionBelowThreshold(t *testing.T) {
	wsBuffer := new(bytes.Buffer)
	httpBuffer := new(bytes.Buffer)

	// Body smaller than compressionThresholdBytes (1024)
	smallBody := strings.Repeat("d", 100)
	httpResp := makeHTTPResponse(200, "application/json", smallBody)

	err := WriteResponse(wsBuffer, httpBuffer, httpResp, 1, SupportedEncoding{IsGzipSupported: true, IsZstdSupported: true})
	assert.NoError(t, err)

	header := wsBuffer.Bytes()[1]
	assert.Equal(t, CompressionNone, CompressionType((header>>2)&0x03))
}

func TestWriteResponse_StatusCodes(t *testing.T) {
	codes := []int{200, 400, 404, 500}
	for _, code := range codes {
		wsBuffer := new(bytes.Buffer)
		httpBuffer := new(bytes.Buffer)
		httpResp := makeHTTPResponse(code, "text/plain", "body")

		err := WriteResponse(wsBuffer, httpBuffer, httpResp, 1, SupportedEncoding{})
		assert.NoError(t, err)

		_, _, _, status, _ := parseResponseHeader(wsBuffer.Bytes())
		assert.Equal(t, uint16(code), status)
	}
}

func TestWriteResponse_EmptyBody(t *testing.T) {
	wsBuffer := new(bytes.Buffer)
	httpBuffer := new(bytes.Buffer)
	httpResp := makeHTTPResponse(204, "", "")

	err := WriteResponse(wsBuffer, httpBuffer, httpResp, 1, SupportedEncoding{})
	assert.NoError(t, err)

	_, _, _, _, body := parseResponseHeader(wsBuffer.Bytes())
	assert.Empty(t, body)
}

func TestWriteResponse_NullPathByte(t *testing.T) {
	wsBuffer := new(bytes.Buffer)
	httpBuffer := new(bytes.Buffer)
	httpResp := makeHTTPResponse(200, "text/plain", "ok")

	err := WriteResponse(wsBuffer, httpBuffer, httpResp, 1, SupportedEncoding{})
	assert.NoError(t, err)

	// Byte 8 should be 0 (empty url path for response)
	assert.Equal(t, uint8(0), wsBuffer.Bytes()[8])
}
