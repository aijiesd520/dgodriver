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
		_, _ = fmt.Fprint(w, `<title>DgoDriver</title><h1>Hello Go</h1><input id="name"><button onclick="document.querySelector('h1').textContent='Hello '+document.querySelector('#name').value">Submit</button>`)
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
	input, err := tab.Select(ctx, "#name")
	if err != nil {
		log.Fatal(err)
	}
	if err = input.SendKeys(ctx, "dgodriver"); err != nil {
		log.Fatal(err)
	}
	button, err := tab.Find(ctx, "Submit")
	if err != nil {
		log.Fatal(err)
	}
	if err = button.MouseClick(ctx); err != nil {
		log.Fatal(err)
	}
	var heading string
	if err = tab.Evaluate(ctx, "document.querySelector('h1').textContent", &heading); err != nil {
		log.Fatal(err)
	}
	fmt.Println(heading)
	version, err := cdp.Browser.GetVersion().Execute(ctx, browser)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Browser:", version.Product)
}
