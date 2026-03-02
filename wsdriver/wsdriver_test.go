package wsdriver

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	assert "github.com/stretchr/testify/require"
)

func startWsDriverServer(t *testing.T, sessionUrl string, isAlive func() bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		HandleConnection(w, r, sessionUrl, isAlive)
	}))
}

func wsConnect(t *testing.T, serverURL string, acceptEncoding string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http")
	header := http.Header{}
	if acceptEncoding != "" {
		header.Set("WsDriver-Accept-Encoding", acceptEncoding)
	}
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	return ws
}

func TestHandleConnection_SuccessfulRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"value":null}`))
	}))
	defer backend.Close()

	srv := startWsDriverServer(t, backend.URL, func() bool { return true })
	defer srv.Close()

	ws := wsConnect(t, srv.URL, "")
	defer ws.Close()

	msg := buildRequestMessage(1, 0x02, 1, uint16(RequestGet), "status", nil)
	err := ws.WriteMessage(websocket.BinaryMessage, msg)
	assert.NoError(t, err)

	msgType, data, err := ws.ReadMessage()
	assert.NoError(t, err)
	assert.Equal(t, websocket.BinaryMessage, msgType)

	// Parse response
	assert.True(t, len(data) >= 9)
	assert.Equal(t, uint8(1), data[0])                                         // version
	assert.Equal(t, MessageTypeResponse, MessageType((data[1]>>4)&0x0f))       // message type
	assert.Equal(t, uint32(1), binary.BigEndian.Uint32(data[2:6]))             // request id
	assert.Equal(t, uint16(http.StatusOK), binary.BigEndian.Uint16(data[6:8])) // status code
	assert.Equal(t, `{"value":null}`, string(data[9:]))                        // body
}

func TestHandleConnection_SessionDead(t *testing.T) {
	srv := startWsDriverServer(t, "http://localhost:1", func() bool { return false })
	defer srv.Close()

	ws := wsConnect(t, srv.URL, "")
	defer ws.Close()

	msg := buildRequestMessage(1, 0x00, 42, uint16(RequestGet), "status", nil)
	err := ws.WriteMessage(websocket.BinaryMessage, msg)
	assert.NoError(t, err)

	_, data, err := ws.ReadMessage()
	assert.NoError(t, err)

	// Should be a session timed out error
	assert.Equal(t, uint8(1), data[0])
	assert.Equal(t, uint32(42), binary.BigEndian.Uint32(data[2:6]))
	assert.Equal(t, uint16(http.StatusNotFound), binary.BigEndian.Uint16(data[6:8]))

	var payload errorPayload
	err = json.Unmarshal(data[9:], &payload)
	assert.NoError(t, err)
	assert.Equal(t, "invalid session id", payload.Value.Error)
}

func TestHandleConnection_BackendUnreachable(t *testing.T) {
	// Use a port that won't be listening
	srv := startWsDriverServer(t, "http://127.0.0.1:1", func() bool { return true })
	defer srv.Close()

	ws := wsConnect(t, srv.URL, "")
	defer ws.Close()

	msg := buildRequestMessage(1, 0x00, 10, uint16(RequestGet), "status", nil)
	err := ws.WriteMessage(websocket.BinaryMessage, msg)
	assert.NoError(t, err)

	_, data, err := ws.ReadMessage()
	assert.NoError(t, err)

	assert.Equal(t, uint16(http.StatusInternalServerError), binary.BigEndian.Uint16(data[6:8]))

	var payload errorPayload
	err = json.Unmarshal(data[9:], &payload)
	assert.NoError(t, err)
	assert.Equal(t, "wsdriver protocol error", payload.Value.Error)
	assert.Contains(t, payload.Value.Message, "couldn't send request to webdriver.")
}

func TestHandleConnection_TextMessageRejected(t *testing.T) {
	srv := startWsDriverServer(t, "http://localhost:1", func() bool { return true })
	defer srv.Close()

	ws := wsConnect(t, srv.URL, "")
	defer ws.Close()

	err := ws.WriteMessage(websocket.TextMessage, []byte("hello"))
	assert.NoError(t, err)

	_, _, err = ws.ReadMessage()
	assert.Error(t, err)
	closeErr, ok := err.(*websocket.CloseError)
	assert.True(t, ok)
	assert.Equal(t, websocket.CloseUnsupportedData, closeErr.Code)
}

func TestHandleConnection_InvalidProtocolMessage(t *testing.T) {
	srv := startWsDriverServer(t, "http://localhost:1", func() bool { return true })
	defer srv.Close()

	ws := wsConnect(t, srv.URL, "")
	defer ws.Close()

	// Send a message that's too short
	err := ws.WriteMessage(websocket.BinaryMessage, []byte{0x01})
	assert.NoError(t, err)

	_, _, err = ws.ReadMessage()
	assert.Error(t, err)
	closeErr, ok := err.(*websocket.CloseError)
	assert.True(t, ok)
	assert.Equal(t, websocket.CloseInvalidFramePayloadData, closeErr.Code)
}

func TestHandleConnection_MultipleRequests(t *testing.T) {
	requestCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"value":null}`))
	}))
	defer backend.Close()

	srv := startWsDriverServer(t, backend.URL, func() bool { return true })
	defer srv.Close()

	ws := wsConnect(t, srv.URL, "")
	defer ws.Close()

	for i := uint32(1); i <= 3; i++ {
		msg := buildRequestMessage(1, 0x00, i, uint16(RequestGet), "status", nil)
		err := ws.WriteMessage(websocket.BinaryMessage, msg)
		assert.NoError(t, err)

		_, data, err := ws.ReadMessage()
		assert.NoError(t, err)
		assert.Equal(t, i, binary.BigEndian.Uint32(data[2:6]))
	}

	assert.Equal(t, 3, requestCount)
}

func TestHandleConnection_UpgradeResponseHeaders(t *testing.T) {
	srv := startWsDriverServer(t, "http://localhost:1", func() bool { return true })
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	assert.NoError(t, err)
	assert.Equal(t, "zstd, gzip", resp.Header.Get("WsDriver-Accept-Encoding"))
}

func TestHandleConnection_ClientAcceptEncoding(t *testing.T) {
	largeBody := strings.Repeat("x", 2000)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(largeBody))
	}))
	defer backend.Close()

	srv := startWsDriverServer(t, backend.URL, func() bool { return true })
	defer srv.Close()

	// Connect with gzip support
	ws := wsConnect(t, srv.URL, "gzip")
	defer ws.Close()

	msg := buildRequestMessage(1, 0x02, 1, uint16(RequestGet), "status", nil)
	err := ws.WriteMessage(websocket.BinaryMessage, msg)
	assert.NoError(t, err)

	_, data, err := ws.ReadMessage()
	assert.NoError(t, err)

	header := data[1]
	assert.Equal(t, CompressionGZIP, CompressionType((header>>2)&0x03))
}
