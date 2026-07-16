package httpx

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/Noooste/azuretls-client"
	fhttp "github.com/Noooste/fhttp"
)

const (
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	SecChUA   = `"Google Chrome";v="145", "Not?A_Brand";v="8", "Chromium";v="145"`

	// DefaultTimeout: OpenAI / ChatGPT API calls. 90s was the main hang source
	// when a proxy half-died mid-request.
	DefaultTimeout = 30 * time.Second
	// GraphTimeout: Microsoft Graph OTP polling (fail faster between polls).
	GraphTimeout = 20 * time.Second
)

// Client wraps azuretls with cookie jar + Chrome fingerprint helpers.
type Client struct {
	Session *azuretls.Session
	UA      string
	Proxy   string
}

func New(proxy string) (*Client, error) {
	s := azuretls.NewSession()
	s.Browser = azuretls.Chrome
	s.GetClientHelloSpec = azuretls.GetBrowserClientHelloFunc(azuretls.Chrome)
	s.SetTimeout(DefaultTimeout)
	s.InsecureSkipVerify = true
	s.MaxRedirects = 10
	if proxy != "" {
		if err := s.SetProxy(proxy); err != nil {
			return nil, fmt.Errorf("set proxy: %w", err)
		}
	}
	return &Client{Session: s, UA: UserAgent, Proxy: proxy}, nil
}

// SetTimeout overrides the per-request timeout on the underlying session.
func (c *Client) SetTimeout(d time.Duration) {
	if c == nil || c.Session == nil || d <= 0 {
		return
	}
	c.Session.SetTimeout(d)
}

func (c *Client) Close() {
	if c.Session != nil {
		c.Session.Close()
	}
}

func (c *Client) SetCookie(name, value, domain string) {
	if c.Session == nil || c.Session.CookieJar == nil {
		return
	}
	host := strings.TrimPrefix(domain, ".")
	u, err := url.Parse("https://" + host)
	if err != nil {
		return
	}
	c.Session.CookieJar.SetCookies(u, []*fhttp.Cookie{
		{Name: name, Value: value, Domain: domain, Path: "/"},
	})
}

type Response struct {
	StatusCode int
	Header     map[string][]string
	Body       []byte
	URL        string
}

func (r *Response) Text() string { return string(r.Body) }

func (r *Response) HeaderGet(k string) string {
	if r.Header == nil {
		return ""
	}
	// case-insensitive
	for key, vals := range r.Header {
		if strings.EqualFold(key, k) && len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

func (c *Client) Do(method, rawURL string, body []byte, headers map[string]string, followRedirects bool) (*Response, error) {
	ordered := azuretls.OrderedHeaders{}
	// Emit caller headers first (dedupe case-insensitive), then browser baseline.
	base := map[string]string{
		"User-Agent":         c.UA,
		"Accept-Language":    "en-US,en;q=0.9",
		"Accept-Encoding":    "gzip, deflate, br",
		"sec-ch-ua":          SecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
	}
	seen := map[string]bool{}
	for k, v := range headers {
		if v == "" {
			continue
		}
		lk := strings.ToLower(k)
		if seen[lk] {
			continue
		}
		seen[lk] = true
		ordered = append(ordered, []string{k, v})
	}
	for k, v := range base {
		if seen[strings.ToLower(k)] {
			continue
		}
		ordered = append(ordered, []string{k, v})
	}

	old := c.Session.MaxRedirects
	if followRedirects {
		c.Session.MaxRedirects = 10
	} else {
		c.Session.MaxRedirects = 0
	}
	defer func() { c.Session.MaxRedirects = old }()

	req := &azuretls.Request{
		Method:           method,
		Url:              rawURL,
		OrderedHeaders:   ordered,
		DisableRedirects: !followRedirects,
		MaxRedirects:     c.Session.MaxRedirects,
	}
	if body != nil {
		req.Body = body
	}
	resp, err := c.Session.Do(req)
	if err != nil {
		return nil, err
	}
	h := make(map[string][]string)
	for k, v := range resp.Header {
		h[k] = v
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Header:     h,
		Body:       resp.Body,
		URL:        rawURL,
	}, nil
}

func (c *Client) Get(rawURL string, headers map[string]string, follow bool) (*Response, error) {
	return c.Do("GET", rawURL, nil, headers, follow)
}

func (c *Client) Post(rawURL string, body []byte, headers map[string]string, follow bool) (*Response, error) {
	return c.Do("POST", rawURL, body, headers, follow)
}

func (c *Client) PostJSON(rawURL string, payload []byte, headers map[string]string, follow bool) (*Response, error) {
	if headers == nil {
		headers = map[string]string{}
	}
	// Normalize: strip any existing content-type variants, set once.
	clean := map[string]string{}
	for k, v := range headers {
		if strings.EqualFold(k, "Content-Type") {
			continue
		}
		clean[k] = v
	}
	clean["Content-Type"] = "application/json"
	return c.Post(rawURL, payload, clean, follow)
}

func (c *Client) PostForm(rawURL string, form url.Values, headers map[string]string) (*Response, error) {
	if headers == nil {
		headers = map[string]string{}
	}
	clean := map[string]string{}
	for k, v := range headers {
		if strings.EqualFold(k, "Content-Type") {
			continue
		}
		clean[k] = v
	}
	clean["Content-Type"] = "application/x-www-form-urlencoded"
	return c.Post(rawURL, []byte(form.Encode()), clean, false)
}

// ReadAll is a tiny helper when body is already []byte from azuretls.
func ReadAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func DumpSnippet(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}

func JoinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// BufferBody for re-use patterns
func BufferBody(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}
