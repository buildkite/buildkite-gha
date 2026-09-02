// Package agentapi provides the authenticated HTTP boundary to the
// job-scoped Buildkite Agent API.
package agentapi

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/buildkite/buildkite-gha/internal/useragent"
)

// Config carries the current job's Agent API connection and authentication
// material.
type Config struct {
	Endpoint      string
	JobID         string
	JobToken      string
	ClientVersion string
	HTTPClient    *http.Client
}

// Client sends authenticated requests to one job-scoped Agent API.
type Client struct {
	jobURL    *url.URL
	jobToken  string
	userAgent string
	client    *http.Client
}

// New constructs a job-scoped client. The service name identifies the calling
// integration in configuration errors.
func New(config Config, service string) (*Client, error) {
	if !ValidJobID(config.JobID) {
		return nil, fmt.Errorf("%s Agent job ID is required", service)
	}
	jobURL, err := url.Parse(config.Endpoint)
	if err != nil || !validEndpoint(jobURL) {
		return nil, fmt.Errorf("safe %s Agent endpoint using HTTPS or loopback HTTP is required", service)
	}
	if config.JobToken == "" || strings.ContainsAny(config.JobToken, "\r\n") {
		return nil, fmt.Errorf("%s Agent job token is required", service)
	}
	jobURL.Path = strings.TrimRight(jobURL.Path, "/") + "/jobs/" + config.JobID
	jobURL.RawPath = ""

	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	bounded := *client
	bounded.Jar = nil
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if bounded.Timeout == 0 {
		bounded.Timeout = 15 * time.Second
	}
	return &Client{
		jobURL: jobURL, jobToken: config.JobToken,
		userAgent: useragent.FromVersion(config.ClientVersion), client: &bounded,
	}, nil
}

// URL returns an endpoint below this client's job-scoped Agent API root.
func (c *Client) URL(path string) string {
	endpoint := *c.jobURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + strings.TrimLeft(path, "/")
	return endpoint.String()
}

// Do adds the Agent API authentication and standard request headers before
// sending request with the bounded HTTP client.
func (c *Client) Do(request *http.Request) (*http.Response, error) {
	if !c.owns(request) {
		return nil, fmt.Errorf("agent API request is outside the configured job endpoint")
	}
	request.Header.Set("Authorization", "Token "+c.jobToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	return c.client.Do(request)
}

func (c *Client) owns(request *http.Request) bool {
	if c == nil || request == nil || request.URL == nil || request.URL.Opaque != "" || request.URL.User != nil ||
		request.URL.Scheme != c.jobURL.Scheme || request.URL.Host != c.jobURL.Host ||
		request.Host != "" && request.Host != c.jobURL.Host {
		return false
	}
	jobPath := strings.TrimRight(path.Clean(c.jobURL.Path), "/")
	requestPath := path.Clean(request.URL.Path)
	return requestPath == jobPath || strings.HasPrefix(requestPath, jobPath+"/")
}

func validEndpoint(u *url.URL) bool {
	if u == nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

// ValidJobID reports whether value is a canonical lowercase Buildkite job UUID.
func ValidJobID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range []byte(value) {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}
