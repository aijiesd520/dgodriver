package dgodriver

import (
	"context"
	"crypto/tls"
	"sync"

	"github.com/aijiesd520/dgodriver/cdp"
)

type ContextOptions struct {
	ProxyServer                       string
	ProxyBypassList                   string
	ProxyTLSConfig                    *tls.Config
	OriginsWithUniversalNetworkAccess []string
	KeepOnDetach                      bool
}
type BrowserContext struct {
	ID       string
	Browser  *Browser
	proxy    *ProxyForwarder
	mu       sync.Mutex
	disposed bool
}

func (b *Browser) CreateContext(ctx context.Context, options ...ContextOptions) (*BrowserContext, error) {
	var opt ContextOptions
	if len(options) > 0 {
		opt = options[0]
	}
	var proxy *ProxyForwarder
	server := opt.ProxyServer
	if server != "" {
		var err error
		proxy, err = NewProxyForwarder(ctx, server, opt.ProxyTLSConfig)
		if err != nil {
			return nil, err
		}
		server = proxy.ProxyServer()
	}
	p := cdp.TargetCreateBrowserContextParams{DisposeOnDetach: cdp.Ptr(!opt.KeepOnDetach)}
	if server != "" {
		p.ProxyServer = &server
	}
	if opt.ProxyBypassList != "" {
		p.ProxyBypassList = &opt.ProxyBypassList
	}
	p.OriginsWithUniversalNetworkAccess = opt.OriginsWithUniversalNetworkAccess
	result, err := cdp.Target.CreateBrowserContext(p).Execute(ctx, b)
	if err != nil {
		if proxy != nil {
			proxy.Close()
		}
		return nil, err
	}
	bc := &BrowserContext{ID: string(result.BrowserContextID), Browser: b, proxy: proxy}
	b.mu.Lock()
	b.contexts[bc.ID] = bc
	b.mu.Unlock()
	return bc, nil
}
func (b *Browser) Contexts() []*BrowserContext {
	b.mu.Lock()
	defer b.mu.Unlock()
	var list []*BrowserContext
	for _, bc := range b.contexts {
		list = append(list, bc)
	}
	return list
}
func (bc *BrowserContext) NewTab(ctx context.Context, url string, newWindow ...bool) (*Tab, error) {
	p := cdp.TargetCreateTargetParams{URL: url, BrowserContextID: cdp.Ptr(cdp.BrowserBrowserContextID(bc.ID))}
	if p.URL == "" {
		p.URL = "about:blank"
	}
	if len(newWindow) > 0 {
		p.NewWindow = &newWindow[0]
	}
	result, err := cdp.Target.CreateTarget(p).Execute(ctx, bc.Browser)
	if err != nil {
		return nil, err
	}
	t, err := bc.Browser.Tab(ctx, string(result.TargetID))
	if err != nil {
		return nil, err
	}
	if err = t.WaitForReadyState(ctx, "interactive"); err != nil {
		return nil, err
	}
	return t, nil
}
func (bc *BrowserContext) Cookies() *CookieJar {
	return &CookieJar{browser: bc.Browser, contextID: bc.ID}
}
func (bc *BrowserContext) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5e9)
	defer cancel()
	return bc.Dispose(ctx)
}
func (bc *BrowserContext) Dispose(ctx context.Context) error {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if bc.disposed {
		return nil
	}
	if !bc.Browser.Connection.Closed() {
		if _, err := cdp.Target.DisposeBrowserContext(cdp.TargetDisposeBrowserContextParams{BrowserContextID: cdp.BrowserBrowserContextID(bc.ID)}).Execute(ctx, bc.Browser); err != nil {
			return err
		}
	}
	bc.disposed = true
	if bc.proxy != nil {
		bc.proxy.Close()
	}
	bc.Browser.mu.Lock()
	delete(bc.Browser.contexts, bc.ID)
	bc.Browser.mu.Unlock()
	return nil
}
