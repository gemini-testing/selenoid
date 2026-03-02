package wsdriver

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

var httpClient = &http.Client{
	Timeout: 3 * time.Minute,
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 5,
	},
}

var gzipReaderPool = sync.Pool{
	New: func() any {
		return new(gzip.Reader)
	},
}
var zstdReaderPool = sync.Pool{
	New: func() any {
		r, _ := zstd.NewReader(nil)
		return r
	},
}

func MakeRequest(sessionUrl string, m *RequestMessage, httpBuffer *bytes.Buffer) (*http.Response, error) {
	var body io.Reader

	switch m.Header.CompressionType {
	case CompressionGZIP:
		gr := gzipReaderPool.Get().(*gzip.Reader)
		defer gzipReaderPool.Put(gr)

		if err := gr.Reset(bytes.NewReader(m.Buffer)); err != nil {
			return nil, err
		}

		httpBuffer.Reset()
		if _, err := io.Copy(httpBuffer, gr); err != nil {
			return nil, err
		}
		body = httpBuffer

	case CompressionZSTD:
		zr := zstdReaderPool.Get().(*zstd.Decoder)
		defer zstdReaderPool.Put(zr)

		if err := zr.Reset(bytes.NewReader(m.Buffer)); err != nil {
			return nil, err
		}

		httpBuffer.Reset()
		if _, err := io.Copy(httpBuffer, zr); err != nil {
			return nil, err
		}
		body = httpBuffer

	default:
		body = bytes.NewReader(m.Buffer)
	}

	url := sessionUrl + "/" + m.RequestPath
	req, err := http.NewRequest(m.RequestMethod.String(), url, body)
	if err != nil {
		return nil, err
	}

	if m.Header.IsJSON {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}

	return httpClient.Do(req)
}
