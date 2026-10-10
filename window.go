package dgodriver

import (
	"context"
	"fmt"

	"github.com/aijiesd520/dgodriver/cdp"
)

type WindowBounds struct {
	Left, Top, Width, Height int
	State                    string
}

func (t *Tab) GetWindow(ctx context.Context) (int, WindowBounds, error) {
	r, err := cdp.Browser.GetWindowForTarget(cdp.BrowserGetWindowForTargetParams{TargetID: cdp.Ptr(cdp.TargetTargetID(t.TargetID()))}).Execute(ctx, t.browser)
	var b WindowBounds
	if r.Bounds.Left != nil {
		b.Left = *r.Bounds.Left
	}
	if r.Bounds.Top != nil {
		b.Top = *r.Bounds.Top
	}
	if r.Bounds.Width != nil {
		b.Width = *r.Bounds.Width
	}
	if r.Bounds.Height != nil {
		b.Height = *r.Bounds.Height
	}
	if r.Bounds.WindowState != nil {
		b.State = string(*r.Bounds.WindowState)
	}
	return int(r.WindowID), b, err
}
func (t *Tab) SetWindowState(ctx context.Context, state string) error {
	switch state {
	case "normal", "maximized", "minimized", "fullscreen":
	default:
		return fmt.Errorf("dgodriver: invalid window state %q", state)
	}
	id, _, err := t.GetWindow(ctx)
	if err != nil {
		return err
	}
	_, err = cdp.Browser.SetWindowBounds(cdp.BrowserSetWindowBoundsParams{WindowID: cdp.BrowserWindowID(id), Bounds: cdp.BrowserBounds{WindowState: cdp.Ptr(cdp.BrowserWindowState(state))}}).Execute(ctx, t.browser)
	return err
}
func (t *Tab) SetWindowSize(ctx context.Context, left, top, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("dgodriver: window dimensions must be positive")
	}
	if err := t.SetWindowState(ctx, "normal"); err != nil {
		return err
	}
	id, _, err := t.GetWindow(ctx)
	if err != nil {
		return err
	}
	_, err = cdp.Browser.SetWindowBounds(cdp.BrowserSetWindowBoundsParams{WindowID: cdp.BrowserWindowID(id), Bounds: cdp.BrowserBounds{Left: &left, Top: &top, Width: &width, Height: &height}}).Execute(ctx, t.browser)
	return err
}
func (t *Tab) Maximize(ctx context.Context) error   { return t.SetWindowState(ctx, "maximized") }
func (t *Tab) Minimize(ctx context.Context) error   { return t.SetWindowState(ctx, "minimized") }
func (t *Tab) Fullscreen(ctx context.Context) error { return t.SetWindowState(ctx, "fullscreen") }
func (t *Tab) Medimize(ctx context.Context) error   { return t.SetWindowState(ctx, "normal") }
