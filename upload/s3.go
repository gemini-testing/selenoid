//go:build s3
// +build s3

package upload

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aerokube/selenoid/event"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	awssession "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/klauspost/compress/zstd"
	"github.com/pkg/errors"
)

const (
	logFileType = "log"

	compressionNone = "none"
	compressionZstd = "zstd"
)

var zstdWriterPool = sync.Pool{
	New: func() any {
		w, _ := zstd.NewWriter(nil)
		return w
	},
}

func compressZstd(r io.Reader) ([]byte, error) {
	enc := zstdWriterPool.Get().(*zstd.Encoder)
	defer zstdWriterPool.Put(enc)

	var buf bytes.Buffer
	enc.Reset(&buf)
	if _, err := io.Copy(enc, r); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func init() {
	s3 := &S3Uploader{}
	flag.StringVar(&(s3.Endpoint), "s3-endpoint", "", "S3 endpoint URL")
	flag.StringVar(&(s3.Region), "s3-region", "", "S3 region")
	flag.StringVar(&(s3.AccessKey), "s3-access-key", "", "S3 access key")
	flag.StringVar(&(s3.SecretKey), "s3-secret-key", "", "S3 secret key")
	flag.StringVar(&(s3.BucketName), "s3-bucket-name", "", "S3 bucket name")
	flag.StringVar(&(s3.KeyPattern), "s3-key-pattern", "$fileName", "S3 bucket name")
	flag.BoolVar(&(s3.ReducedRedundancy), "s3-reduced-redundancy", false, "Use reduced redundancy storage class")
	flag.BoolVar(&(s3.KeepFiles), "s3-keep-files", false, "Do not remove uploaded files")
	flag.StringVar(&(s3.IncludeFiles), "s3-include-files", "", "Pattern used to match and include files")
	flag.StringVar(&(s3.ExcludeFiles), "s3-exclude-files", "", "Pattern used to match and exclude files")
	flag.BoolVar(&(s3.ForcePathStyle), "s3-force-path-style", false, "Force path-style addressing for file upload")
	flag.StringVar(&(s3.Compression), "s3-compression", compressionNone, "Compression for logs before upload: \"none\" or \"zstd\"")
	AddUploader(s3)
}

type S3Uploader struct {
	Endpoint          string
	Region            string
	AccessKey         string
	SecretKey         string
	BucketName        string
	KeyPattern        string
	ReducedRedundancy bool
	KeepFiles         bool
	IncludeFiles      string
	ExcludeFiles      string
	ForcePathStyle    bool
	Compression       string

	manager *s3manager.Uploader
}

func (s3 *S3Uploader) Init() {
	if s3.Endpoint != "" {
		switch s3.Compression {
		case "", compressionNone:
			s3.Compression = compressionNone
		case compressionZstd:
		default:
			log.Fatalf("[-] [INIT] [Failed to initialize S3 support: unsupported compression %q, allowed values are %q and %q]", s3.Compression, compressionNone, compressionZstd)
		}
		config := &aws.Config{
			Endpoint:         aws.String(s3.Endpoint),
			Region:           aws.String(s3.Region),
			S3ForcePathStyle: aws.Bool(s3.ForcePathStyle),
		}
		if s3.AccessKey != "" && s3.SecretKey != "" {
			config.Credentials = credentials.NewStaticCredentials(s3.AccessKey, s3.SecretKey, "")
		}
		sess, err := awssession.NewSession(config)
		if err != nil {
			log.Fatalf("[-] [INIT] [Failed to initialize S3 support: %v]", err)
		}
		log.Printf("[-] [INIT] [Initialized S3 support: endpoint = %s, region = %s, bucketName = %s, accessKey = %s, keyPattern = %s, includeFiles = %s, excludeFiles = %s, forcePathStyle = %t, compression = %s]", s3.Endpoint, s3.Region, s3.BucketName, s3.AccessKey, s3.KeyPattern, s3.IncludeFiles, s3.ExcludeFiles, s3.ForcePathStyle, s3.Compression)
		s3.manager = s3manager.NewUploader(sess)
	}
}

func (s3 *S3Uploader) Upload(createdFile event.CreatedFile) (bool, error) {
	if s3.manager != nil {
		filename := createdFile.Name
		fileMatches, err := FileMatches(s3.IncludeFiles, s3.ExcludeFiles, filename)
		if err != nil {
			return false, fmt.Errorf("invalid pattern: %v", err)
		}
		if !fileMatches {
			log.Printf("[%d] [SKIPPING_FILE] [%s] [Does not match specified patterns]", createdFile.RequestId, createdFile.Name)
			return false, nil
		}
		key := GetS3Key(s3.KeyPattern, createdFile)
		file, err := os.Open(filename)
		if err != nil {
			return false, fmt.Errorf("failed to open file %s: %v", filename, err)
		}
		defer file.Close()
		uploadInput := &s3manager.UploadInput{
			Bucket: new(s3.BucketName),
			Key:    new(key),
			Body:   file,
		}
		if s3.Compression == compressionZstd && createdFile.Type == logFileType {
			compressed, err := compressZstd(file)
			if err != nil {
				return false, fmt.Errorf("failed to compress log file %s: %v", filename, err)
			}
			uploadInput.Body = bytes.NewReader(compressed)
			uploadInput.ContentType = new("text/plain; charset=utf-8")
			uploadInput.ContentEncoding = new("zstd")
		} else {
			contentType := mime.TypeByExtension(filepath.Ext(filename))
			if contentType != "" {
				uploadInput.ContentType = new(contentType)
			}
		}
		if s3.ReducedRedundancy {
			uploadInput.StorageClass = new("REDUCED_REDUNDANCY")
		}
		_, err = s3.manager.Upload(uploadInput)
		if err != nil {
			return false, fmt.Errorf("failed to S3 upload %s as %s: %v", filename, key, err)
		}
		if !s3.KeepFiles {
			err := os.Remove(filename)
			if err != nil {
				return true, fmt.Errorf("failed to remove uploaded file %s: %v", filename, err)
			}
		}
		return true, nil
	}
	return false, errors.New("S3 uploader is not initialized")
}

func FileMatches(includedFiles string, excludedFiles string, filename string) (bool, error) {
	fileIncluded := true
	if includedFiles != "" {
		fi, err := filepath.Match(includedFiles, filepath.Base(filename))
		if err != nil {
			return false, fmt.Errorf("failed to match included file: %v", err)
		}
		fileIncluded = fi
	}
	fileExcluded := false
	if excludedFiles != "" {
		fe, err := filepath.Match(excludedFiles, filepath.Base(filename))
		if err != nil {
			return false, fmt.Errorf("failed to match excluded file: %v", err)
		}
		fileExcluded = fe
	}
	return fileIncluded && !fileExcluded, nil
}

func GetS3Key(keyPattern string, createdFile event.CreatedFile) string {
	sess := createdFile.Session
	pt := keyPattern
	if sess.Caps.S3KeyPattern != "" {
		pt = sess.Caps.S3KeyPattern
	}
	filename := createdFile.Name
	key := strings.Replace(pt, "$fileName", filepath.Base(filename), -1)
	key = strings.Replace(key, "$fileExtension", strings.ToLower(filepath.Ext(filename)), -1)
	key = strings.Replace(key, "$browserName", strings.ToLower(sess.Caps.BrowserName()), -1)
	key = strings.Replace(key, "$browserVersion", strings.ToLower(sess.Caps.Version), -1)
	key = strings.Replace(key, "$platformName", strings.ToLower(sess.Caps.Platform), -1)
	key = strings.Replace(key, "$quota", strings.ToLower(sess.Quota), -1)
	key = strings.Replace(key, "$sessionId", createdFile.SessionId, -1)
	key = strings.Replace(key, "$fileType", strings.ToLower(createdFile.Type), -1)
	key = strings.Replace(key, "$date", time.Now().Format("2006-01-02"), -1)
	key = strings.Replace(key, " ", "-", -1)
	return key
}
