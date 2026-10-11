package dgodriver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
)

// CookieJar manages browser cookies in the default or an isolated browser context.
type CookieJar struct {
	browser   *Browser
	contextID string
}

// Cookies returns a manager for the default browser context's cookies.
func (b *Browser) Cookies() *CookieJar { return &CookieJar{browser: b} }

// GetAll returns cookies from this jar's browser context.
func (j *CookieJar) GetAll(ctx context.Context) ([]cdp.NetworkCookie, error) {
	p := cdp.StorageGetCookiesParams{}
	if j.contextID != "" {
		p.BrowserContextID = cdp.Ptr(cdp.BrowserBrowserContextID(j.contextID))
	}
	r, err := cdp.Storage.GetCookies(p).Execute(ctx, j.browser)
	return r.Cookies, err
}

// SetAll sets cookies in this jar's browser context.
func (j *CookieJar) SetAll(ctx context.Context, cookies []cdp.NetworkCookieParam) error {
	p := cdp.StorageSetCookiesParams{Cookies: cookies}
	if j.contextID != "" {
		p.BrowserContextID = cdp.Ptr(cdp.BrowserBrowserContextID(j.contextID))
	}
	_, err := cdp.Storage.SetCookies(p).Execute(ctx, j.browser)
	return err
}

// Clear removes all cookies from this jar's browser context.
func (j *CookieJar) Clear(ctx context.Context) error {
	p := cdp.StorageClearCookiesParams{}
	if j.contextID != "" {
		p.BrowserContextID = cdp.Ptr(cdp.BrowserBrowserContextID(j.contextID))
	}
	_, err := cdp.Storage.ClearCookies(p).Execute(ctx, j.browser)
	return err
}

// HTTPCookies converts stored browser cookies into net/http cookies.
func (j *CookieJar) HTTPCookies(ctx context.Context) ([]*http.Cookie, error) {
	list, err := j.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	var result []*http.Cookie
	for _, c := range list {
		cookie := &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
		if c.Expires != nil && *c.Expires > 0 {
			cookie.Expires = time.Unix(int64(*c.Expires), 0)
		}
		if c.SameSite != nil {
			switch string(*c.SameSite) {
			case "Strict":
				cookie.SameSite = http.SameSiteStrictMode
			case "Lax":
				cookie.SameSite = http.SameSiteLaxMode
			case "None":
				cookie.SameSite = http.SameSiteNoneMode
			}
		}
		result = append(result, cookie)
	}
	return result, nil
}
func cookieParams(c cdp.NetworkCookie) cdp.NetworkCookieParam {
	p := cdp.NetworkCookieParam{Name: c.Name, Value: c.Value, Domain: &c.Domain, Path: &c.Path, Secure: &c.Secure, HTTPOnly: &c.HTTPOnly, SameSite: c.SameSite, Priority: &c.Priority, SourceScheme: &c.SourceScheme, SourcePort: &c.SourcePort, PartitionKey: c.PartitionKey}
	if !c.Session && c.Expires != nil {
		p.Expires = cdp.Ptr(cdp.NetworkTimeSinceEpoch(*c.Expires))
	}
	return p
}
func cookiePattern(pattern []string) (*regexp.Regexp, error) {
	s := ".*"
	if len(pattern) > 0 {
		s = pattern[0]
	}
	return regexp.Compile(s)
}

// Save writes cookies as JSON using file mode 0600, optionally filtering
// the cookie name and domain using a regular expression.
func (j *CookieJar) Save(ctx context.Context, path string, pattern ...string) error {
	re, err := cookiePattern(pattern)
	if err != nil {
		return err
	}
	list, err := j.GetAll(ctx)
	if err != nil {
		return err
	}
	var filtered []cdp.NetworkCookie
	for _, c := range list {
		if re.MatchString(c.Name + " " + c.Domain) {
			filtered = append(filtered, c)
		}
	}
	raw, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, raw, 0600)
}

// Load imports a saved cookie JSON file, optionally filtering names and domains by regexp.
func (j *CookieJar) Load(ctx context.Context, path string, pattern ...string) error {
	re, err := cookiePattern(pattern)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var list []cdp.NetworkCookie
	if err = json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("dgodriver: invalid cookie file: %w", err)
	}
	var params []cdp.NetworkCookieParam
	for _, c := range list {
		if re.MatchString(c.Name + " " + c.Domain) {
			params = append(params, cookieParams(c))
		}
	}
	return j.SetAll(ctx, params)
}
func writeAtomic(path string, raw []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".dgodriver-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(raw)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}
