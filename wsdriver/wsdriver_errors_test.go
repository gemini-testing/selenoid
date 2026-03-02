package wsdriver

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"testing"

	assert "github.com/stretchr/testify/require"
)

type errorPayload struct {
	Value struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	} `json:"value"`
}

func parseErrorMessage(t *testing.T, data []byte) (version uint8, header uint8, requestID uint32, statusCode uint16, payload errorPayload) {
	t.Helper()
	assert.True(t, len(data) >= 9, "error message too short")
	version = data[0]
	header = data[1]
	requestID = binary.BigEndian.Uint32(data[2:6])
	statusCode = binary.BigEndian.Uint16(data[6:8])
	assert.Equal(t, uint8(0), data[8], "expected null byte for empty url path")
	err := json.Unmarshal(data[9:], &payload)
	assert.NoError(t, err)
	return
}

func TestWriteSessionTimedOutError(t *testing.T) {
	buf := new(bytes.Buffer)
	WriteSessionTimedOutError(buf, 42)

	version, header, reqID, status, payload := parseErrorMessage(t, buf.Bytes())

	assert.Equal(t, uint8(1), version)
	assert.Equal(t, MessageTypeResponse, MessageType((header>>4)&0x0f))
	assert.Equal(t, CompressionNone, CompressionType((header>>2)&0x03))
	assert.True(t, (header>>1)&0x01 != 0, "IsJSON should be set")
	assert.False(t, header&0x01 != 0, "IsWsdriverError should not be set")
	assert.Equal(t, uint32(42), reqID)
	assert.Equal(t, uint16(http.StatusNotFound), status)
	assert.Equal(t, "invalid session id", payload.Value.Error)
	assert.Equal(t, "session timed out or not found", payload.Value.Message)
}

func TestWriteHttpRequestError(t *testing.T) {
	buf := new(bytes.Buffer)
	WriteHttpRequestError(buf, 99, nil)

	version, header, reqID, status, payload := parseErrorMessage(t, buf.Bytes())

	assert.Equal(t, uint8(1), version)
	assert.True(t, header&0x01 != 0, "IsWsdriverError should be set")
	assert.Equal(t, uint32(99), reqID)
	assert.Equal(t, uint16(http.StatusInternalServerError), status)
	assert.Equal(t, "wsdriver protocol error", payload.Value.Error)
	assert.Equal(t, "couldn't send request to webdriver.", payload.Value.Message)
}

func TestWriteConstructResponseError(t *testing.T) {
	buf := new(bytes.Buffer)
	WriteConstructResponseError(buf, 7, nil)

	version, header, reqID, status, payload := parseErrorMessage(t, buf.Bytes())

	assert.Equal(t, uint8(1), version)
	assert.True(t, header&0x01 != 0, "IsWsdriverError should be set")
	assert.Equal(t, uint32(7), reqID)
	assert.Equal(t, uint16(http.StatusInternalServerError), status)
	assert.Equal(t, "wsdriver protocol error", payload.Value.Error)
	assert.Equal(t, "couldn't construct wsdriver response.", payload.Value.Message)
}

func TestWriteErrorMessage_BufferReuse(t *testing.T) {
	buf := new(bytes.Buffer)
	buf.WriteString("old data that should be cleared")

	WriteSessionTimedOutError(buf, 1)
	assert.Equal(t, uint8(1), buf.Bytes()[0], "buffer should be reset before writing")
}
