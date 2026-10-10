package dgodriver

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
)

func TestBrowserIntegration(t *testing.T) {
	executable := os.Getenv("DGODRIVER_BROWSER")
	if executable == "" {
		var err error
		executable, err = FindExecutable("auto")
		if err != nil {
			t.Skip(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	frame := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<button id="frame-button" onclick="this.textContent='clicked'">Frame button</button>`))
	}))
	defer frame.Close()
	frameURL := strings.Replace(frame.URL, "127.0.0.1", "localhost", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"hello":"world"}`))
		case "/file":
			_, _ = w.Write([]byte("download content"))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!doctype html><title>Dgo test</title><h1>Hello Go</h1><input id="input"><textarea id="area"></textarea><button id="button" onclick="this.textContent='Clicked'">Click me</button><select id="select"><option value="a">A</option><option value="b">B</option></select><div id="parent"><span>child</span></div><a href="/api">API</a><div id="shadow"></div><iframe id="same" srcdoc="<button id='same-button'>Same frame</button>"></iframe><iframe id="cross" src="` + frameURL + `"></iframe><canvas id="canvas" width="100" height="100"></canvas><script>const shadow=document.querySelector('#shadow').attachShadow({mode:'closed'});shadow.innerHTML='<button id="shadow-button">Shadow</button>';const input=document.querySelector('#input');let tracker='';Object.defineProperty(input,'value',{get(){return Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').get.call(this)},set(v){tracker=v;Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(this,v)}});input.addEventListener('input',()=>{window.changed=input.value!==tracker;tracker=input.value});const c=document.querySelector('#canvas');c.getContext('2d').fillRect(0,0,100,100);</script>`))
		}
	}))
	defer server.Close()
	browser, err := Start(ctx, Config{Headless: true, NoSandbox: true, BrowserExecutablePath: executable, BrowserArgs: []string{"--site-per-process"}})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	profile := browser.Config.UserDataDir
	tab, err := browser.Get(ctx, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("DOM and controlled inputs", func(t *testing.T) {
		e, err := tab.Select(ctx, "#input")
		if err != nil {
			t.Fatal(err)
		}
		if err = e.SetValue(ctx, "你好 Go"); err != nil {
			t.Fatal(err)
		}
		var changed bool
		if err = tab.Evaluate(ctx, "window.changed", &changed); err != nil || !changed {
			t.Fatalf("controlled input %v %v", changed, err)
		}
		if err = e.ClearInput(ctx); err != nil {
			t.Fatal(err)
		}
		if err = e.SendKeys(ctx, "abc中文"); err != nil {
			t.Fatal(err)
		}
		var value string
		if err = e.Apply(ctx, "el=>el.value", &value); err != nil || value != "abc中文" {
			t.Fatalf("value %q %v", value, err)
		}
		if err = e.ClearInputByDeleting(ctx); err != nil {
			t.Fatal(err)
		}
		if err = e.Apply(ctx, "el=>el.value", &value); err != nil || value != "" {
			t.Fatalf("clear by deleting %q %v", value, err)
		}
		items, err := tab.XPath(ctx, "//h1")
		if err != nil || len(items) != 1 {
			t.Fatalf("XPath %d %v", len(items), err)
		}
		text, err := items[0].TextAll(ctx)
		if err != nil || text != "Hello Go" {
			t.Fatalf("text %q %v", text, err)
		}
		found, err := tab.Find(ctx, "Hello Go")
		if err != nil {
			t.Fatal(err)
		}
		if found.TagName() != "h1" {
			t.Fatalf("Find returned %s", found.TagName())
		}
		parent, err := tab.Select(ctx, "#parent")
		if err != nil {
			t.Fatal(err)
		}
		child, err := parent.QuerySelector(ctx, "span")
		if err != nil {
			t.Fatal(err)
		}
		if err = child.SetText(ctx, "updated"); err != nil {
			t.Fatal(err)
		}
		b, err := tab.Select(ctx, "#button")
		if err != nil {
			t.Fatal(err)
		}
		if err = b.MouseClick(ctx); err != nil {
			t.Fatal(err)
		}
		text, err = b.TextAll(ctx)
		if err != nil || text != "Clicked" {
			t.Fatalf("mouse click %q %v", text, err)
		}
		selectEl, err := tab.Select(ctx, "#select")
		if err != nil {
			t.Fatal(err)
		}
		if err = selectEl.SelectOption(ctx, "b"); err != nil {
			t.Fatal(err)
		}
		if _, err = tab.Select(ctx, "#missing", QueryOptions{Timeout: 100 * time.Millisecond}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("query timeout %v", err)
		}
		if err = tab.Evaluate(ctx, "throw new Error('expected')", nil); err == nil {
			t.Fatal("missing JS error")
		}
	})
	t.Run("frames and closed shadow DOM", func(t *testing.T) {
		frames, err := tab.GetFrames(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) == 0 {
			t.Fatal("expected an out-of-process iframe")
		}
		for _, selector := range []string{"#same-button", "#shadow-button", "#frame-button"} {
			e, err := tab.Select(ctx, selector, QueryOptions{IncludeFrames: true, Timeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("%s: %v", selector, err)
			}
			if selector == "#frame-button" {
				if err = e.MouseClick(ctx); err != nil {
					t.Fatal(err)
				}
				text, err := e.TextAll(ctx)
				if err != nil || text != "clicked" {
					t.Fatalf("frame click %q %v", text, err)
				}
			}
		}
	})
	t.Run("network response and interception", func(t *testing.T) {
		// Manual CDP disable must invalidate automatic enable bookkeeping.
		if _, err := cdp.Network.Disable().Execute(ctx, tab); err != nil {
			t.Fatal(err)
		}
		expectation, err := tab.ExpectResponse(ctx, ".*/api")
		if err != nil {
			t.Fatal(err)
		}
		defer expectation.Close()
		if err = tab.Evaluate(ctx, "fetch('/api').then(r=>r.json())", nil, EvaluateOptions{AwaitPromise: true}); err != nil {
			t.Fatal(err)
		}
		body, err := expectation.Body(ctx)
		if err != nil || string(body) != `{"hello":"world"}` {
			t.Fatalf("response body %q %v", body, err)
		}
		interception, err := tab.Intercept(ctx, cdp.FetchRequestPattern{URLPattern: cdp.Ptr("*/mock")})
		if err != nil {
			t.Fatal(err)
		}
		defer interception.Close()
		result := make(chan error, 1)
		go func() {
			r, err := interception.Next(ctx)
			if err == nil {
				err = r.Fulfill(ctx, 200, []byte(`{"mock":true}`), cdp.FetchHeaderEntry{Name: "Content-Type", Value: "application/json"})
			}
			result <- err
		}()
		var value map[string]bool
		if err = tab.Evaluate(ctx, "fetch('/mock').then(r=>r.json())", &value, EvaluateOptions{AwaitPromise: true}); err != nil {
			t.Fatal(err)
		}
		if err = <-result; err != nil {
			t.Fatal(err)
		}
		if !value["mock"] {
			t.Fatal(value)
		}
	})
	t.Run("navigation, window, keyboard events and scrolling", func(t *testing.T) {
		if err := tab.BringToFront(ctx); err != nil {
			t.Fatal(err)
		}
		if err := tab.SetWindowSize(ctx, 10, 10, 1000, 700); err != nil {
			t.Fatal(err)
		}
		_, bounds, err := tab.GetWindow(ctx)
		if err != nil || bounds.Width != 1000 {
			t.Fatalf("bounds %#v %v", bounds, err)
		}
		if err = tab.Evaluate(ctx, "window.keys=[];document.addEventListener('keydown',e=>keys.push(e.key));document.body.style.height='3000px'", nil); err != nil {
			t.Fatal(err)
		}
		input, err := tab.Select(ctx, "#input")
		if err != nil {
			t.Fatal(err)
		}
		if err = input.SendKeys(ctx, "Hi!"); err != nil {
			t.Fatal(err)
		}
		var keys []string
		if err = tab.Evaluate(ctx, "keys", &keys); err != nil || len(keys) < 3 {
			t.Fatalf("keyboard events %v %v", keys, err)
		}
		if err = tab.ScrollDown(ctx, 25, 1000); err != nil {
			t.Fatal(err)
		}
		var scroll float64
		if err = tab.Evaluate(ctx, "scrollY", &scroll); err != nil || scroll <= 0 {
			t.Fatalf("scroll %f %v", scroll, err)
		}
		if err = tab.Navigate(ctx, server.URL+"/second"); err != nil {
			t.Fatal(err)
		}
		if err = tab.Back(ctx); err != nil {
			t.Fatal(err)
		}
		if err = tab.Forward(ctx); err != nil {
			t.Fatal(err)
		}
		if err = tab.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		if err = tab.UpdateTarget(ctx); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(tab.URL(), "/second") {
			t.Fatalf("navigation returned before commit: %s", tab.URL())
		}
		if _, err = tab.GetAllLinkedSources(ctx); err != nil {
			t.Fatal(err)
		}
		urls, err := tab.GetAllURLs(ctx)
		if err != nil || len(urls) == 0 {
			t.Fatal(urls, err)
		}
	})
	t.Run("attach and caller-owned profile", func(t *testing.T) {
		attached, err := Connect(ctx, browser.WebSocketURL())
		if err != nil {
			t.Fatal(err)
		}
		if err = attached.TestConnection(ctx); err != nil {
			t.Fatal(err)
		}
		if err = attached.Close(); err != nil {
			t.Fatal(err)
		}
		if err = browser.TestConnection(ctx); err != nil {
			t.Fatalf("attaching closed the original browser: %v", err)
		}
		path := t.TempDir()
		owned, err := Start(ctx, Config{Headless: true, NoSandbox: true, BrowserExecutablePath: executable, UserDataDir: path})
		if err != nil {
			t.Fatal(err)
		}
		if err = owned.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(path); err != nil {
			t.Fatalf("caller-owned profile deleted: %v", err)
		}
	})
	t.Run("storage and contexts", func(t *testing.T) {
		if err := tab.SetLocalStorage(ctx, map[string]string{"key": "value"}); err != nil {
			t.Fatal(err)
		}
		items, err := tab.GetLocalStorage(ctx)
		if err != nil || items["key"] != "value" {
			t.Fatal(items, err)
		}
		if err = browser.Cookies().SetAll(ctx, []cdp.NetworkCookieParam{{Name: "session", Value: "default", URL: cdp.Ptr(server.URL)}}); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "cookies.json")
		if err = browser.Cookies().Save(ctx, file); err != nil {
			t.Fatal(err)
		}
		if err = browser.Cookies().Clear(ctx); err != nil {
			t.Fatal(err)
		}
		if err = browser.Cookies().Load(ctx, file); err != nil {
			t.Fatal(err)
		}
		bc, err := browser.CreateContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer bc.Close()
		contextTab, err := bc.NewTab(ctx, server.URL)
		if err != nil {
			t.Fatal(err)
		}
		var cookies string
		if err = contextTab.Evaluate(ctx, "document.cookie", &cookies); err != nil || cookies != "" {
			t.Fatalf("context cookies %q %v", cookies, err)
		}
		if err = contextTab.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("authenticated context proxy", func(t *testing.T) {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, password, ok := r.BasicAuth()
			if !ok {
				encoded := r.Header.Get("Proxy-Authorization")
				r.Header.Set("Authorization", encoded)
				user, password, ok = r.BasicAuth()
			}
			if !ok || user != "user" || password != "pass" {
				w.WriteHeader(407)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<title>Proxied</title><h1>Proxy works</h1>"))
		}))
		defer proxy.Close()
		bc, err := browser.CreateContext(ctx, ContextOptions{ProxyServer: "http://user:pass@" + proxy.Listener.Addr().String()})
		if err != nil {
			t.Fatal(err)
		}
		defer bc.Close()
		proxied, err := bc.NewTab(ctx, "http://dgodriver.invalid/")
		if err != nil {
			t.Fatal(err)
		}
		e, err := proxied.Select(ctx, "h1")
		if err != nil {
			t.Fatal(err)
		}
		text, err := e.TextAll(ctx)
		if err != nil || text != "Proxy works" {
			t.Fatal(text, err)
		}
	})
	t.Run("Cloudflare helper with local simulated widget", func(t *testing.T) {
		code := `(()=>{const host=document.createElement('div');document.body.prepend(host);const root=host.attachShadow({mode:'closed'});const frame=document.createElement('iframe');frame.style.cssText='width:300px;height:65px;border:0';frame.src='about:blank#challenges.cloudflare.com';frame.srcdoc='<body></body>';frame.onload=()=>{const d=frame.contentDocument;d.body.style.margin='0';d.body.innerHTML='<input type="checkbox" style="position:absolute;left:20px;top:18px;width:28px;height:28px;margin:0">';d.querySelector('input').onclick=()=>document.querySelector('[name=cf-turnstile-response]').value='local-token'};root.appendChild(frame);const result=document.createElement('input');result.type='hidden';result.name='cf-turnstile-response';document.body.appendChild(result)})()`
		if err := tab.Evaluate(ctx, code, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := tab.FindCloudflareChallenge(ctx); err != nil {
			t.Fatal(err)
		}
		if err := tab.VerifyCF(ctx, CloudflareOptions{Timeout: 3 * time.Second, ClickDelay: 100 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		var token string
		if err := tab.Evaluate(ctx, "document.querySelector('[name=cf-turnstile-response]').value", &token); err != nil || token != "local-token" {
			t.Fatal(token, err)
		}
	})
	t.Run("artifacts and downloads", func(t *testing.T) {
		dir := t.TempDir()
		data, err := tab.Screenshot(ctx, ScreenshotOptions{FullPage: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = png.Decode(bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		element, err := tab.Select(ctx, "h1")
		if err != nil {
			t.Fatal(err)
		}
		data, err = element.Screenshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = png.Decode(bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		pdf := filepath.Join(dir, "page.pdf")
		if err = tab.PrintToPDF(ctx, pdf); err != nil {
			t.Fatal(err)
		}
		data, _ = os.ReadFile(pdf)
		if !bytes.HasPrefix(data, []byte("%PDF")) {
			t.Fatal("invalid PDF")
		}
		if err = tab.SaveSnapshot(ctx, filepath.Join(dir, "page.mhtml")); err != nil {
			t.Fatal(err)
		}
		download := filepath.Join(dir, "download.txt")
		if err = tab.DownloadFile(ctx, server.URL+"/file", download); err != nil {
			t.Fatal(err)
		}
		data, err = os.ReadFile(download)
		if err != nil || string(data) != "download content" {
			t.Fatalf("download %q %v", data, err)
		}
		canvas, err := tab.Select(ctx, "#canvas")
		if err != nil {
			t.Fatal(err)
		}
		if err = canvas.Apply(ctx, "el=>{let n=0;const c=el.getContext('2d');function draw(){c.fillStyle=n++%2?'red':'blue';c.fillRect(0,0,100,100);el.__animation=requestAnimationFrame(draw)}draw()}", nil); err != nil {
			t.Fatal(err)
		}
		recording, err := canvas.RecordVideo(ctx, filepath.Join(dir, "canvas.webm"))
		if err != nil {
			t.Fatal(err)
		}
		encoded, finish := context.WithTimeout(ctx, 5*time.Second)
		defer finish()
		if err = poll(encoded, func() (bool, error) {
			var ready bool
			err := canvas.Apply(encoded, "el=>el.__dgodriverRecording.chunks.some(c=>c.size>0)", &ready)
			return ready, err
		}); err != nil {
			t.Fatal("video encoder did not produce data", err)
		}
		if err = recording.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		data, err = os.ReadFile(filepath.Join(dir, "canvas.webm"))
		if err != nil || len(data) < 10 {
			t.Fatal("video", len(data), err)
		}
	})
	if err = browser.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("temporary profile not removed: %v", err)
	}
}
