package wsdriver

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// Almost twice as fast, as default 6
// Produces only a little bit worse result
const gzipCompressionLevel = 4

// A little bit less than MTU
const compressionThresholdBytes = 1024

var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(nil, gzipCompressionLevel)
		return w
	},
}

var zstdWriterPool = sync.Pool{
	New: func() any {
		w, _ := zstd.NewWriter(nil)
		return w
	},
}

func WriteResponse(wsBuffer *bytes.Buffer, httpBuffer *bytes.Buffer, httpResponse *http.Response, requestId uint32, clientSupportedEncoding SupportedEncoding) error {
	wsBuffer.Reset()
	httpBuffer.Reset()

	_, err := io.Copy(httpBuffer, httpResponse.Body)
	closeErr := httpResponse.Body.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}

	wsBuffer.WriteByte(1)

	compression := CompressionNone

	var header uint8 = uint8(MessageTypeResponse) << 4

	if httpBuffer.Len() > compressionThresholdBytes {
		if clientSupportedEncoding.IsZstdSupported {
			header |= uint8(CompressionZSTD) << 2
			compression = CompressionZSTD
		} else if clientSupportedEncoding.IsGzipSupported {
			header |= uint8(CompressionGZIP) << 2
			compression = CompressionGZIP
		}
	}

	if strings.HasPrefix(httpResponse.Header.Get("Content-Type"), "application/json") {
		header |= 1 << 1
	}

	wsBuffer.WriteByte(header)

	var payloadHeaders [6]byte // request id, status code
	binary.BigEndian.PutUint32(payloadHeaders[0:4], requestId)
	binary.BigEndian.PutUint16(payloadHeaders[4:6], uint16(httpResponse.StatusCode))

	wsBuffer.Write(payloadHeaders[:])
	wsBuffer.WriteByte(0) // empty url path for response

	switch compression {
	case CompressionGZIP:
		gw := gzipWriterPool.Get().(*gzip.Writer)
		defer gzipWriterPool.Put(gw)

		gw.Reset(wsBuffer)
		if _, err := gw.Write(httpBuffer.Bytes()); err != nil {
			return err
		}
		if err := gw.Close(); err != nil {
			return err
		}

	case CompressionZSTD:
		zw := zstdWriterPool.Get().(*zstd.Encoder)
		defer zstdWriterPool.Put(zw)

		zw.Reset(wsBuffer)
		if _, err := zw.Write(httpBuffer.Bytes()); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}

	default:
		wsBuffer.Write(httpBuffer.Bytes())
	}

	return nil
}
