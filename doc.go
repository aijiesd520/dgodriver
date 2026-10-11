// Package dgodriver controls Chromium browsers directly over Chrome DevTools
// Protocol, without WebDriver. It ports the browser automation features of
// zendriver 0.17.1 to Go. Use Start to launch, Browser.Get to navigate, and
// Tab.Select to obtain an Element. All I/O accepts context.Context. Close the
// browser, event subscriptions, expectations and interceptors when finished.
package dgodriver

// Version is the library's current version.
const Version = "0.1.0"

// ZendriverVersion identifies the upstream version used for the CDP snapshot.
const ZendriverVersion = "0.17.1"
