package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/minio/minio-go"
	"github.com/minio/minio-go/pkg/policy"
	"github.com/pborman/uuid"
	"github.com/rcrowley/go-metrics"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"syscall"
)

var (
	rootDir    string
	listenAddr string
	s3         S3
)

func init() {
	flag.StringVar(&rootDir, "root", "", "http root directory (default: .)")
	flag.StringVar(&listenAddr, "listen", "", "address to listen to (default: :8080)")
	flag.StringVar(&s3.endpoint, "s3-endpoint", "", "s3 endpoint")
	flag.StringVar(&s3.region, "s3-region", "", "s3 region")
	flag.StringVar(&s3.bucket, "s3-bucket", "", "s3 bucket")
	flag.StringVar(&s3.accessKey, "s3-access-key", "", "s3 access key")
	flag.StringVar(&s3.secretKey, "s3-secret-key", "", "s3 secret key")
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

	if len(s3.endpoint) == 0 {
		s3.endpoint = os.Getenv("PUT_S3_ENDPOINT")
	}

	if len(s3.region) == 0 {
		s3.region = os.Getenv("PUT_S3_REGION")
	}

	if len(s3.bucket) == 0 {
		s3.bucket = os.Getenv("PUT_S3_BUCKET")
	}

	if len(s3.accessKey) == 0 {
		s3.accessKey = os.Getenv("PUT_S3_ACCESS_KEY")
	}

	if len(s3.secretKey) == 0 {
		s3.secretKey = os.Getenv("PUT_S3_SECRET_KEY")
	}
	l, err := listen(listenAddr)

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer l.Close()

	metrics := NewMetrics()

	mux := http.NewServeMux()
	mux.Handle("/", NewRequestHandler(rootDir, &s3, metrics))
	mux.Handle("/health", NewHealthHandler(&s3))
	mux.Handle("/metrics", NewMetricsHandler(metrics))

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
	minio     *minio.Client
	url       *url.URL
	endpoint  string
	bucket    string
	region    string
	secretKey string
	accessKey string
}

func (s3 *S3) Put(name, content_type string, body io.Reader) (string, error) {
	client, err := s3.client()

	if err != nil {
		return "", err
	}

	if _, err := client.PutObject(s3.bucket, name, body, content_type); err != nil {
		return "", err
	}
	s3.url.Path = path.Join(s3.bucket, name)
	return s3.url.String(), nil
}

func (s3 *S3) HealthCheck() error {
	client, err := s3.client()

	if err != nil {
		return err
	}
	ok, err := client.BucketExists(s3.bucket)

	if err != nil {
		return err
	}

	if !ok {
		fmt.Errorf("bucket unavailable: %s", s3.bucket)
	}
	return nil
}

func (s3 *S3) client() (client *minio.Client, err error) {
	if s3.minio != nil {
		return s3.minio, nil
	}
	s3.url, err = url.Parse(s3.endpoint)

	if err != nil {
		return nil, err
	}
	client, err = minio.New(s3.url.Host, s3.accessKey, s3.secretKey, s3.url.Scheme == "https")

	if err != nil {
		return nil, err
	}
	ok, err := client.BucketExists(s3.bucket)

	if err != nil {
		return nil, err
	}

	if !ok {
		if err := client.MakeBucket(s3.bucket, s3.region); err != nil {
			return nil, err
		}
	}

	if err := client.SetBucketPolicy(s3.bucket, "", policy.BucketPolicyReadOnly); err != nil {
		return nil, err
	}

	s3.minio = client
	return client, nil
}

type RequestHandler struct {
	s3         *S3
	metrics    Metrics
	fileServer http.Handler
}

func NewRequestHandler(rootDir string, s3 *S3, metrics Metrics) *RequestHandler {
	return &RequestHandler{
		s3:         s3,
		metrics:    metrics,
		fileServer: http.FileServer(http.Dir(rootDir)),
	}
}

func (rh *RequestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		rh.fileServer.ServeHTTP(w, r)
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
		fmt.Println("could respond with object url:", err)
		return
	}
	rh.metrics.Success.Inc(1)
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

type Counter struct {
	metrics.Counter
}

func NewCounter() Counter {
	return Counter{metrics.NewCounter()}
}

func (c Counter) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.Count())
}

type Metrics struct {
	Total   Counter `json:"total"`
	Invalid Counter `json:"invalid"`
	Failed  Counter `json:"failed"`
	Success Counter `json:"success"`
}

func NewMetrics() Metrics {
	return Metrics{
		Total:   NewCounter(),
		Invalid: NewCounter(),
		Failed:  NewCounter(),
		Success: NewCounter(),
	}
}

type MetricsHandler struct {
	metrics Metrics
}

func NewMetricsHandler(metrics Metrics) *MetricsHandler {
	return &MetricsHandler{metrics: metrics}
}

func (mh *MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(mh.metrics); err != nil {
		fmt.Println("could not respond with metrics payload:", err)
	}
}
