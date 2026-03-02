package wsdriver

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	assert "github.com/stretchr/testify/require"
)

func TestMakeRequest_GetNoBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/session/abc/url", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		assert.Empty(t, body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"value":"http://example.com"}`))
	}))
	defer server.Close()

	msg := &RequestMessage{
		Header:        Header{CompressionType: CompressionNone},
		RequestMethod: RequestGet,
		RequestPath:   "session/abc/url",
		Buffer:        nil,
	}

	httpBuffer := new(bytes.Buffer)
	resp, err := MakeRequest(server.URL, msg, httpBuffer)
	assert.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMakeRequest_PostWithJSONBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "application/json; charset=utf-8", r.Header.Get("Content-Type"))
		body, _ := io.ReadAll(r.Body)
		assert.Equal(t, `{"url":"http://example.com"}`, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	msg := &RequestMessage{
		Header:        Header{CompressionType: CompressionNone, IsJSON: true},
		RequestMethod: RequestPost,
		RequestPath:   "session/abc/url",
		Buffer:        []byte(`{"url":"http://example.com"}`),
	}

	httpBuffer := new(bytes.Buffer)
	resp, err := MakeRequest(server.URL, msg, httpBuffer)
	assert.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMakeRequest_PostNoContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	msg := &RequestMessage{
		Header:        Header{CompressionType: CompressionNone, IsJSON: false},
		RequestMethod: RequestPost,
		RequestPath:   "session/abc/element",
		Buffer:        []byte("raw data"),
	}

	httpBuffer := new(bytes.Buffer)
	resp, err := MakeRequest(server.URL, msg, httpBuffer)
	assert.NoError(t, err)
	defer resp.Body.Close()
}

func TestMakeRequest_GzipDecompression(t *testing.T) {
	expectedBody := `{"url":"http://example.com"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.Equal(t, expectedBody, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Compress the body with gzip
	var compressed bytes.Buffer
	gw, err := gzip.NewWriterLevel(&compressed, gzip.DefaultCompression)
	assert.NoError(t, err)
	_, err = gw.Write([]byte(expectedBody))
	assert.NoError(t, err)
	err = gw.Close()
	assert.NoError(t, err)

	msg := &RequestMessage{
		Header:        Header{CompressionType: CompressionGZIP, IsJSON: true},
		RequestMethod: RequestPost,
		RequestPath:   "session/abc/url",
		Buffer:        compressed.Bytes(),
	}

	httpBuffer := new(bytes.Buffer)
	resp, err := MakeRequest(server.URL, msg, httpBuffer)
	assert.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMakeRequest_ZstdDecompression(t *testing.T) {
	expectedBody := `{"url":"http://example.com"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.Equal(t, expectedBody, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Compress the body with zstd
	var compressed bytes.Buffer
	zw, err := zstd.NewWriter(&compressed)
	assert.NoError(t, err)
	_, err = zw.Write([]byte(expectedBody))
	assert.NoError(t, err)
	err = zw.Close()
	assert.NoError(t, err)

	msg := &RequestMessage{
		Header:        Header{CompressionType: CompressionZSTD, IsJSON: true},
		RequestMethod: RequestPost,
		RequestPath:   "session/abc/url",
		Buffer:        compressed.Bytes(),
	}

	httpBuffer := new(bytes.Buffer)
	resp, err := MakeRequest(server.URL, msg, httpBuffer)
	assert.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMakeRequest_DeleteMethod(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "DELETE", r.Method)
		assert.Equal(t, "/session/abc", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	msg := &RequestMessage{
		Header:        Header{CompressionType: CompressionNone},
		RequestMethod: RequestDelete,
		RequestPath:   "session/abc",
		Buffer:        nil,
	}

	httpBuffer := new(bytes.Buffer)
	resp, err := MakeRequest(server.URL, msg, httpBuffer)
	assert.NoError(t, err)
	defer resp.Body.Close()
}

func TestMakeRequest_InvalidGzipData(t *testing.T) {
	msg := &RequestMessage{
		Header:        Header{CompressionType: CompressionGZIP},
		RequestMethod: RequestPost,
		RequestPath:   "session/abc/url",
		Buffer:        []byte("not-gzip-data"),
	}

	httpBuffer := new(bytes.Buffer)
	_, err := MakeRequest("http://localhost:0", msg, httpBuffer)
	assert.Error(t, err)
}

func TestMakeRequest_ConnectionRefused(t *testing.T) {
	msg := &RequestMessage{
		Header:        Header{CompressionType: CompressionNone},
		RequestMethod: RequestGet,
		RequestPath:   "status",
		Buffer:        nil,
	}

	httpBuffer := new(bytes.Buffer)
	_, err := MakeRequest("http://127.0.0.1:1", msg, httpBuffer)
	assert.Error(t, err)
}

func TestMakeRequest_URLConstruction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/session/abc/element/xyz/click", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	msg := &RequestMessage{
		Header:        Header{CompressionType: CompressionNone},
		RequestMethod: RequestPost,
		RequestPath:   "session/abc/element/xyz/click",
		Buffer:        nil,
	}

	httpBuffer := new(bytes.Buffer)
	resp, err := MakeRequest(server.URL, msg, httpBuffer)
	assert.NoError(t, err)
	defer resp.Body.Close()
}
