package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/minio/minio-go"
	"github.com/minio/minio-go/pkg/policy"
	"github.com/pborman/uuid"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"syscall"
)

var (
	rootDir     string
	listenAddr  string
	s3Endpoint  string
	s3Region    string
	s3Bucket    string
	s3AccessKey string
	s3SecretKey string
)

func init() {
	flag.StringVar(&rootDir, "root", "", "http root directory (default: .)")
	flag.StringVar(&listenAddr, "listen", "", "address to listen to (default: :8080)")
	flag.StringVar(&s3Endpoint, "s3-endpoint", "", "s3 endpoint")
	flag.StringVar(&s3Region, "s3-region", "", "s3 region")
	flag.StringVar(&s3Bucket, "s3-bucket", "", "s3 bucket")
	flag.StringVar(&s3AccessKey, "s3-access-key", "", "s3 access key")
	flag.StringVar(&s3SecretKey, "s3-secret-key", "", "s3 secret key")
}

func main() {
	flag.Parse()

	if len(rootDir) == 0 {
		listenAddr = os.Getenv("PUT_ROOT")
	}

	if len(rootDir) == 0 {
		rootDir = "."
	}

	if len(listenAddr) == 0 {
		listenAddr = os.Getenv("PUT_LISTEN")
	}

	if len(listenAddr) == 0 {
		listenAddr = ":8080"
	}

	if len(s3Endpoint) == 0 {
		s3Endpoint = os.Getenv("PUT_S3_ENDPOINT")
	}

	if len(s3Region) == 0 {
		s3Region = os.Getenv("PUT_S3_REGION")
	}

	if len(s3Bucket) == 0 {
		s3Bucket = os.Getenv("PUT_S3_BUCKET")
	}

	if len(s3AccessKey) == 0 {
		s3AccessKey = os.Getenv("PUT_S3_ACCESS_KEY")
	}

	if len(s3SecretKey) == 0 {
		s3SecretKey = os.Getenv("PUT_S3_SECRET_KEY")
	}
	s3Url, err := url.Parse(s3Endpoint)

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	s3Client, err := minio.New(s3Url.Host, s3AccessKey, s3SecretKey, s3Url.Scheme == "https")

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	ok, err := s3Client.BucketExists(s3Bucket)

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if !ok {
		if err := s3Client.MakeBucket(s3Bucket, s3Region); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	}

	if err := s3Client.SetBucketPolicy(s3Bucket, "", policy.BucketPolicyReadOnly); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	l, err := listen(listenAddr)

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer l.Close()

	mux := http.NewServeMux()
	mux.Handle("/", NewRequestHandler(rootDir, s3Client, s3Endpoint, s3Bucket))
	mux.Handle("/health", NewHealthHandler(s3Client, s3Bucket))

	go http.Serve(l, mux)

	sig := make(chan os.Signal)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	<-sig
}

func listen(addr string) (net.Listener, error) {
	if len(addr) > 0 && addr[0] != '/' {
		return net.Listen("tcp", addr)
	}
	return net.Listen("unix", addr)
}

type RequestHandler struct {
	s3Client   *minio.Client
	s3Endpoint string
	s3Bucket   string
	fileServer http.Handler
}

func NewRequestHandler(rootDir string, s3Client *minio.Client, s3Endpoint, s3Bucket string) *RequestHandler {
	return &RequestHandler{s3Client, s3Endpoint, s3Bucket, http.FileServer(http.Dir(rootDir))}
}

func (rh *RequestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		rh.fileServer.ServeHTTP(w, r)
		return
	}

	if r.Method != "POST" && r.Method != "PUT" {
		http.Error(w, "page not found", 404)
		return
	}
	name := uuid.New()

	if _, err := rh.s3Client.PutObject(rh.s3Bucket, name, r.Body, r.Header.Get("Content-Type")); err != nil {
		fmt.Println("could not store object:", err)
		http.Error(w, "internal server error", 500)
		return
	}
	objectUrl, err := url.Parse(rh.s3Endpoint)

	if err != nil {
		fmt.Println("could not parse endpoint url:", err)
		http.Error(w, "internal server error", 500)
		return
	}
	objectUrl.Path = path.Join(rh.s3Bucket, name)

	if _, err := fmt.Fprintln(w, objectUrl.String()); err != nil {
		fmt.Println("could respond with object url:", err)
	}
}

type HealthHandler struct {
	s3Client *minio.Client
	s3Bucket string
}

type HealthPayload struct {
	Status string `json:"status"`
}

func NewHealthHandler(s3Client *minio.Client, s3Bucket string) *HealthHandler {
	return &HealthHandler{s3Client: s3Client, s3Bucket: s3Bucket}
}

func (hh *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	payload := &HealthPayload{Status: "ok"}
	ok, err := hh.s3Client.BucketExists(hh.s3Bucket)

	if err != nil {
		payload.Status = fmt.Sprintf("critical: %s", err)
	} else if !ok {
		payload.Status = fmt.Sprintf("critical: bucket \"%s\" unavailable", hh.s3Bucket)
	}
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		fmt.Println("could not respond with health payload:", err)
	}
}
