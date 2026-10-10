# dgodriver

[![Go Reference](https://pkg.go.dev/badge/github.com/aijiesd520/dgodriver.svg)](https://pkg.go.dev/github.com/aijiesd520/dgodriver)
[![Build](https://github.com/aijiesd520/dgodriver/actions/workflows/test.yml/badge.svg)](https://github.com/aijiesd520/dgodriver/actions/workflows/test.yml)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)

A Go library for controlling Chromium-based browsers directly through the Chrome DevTools Protocol (CDP). `dgodriver` brings the browser automation capabilities of [nodriver](https://github.com/ultrafunkamsterdam/nodriver) and [zendriver](https://github.com/cdpdriver/zendriver) to Go, with context-aware operations and typed CDP APIs. It does not require Selenium or ChromeDriver.

The project is at an early stage (`v0.1.0`); APIs and browser compatibility may change as it develops.

## Use cases

`dgodriver` is intended for legitimate browser automation, especially:

- End-to-end and regression testing of web applications you own or are authorized to test.
- Reproducing browser bugs and debugging web applications with a real Chromium engine.
- Testing network behavior, including observing requests and mocking responses in controlled environments.
- Automating internal workflows on systems where you have permission.
- Learning and experimenting with CDP.

It is a general-purpose browser control library. It does not grant access to any website or account, and it cannot guarantee that a site will allow automation.

## Requirements

- Go 1.24 or later.
- Google Chrome, Chromium, Microsoft Edge, or Brave.

The library launches a local browser or connects to an existing Chromium browser over its CDP endpoint. Supported behavior can vary by browser and browser version.

## Install

```sh
go get github.com/aijiesd520/dgodriver
```

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aijiesd520/dgodriver"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	browser, err := dgodriver.Start(ctx, dgodriver.Config{Headless: true})
	if err != nil {
		log.Fatal(err)
	}
	defer browser.Close()

	tab, err := browser.Get(ctx, "https://example.com")
	if err != nil {
		log.Fatal(err)
	}

	var title string
	if err := tab.Evaluate(ctx, "document.title", &title); err != nil {
		log.Fatal(err)
	}
	fmt.Println(title)
}
```

Run the included examples from a checkout:

```sh
go run ./examples/basic
go run ./examples/network
```

## Capabilities

- Launch or attach to Chromium-based browsers, use temporary or persistent profiles, and manage tabs, windows, browser contexts, permissions, and extensions.
- Typed CDP commands, events, parameters, and results. The checked-in CDP snapshot contains 56 domains generated from the protocol bindings in zendriver 0.17.1.
- Navigate pages; query by CSS, XPath, or text; evaluate JavaScript; interact with DOM elements, Shadow DOM, and frames.
- Dispatch keyboard and mouse input, upload files, and work with form controls.
- Subscribe to browser events, wait for requests and responses, and intercept or fulfill network requests.
- Configure authenticated HTTP, HTTPS, and SOCKS5 proxies for isolated browser contexts.
- Manage cookies and local storage, and set a User-Agent.
- Capture screenshots, print pages to PDF, save MHTML snapshots, download files, and record WebM video where the browser and platform permit it.
- Locate and attempt the supported interactive Cloudflare checkbox flow in authorized testing environments. Provider behavior can change, success is not guaranteed, and the helper does not grant permission to evade site access policies.

See [API_PARITY.md](API_PARITY.md) for a detailed mapping from zendriver APIs to their Go counterparts.

## Browser configuration notes

`Config{Headless: true}` launches Chromium in headless mode. In headless mode, dgodriver removes the `Headless` token from the User-Agent it reads from the browser unless you provide a custom User-Agent.

`Config{Expert: true}` injects a script that makes newly created Shadow DOM roots open, so automation code can inspect them. These options are convenience and debugging features, not a comprehensive stealth or fingerprint-masking system. Websites may still identify automation or restrict access.

## Responsible use and disclaimer

You are responsible for how you use this software. Before automating a third-party site, review and follow that site's Terms of Service, automation and access policies, and rate limits. Obtain permission when required. Comply with all applicable local, national, and international laws, including laws concerning computer access, privacy, personal data, and intellectual property.

Use `dgodriver` only with systems, accounts, and data that you own or are authorized to access. Do not use it to bypass authentication, access controls, paywalls, rate limits, CAPTCHAs, anti-bot protections, or other security measures, or to collect data without the required rights or legal basis. The presence of an API or example in this project is not authorization to use it against a service.

This software is provided "AS IS", without warranties of any kind, express or implied. To the extent permitted by law, the authors and maintainers disclaim liability for claims, damages, or other consequences arising from its use or misuse. This notice is not legal advice and does not replace your responsibility to assess whether a particular use is lawful and permitted.

## Development

```sh
go test ./...
go vet ./...
go generate ./cdp
```

Some browser integration checks require a Chromium installation. The GitHub Actions workflow runs the test suite and checks that generated CDP bindings are reproducible.

## License and acknowledgements

This project is distributed under the GNU Affero General Public License, version 3 (AGPL-3.0). See [LICENSE](LICENSE) and [NOTICE](NOTICE) for license and attribution details.

`dgodriver` is based on the work of the nodriver and zendriver projects. The CDP bindings are generated from a checked-in protocol snapshot and reference declarations from zendriver 0.17.1. `github.com/gorilla/websocket` is used for WebSocket transport.

## Star history

[![Star History Chart](https://api.star-history.com/svg?repos=aijiesd520/dgodriver&type=Date)](https://star-history.com/#aijiesd520/dgodriver&Date)
