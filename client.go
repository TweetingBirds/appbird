package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var defaultAPIBase = "https://api.appstoreconnect.apple.com"

// userError is a failure worth showing without a stack of context.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func userErr(format string, args ...any) error { return userError{fmt.Sprintf(format, args...)} }

// Credentials for an App Store Connect API key.
type Credentials struct {
	KeyID    string
	IssuerID string // empty for an individual key
	Key      *ecdsa.PrivateKey
}

func loadCredentials() (*Credentials, error) {
	keyID := strings.TrimSpace(os.Getenv("ASC_KEY_ID"))
	issuer := strings.TrimSpace(os.Getenv("ASC_ISSUER_ID"))
	if keyID == "" {
		return nil, userErr("Set ASC_KEY_ID (and ASC_ISSUER_ID for a team key). See the README's API key section.")
	}
	var pemBytes []byte
	if content := os.Getenv("ASC_PRIVATE_KEY"); strings.TrimSpace(content) != "" {
		pemBytes = []byte(strings.ReplaceAll(content, `\n`, "\n"))
	} else {
		var candidates []string
		if p := strings.TrimSpace(os.Getenv("ASC_PRIVATE_KEY_PATH")); p != "" {
			candidates = []string{expandHome(p)}
		} else {
			for _, d := range []string{"./private_keys", "~/private_keys", "~/.private_keys", "~/.appstoreconnect/private_keys"} {
				candidates = append(candidates, filepath.Join(expandHome(d), "AuthKey_"+keyID+".p8"))
			}
		}
		for _, c := range candidates {
			if b, err := os.ReadFile(c); err == nil {
				pemBytes = b
				break
			}
		}
		if pemBytes == nil {
			return nil, userErr("No private key: set ASC_PRIVATE_KEY_PATH, or ASC_PRIVATE_KEY to the .p8 file's contents, or put AuthKey_%s.p8 in ~/.appstoreconnect/private_keys/.", keyID)
		}
	}
	key, err := parseKey(pemBytes)
	if err != nil {
		return nil, err
	}
	return &Credentials{KeyID: keyID, IssuerID: issuer, Key: key}, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func parseKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(bytes.TrimSpace(pemBytes))
	if block == nil {
		return nil, userErr("The private key isn't a PEM .p8 file.")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if ec, ok := k.(*ecdsa.PrivateKey); ok {
			return ec, nil
		}
		return nil, userErr("The private key isn't an EC key.")
	}
	if ec, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return ec, nil
	}
	return nil, userErr("Can't read the private key.")
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// makeToken signs an ES256 JWT for the App Store Connect API.
func makeToken(c *Credentials, now time.Time) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": c.KeyID, "typ": "JWT"})
	claims := map[string]any{"iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(), "aud": "appstoreconnect-v1"}
	if c.IssuerID != "" {
		claims["iss"] = c.IssuerID
	} else {
		claims["sub"] = "user" // an individual key has no issuer
	}
	payload, _ := json.Marshal(claims)
	input := b64url(header) + "." + b64url(payload)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, c.Key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64url(sig), nil
}

// Resource is one JSON:API resource object.
type Resource struct {
	Type          string                     `json:"type"`
	ID            string                     `json:"id"`
	Attributes    map[string]any             `json:"attributes"`
	Relationships map[string]json.RawMessage `json:"relationships,omitempty"`
}

// Str returns a string attribute, or "" when it's missing or null.
func (r *Resource) Str(key string) string {
	if r == nil {
		return ""
	}
	if s, ok := r.Attributes[key].(string); ok {
		return s
	}
	return ""
}

// Related returns the (type, id) a to-one relationship points at.
func (r *Resource) Related(name string) (string, string) {
	raw, ok := r.Relationships[name]
	if !ok {
		return "", ""
	}
	var rel struct {
		Data *struct{ Type, ID string } `json:"data"`
	}
	if json.Unmarshal(raw, &rel) != nil || rel.Data == nil {
		return "", ""
	}
	return rel.Data.Type, rel.Data.ID
}

type document struct {
	Data     json.RawMessage `json:"data"`
	Included []Resource      `json:"included"`
	Links    struct {
		Next string `json:"next"`
	} `json:"links"`
}

// Client talks to the App Store Connect API.
type Client struct {
	creds     *Credentials
	base      string
	verbose   bool
	http      *http.Client
	token     string
	tokenTime time.Time
}

func newClient(creds *Credentials, verbose bool) *Client {
	base := defaultAPIBase
	if b := os.Getenv("ASC_API_BASE"); b != "" {
		base = b
	}
	return &Client{creds: creds, base: strings.TrimRight(base, "/"), verbose: verbose,
		http: &http.Client{Timeout: 5 * time.Minute}}
}

func (c *Client) auth() (string, error) {
	if c.token == "" || time.Since(c.tokenTime) > 10*time.Minute {
		t, err := makeToken(c.creds, time.Now())
		if err != nil {
			return "", err
		}
		c.token, c.tokenTime = t, time.Now()
	}
	return c.token, nil
}

var errNotFound = fmt.Errorf("not found")

// retryDelay is the first backoff between attempts; tests shorten it.
var retryDelay = time.Second

// uncertainError means a create may or may not have reached App Store Connect: the connection
// failed or Apple answered with a server error after the request was sent. Creates aren't
// retried, so a second attempt can't make a duplicate.
type uncertainError struct{ msg string }

func (e uncertainError) Error() string { return e.msg }

func (c *Client) do(method, path string, params url.Values, body any) (*document, error) {
	u := path
	if !strings.HasPrefix(u, "http") {
		u = c.base + path
	}
	if len(params) > 0 {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + params.Encode()
	}
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	// GET, PATCH and DELETE can be sent again safely. A POST creates something, so it's only
	// retried when Apple refused it outright (429: rate limited, nothing was done).
	idempotent := method != http.MethodPost
	for attempt := 0; ; attempt++ {
		token, err := c.auth()
		if err != nil {
			return nil, err
		}
		req, _ := http.NewRequest(method, u, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "appbird/"+version)
		if c.verbose {
			fmt.Fprintf(os.Stderr, "  %s %s\n", method, u)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if idempotent && attempt < 4 {
				time.Sleep(retryDelay << attempt)
				continue
			}
			if !idempotent {
				return nil, uncertainError{fmt.Sprintf("%s %s: %v", method, path, err)}
			}
			return nil, userErr("%s %s: %v", method, path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == 404 && method == http.MethodGet:
			return nil, errNotFound
		case resp.StatusCode == 404 && method == http.MethodDelete && attempt > 0:
			// An earlier attempt deleted it and the answer was lost.
			return &document{}, nil
		case resp.StatusCode == 429 && attempt < 4:
			time.Sleep(retryDelay << attempt)
			continue
		case resp.StatusCode >= 500 && idempotent && attempt < 4:
			time.Sleep(retryDelay << attempt)
			continue
		case resp.StatusCode >= 500 && !idempotent:
			return nil, uncertainError{fmt.Sprintf("%s %s failed with HTTP %d: %s", method, path, resp.StatusCode, apiErrors(raw))}
		case resp.StatusCode >= 400:
			return nil, userErr("%s %s failed with HTTP %d: %s", method, path, resp.StatusCode, apiErrors(raw))
		}
		doc := &document{}
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, doc); err != nil {
				return nil, userErr("%s %s: unexpected response: %v", method, path, err)
			}
		}
		return doc, nil
	}
}

func apiErrors(raw []byte) string {
	var body struct {
		Errors []struct{ Title, Detail string } `json:"errors"`
	}
	if json.Unmarshal(raw, &body) == nil && len(body.Errors) > 0 {
		parts := make([]string, len(body.Errors))
		for i, e := range body.Errors {
			parts[i] = strings.Trim(e.Title+": "+e.Detail, ": ")
		}
		return strings.Join(parts, "; ")
	}
	if len(raw) > 500 {
		raw = raw[:500]
	}
	return string(raw)
}

// GetAll follows pagination and returns every resource, plus anything `include`d.
func (c *Client) GetAll(path string, params url.Values) ([]*Resource, map[string]*Resource, error) {
	if params == nil {
		params = url.Values{}
	}
	if params.Get("limit") == "" {
		params.Set("limit", "200")
	}
	var out []*Resource
	included := map[string]*Resource{}
	doc, err := c.do(http.MethodGet, path, params, nil)
	if err == errNotFound {
		return nil, included, nil
	}
	for err == nil && doc != nil {
		var page []*Resource
		if len(doc.Data) > 0 && doc.Data[0] == '[' {
			if err := json.Unmarshal(doc.Data, &page); err != nil {
				return nil, nil, err
			}
		}
		out = append(out, page...)
		for i := range doc.Included {
			r := doc.Included[i]
			included[r.Type+"/"+r.ID] = &r
		}
		if doc.Links.Next == "" {
			break
		}
		doc, err = c.do(http.MethodGet, doc.Links.Next, nil, nil)
	}
	return out, included, err
}

func (c *Client) List(path string, params url.Values) ([]*Resource, error) {
	r, _, err := c.GetAll(path, params)
	return r, err
}

// GetOne returns a single resource, or nil if there isn't one.
func (c *Client) GetOne(path string) (*Resource, error) {
	doc, err := c.do(http.MethodGet, path, nil, nil)
	if err == errNotFound {
		return nil, nil
	}
	if err != nil || len(doc.Data) == 0 || string(doc.Data) == "null" || doc.Data[0] == '[' {
		return nil, err
	}
	r := &Resource{}
	return r, json.Unmarshal(doc.Data, r)
}

func (c *Client) Patch(rtype, id string, attrs map[string]any) error {
	return c.PatchAt("/v1/"+rtype+"/"+id, rtype, id, attrs, nil)
}

// PatchAt patches a resource at an explicit path (some live under /v2), with optional
// to-one relationships.
func (c *Client) PatchAt(path, rtype, id string, attrs map[string]any, relationships map[string]any) error {
	data := map[string]any{"type": rtype, "id": id}
	if len(attrs) > 0 {
		data["attributes"] = attrs
	}
	if len(relationships) > 0 {
		data["relationships"] = relationships
	}
	_, err := c.do(http.MethodPatch, path, nil, map[string]any{"data": data})
	return err
}

func rel(rtype, id string) map[string]any {
	return map[string]any{"data": map[string]any{"type": rtype, "id": id}}
}

func (c *Client) Create(rtype string, attrs map[string]any, relationships map[string]any) (*Resource, error) {
	doc, err := c.do(http.MethodPost, "/v1/"+rtype, nil, map[string]any{
		"data": map[string]any{"type": rtype, "attributes": attrs, "relationships": relationships}})
	if err != nil {
		return nil, err
	}
	r := &Resource{}
	if len(doc.Data) > 0 {
		err = json.Unmarshal(doc.Data, r)
	}
	return r, err
}

func (c *Client) Delete(rtype, id string) error {
	_, err := c.do(http.MethodDelete, "/v1/"+rtype+"/"+id, nil, nil)
	return err
}

type uploadOperation struct {
	Method         string `json:"method"`
	URL            string `json:"url"`
	Length         int    `json:"length"`
	Offset         int    `json:"offset"`
	RequestHeaders []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"requestHeaders"`
}

// UploadParts PUTs a file's parts where Apple says, with Apple's headers and no bearer token.
func (c *Client) UploadParts(res *Resource, data []byte) error {
	raw, _ := json.Marshal(res.Attributes["uploadOperations"])
	var ops []uploadOperation
	_ = json.Unmarshal(raw, &ops)
	for _, op := range ops {
		if op.Offset < 0 || op.Offset+op.Length > len(data) {
			return userErr("App Store Connect asked for a part outside the file")
		}
		part := data[op.Offset : op.Offset+op.Length]
		method := op.Method
		if method == "" {
			method = http.MethodPut
		}
		var lastErr error
		for attempt := 0; attempt < 4; attempt++ {
			req, _ := http.NewRequest(method, op.URL, bytes.NewReader(part))
			for _, h := range op.RequestHeaders {
				req.Header.Set(h.Name, h.Value)
			}
			resp, err := c.http.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode < 300 {
					lastErr = nil
					break
				}
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			lastErr = err
			time.Sleep(retryDelay << attempt)
		}
		if lastErr != nil {
			return userErr("uploading a part failed: %v", lastErr)
		}
	}
	return nil
}
