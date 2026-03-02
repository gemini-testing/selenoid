package wsdriver

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
)

func writeErrorMessage(data *bytes.Buffer, requestId uint32, isProtocolError bool, statusCode uint16, errorName string, errorMessage string) {
	data.Reset()
	data.WriteByte(1) // version: v1

	// headers: http response, no compression, json
	var header uint8 = (uint8(MessageTypeResponse) << 4) | (1 << 1)

	if isProtocolError {
		header |= 1
	}

	data.WriteByte(header)

	var payloadHeaders [6]byte // request id, status code
	binary.BigEndian.PutUint32(payloadHeaders[0:4], requestId)
	binary.BigEndian.PutUint16(payloadHeaders[4:6], statusCode)

	data.Write(payloadHeaders[:])
	data.WriteByte(0) // empty url path for response

	errorObject := map[string]interface{}{
		"value": map[string]string{
			"error":   errorName,
			"message": errorMessage,
		},
	}

	payloadBody, err := json.Marshal(errorObject)

	if err != nil {
		panic(err)
	}

	data.Write(payloadBody)
}

func WriteSessionTimedOutError(data *bytes.Buffer, requestId uint32) {
	writeErrorMessage(data, requestId, false, http.StatusNotFound, "invalid session id", "session timed out or not found")
}

func WriteHttpRequestError(data *bytes.Buffer, requestId uint32, err error) {
	errMsg := "couldn't send request to webdriver."

	if err != nil {
		errMsg += fmt.Sprintf(" Cause: %s", err.Error())
	}

	writeErrorMessage(data, requestId, true, http.StatusInternalServerError, "wsdriver protocol error", errMsg)
}

func WriteConstructResponseError(data *bytes.Buffer, requestId uint32, err error) {
	errMsg := "couldn't construct wsdriver response."

	if err != nil {
		errMsg += fmt.Sprintf(" Cause: %s", err.Error())
	}

	writeErrorMessage(data, requestId, true, http.StatusInternalServerError, "wsdriver protocol error", errMsg)
}
