package dgodriver

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aijiesd520/dgodriver/cdp"
)

func runtimeOS() string { return runtime.GOOS }

type KeyModifiers int

const (
	Alt     KeyModifiers = 1
	Control KeyModifiers = 2
	Meta    KeyModifiers = 4
	Shift   KeyModifiers = 8
)

type KeyInput struct {
	Text      string
	Key       string
	Modifiers KeyModifiers
}
type keyDefinition struct {
	Code string
	VK   int
	Text string
}

var specialKeys = map[string]keyDefinition{
	"Enter": {"Enter", 13, "\r"}, "Tab": {"Tab", 9, "\t"}, "Backspace": {"Backspace", 8, ""}, "Delete": {"Delete", 46, ""}, "Escape": {"Escape", 27, ""}, "ArrowLeft": {"ArrowLeft", 37, ""}, "ArrowUp": {"ArrowUp", 38, ""}, "ArrowRight": {"ArrowRight", 39, ""}, "ArrowDown": {"ArrowDown", 40, ""}, "Home": {"Home", 36, ""}, "End": {"End", 35, ""}, "PageUp": {"PageUp", 33, ""}, "PageDown": {"PageDown", 34, ""}, "Insert": {"Insert", 45, ""}, "Control": {"ControlLeft", 17, ""}, "Shift": {"ShiftLeft", 16, ""}, "Alt": {"AltLeft", 18, ""}, "Meta": {"MetaLeft", 91, ""}, "CapsLock": {"CapsLock", 20, ""}, "NumLock": {"NumLock", 144, ""}, "ScrollLock": {"ScrollLock", 145, ""}, "Pause": {"Pause", 19, ""}, "PrintScreen": {"PrintScreen", 44, ""}, "ContextMenu": {"ContextMenu", 93, ""}, " ": {"Space", 32, " "},
}

func keyInfo(key string) (keyDefinition, error) {
	if info, ok := specialKeys[key]; ok {
		return info, nil
	}
	if i := strings.Index(")!@#$%^&*(", key); len(key) == 1 && i >= 0 {
		return keyDefinition{"Digit" + fmt.Sprint(i), 48 + i, key}, nil
	}
	if i := strings.Index("_+{}:\"<>?|~", key); len(key) == 1 && i >= 0 {
		base := string("-=[];',./\\`"[i])
		info, err := keyInfo(base)
		info.Text = key
		return info, err
	}
	if strings.HasPrefix(key, "F") {
		var n int
		if _, err := fmt.Sscanf(key, "F%d", &n); err == nil && n >= 1 && n <= 24 {
			return keyDefinition{key, 111 + n, ""}, nil
		}
	}
	if utf8.RuneCountInString(key) != 1 {
		return keyDefinition{}, fmt.Errorf("dgodriver: unknown key %q", key)
	}
	r, _ := utf8.DecodeRuneInString(key)
	info := keyDefinition{Text: key}
	if unicode.IsLetter(r) && r < 128 {
		info.Code = "Key" + strings.ToUpper(key)
		info.VK = int(unicode.ToUpper(r))
	} else if r >= '0' && r <= '9' {
		info.Code = "Digit" + key
		info.VK = int(r)
	} else {
		codes := map[rune]keyDefinition{'-': {"Minus", 189, "-"}, '=': {"Equal", 187, "="}, '[': {"BracketLeft", 219, "["}, ']': {"BracketRight", 221, "]"}, ';': {"Semicolon", 186, ";"}, '\'': {"Quote", 222, "'"}, ',': {"Comma", 188, ","}, '.': {"Period", 190, "."}, '/': {"Slash", 191, "/"}, '\\': {"Backslash", 220, "\\"}, '`': {"Backquote", 192, "`"}}
		if d, ok := codes[r]; ok {
			info = d
		}
	}
	return info, nil
}
func (t *Tab) InsertText(ctx context.Context, text string) error {
	_, err := cdp.Input.InsertText(cdp.InputInsertTextParams{Text: text}).Execute(ctx, t)
	return err
}

// TypeText emits down/press/up keyboard events, including Unicode text.
func (t *Tab) TypeText(ctx context.Context, text string) error {
	for _, r := range text {
		key := string(r)
		if r == '\n' || r == '\r' {
			key = "Enter"
		}
		if r == '\t' {
			key = "Tab"
		}
		if err := t.PressKey(ctx, key); err != nil {
			return err
		}
	}
	return nil
}
func (t *Tab) DispatchKeyEvents(ctx context.Context, events ...cdp.InputDispatchKeyEventParams) error {
	for _, e := range events {
		if _, err := cdp.Input.DispatchKeyEvent(e).Execute(ctx, t); err != nil {
			return err
		}
	}
	return nil
}
func (t *Tab) SendKeys(ctx context.Context, inputs ...KeyInput) error {
	for _, input := range inputs {
		var err error
		if input.Key != "" {
			err = t.PressKey(ctx, input.Key, input.Modifiers)
		} else {
			err = t.TypeText(ctx, input.Text)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (t *Tab) PressKey(ctx context.Context, key string, modifiers ...KeyModifiers) error {
	info, err := keyInfo(key)
	if err != nil {
		return err
	}
	var mods KeyModifiers
	for _, m := range modifiers {
		mods |= m
	}
	if len(key) == 1 && (key[0] >= 'A' && key[0] <= 'Z' || strings.ContainsAny(key, ")!@#$%^&*(_+{}:\"<>?|~")) {
		mods |= Shift
	}
	text := info.Text
	if mods&(Control|Meta|Alt) != 0 {
		text = ""
	}
	// Physical modifier down/up events are observable to page scripts too.
	held := []struct {
		mask KeyModifiers
		key  string
	}{{Control, "Control"}, {Alt, "Alt"}, {Shift, "Shift"}, {Meta, "Meta"}}
	var pressed []string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2e9)
		defer cancel()
		for i := len(pressed) - 1; i >= 0; i-- {
			d := specialKeys[pressed[i]]
			_ = t.Send(cleanup, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": pressed[i], "code": d.Code, "windowsVirtualKeyCode": d.VK, "modifiers": int(mods)}, nil)
		}
	}()
	for _, h := range held {
		if mods&h.mask != 0 {
			d := specialKeys[h.key]
			if err = t.Send(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "key": h.key, "code": d.Code, "windowsVirtualKeyCode": d.VK, "modifiers": int(mods)}, nil); err != nil {
				return err
			}
			pressed = append(pressed, h.key)
		}
	}
	eventType := "rawKeyDown"
	if text != "" {
		eventType = "keyDown"
	}
	params := map[string]any{"type": eventType, "key": key, "code": info.Code, "windowsVirtualKeyCode": info.VK, "nativeVirtualKeyCode": info.VK, "modifiers": int(mods)}
	if text != "" {
		params["text"] = text
		params["unmodifiedText"] = info.Text
	}
	if err = t.Send(ctx, "Input.dispatchKeyEvent", params, nil); err != nil {
		return err
	}
	params["type"] = "keyUp"
	delete(params, "text")
	delete(params, "unmodifiedText")
	return t.Send(ctx, "Input.dispatchKeyEvent", params, nil)
}

type MouseOptions struct {
	Button     string
	Modifiers  KeyModifiers
	ClickCount int
	Hold       bool
}

func mouseOptions(options []MouseOptions) MouseOptions {
	opt := MouseOptions{Button: "left", ClickCount: 1}
	if len(options) > 0 {
		opt = options[0]
		if opt.Button == "" {
			opt.Button = "left"
		}
		if opt.ClickCount == 0 {
			opt.ClickCount = 1
		}
	}
	return opt
}
func buttonMask(button string) int {
	switch button {
	case "left":
		return 1
	case "right":
		return 2
	case "middle":
		return 4
	case "back":
		return 8
	case "forward":
		return 16
	}
	return 0
}
func (t *Tab) mouseEvent(ctx context.Context, kind string, x, y float64, opt MouseOptions, buttons int) error {
	return t.Send(ctx, "Input.dispatchMouseEvent", map[string]any{"type": kind, "x": x, "y": y, "button": opt.Button, "buttons": buttons, "modifiers": int(opt.Modifiers), "clickCount": opt.ClickCount}, nil)
}
func (t *Tab) MouseDown(ctx context.Context, x, y float64, options ...MouseOptions) error {
	opt := mouseOptions(options)
	if buttonMask(opt.Button) == 0 {
		return fmt.Errorf("dgodriver: invalid mouse button %q", opt.Button)
	}
	t.mouseMu.Lock()
	defer t.mouseMu.Unlock()
	buttons := t.mouseButtons | buttonMask(opt.Button)
	err := t.mouseEvent(ctx, "mousePressed", x, y, opt, buttons)
	if err == nil {
		t.mouseX = x
		t.mouseY = y
		t.mouseButtons = buttons
	}
	return err
}
func (t *Tab) MouseUp(ctx context.Context, x, y float64, options ...MouseOptions) error {
	opt := mouseOptions(options)
	if buttonMask(opt.Button) == 0 {
		return fmt.Errorf("dgodriver: invalid mouse button %q", opt.Button)
	}
	t.mouseMu.Lock()
	defer t.mouseMu.Unlock()
	buttons := t.mouseButtons &^ buttonMask(opt.Button)
	err := t.mouseEvent(ctx, "mouseReleased", x, y, opt, buttons)
	if err == nil {
		t.mouseX = x
		t.mouseY = y
		t.mouseButtons = buttons
	}
	return err
}
func (t *Tab) MouseMove(ctx context.Context, x, y float64, steps ...int) error {
	n := 10
	if len(steps) > 0 {
		n = steps[0]
	}
	if n < 1 {
		return fmt.Errorf("dgodriver: mouse steps must be positive")
	}
	if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		return fmt.Errorf("dgodriver: mouse coordinates must be finite")
	}
	t.mouseMu.Lock()
	defer t.mouseMu.Unlock()
	sx, sy := t.mouseX, t.mouseY
	for i := 1; i <= n; i++ {
		nx := sx + (x-sx)*float64(i)/float64(n)
		ny := sy + (y-sy)*float64(i)/float64(n)
		if err := t.mouseEvent(ctx, "mouseMoved", nx, ny, MouseOptions{Button: "none"}, t.mouseButtons); err != nil {
			return err
		}
		t.mouseX = nx
		t.mouseY = ny
	}
	return nil
}
func (t *Tab) MouseClick(ctx context.Context, x, y float64, options ...MouseOptions) error {
	opt := mouseOptions(options)
	if err := t.MouseMove(ctx, x, y, 1); err != nil {
		return err
	}
	if err := t.MouseDown(ctx, x, y, opt); err != nil {
		return err
	}
	if opt.Hold {
		return nil
	}
	return t.MouseUp(ctx, x, y, opt)
}
func (t *Tab) MouseDrag(ctx context.Context, fromX, fromY, toX, toY float64, steps ...int) error {
	if err := t.MouseMove(ctx, fromX, fromY, 1); err != nil {
		return err
	}
	if err := t.MouseDown(ctx, fromX, fromY); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2e9)
		defer cancel()
		_ = t.MouseUp(cleanup, toX, toY)
	}()
	return t.MouseMove(ctx, toX, toY, steps...)
}
func (t *Tab) ScrollDown(ctx context.Context, amount int, speed ...int) error {
	velocity := 800
	if len(speed) > 0 {
		velocity = speed[0]
	}
	if velocity <= 0 {
		return fmt.Errorf("dgodriver: scroll speed must be positive")
	}
	layout, err := cdp.Page.GetLayoutMetrics().Execute(ctx, t)
	if err != nil {
		return err
	}
	viewport := layout.CSSLayoutViewport
	distance := -float64(viewport.ClientHeight) * float64(amount) / 100
	_, err = cdp.Input.SynthesizeScrollGesture(cdp.InputSynthesizeScrollGestureParams{X: float64(viewport.ClientWidth) / 2, Y: float64(viewport.ClientHeight) / 2, YDistance: &distance, Speed: &velocity, PreventFling: cdp.Ptr(true), GestureSourceType: cdp.Ptr(cdp.InputGestureSourceType("mouse"))}).Execute(ctx, t)
	return err
}
func (t *Tab) ScrollUp(ctx context.Context, amount int, speed ...int) error {
	return t.ScrollDown(ctx, -amount, speed...)
}
func (t *Tab) IsScrolledToBottom(ctx context.Context) (bool, error) {
	var result bool
	err := t.Evaluate(ctx, "window.innerHeight+window.scrollY>=document.documentElement.scrollHeight-1", &result)
	return result, err
}
func (t *Tab) FlashPoint(ctx context.Context, x, y float64, duration ...float64) error {
	seconds := 0.5
	if len(duration) > 0 {
		seconds = duration[0]
	}
	return t.Evaluate(ctx, fmt.Sprintf("(()=>{const p=document.createElement('div');p.style.cssText='position:fixed;pointer-events:none;z-index:2147483647;background:#ff4081;border-radius:50%%;width:12px;height:12px;left:%fpx;top:%fpx';document.documentElement.appendChild(p);setTimeout(()=>p.remove(),%f)})()", x-6, y-6, seconds*1000), nil)
}
