/*
package stream provides http.Handler middleware that solves several problems when dealing with http requests:

Reads the entire request and response into buffer, optionally buffering it to disk for large requests.
Checks the limits for the requests and responses, rejecting in case if the limit was exceeded.
Changes request content-transfer-encoding from chunked and provides total size to the handlers.

Examples of a streaming middleware:

  // sample HTTP handler
  handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
    w.Write([]byte("hello"))
  })

  // Stream will read the body in buffer before passing the request to the handler
  // calculate total size of the request and transform it from chunked encoding
  // before passing to the server
  stream.New(handler)

  // This version will buffer up to 2MB in memory and will serialize any extra
  // to a temporary file, if the request size exceeds 10MB it will reject the request
  stream.New(handler,
    stream.MemRequestBodyBytes(2 * 1024 * 1024),
    stream.MaxRequestBodyBytes(10 * 1024 * 1024))

  // Will do the same as above, but with responses
  stream.New(handler,
    stream.MemResponseBodyBytes(2 * 1024 * 1024),
    stream.MaxResponseBodyBytes(10 * 1024 * 1024))

  // Stream will replay the request if the handler returns error at least 3 times
  // before returning the response
  stream.New(handler, stream.Retry(`IsNetworkError() && Attempts() <= 2`))

*/
package stream

import (
	"fmt"
	"io"
	"net/http"

	"github.com/mailgun/multibuf"
	"github.com/mailgun/oxy/utils"
)

const (
	// Store up to 1MB in RAM
	DefaultMemBodyBytes = 1048576
	// No limit by default
	DefaultMaxBodyBytes = -1
	// Maximum retry attempts
	DefaultMaxRetryAttempts = 10
)

var errHandler utils.ErrorHandler = &SizeErrHandler{}

// Streamer is responsible for streaming requests and responses
// It buffers large reqeuests and responses to disk,
type Streamer struct {
	maxRequestBodyBytes int64
	memRequestBodyBytes int64

	maxResponseBodyBytes int64
	memResponseBodyBytes int64

	retryPredicate hpredicate

	next       http.Handler
	errHandler utils.ErrorHandler
	log        utils.Logger
}

// New returns a new streamer middleware. New() function supports optional functional arguments
func New(next http.Handler, setters ...optSetter) (*Streamer, error) {
	strm := &Streamer{
		next: next,

		maxRequestBodyBytes: DefaultMaxBodyBytes,
		memRequestBodyBytes: DefaultMemBodyBytes,

		maxResponseBodyBytes: DefaultMaxBodyBytes,
		memResponseBodyBytes: DefaultMemBodyBytes,
	}
	for _, s := range setters {
		if err := s(strm); err != nil {
			return nil, err
		}
	}
	if strm.errHandler == nil {
		strm.errHandler = errHandler
	}

	if strm.log == nil {
		strm.log = utils.NullLogger
	}

	return strm, nil
}

type optSetter func(s *Streamer) error

// Retry provides a predicate that allows stream middleware to replay the request
// if it matches certain condition, e.g. returns special error code. Available functions are:
//
// Attempts() - limits the amount of retry attempts
// ResponseCode() - returns http response code
// IsNetworkError() - tests if response code is related to networking error
//
// Example of the predicate:
//
// `Attempts() <= 2 && ResponseCode() == 502`
//
func Retry(predicate string) optSetter {
	return func(s *Streamer) error {
		p, err := parseExpression(predicate)
		if err != nil {
			return err
		}
		s.retryPredicate = p
		return nil
	}
}

// Logger sets the logger that will be used by this middleware.
func Logger(l utils.Logger) optSetter {
	return func(s *Streamer) error {
		s.log = l
		return nil
	}
}

// ErrorHandler sets error handler of the server
func ErrorHandler(h utils.ErrorHandler) optSetter {
	return func(s *Streamer) error {
		s.errHandler = h
		return nil
	}
}

// MaxRequestBodyBytes sets the maximum request body size in bytes
func MaxRequestBodyBytes(m int64) optSetter {
	return func(s *Streamer) error {
		if m < 0 {
			return fmt.Errorf("max bytes should be >= 0 got %d", m)
		}
		s.maxRequestBodyBytes = m
		return nil
	}
}

// MaxRequestBody bytes sets the maximum request body to be stored in memory
// stream middleware will serialize the excess to disk.
func MemRequestBodyBytes(m int64) optSetter {
	return func(s *Streamer) error {
		if m < 0 {
			return fmt.Errorf("mem bytes should be >= 0 got %d", m)
		}
		s.memRequestBodyBytes = m
		return nil
	}
}

// MaxResponseBodyBytes sets the maximum request body size in bytes
func MaxResponseBodyBytes(m int64) optSetter {
	return func(s *Streamer) error {
		if m < 0 {
			return fmt.Errorf("max bytes should be >= 0 got %d", m)
		}
		s.maxResponseBodyBytes = m
		return nil
	}
}

// MemResponseBodyBytes sets the maximum request body to be stored in memory
// stream middleware will serialize the excess to disk.
func MemResponseBodyBytes(m int64) optSetter {
	return func(s *Streamer) error {
		if m < 0 {
			return fmt.Errorf("mem bytes should be >= 0 got %d", m)
		}
		s.memResponseBodyBytes = m
		return nil
	}
}

// Wrap sets the next handler to be called by stream handler.
func (s *Streamer) Wrap(next http.Handler) error {
	s.next = next
	return nil
}

func (s *Streamer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if err := s.checkLimit(req); err != nil {
		s.log.Infof("request body over limit: %v", err)
		s.errHandler.ServeHTTP(w, req, err)
		return
	}

	// Read the body while keeping limits in mind. This reader controls the maximum bytes
	// to read into memory and disk. This reader returns an error if the total request size exceeds the
	// prefefined MaxSizeBytes. This can occur if we got chunked request, in this case ContentLength would be set to -1
	// and the reader would be unbounded bufio in the http.Server
	body, err := multibuf.New(req.Body, multibuf.MaxBytes(s.maxRequestBodyBytes), multibuf.MemBytes(s.memRequestBodyBytes))
	if err != nil || body == nil {
		s.errHandler.ServeHTTP(w, req, err)
		return
	}

	// Set request body to buffered reader that can replay the read and execute Seek
	// Note that we don't change the original request body as it's handled by the http server
	// and we don'w want to mess with standard library
	defer body.Close()

	// We need to set ContentLength based on known request size. The incoming request may have been
	// set without content length or using chunked TransferEncoding
	totalSize, err := body.Size()
	if err != nil {
		s.log.Errorf("failed to get size, err %v", err)
		s.errHandler.ServeHTTP(w, req, err)
		return
	}

	outreq := s.copyRequest(req, body, totalSize)

	attempt := 1
	for {
		// We create a special writer that will limit the response size, buffer it to disk if necessary
		writer, err := multibuf.NewWriterOnce(multibuf.MaxBytes(s.maxResponseBodyBytes), multibuf.MemBytes(s.memResponseBodyBytes))
		if err != nil {
			s.errHandler.ServeHTTP(w, req, err)
			return
		}

		// We are mimicking http.ResponseWriter to replace writer with our special writer
		b := &bufferWriter{
			header: make(http.Header),
			buffer: writer,
		}
		defer b.Close()

		s.next.ServeHTTP(b, outreq)

		s.log.Infof("next responded: %v", b.code)

		reader, err := writer.Reader()
		if err != nil {
			s.log.Errorf("failed to read response, err %v", err)
			s.errHandler.ServeHTTP(w, req, err)
			return
		}
		defer reader.Close()

		if (s.retryPredicate == nil || attempt > DefaultMaxRetryAttempts) ||
			!s.retryPredicate(&context{r: req, attempt: attempt, responseCode: b.code, log: s.log}) {
			copyHeaders(w.Header(), b.Header())
			w.WriteHeader(b.code)
			io.Copy(w, reader)
			return
		}

		attempt += 1
		if _, err := body.Seek(0, 0); err != nil {
			s.log.Errorf("Failed to rewind: error: %v", err)
			s.errHandler.ServeHTTP(w, req, err)
			return
		}
		outreq = s.copyRequest(req, body, totalSize)
		s.log.Infof("retry Request(%v %v) attempt %v", req.Method, req.URL, attempt)
	}
}

func (s *Streamer) copyRequest(req *http.Request, body io.ReadCloser, bodySize int64) *http.Request {
	o := *req
	o.URL = utils.CopyURL(req.URL)
	o.Header = make(http.Header)
	utils.CopyHeaders(o.Header, req.Header)
	o.ContentLength = bodySize
	// remove TransferEncoding that could have been previously set because we have transformed the request from chunked encoding
	o.TransferEncoding = []string{}
	o.Body = body
	return &o
}

func (s *Streamer) checkLimit(req *http.Request) error {
	if s.maxRequestBodyBytes <= 0 {
		return nil
	}
	if req.ContentLength > s.maxRequestBodyBytes {
		return &multibuf.MaxSizeReachedError{MaxSize: s.maxRequestBodyBytes}
	}
	return nil
}

type bufferWriter struct {
	header http.Header
	code   int
	buffer multibuf.WriterOnce
}

func (b *bufferWriter) Close() error {
	return b.buffer.Close()
}

func (b *bufferWriter) Header() http.Header {
	return b.header
}

func (b *bufferWriter) Write(buf []byte) (int, error) {
	return b.buffer.Write(buf)
}

// WriteHeader sets rw.Code.
func (b *bufferWriter) WriteHeader(code int) {
	b.code = code
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

type SizeErrHandler struct {
}

func (e *SizeErrHandler) ServeHTTP(w http.ResponseWriter, req *http.Request, err error) {
	if _, ok := err.(*multibuf.MaxSizeReachedError); ok {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		w.Write([]byte(http.StatusText(http.StatusRequestEntityTooLarge)))
		return
	}
	utils.DefaultHandler.ServeHTTP(w, req, err)
}
