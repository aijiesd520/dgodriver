package dgodriver

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
)

// RequestExpectation is installed before an action. WaitRequest waits for the
// request; WaitResponse waits for headers; Body waits for loadingFinished.
type RequestExpectation struct {
	tab                                   *Tab
	sub                                   *Subscription
	mu                                    sync.Mutex
	request                               *cdp.NetworkRequestWillBeSent
	response                              *cdp.NetworkResponseReceived
	err                                   error
	requestReady, responseReady, finished chan struct{}
	requestOnce, responseOnce, finishOnce sync.Once
	pattern                               string
}

// ExpectRequest subscribes before an action and captures the first request whose
// entire URL matches the regular expression. Close the expectation when finished.
func (t *Tab) ExpectRequest(ctx context.Context, urlPattern string) (*RequestExpectation, error) {
	pattern, err := regexp.Compile("^(?:" + urlPattern + ")$")
	if err != nil {
		return nil, err
	}
	e := &RequestExpectation{tab: t, pattern: urlPattern, requestReady: make(chan struct{}), responseReady: make(chan struct{}), finished: make(chan struct{})}
	e.sub, err = t.On(ctx, "Network.*", func(event Event) {
		e.mu.Lock()
		defer e.mu.Unlock()
		switch event.Method {
		case "Network.requestWillBeSent":
			var request cdp.NetworkRequestWillBeSent
			if event.Decode(&request) != nil {
				return
			}
			if e.request == nil && pattern.MatchString(request.Request.URL) {
				e.request = &request
				e.requestOnce.Do(func() { close(e.requestReady) })
			}
		case "Network.responseReceived":
			var response cdp.NetworkResponseReceived
			if event.Decode(&response) != nil {
				return
			}
			if e.request != nil && response.RequestID == e.request.RequestID {
				e.response = &response
				e.responseOnce.Do(func() { close(e.responseReady) })
			}
		case "Network.loadingFinished":
			var finished cdp.NetworkLoadingFinished
			if event.Decode(&finished) != nil {
				return
			}
			if e.request != nil && finished.RequestID == e.request.RequestID {
				e.finishOnce.Do(func() { close(e.finished) })
			}
		case "Network.loadingFailed":
			var failed cdp.NetworkLoadingFailed
			if event.Decode(&failed) != nil {
				return
			}
			if e.request != nil && failed.RequestID == e.request.RequestID {
				e.err = fmt.Errorf("dgodriver: request failed: %s", failed.ErrorText)
				e.finishOnce.Do(func() { close(e.finished) })
				e.responseOnce.Do(func() { close(e.responseReady) })
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// ExpectResponse creates a request expectation whose response can be awaited.
func (t *Tab) ExpectResponse(ctx context.Context, urlPattern string) (*RequestExpectation, error) {
	return t.ExpectRequest(ctx, urlPattern)
}

// Close unsubscribes and releases pending waiters with ErrClosed.
func (e *RequestExpectation) Close() error { return e.sub.Close() }

// Reset returns a fresh expectation; existing waiters are released by Close.
func (e *RequestExpectation) Reset(ctx context.Context) (*RequestExpectation, error) {
	_ = e.Close()
	return e.tab.ExpectRequest(ctx, e.pattern)
}
func (e *RequestExpectation) wait(ctx context.Context, ready <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.tab.browser.Done():
		return ErrClosed
	case <-e.sub.done:
		return ErrClosed
	case <-ready:
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.err
	}
}

// WaitRequest waits for the matching request or context cancellation.
func (e *RequestExpectation) WaitRequest(ctx context.Context) (cdp.NetworkRequestWillBeSent, error) {
	if err := e.wait(ctx, e.requestReady); err != nil {
		return cdp.NetworkRequestWillBeSent{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return *e.request, nil
}

// WaitResponse waits for response headers or a request failure.
func (e *RequestExpectation) WaitResponse(ctx context.Context) (cdp.NetworkResponseReceived, error) {
	if err := e.wait(ctx, e.responseReady); err != nil {
		return cdp.NetworkResponseReceived{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return *e.response, nil
}

// Body waits for loading to finish and returns the decoded response body.
func (e *RequestExpectation) Body(ctx context.Context) ([]byte, error) {
	if err := e.wait(ctx, e.finished); err != nil {
		return nil, err
	}
	e.mu.Lock()
	id := e.request.RequestID
	e.mu.Unlock()
	r, err := cdp.Network.GetResponseBody(cdp.NetworkGetResponseBodyParams{RequestID: id}).Execute(ctx, e.tab)
	if err != nil {
		return nil, err
	}
	if r.Base64Encoded {
		return base64.StdEncoding.DecodeString(r.Body)
	}
	return []byte(r.Body), nil
}

// Interceptor delivers paused Fetch requests. Keep consuming Next and resolve
// each request promptly; queued requests remain paused until resolved or Close.
type Interceptor struct {
	tab      *Tab
	sub      *Subscription
	requests chan *InterceptedRequest
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	err      error
}

// Intercept intercepts every request matching CDP's wildcard patterns until Close.
// Only one Fetch interceptor can own a target at a time. Consume Next and
// Continue/Fail/Fulfill each request; an unresolved request remains paused.
func (t *Tab) Intercept(ctx context.Context, patterns ...cdp.FetchRequestPattern) (*Interceptor, error) {
	t.interceptMu.Lock()
	defer t.interceptMu.Unlock()
	if t.interceptor != nil {
		return nil, fmt.Errorf("dgodriver: target already has an interceptor")
	}
	lifetime, cancel := context.WithCancel(ctx)
	i := &Interceptor{tab: t, requests: make(chan *InterceptedRequest, 64), ctx: lifetime, cancel: cancel}
	conn, err := t.Connection(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	i.sub = conn.AddHandler(lifetime, "Fetch.requestPaused", func(event Event) {
		var request cdp.FetchRequestPaused
		if event.Decode(&request) != nil {
			return
		}
		r := &InterceptedRequest{Event: request, tab: t}
		select {
		case i.requests <- r:
		case <-lifetime.Done():
		}
	})
	if len(patterns) == 0 {
		patterns = []cdp.FetchRequestPattern{{URLPattern: cdp.Ptr("*")}}
	}
	if _, err = cdp.Fetch.Enable(cdp.FetchEnableParams{Patterns: patterns}).Execute(ctx, t); err != nil {
		i.sub.Close()
		cancel()
		return nil, err
	}
	t.interceptor = i
	go func() { <-lifetime.Done(); _ = i.Close() }()
	return i, nil
}

// Next waits for a paused request. Resolve it with Continue, Fail or Fulfill.
func (i *Interceptor) Next(ctx context.Context) (*InterceptedRequest, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-i.ctx.Done():
		return nil, ErrClosed
	case <-i.tab.browser.Done():
		return nil, ErrClosed
	case r := <-i.requests:
		return r, nil
	}
}

// Close disables Fetch interception and releases the target's interceptor slot.
func (i *Interceptor) Close() error {
	i.once.Do(func() {
		i.cancel()
		i.sub.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, i.err = cdp.Fetch.Disable().Execute(ctx, i.tab)
		i.tab.interceptMu.Lock()
		if i.tab.interceptor == i {
			i.tab.interceptor = nil
		}
		i.tab.interceptMu.Unlock()
	})
	return i.err
}

// InterceptedRequest is a paused request that can be resolved once successfully.
type InterceptedRequest struct {
	Event    cdp.FetchRequestPaused
	tab      *Tab
	mu       sync.Mutex
	resolved bool
}

func (r *InterceptedRequest) resolve(fn func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resolved {
		return fmt.Errorf("dgodriver: request already resolved")
	}
	err := fn()
	if err == nil {
		r.resolved = true
	}
	return err
}

// Continue resumes the request, optionally overriding its URL, method, headers or body.
func (r *InterceptedRequest) Continue(ctx context.Context, options ...cdp.FetchContinueRequestParams) error {
	return r.resolve(func() error {
		p := cdp.FetchContinueRequestParams{}
		if len(options) > 0 {
			p = options[0]
		}
		p.RequestID = r.Event.RequestID
		_, err := cdp.Fetch.ContinueRequest(p).Execute(ctx, r.tab)
		return err
	})
}

// Fail aborts the paused request with reason.
func (r *InterceptedRequest) Fail(ctx context.Context, reason cdp.NetworkErrorReason) error {
	return r.resolve(func() error {
		_, err := cdp.Fetch.FailRequest(cdp.FetchFailRequestParams{RequestID: r.Event.RequestID, ErrorReason: reason}).Execute(ctx, r.tab)
		return err
	})
}

// Fulfill supplies a synthetic response for the paused request.
func (r *InterceptedRequest) Fulfill(ctx context.Context, status int, body []byte, headers ...cdp.FetchHeaderEntry) error {
	return r.resolve(func() error {
		_, err := cdp.Fetch.FulfillRequest(cdp.FetchFulfillRequestParams{RequestID: r.Event.RequestID, ResponseCode: status, Body: cdp.Ptr(base64.StdEncoding.EncodeToString(body)), ResponseHeaders: headers}).Execute(ctx, r.tab)
		return err
	})
}

// ContinueResponse resumes a request paused at the response stage.
func (r *InterceptedRequest) ContinueResponse(ctx context.Context, options ...cdp.FetchContinueResponseParams) error {
	return r.resolve(func() error {
		p := cdp.FetchContinueResponseParams{}
		if len(options) > 0 {
			p = options[0]
		}
		p.RequestID = r.Event.RequestID
		_, err := cdp.Fetch.ContinueResponse(p).Execute(ctx, r.tab)
		return err
	})
}

// Body reads the decoded response body of a request paused at the response stage.
func (r *InterceptedRequest) Body(ctx context.Context) ([]byte, error) {
	value, err := cdp.Fetch.GetResponseBody(cdp.FetchGetResponseBodyParams{RequestID: r.Event.RequestID}).Execute(ctx, r.tab)
	if err != nil {
		return nil, err
	}
	if value.Base64Encoded {
		return base64.StdEncoding.DecodeString(value.Body)
	}
	return []byte(value.Body), nil
}
