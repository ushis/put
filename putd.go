package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/minio/minio-go"
	"github.com/pborman/uuid"
	"github.com/rcrowley/go-metrics"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"syscall"
)

var (
	listenAddr  string
	s3Endpoint  string
	s3Bucket    string
	s3AccessKey string
	s3SecretKey string
)

func init() {
	flag.StringVar(&listenAddr, "listen", "", "address to listen to (default: :8080)")
	flag.StringVar(&s3Endpoint, "s3-endpoint", "", "s3 endpoint")
	flag.StringVar(&s3Bucket, "s3-bucket", "", "s3 bucket")
	flag.StringVar(&s3AccessKey, "s3-access-key", "", "s3 access key")
	flag.StringVar(&s3SecretKey, "s3-secret-key", "", "s3 secret key")
}

func main() {
	flag.Parse()

	if len(listenAddr) == 0 {
		listenAddr = os.Getenv("PUT_LISTEN")
	}

	if len(listenAddr) == 0 {
		listenAddr = ":8080"
	}

	if len(s3Endpoint) == 0 {
		s3Endpoint = os.Getenv("PUT_S3_ENDPOINT")
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
	url, err := url.Parse(s3Endpoint)

	if err != nil {
		fmt.Println("could not parse s3 endpoint:", err)
		os.Exit(1)
	}
	s3Client, err := minio.New(url.Host, s3AccessKey, s3SecretKey, url.Scheme == "https")

	if err != nil {
		fmt.Println("could not set up s3 client:", err)
		os.Exit(1)
	}
	l, err := listen(listenAddr)

	if err != nil {
		fmt.Println("could not set up listener:", err)
		os.Exit(1)
	}
	defer l.Close()

	s3 := NewS3(s3Client, s3Endpoint, s3Bucket)
	metrics := NewMetrics()

	mux := http.NewServeMux()
	mux.Handle("/", NewRequestHandler(s3, metrics))
	mux.Handle("/health", NewHealthHandler(s3))
	mux.Handle("/metrics", NewMetricsHandler(metrics, s3))

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

type S3 struct {
	client   *minio.Client
	endpoint string
	bucket   string
}

func NewS3(client *minio.Client, endpoint, bucket string) *S3 {
	return &S3{client: client, endpoint: endpoint, bucket: bucket}
}

func (s3 *S3) Put(name, contentType string, body io.Reader) (string, error) {
	url, err := url.Parse(s3.endpoint)

	if err != nil {
		return "", err
	}
	meta := minio.PutObjectOptions{ContentType: contentType}

	if _, err := s3.client.PutObject(s3.bucket, name, body, -1, meta); err != nil {
		return "", err
	}
	url.Path = path.Join(s3.bucket, name)
	return url.String(), nil
}

func (s3 *S3) HealthCheck() error {
	ok, err := s3.client.BucketExists(s3.bucket)

	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("bucket unavailable: %s", s3.bucket)
	}
	return nil
}

func (s3 *S3) Metrics() (count int64, size int64, err error) {
	for obj := range s3.client.ListObjects(s3.bucket, "", false, make(chan struct{})) {
		count += 1
		size += obj.Size
	}
	return count, size, err
}

type RequestHandler struct {
	s3      *S3
	metrics Metrics
}

func NewRequestHandler(s3 *S3, metrics Metrics) *RequestHandler {
	return &RequestHandler{
		s3:      s3,
		metrics: metrics,
	}
}

func (rh *RequestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		rh.ServeIndex(w)
		return
	}
	rh.metrics.Total.Inc(1)

	if r.Method != "POST" && r.Method != "PUT" {
		rh.metrics.Invalid.Inc(1)
		w.Header().Set("Allow", "GET, POST, PUT")
		http.Error(w, "method not allowed", 405)
		return
	}
	objectUrl, err := rh.s3.Put(uuid.New(), r.Header.Get("Content-Type"), r.Body)

	if err != nil {
		rh.metrics.Failed.Inc(1)
		fmt.Println("could not store object:", err)
		http.Error(w, "internal server error", 500)
		return
	}

	if _, err := fmt.Fprintln(w, objectUrl); err != nil {
		rh.metrics.Failed.Inc(1)
		fmt.Println("could not respond with object url:", err)
		return
	}
	rh.metrics.Success.Inc(1)
}

func (rh *RequestHandler) ServeIndex(w http.ResponseWriter) {
	asset, err := indexHtml()

	if err != nil {
		fmt.Println("could not fetch index.html:", err)
		http.Error(w, "internal server error", 500)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.FormatInt(asset.info.Size(), 10))
	w.Header().Set("Last-Modified", asset.info.ModTime().Format(http.TimeFormat))

	if _, err = w.Write(asset.bytes); err != nil {
		fmt.Println("could not send index.html:", err)
	}
}

type HealthHandler struct {
	s3 *S3
}

type HealthPayload struct {
	Status string `json:"status"`
}

func NewHealthHandler(s3 *S3) *HealthHandler {
	return &HealthHandler{s3: s3}
}

func (hh *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	payload := &HealthPayload{Status: "ok"}

	if err := hh.s3.HealthCheck(); err != nil {
		payload.Status = fmt.Sprintf("critical: %s", err)
	}
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		fmt.Println("could not respond with health payload:", err)
	}
}

type Metrics struct {
	Total   metrics.Counter
	Invalid metrics.Counter
	Failed  metrics.Counter
	Success metrics.Counter
}

func NewMetrics() Metrics {
	return Metrics{
		Total:   metrics.NewCounter(),
		Invalid: metrics.NewCounter(),
		Failed:  metrics.NewCounter(),
		Success: metrics.NewCounter(),
	}
}

type MetricsHandler struct {
	metrics Metrics
	s3      *S3
}

type MetricsPayload struct {
	RequestsTotal   int64 `json:"requests_total"`
	RequestsInvalid int64 `json:"requests_invalid"`
	RequestsFailed  int64 `json:"requests_failed"`
	RequestsSuccess int64 `json:"requests_success"`
	S3Objects       int64 `json:"s3_objects"`
	S3Usage         int64 `json:"s3_usage"`
}

func NewMetricsHandler(metrics Metrics, s3 *S3) *MetricsHandler {
	return &MetricsHandler{metrics: metrics, s3: s3}
}

func (mh *MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s3Objects, s3Usage, _ := mh.s3.Metrics()

	payload := MetricsPayload{
		RequestsTotal:   mh.metrics.Total.Count(),
		RequestsInvalid: mh.metrics.Invalid.Count(),
		RequestsFailed:  mh.metrics.Failed.Count(),
		RequestsSuccess: mh.metrics.Success.Count(),
		S3Objects:       s3Objects,
		S3Usage:         s3Usage,
	}
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		fmt.Println("could not respond with metrics payload:", err)
	}
}
