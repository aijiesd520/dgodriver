// Package dgodriver controls Chromium browsers directly over Chrome DevTools
// Protocol, without WebDriver. It ports the browser automation features of
// zendriver 0.17.1 to Go. Use Start to launch, Browser.Get to navigate, and
// Tab.Select to obtain an Element. All I/O accepts context.Context. Close the
// browser, event subscriptions, expectations and interceptors when finished.
package dgodriver

const Version = "0.1.0"
const ZendriverVersion = "0.17.1"
