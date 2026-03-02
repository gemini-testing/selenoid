package wsdriver

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

type SupportedEncoding struct {
	IsGzipSupported bool
	IsZstdSupported bool
}

/**
1. Incoming wsdriver message is parsed (./ws_req.go)
2. Http request, made from wsdrive message, is sent to local webdriver client (./http_req.go)
3. Http response, produced by http request, is written as wsdriver message (./ws_res.go)

Error messages are constructed in ./wsdriver_errors.go
*/

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var wsdriverHandshakeHeaders = http.Header{
	"WsDriver-Accept-Encoding": {"zstd, gzip"},
}

func HandleConnection(w http.ResponseWriter, r *http.Request, sessionUrl string, isSessionAliveFn func() bool) {
	ws, err := upgrader.Upgrade(w, r, wsdriverHandshakeHeaders)
	if err != nil {
		log.Printf("[-] [WSDRIVER] [WebSocket upgrade error: %v]", err)
		return
	}
	defer ws.Close()

	var clientSupportedEncoding = SupportedEncoding{
		IsGzipSupported: strings.Contains(r.Header.Get("WsDriver-Accept-Encoding"), "gzip"),
		IsZstdSupported: strings.Contains(r.Header.Get("WsDriver-Accept-Encoding"), "zstd"),
	}

	var reqMsg RequestMessage
	var wsBuffer = new(bytes.Buffer)
	var httpBuffer = new(bytes.Buffer)

	wsBuffer.Grow(256 * 1024)   // 256 KB buffer
	httpBuffer.Grow(256 * 1024) // 256 KB buffer

	for {
		messageType, reader, err := ws.NextReader()

		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) && err != io.EOF {
				log.Printf("[-] [WSDRIVER] [WebSocket receive error: %v]", err)
			}

			break
		}

		if messageType != websocket.BinaryMessage {
			log.Printf("[-] [WSDRIVER] [Unexpected message type: %d, expected binary]", messageType)

			closeMessage := websocket.FormatCloseMessage(
				websocket.CloseUnsupportedData,
				"WsDriver protocol only accepts binary data",
			)

			ws.WriteMessage(websocket.CloseMessage, closeMessage)

			break
		}

		wsBuffer.Reset()
		_, err = wsBuffer.ReadFrom(reader)
		if err != nil {
			log.Printf("[-] [WSDRIVER] [Failed to receive message to '%s': %v]", sessionUrl, err)
			break
		}

		err = ParseRequestV1(wsBuffer.Bytes(), &reqMsg)
		if err != nil {
			log.Printf("[-] [WSDRIVER] [Parse error: %v]", err)

			closeMessage := websocket.FormatCloseMessage(
				websocket.CloseInvalidFramePayloadData,
				"Invalid message format: "+err.Error(),
			)

			ws.WriteMessage(websocket.CloseMessage, closeMessage)
			break
		}

		if !isSessionAliveFn() {
			WriteSessionTimedOutError(wsBuffer, reqMsg.RequestID)
			if err := ws.WriteMessage(websocket.BinaryMessage, wsBuffer.Bytes()); err != nil {
				log.Printf("[-] [WSDRIVER] [WebSocket send error: %v]", err)
				break
			}
			log.Printf("[-] [WSDRIVER] [Received message to closed session '%s']", sessionUrl)
			continue
		}

		httpResponse, err := MakeRequest(sessionUrl, &reqMsg, httpBuffer)
		if err != nil {
			WriteHttpRequestError(wsBuffer, reqMsg.RequestID, err)
			if err := ws.WriteMessage(websocket.BinaryMessage, wsBuffer.Bytes()); err != nil {
				log.Printf("[-] [WSDRIVER] [WebSocket send error: %v]", err)
				break
			}
			log.Printf("[-] [WSDRIVER] [Failed to send http request to '%s': %v]", sessionUrl, err)
			continue
		}

		err = WriteResponse(wsBuffer, httpBuffer, httpResponse, reqMsg.RequestID, clientSupportedEncoding)
		if err != nil {
			WriteConstructResponseError(wsBuffer, reqMsg.RequestID, err)
			if err := ws.WriteMessage(websocket.BinaryMessage, wsBuffer.Bytes()); err != nil {
				log.Printf("[-] [WSDRIVER] [WebSocket send error: %v]", err)
				break
			}
			log.Printf("[-] [WSDRIVER] [Failed to construct wsdriver response from '%s': %v]", sessionUrl, err)
			continue
		}

		err = ws.WriteMessage(websocket.BinaryMessage, wsBuffer.Bytes())
		if err != nil {
			log.Printf("[-] [WSDRIVER] [WebSocket send error: %v]", err)
			break
		}
	}
}
