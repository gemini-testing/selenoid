//go:build s3
// +build s3

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aerokube/selenoid/event"
	"github.com/aerokube/selenoid/session"
	"github.com/aerokube/selenoid/upload"
	"github.com/klauspost/compress/zstd"
	assert "github.com/stretchr/testify/require"
)

var (
	s3Srv *httptest.Server
)

func init() {
	s3Srv = httptest.NewServer(s3Mux())
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}
	http.DefaultTransport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.Contains(addr, "s3-mock.example.com") {
			addr = s3Srv.Listener.Addr().String()
		}
		return dialer.DialContext(ctx, network, addr)
	}
}

func s3Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, _ *http.Request) {})
	return mux
}

var testSession = &session.Session{
	Quota: "some-user",
	Caps: session.Caps{
		Name:     "internet explorer",
		Version:  "11",
		Platform: "WINDOWS",
	},
}

func TestS3Uploader(t *testing.T) {
	uploader := &upload.S3Uploader{
		Endpoint:          "http://s3-mock.example.com",
		Region:            "us-west-1",
		AccessKey:         "some-access-key",
		SecretKey:         "some-secret-key",
		BucketName:        "test-bucket",
		KeyPattern:        "$fileName",
		ReducedRedundancy: true,
	}
	uploader.Init()
	f, _ := os.CreateTemp("", "some-file")
	input := event.CreatedFile{
		Event: event.Event{
			RequestId: 4342,
			SessionId: "some-session-id",
			Session:   testSession,
		},
		Name: f.Name(),
		Type: "log",
	}
	uploaded, err := uploader.Upload(input)
	assert.NoError(t, err)
	assert.True(t, uploaded)
}

type capturedUpload struct {
	contentType     string
	contentEncoding string
	body            []byte
}

func newCapturingUploader(t *testing.T, compression string) (*upload.S3Uploader, *capturedUpload, func()) {
	t.Helper()
	captured := &capturedUpload{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			captured.body = body
			captured.contentType = r.Header.Get("Content-Type")
			captured.contentEncoding = r.Header.Get("Content-Encoding")
		}
		w.WriteHeader(http.StatusOK)
	}))
	uploader := &upload.S3Uploader{
		Endpoint:       srv.URL,
		Region:         "us-west-1",
		AccessKey:      "some-access-key",
		SecretKey:      "some-secret-key",
		BucketName:     "test-bucket",
		KeyPattern:     "$fileName",
		ForcePathStyle: true,
		KeepFiles:      true,
		Compression:    compression,
	}
	uploader.Init()
	return uploader, captured, srv.Close
}

func logFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "some-file-*.log")
	assert.NoError(t, err)
	_, err = f.WriteString(content)
	assert.NoError(t, err)
	assert.NoError(t, f.Close())
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func TestS3UploadZstdCompressesLogs(t *testing.T) {
	uploader, captured, closeSrv := newCapturingUploader(t, "zstd")
	defer closeSrv()

	const content = "hello selenoid log content that should be compressed"
	input := event.CreatedFile{
		Event: event.Event{RequestId: 1, SessionId: "some-session-id", Session: testSession},
		Name:  logFile(t, content),
		Type:  "log",
	}

	uploaded, err := uploader.Upload(input)
	assert.NoError(t, err)
	assert.True(t, uploaded)

	assert.Equal(t, "text/plain; charset=utf-8", captured.contentType)
	assert.Equal(t, "zstd", captured.contentEncoding)
	assert.NotEqual(t, []byte(content), captured.body)

	dec, err := zstd.NewReader(bytes.NewReader(captured.body))
	assert.NoError(t, err)
	defer dec.Close()
	decompressed, err := io.ReadAll(dec)
	assert.NoError(t, err)
	assert.Equal(t, content, string(decompressed))
}

func TestS3UploadZstdOnlyCompressesLogType(t *testing.T) {
	uploader, captured, closeSrv := newCapturingUploader(t, "zstd")
	defer closeSrv()

	const content = "this is a video artifact, not a log"
	input := event.CreatedFile{
		Event: event.Event{RequestId: 2, SessionId: "some-session-id", Session: testSession},
		Name:  logFile(t, content),
		Type:  "video",
	}

	uploaded, err := uploader.Upload(input)
	assert.NoError(t, err)
	assert.True(t, uploaded)

	assert.Empty(t, captured.contentEncoding)
	assert.Equal(t, content, string(captured.body))
}

func TestS3UploadNoneLeavesLogsUncompressed(t *testing.T) {
	uploader, captured, closeSrv := newCapturingUploader(t, "none")
	defer closeSrv()

	const content = "plain uncompressed log content"
	input := event.CreatedFile{
		Event: event.Event{RequestId: 3, SessionId: "some-session-id", Session: testSession},
		Name:  logFile(t, content),
		Type:  "log",
	}

	uploaded, err := uploader.Upload(input)
	assert.NoError(t, err)
	assert.True(t, uploaded)

	assert.Empty(t, captured.contentEncoding)
	assert.Equal(t, content, string(captured.body))
}

func TestS3UploadEmptyCompressionDefaultsToNone(t *testing.T) {
	uploader, captured, closeSrv := newCapturingUploader(t, "")
	defer closeSrv()

	const content = "log content with default compression setting"
	input := event.CreatedFile{
		Event: event.Event{RequestId: 4, SessionId: "some-session-id", Session: testSession},
		Name:  logFile(t, content),
		Type:  "log",
	}

	uploaded, err := uploader.Upload(input)
	assert.NoError(t, err)
	assert.True(t, uploaded)

	assert.Empty(t, captured.contentEncoding)
	assert.Equal(t, content, string(captured.body))
}

func TestGetKey(t *testing.T) {
	const testPattern = "$quota/$sessionId_$browserName_$browserVersion_$platformName/$fileType$fileExtension"
	input := event.CreatedFile{
		Event: event.Event{
			SessionId: "some-Session-id",
			Session:   testSession,
			RequestId: 12345,
		},

		Name: "/path/to/Some-File.txt",
		Type: "log",
	}

	key := upload.GetS3Key(testPattern, input)
	assert.Equal(t, key, "some-user/some-Session-id_internet-explorer_11_windows/log.txt")

	input.Session.Caps.Name = ""
	input.Session.Caps.DeviceName = "internet explorer"
	key = upload.GetS3Key(testPattern, input)
	assert.Equal(t, key, "some-user/some-Session-id_internet-explorer_11_windows/log.txt")

	input.Session.Caps.S3KeyPattern = "$quota/$fileType$fileExtension"
	key = upload.GetS3Key(testPattern, input)
	assert.Equal(t, key, "some-user/log.txt")

	input.Session.Caps.S3KeyPattern = "$fileName"
	key = upload.GetS3Key(testPattern, input)
	assert.Equal(t, key, "Some-File.txt")
}

func TestFileMatches(t *testing.T) {
	matches, err := upload.FileMatches("", "", "any-file-name")
	assert.NoError(t, err)
	assert.True(t, matches)

	matches, err = upload.FileMatches("[", "", "/path/to/file.mp4")
	assert.Error(t, err)
	assert.False(t, matches)

	matches, err = upload.FileMatches("", "[", "/path/to/file.mp4")
	assert.Error(t, err)
	assert.False(t, matches)

	matches, err = upload.FileMatches("*.mp4", "", "/path/to/file.mp4")
	assert.NoError(t, err)
	assert.True(t, matches)

	matches, err = upload.FileMatches("*.mp4", "", "/path/to/file.log")
	assert.NoError(t, err)
	assert.False(t, matches)

	matches, err = upload.FileMatches("*.mp4", "", "/path/to/file.log")
	assert.NoError(t, err)
	assert.False(t, matches)

	matches, err = upload.FileMatches("", "*.log", "/path/to/file.log")
	assert.NoError(t, err)
	assert.False(t, matches)
}
