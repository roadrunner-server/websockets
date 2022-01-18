package validator

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	json "github.com/json-iterator/go"
	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/websockets/v2/attributes"
)

type AccessValidatorFn = func(r *http.Request, channels ...string) (*AccessValidator, error)

const (
	joinServer string = "ws:joinServer"
	joinTopics string = "ws:joinTopics"
)

type AccessValidator struct {
	Header http.Header `json:"headers"`
	Status int         `json:"status"`
	Body   []byte
}

// Request maps net/http requests to PSR7 compatible structure and managed state of temporary uploaded files.
type Request struct {
	// RemoteAddr contains ip address of client, make sure to check X-Real-Ip and X-Forwarded-For for real client address.
	RemoteAddr string `json:"remoteAddr"`

	// Protocol includes HTTP protocol version.
	Protocol string `json:"protocol"`

	// Method contains name of HTTP method used for the request.
	Method string `json:"method"`

	// URI contains full request URI with scheme and query.
	URI string `json:"uri"`

	// Header contains list of request headers.
	Header http.Header `json:"headers"`

	// Cookies contains list of request cookies.
	Cookies map[string]string `json:"cookies"`

	// RawQuery contains non parsed query string (to be parsed on php end).
	RawQuery string `json:"rawQuery"`

	// Parsed indicates that request body has been parsed on RR end.
	Parsed bool `json:"parsed"`

	// Attributes can be set by chained mdwr to safely pass value from Golang to PHP. See: GetAttribute, SetAttribute functions.
	Attributes map[string]interface{} `json:"attributes"`
}

func ServerAccessValidator(r *http.Request) ([]byte, error) {
	const op = errors.Op("server_access_validator")

	err := attributes.Set(r, "ws:joinServer", true)
	if err != nil {
		return nil, errors.E(op, err)
	}

	defer delete(attributes.All(r), joinServer)

	rq := r.URL.RawQuery
	rq = strings.ReplaceAll(rq, "\n", "")
	rq = strings.ReplaceAll(rq, "\r", "")

	req := &Request{
		RemoteAddr: FetchIP(r.RemoteAddr),
		Protocol:   r.Proto,
		Method:     r.Method,
		URI:        URI(r),
		Header:     r.Header,
		Cookies:    make(map[string]string),
		RawQuery:   rq,
		Attributes: attributes.All(r),
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, errors.E(op, err)
	}

	return data, nil
}

func TopicsAccessValidator(r *http.Request, topics ...string) ([]byte, error) {
	const op = errors.Op("topic_access_validator")
	err := attributes.Set(r, "ws:joinTopics", strings.Join(topics, ","))
	if err != nil {
		return nil, errors.E(op, err)
	}

	defer delete(attributes.All(r), joinTopics)

	rq := r.URL.RawQuery
	rq = strings.ReplaceAll(rq, "\n", "")
	rq = strings.ReplaceAll(rq, "\r", "")

	req := &Request{
		RemoteAddr: FetchIP(r.RemoteAddr),
		Protocol:   r.Proto,
		Method:     r.Method,
		URI:        URI(r),
		Header:     r.Header,
		Cookies:    make(map[string]string),
		RawQuery:   rq,
		Attributes: attributes.All(r),
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, errors.E(op, err)
	}

	return data, nil
}

// TODO(rustatian): to sdk/utils
func FetchIP(pair string) string {
	if !strings.ContainsRune(pair, ':') {
		return pair
	}

	addr, _, _ := net.SplitHostPort(pair)
	return addr
}

// URI fetches full uri from request in a form of string (including https scheme if TLS connection is enabled).
// TODO(rustatian): to sdk/utils
func URI(r *http.Request) string {
	uri := r.URL.String()
	uri = strings.ReplaceAll(uri, "\n", "")
	uri = strings.ReplaceAll(uri, "\r", "")

	if r.URL.Host != "" {
		return uri
	}

	if r.TLS != nil {
		return fmt.Sprintf("https://%s%s", r.Host, uri)
	}

	return fmt.Sprintf("http://%s%s", r.Host, uri)
}
