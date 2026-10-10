package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/aijiesd520/dgodriver"
	"github.com/aijiesd520/dgodriver/cdp"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"message":"from server"}`)
		} else {
			_, _ = fmt.Fprint(w, `<h1>Network example</h1>`)
		}
	}))
	defer site.Close()
	browser, err := dgodriver.Start(ctx, dgodriver.Config{Headless: true})
	if err != nil {
		log.Fatal(err)
	}
	defer browser.Close()
	tab, err := browser.Get(ctx, site.URL)
	if err != nil {
		log.Fatal(err)
	}
	expected, err := tab.ExpectResponse(ctx, ".*/api")
	if err != nil {
		log.Fatal(err)
	}
	defer expected.Close()
	if err = tab.Evaluate(ctx, "fetch('/api')", nil, dgodriver.EvaluateOptions{AwaitPromise: true}); err != nil {
		log.Fatal(err)
	}
	body, err := expected.Body(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(body))
	interception, err := tab.Intercept(ctx, cdp.FetchRequestPattern{URLPattern: cdp.Ptr("*/mock")})
	if err != nil {
		log.Fatal(err)
	}
	defer interception.Close()
	result := make(chan error, 1)
	go func() {
		request, err := interception.Next(ctx)
		if err == nil {
			err = request.Fulfill(ctx, 200, []byte(`{"message":"from Go"}`), cdp.FetchHeaderEntry{Name: "Content-Type", Value: "application/json"})
		}
		result <- err
	}()
	var text string
	if err = tab.Evaluate(ctx, "fetch('/mock').then(r=>r.text())", &text, dgodriver.EvaluateOptions{AwaitPromise: true}); err != nil {
		log.Fatal(err)
	}
	if err = <-result; err != nil {
		log.Fatal(err)
	}
	fmt.Println(text)
}
