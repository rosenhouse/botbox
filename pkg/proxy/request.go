package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Request is one recorded exchange between the target and the API server.
// Status is the status the target saw, and 0 while it has seen none.
type Request struct {
	Start       time.Time `json:"start"`
	Verb        string    `json:"verb"`
	Group       string    `json:"group,omitempty"`
	Version     string    `json:"version,omitempty"`
	Resource    string    `json:"resource,omitempty"`
	Subresource string    `json:"subresource,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	Name        string    `json:"name,omitempty"`
	Path        string    `json:"path"`
	Watch       bool      `json:"watch,omitempty"`
	Status      int       `json:"status"`
	// Latency is unset until the exchange ends, so an open watch carries its
	// start and its status alone. A report sampled mid-run therefore carries
	// no latency where requests.jsonl carries what the request took
	// (DESIGN.md §5.7).
	Latency time.Duration `json:"latencyNs,omitempty"`
	Fault   string        `json:"fault,omitempty"`
}

// namespaceSubresources follow a namespace name instead of scoping a resource.
var namespaceSubresources = map[string]bool{"status": true, "finalize": true}

func newRequest(r *http.Request) Request {
	watch, err := strconv.ParseBool(r.URL.Query().Get("watch"))
	recorded := Request{
		Start: time.Now(),
		Path:  r.URL.Path,
		Watch: err == nil && watch,
	}
	recorded.parsePath(strings.Split(strings.Trim(r.URL.Path, "/"), "/"))
	recorded.Verb = verb(r.Method, recorded)
	return recorded
}

// parsePath reads /api/<version> or /apis/<group>/<version>, an optional
// /namespaces/<namespace>, then <resource>[/<name>[/<subresource>]]. A path
// that is not an API path leaves every field empty.
func (r *Request) parsePath(segments []string) {
	switch {
	case len(segments) >= 2 && segments[0] == "api":
		r.Version = segments[1]
		segments = segments[2:]
	case len(segments) >= 3 && segments[0] == "apis":
		r.Group, r.Version = segments[1], segments[2]
		segments = segments[3:]
	default:
		return
	}
	if len(segments) > 1 && segments[0] == "namespaces" {
		r.Namespace = segments[1]
		if len(segments) > 2 && !namespaceSubresources[segments[2]] {
			segments = segments[2:]
		}
	}
	if len(segments) > 0 {
		r.Resource = segments[0]
	}
	if len(segments) > 1 {
		r.Name = segments[1]
	}
	if len(segments) > 2 {
		r.Subresource = segments[2]
	}
}

// Verbs are the verbs verb records for a resource request.
var Verbs = []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}

// verb is the Kubernetes verb the method and the path imply. A path that
// names no resource takes the lowercased method, as non-resource URLs do.
func verb(method string, r Request) string {
	if r.Resource == "" {
		return strings.ToLower(method)
	}
	switch method {
	case http.MethodPost:
		return "create"
	case http.MethodPut:
		return "update"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		if r.Name == "" {
			return "deletecollection"
		}
		return "delete"
	case http.MethodGet, http.MethodHead:
		switch {
		case r.Watch:
			return "watch"
		case r.Name == "":
			return "list"
		default:
			return "get"
		}
	}
	return strings.ToLower(method)
}
